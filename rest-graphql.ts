import { sendJson, type Router } from './app.ts';
import { VIEWER, type Store, type RepoRecord, type IssueRecord, type PullRecord } from './store.ts';
import { getRef } from './gitstore.ts';
import { pullFiles, pullMergeable, mergePull } from './rest-pulls.ts';

type Field = { name: string; alias: string; args: Record<string, any>; fields: Field[] };
const opaque = (text: string) => Buffer.from(text).toString('base64');
const decode = (text: string) => Buffer.from(text, 'base64').toString();

// The wire uses operation declarations, variables, aliases and inline type fragments.
function parse(query: string, variables: Record<string, any>): Field[] {
  const tokens: string[] = [];
  const lexer = /\s+|#[^\n]*|,|(\.\.\.|"(?:\\.|[^"\\])*"|[_A-Za-z][_0-9A-Za-z]*|-?\d+(?:\.\d+)?|[!$():{}\[\]=])/gy;
  let pos = 0;
  while (pos < query.length) {
    lexer.lastIndex = pos;
    const match = lexer.exec(query);
    if (!match) throw new Error(`Unexpected token at ${pos}`);
    pos = lexer.lastIndex;
    if (match[1]) tokens.push(match[1]);
  }
  let i = 0;
  const take = (expected?: string): string => {
    const token = tokens[i++];
    if (!token || (expected && token !== expected)) throw new Error(`Expected ${expected ?? 'token'}`);
    return token;
  };
  const value = (): any => {
    const token = take();
    if (token === '$') return variables[take()];
    if (token === '{') {
      const result: Record<string, any> = {};
      while (tokens[i] !== '}') { const key = take(); take(':'); result[key] = value(); }
      take('}'); return result;
    }
    if (token === '[') { const result = []; while (tokens[i] !== ']') result.push(value()); take(']'); return result; }
    if (token.startsWith('"')) return JSON.parse(token);
    if (token === 'null') return null;
    if (token === 'true' || token === 'false') return token === 'true';
    return /^-?\d/.test(token) ? Number(token) : token;
  };
  const selection = (): Field[] => {
    take('{'); const fields: Field[] = [];
    while (tokens[i] !== '}') {
      if (tokens[i] === '...') { take(); take('on'); take(); fields.push(...selection()); continue; }
      let name = take(); const alias = name;
      if (tokens[i] === ':') { take(); name = take(); }
      const args: Record<string, any> = {};
      if (tokens[i] === '(') {
        take(); while (tokens[i] !== ')') { const key = take(); take(':'); args[key] = value(); } take(')');
      }
      fields.push({ name, alias, args, fields: tokens[i] === '{' ? selection() : [] });
    }
    take('}'); return fields;
  };
  if (tokens[i] !== '{') {
    const operation = take(); if (!['query', 'mutation'].includes(operation)) throw new Error('Unknown operation');
    if (tokens[i] !== '(' && tokens[i] !== '{') take();
    if (tokens[i] === '(') { let depth = 0; do { const t = take(); if (t === '(') depth++; if (t === ')') depth--; } while (depth); }
  }
  const fields = selection(); if (i !== tokens.length) throw new Error('Unexpected trailing tokens'); return fields;
}

const schema: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'author', 'labels', 'comments'],
  PullRequest: ['id', 'number', 'title', 'body', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files', 'createdAt', 'updatedAt', 'author'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'isPrivate', 'createdAt', 'updatedAt'],
};

