import { VIEWER, type Store, type RepoRecord, type PullRecord } from './store.ts';
import { readObject, parseCommit, flattenTree, buildTree, writeCommit, getRef, setRef } from './gitstore.ts';
import { sendJson, type Router } from './app.ts';

type TreeMap = Map<string, { mode: string; sha: string }>;
type Change = { status: 'added' | 'removed' | 'modified'; baseSha?: string; headSha?: string };

function resolveRef(repo: RepoRecord, ref: string): string | undefined {
  const sha = getRef(repo, `refs/heads/${ref}`) ?? (/^[a-f0-9]{40}$/i.test(ref) ? ref.toLowerCase() : undefined);
  return sha && readObject(sha)?.type === 'commit' ? sha : undefined;
}

function ancestors(start: string): string[] {
  const queue = [start], seen = new Set<string>(), result: string[] = [];
  for (let i = 0; i < queue.length; i++) {
    const sha = queue[i];
    if (seen.has(sha)) continue;
    seen.add(sha);
    const object = readObject(sha);
    if (object?.type !== 'commit') continue;
    result.push(sha);
    queue.push(...parseCommit(object.data).parents);
  }
  return result;
}

function mergeBase(_repo: RepoRecord, a: string, b: string): string | undefined {
  const common = new Set(ancestors(a));
  return ancestors(b).find(sha => common.has(sha));
}

function treeMap(sha: string | undefined): TreeMap {
  const object = sha ? readObject(sha) : undefined;
  return object?.type === 'commit' ? flattenTree(parseCommit(object.data).tree) : new Map();
}

function diffTrees(base: TreeMap, head: TreeMap): Map<string, Change> {
  const changes = new Map<string, Change>();
  for (const path of [...new Set([...base.keys(), ...head.keys()])].sort()) {
    const baseSha = base.get(path)?.sha, headSha = head.get(path)?.sha;
    if (baseSha !== headSha) changes.set(path, { status: !baseSha ? 'added' : !headSha ? 'removed' : 'modified', baseSha, headSha });
  }
  return changes;
}

function lines(sha?: string): string[] {
  const text = sha ? readObject(sha)?.data.toString('utf8') ?? '' : '';
  if (!text) return [];
  const result = text.split('\n');
  if (result[result.length - 1] === '') result.pop();
  return result;
}

function lineCounts(change: Change) {
  const base = lines(change.baseSha), head = lines(change.headSha);
  // A rolling LCS row keeps memory proportional to the head file's line count.
  const row = new Array<number>(head.length + 1).fill(0);
  for (const line of base) {
    let diagonal = 0;
    for (let j = 1; j <= head.length; j++) {
      const previous = row[j];
      row[j] = line === head[j - 1] ? diagonal + 1 : Math.max(row[j], row[j - 1]);
      diagonal = previous;
    }
  }
  return { additions: head.length - row[head.length], deletions: base.length - row[head.length] };
}

function pullFiles(record: PullRecord, repo: RepoRecord) {
  const head = resolveRef(repo, record.headRef), base = resolveRef(repo, record.baseRef);
  const mb = head && base ? mergeBase(repo, head, base) : undefined;
  return [...diffTrees(treeMap(mb), treeMap(head))].map(([filename, change]) => {
    const counts = lineCounts(change);
    return { filename, status: change.status, sha: change.headSha ?? null, ...counts, changes: counts.additions + counts.deletions, blob_url: '', raw_url: '', contents_url: '' };
  });
}

function pullJson(record: PullRecord, repo: RepoRecord) {
  const files = pullFiles(record, repo);
  return {
    id: record.number, number: record.number, title: record.title, body: record.body,
    state: record.state, merged: record.merged, merged_at: record.mergedAt, merge_commit_sha: record.mergeCommitSha,
    draft: false, user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    head: { ref: record.headRef, sha: resolveRef(repo, record.headRef) ?? null, label: `${VIEWER.login}:${record.headRef}` },
    base: { ref: record.baseRef, sha: resolveRef(repo, record.baseRef) ?? null, label: `${VIEWER.login}:${record.baseRef}` },
    created_at: record.createdAt, updated_at: record.updatedAt, closed_at: record.state === 'closed' ? record.mergedAt : null,
    html_url: '', url: '', issue_url: '', changed_files: files.length,
    additions: files.reduce((sum, file) => sum + file.additions, 0), deletions: files.reduce((sum, file) => sum + file.deletions, 0)
  };
}

export function registerPullRoutes(router: Router, store: Store) {
  const root = '/api/v3/repos/:owner/:repo';
  const find = (p: Record<string, string>) => p.owner === VIEWER.login ? store.repos.get(store.repoKey(p.owner, p.repo)) : undefined;
  const findPull = (repo: RepoRecord, number: string) => {
    const item = /^[1-9]\d*$/.test(number) ? repo.items.get(Number(number)) : undefined;
    return item?.kind === 'pull' ? item : undefined;
  };
  router.register('POST', `${root}/pulls`, (_req, res, p, body: any) => {
    const repo = find(p);
    if (!repo) return sendJson(res, 404, { message: 'Not Found' });
    if (!body || typeof body.title !== 'string' || !body.title.trim() || typeof body.head !== 'string' || typeof body.base !== 'string' || !resolveRef(repo, body.head) || !resolveRef(repo, body.base) || (body.body !== undefined && typeof body.body !== 'string')) return sendJson(res, 422, { message: 'Validation Failed' });
    const now = new Date().toISOString(), number = ++repo.numberSeq;
    const pull: PullRecord = { number, kind: 'pull', title: body.title, body: body.body ?? '', state: 'open', headRef: body.head, baseRef: body.base, merged: false, mergedAt: null, mergeCommitSha: null, createdAt: now, updatedAt: now, seq: number, comments: [] };
    repo.items.set(number, pull);
    sendJson(res, 201, pullJson(pull, repo));
  });
  router.register('GET', `${root}/pulls`, (req, res, p) => {
    const repo = find(p);
    if (!repo) return sendJson(res, 404, { message: 'Not Found' });
    const query = new URL(req.url || '', 'http://localhost').searchParams, state = query.get('state') || 'open';
    const positive = (value: string | null, fallback: number) => value && Number.isFinite(Number(value)) ? Math.max(1, Math.floor(Number(value))) : fallback;
    const size = Math.min(100, positive(query.get('per_page'), 30)), page = positive(query.get('page'), 1);
    const pulls = [...repo.items.values()].filter((item): item is PullRecord => item.kind === 'pull' && (state === 'all' || item.state === state)).sort((a, b) => b.createdAt.localeCompare(a.createdAt) || b.seq - a.seq);
    sendJson(res, 200, pulls.slice((page - 1) * size, page * size).map(pull => pullJson(pull, repo)));
  });
  for (const suffix of ['', '/files']) router.register('GET', `${root}/pulls/:number${suffix}`, (_req, res, p) => {
    const repo = find(p), pull = repo && findPull(repo, p.number);
    if (!repo || !pull) return sendJson(res, 404, { message: 'Not Found' });
    sendJson(res, 200, suffix ? pullFiles(pull, repo) : pullJson(pull, repo));
  });
  router.register('PUT', `${root}/pulls/:number/merge`, (_req, res, p, body: any) => {
    const repo = find(p), pull = repo && findPull(repo, p.number);
    if (!repo || !pull) return sendJson(res, 404, { message: 'Not Found' });
    if (pull.merged || pull.state === 'closed') return sendJson(res, 405, { message: 'Pull request already merged' });
    const baseTip = resolveRef(repo, pull.baseRef), headTip = resolveRef(repo, pull.headRef);
    if (!baseTip || !headTip) return sendJson(res, 405, { message: 'Pull Request is not mergeable' });
    const common = treeMap(mergeBase(repo, baseTip, headTip)), base = treeMap(baseTip), head = treeMap(headTip);
    const baseChanges = diffTrees(common, base), headChanges = diffTrees(common, head);
    for (const [path, change] of headChanges) {
      if (baseChanges.has(path) && baseChanges.get(path)!.headSha !== change.headSha) return sendJson(res, 405, { message: 'Pull Request is not mergeable' });
    }
    for (const [path, change] of headChanges) {
      if (change.headSha) base.set(path, head.get(path)!);
      else base.delete(path);
    }
    const now = new Date(), identity = `${VIEWER.name} <${VIEWER.email}> ${Math.floor(now.getTime() / 1000)} +0000`;
    const title = typeof body?.commit_title === 'string' && body.commit_title ? body.commit_title : pull.title;
    const message = typeof body?.commit_message === 'string' && body.commit_message ? `${title}\n\n${body.commit_message}` : title;
    const sha = writeCommit({ tree: buildTree(base), parents: body?.merge_method === 'merge' ? [baseTip, headTip] : [baseTip], author: identity, committer: identity, message });
    setRef(repo, `refs/heads/${pull.baseRef}`, sha);
    pull.state = 'closed'; pull.merged = true; pull.mergedAt = pull.updatedAt = now.toISOString(); pull.mergeCommitSha = sha;
    sendJson(res, 200, { merged: true, sha, message: 'Pull request successfully merged and closed.' });
  });
}