export function registerGraphqlRoutes(router: Router, store: Store) {
  const user = { login: VIEWER.login, id: opaque(`User:${VIEWER.id}`) };
  const itemView = (repo: RepoRecord, item: IssueRecord | PullRecord): any => ({
    id: opaque(`${item.kind === 'issue' ? 'Issue' : 'PullRequest'}:${repo.id}:${item.number}`),
    number: item.number, title: item.title, body: item.body,
    state: item.kind === 'pull' && item.merged ? 'MERGED' : item.state.toUpperCase(),
    createdAt: item.createdAt, updatedAt: item.updatedAt, author: user,
    labels: { nodes: item.kind === 'issue' ? item.labels.map(name => ({ name })) : [] },
    comments: { totalCount: item.comments.length },
    ...(item.kind === 'pull' ? {
      headRefName: item.headRef, baseRefName: item.baseRef, merged: item.merged,
      mergeable: pullMergeable(repo, item) ? 'MERGEABLE' : 'CONFLICTING',
      files: (args: any) => connection(pullFiles(item, repo).map(file => ({ path: file.filename })), args),
    } : {}),
  });
  const connection = (items: any[], args: any) => {
    const start = args.after ? Number(decode(args.after)) + 1 : 0;
    const nodes = items.slice(start, start + (args.first ?? 100));
    const edges = nodes.map((node, index) => ({ node, cursor: opaque(String(start + index)) }));
    return { nodes, edges, totalCount: items.length, pageInfo: { hasNextPage: start + nodes.length < items.length, endCursor: edges.at(-1)?.cursor ?? null } };
  };
  const repoView = (repo: RepoRecord): any => ({
    id: opaque(`Repository:${repo.id}`), databaseId: repo.id, name: repo.name, nameWithOwner: repo.fullName,
    owner: user, defaultBranchRef: { name: repo.defaultBranch, target: { oid: getRef(repo, `refs/heads/${repo.defaultBranch}`) ?? null } },
    viewerPermission: 'ADMIN', hasIssuesEnabled: true, isPrivate: repo.private, createdAt: repo.createdAt, updatedAt: repo.updatedAt,
    issue: (args: any) => { const item = repo.items.get(args.number); return item?.kind === 'issue' ? itemView(repo, item) : null; },
    pullRequest: (args: any) => { const item = repo.items.get(args.number); return item?.kind === 'pull' ? itemView(repo, item) : null; },
    issues: (args: any) => {
      const items = [...repo.items.values()].filter(item => item.kind === 'issue' && (!args.states || args.states.includes(item.state.toUpperCase())));
      if (args.orderBy) items.sort((a, b) => (a.createdAt.localeCompare(b.createdAt) || a.seq - b.seq) * (args.orderBy.direction === 'DESC' ? -1 : 1));
      return connection(items.map(item => itemView(repo, item)), args);
    },
    pullRequests: (args: any) => connection([...repo.items.values()].filter(item => item.kind === 'pull').map(item => itemView(repo, item)), args),
  });
  const locate = (id: string) => {
    const [kind, repoId, number] = decode(id).split(':');
    const repo = [...store.repos.values()].find(repo => repo.id === Number(repoId));
    if (!repo) throw new Error('Unknown node');
    const item = repo.items.get(Number(number));
    return { kind, repo, item };
  };
  const execute = (operation: any) => {
    try {
      if (typeof operation?.query !== 'string') throw new Error('Expected query document');
      const fields = parse(operation.query, operation.variables ?? {});
      const mutate = (name: string, args: any) => {
        const input = args.input;
        const { repo, item } = locate(input.repositoryId ?? input.issueId ?? input.pullRequestId ?? input.subjectId);
        const now = new Date().toISOString();
        if (name === 'createIssue' || name === 'createPullRequest') {
          if (typeof input.title !== 'string' || !input.title.trim()) throw new Error('Title required');
          const number = ++repo.numberSeq;
          const common = { number, title: input.title, body: input.body ?? '', state: 'open' as const, createdAt: now, updatedAt: now, seq: number, comments: [] };
          let created: IssueRecord | PullRecord;
          if (name === 'createIssue') created = { ...common, kind: 'issue', labels: [], closedAt: null };
          else {
            if (!getRef(repo, `refs/heads/${input.headRefName}`) || !getRef(repo, `refs/heads/${input.baseRefName}`)) throw new Error('Unknown branch');
            created = { ...common, kind: 'pull', headRef: input.headRefName, baseRef: input.baseRefName, merged: false, mergedAt: null, mergeCommitSha: null };
          }
          repo.items.set(number, created);
          return { [name === 'createIssue' ? 'issue' : 'pullRequest']: itemView(repo, created) };
        }
        if (!item) throw new Error('Unknown subject');
        if (name === 'closeIssue' && item.kind === 'issue') {
          item.state = 'closed'; item.closedAt = item.updatedAt = now; return { issue: itemView(repo, item) };
        }
        if (name === 'mergePullRequest' && item.kind === 'pull') {
          mergePull(repo, item, { merge_method: String(input.mergeMethod ?? 'MERGE').toLowerCase() }); return { pullRequest: itemView(repo, item) };
        }
        if (name === 'addComment') {
          if (typeof input.body !== 'string' || !input.body.trim()) throw new Error('Comment body required');
          const comment = { id: store.nextId(), body: input.body, createdAt: now, updatedAt: now };
          item.comments.push(comment); item.updatedAt = now;
          return { commentEdge: { node: { id: opaque(`Comment:${comment.id}`), body: comment.body } } };
        }
        throw new Error(`Invalid mutation ${name}`);
      };
      const root: any = {
        viewer: user,
        repository: (args: any) => { const repo = store.repos.get(store.repoKey(args.owner, args.name)); return repo ? repoView(repo) : null; },
        node: (args: any) => { const { repo, item, kind } = locate(args.id); return kind === 'Repository' ? repoView(repo) : item ? itemView(repo, item) : null; },
        __type: (args: any) => args.name === 'SearchType' ? { enumValues: ['ISSUE', 'REPOSITORY', 'USER'].map(name => ({ name })) } : schema[args.name] ? { fields: schema[args.name].map(name => ({ name })) } : null,
      };
      for (const name of ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment']) root[name] = (args: any) => mutate(name, args);
      const project = (object: any, selection: Field[]): any => {
        if (object == null) return null;
        if (Array.isArray(object)) return object.map(entry => project(entry, selection));
        const result: Record<string, any> = {};
        for (const field of selection) {
          if (!Object.hasOwn(object, field.name)) throw new Error(`Cannot query field "${field.name}"`);
          const value = typeof object[field.name] === 'function' ? object[field.name](field.args) : object[field.name];
          result[field.alias] = field.fields.length ? project(value, field.fields) : value;
        }
        return result;
      };
      return { data: project(root, fields) };
    } catch (error) { return { data: null, errors: [{ message: error instanceof Error ? error.message : String(error) }] }; }
  };
  router.register('POST', '/api/graphql', (_req, res, _params, body) => sendJson(res, 200, Array.isArray(body) ? body.map(execute) : execute(body)));
}
