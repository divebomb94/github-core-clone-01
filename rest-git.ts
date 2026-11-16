import { sendJson, type Router, type Handler } from './app.ts';
import { VIEWER, type Store, type RepoRecord } from './store.ts';
import { readObject, parseCommit, getRef, setRef, deleteRef, listRefs } from './gitstore.ts';
import { pktLine, pktFlush, pktDelim, parsePktLines, buildPack, parsePack, collectFetchObjects, shallowBoundary, isAncestor } from './pack.ts';

const ZERO = '0'.repeat(40);
const headTarget = (repo: RepoRecord) => `refs/heads/${repo.head}`;
const refList = (repo: RepoRecord): Array<{ name: string; oid: string }> =>
  listRefs(repo, 'refs/').map(([name, oid]) => ({ name, oid })).sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : 0);

function peelToCommit(sha: string): string {
  const seen = new Set<string>();
  while (readObject(sha)?.type === 'tag') {
    if (seen.has(sha)) throw new Error('Cyclic tag');
    seen.add(sha);
    const match = /^object ([0-9a-f]{40})$/m.exec(readObject(sha)!.data.toString());
    if (!match) throw new Error('Invalid tag');
    sha = match[1];
  }
  return sha;
}

function sideband(pack: Buffer): Buffer {
  const chunks: Buffer[] = [];
  for (let i = 0; i < pack.length; i += 65515) chunks.push(pktLine(Buffer.concat([Buffer.from([1]), pack.subarray(i, i + 65515)])));
  return Buffer.concat(chunks);
}

function fetchBoundary(wants: string[], clientShallow: Set<string>, depth: number | undefined): Set<string> {
  return depth == null ? new Set() : shallowBoundary(clientShallow.size ? [...clientShallow] : wants, depth);
}

function selectFetchObjects(wants: string[], haves: Set<string>, depth: number | undefined, clientShallow: Set<string>): string[] {
  if (wants.some(sha => !readObject(sha))) throw new Error('Unknown want');
  const knownHaves = new Set([...haves].filter(sha => readObject(sha)));
  const boundary = fetchBoundary(wants, clientShallow, depth);
  const raw = collectFetchObjects(wants, depth != null || clientShallow.size ? new Set() : knownHaves, boundary);
  const haveClosure = new Set(knownHaves.size ? collectFetchObjects([...knownHaves], new Set(), clientShallow) : []);
  return raw.filter(sha => !haveClosure.has(sha));
}

function shallowLines(wants: string[], haves: Set<string>, clientShallow: Set<string>, depth: number | undefined, delivered: Set<string>): string[] {
  const boundary = fetchBoundary(wants, clientShallow, depth);
  const lines = [...boundary].map(sha => `shallow ${sha}\n`);
  for (const sha of clientShallow) {
    const object = readObject(sha);
    if (!boundary.has(sha) && object?.type === 'commit' &&
      parseCommit(object.data).parents.every((parent: string) => delivered.has(parent) || haves.has(parent))) lines.push(`unshallow ${sha}\n`);
  }
  return lines;
}

export function registerGitHttpRoutes(router: Router, store: Store): void {
  const repoOf = (params: Record<string, string>) => params.owner === VIEWER.login
    ? store.repos.get(store.repoKey(VIEWER.login, params.repo.replace(/\.git$/, ''))) : undefined;
  const route = (method: string, path: string, handler: (repo: RepoRecord, ...args: Parameters<Handler>) => void) => {
    router.register(method, path, (req, res, params, body) => {
      const repo = repoOf(params);
      if (!repo) return sendJson(res, 404, { message: 'Not Found' });
      try { handler(repo, req, res, params, body); }
      catch { sendJson(res, 400, { message: 'Bad Request' }); }
    });
  };
  route('GET', '/:owner/:repo/info/refs', (repo, req, res) => {
    const service = new URL(req.url || '', 'http://localhost').searchParams.get('service');
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') return sendJson(res, 403, { message: 'Forbidden' });
    const chunks: Buffer[] = [];
    if (service === 'git-upload-pack' && req.headers['git-protocol'] === 'version=2') {
      chunks.push(...['version 2\n', 'ls-refs=unborn\n', 'fetch=shallow\n', 'object-format=sha1\n'].map(pktLine));
    } else {
      const capabilities = `${service === 'git-upload-pack' ? 'side-band-64k ofs-delta shallow' : 'report-status side-band-64k delete-refs ofs-delta'} symref=HEAD:${headTarget(repo)}`;
      chunks.push(pktLine(`# service=${service}\n`), pktFlush());
      const refs = refList(repo).filter(r => r.name.startsWith('refs/heads/') || r.name.startsWith('refs/tags/'));
      if (!refs.length) chunks.push(pktLine(`${ZERO} capabilities^{}\0${capabilities}\n`));
      else {
        chunks.push(pktLine(`${getRef(repo, headTarget(repo)) || ZERO} HEAD\0${capabilities}\n`));
        for (const ref of refs) {
          chunks.push(pktLine(`${ref.oid} ${ref.name}\n`));
          if (ref.name.startsWith('refs/tags/') && readObject(ref.oid)?.type === 'tag') chunks.push(pktLine(`${peelToCommit(ref.oid)} ${ref.name}^{}\n`));
        }
      }
    }
    chunks.push(pktFlush());
    res.writeHead(200, { 'content-type': `application/x-${service}-advertisement` }); res.end(Buffer.concat(chunks));
  });

  route('POST', '/:owner/:repo/git-upload-pack', (repo, req, res, _params, body) => {
    if (!Buffer.isBuffer(body)) throw new Error('Missing body');
    const items = parsePktLines(body), v2 = req.headers['git-protocol'] === 'version=2';
    const text = (data: Buffer) => data.toString().replace(/\n$/, '');
    const first = items[0];
    let args: string[];
    if (v2) {
      const delim = items.findIndex(item => item.type === 'delim');
      if (first?.type !== 'line' || delim < 1) throw new Error('Missing command');
      args = items.slice(delim + 1).flatMap(item => item.type === 'line' ? [text(item.data)] : []);
      if (text(first.data) === 'command=ls-refs') {
        const prefixes = args.filter(a => a.startsWith('ref-prefix ')).map(a => a.slice(11));
        const refs = refList(repo), tip = getRef(repo, headTarget(repo));
        if (tip || args.includes('unborn')) refs.unshift({ name: 'HEAD', oid: tip || 'unborn' });
        const lines = refs.filter(ref => !prefixes.length || prefixes.some(p => ref.name.startsWith(p))).map(ref => {
          let line = `${ref.oid} ${ref.name}`;
          if (ref.name === 'HEAD' && args.includes('symrefs')) line += ` symref-target:${headTarget(repo)}`;
          if (args.includes('peel') && readObject(ref.oid)?.type === 'tag') line += ` peeled:${peelToCommit(ref.oid)}`;
          return pktLine(line + '\n');
        });
        res.writeHead(200, { 'content-type': 'application/x-git-upload-pack-result' }); res.end(Buffer.concat([...lines, pktFlush()])); return;
      }
      if (text(first.data) !== 'command=fetch') throw new Error('Unknown command');
    } else args = items.flatMap(item => item.type === 'line' ? [text(item.data)] : []);
    const wants: string[] = [], haves = new Set<string>(), clientShallow = new Set<string>();
    let depth: number | undefined;
    for (const arg of args) {
      const [key, value] = arg.split(' ');
      if (['want', 'have', 'shallow'].includes(key)) {
        if (!/^[0-9a-f]{40}$/.test(value || '')) throw new Error('Invalid oid');
        if (key === 'want') wants.push(value); else (key === 'have' ? haves : clientShallow).add(value);
      } else if (key === 'deepen') {
        if (!/^[1-9][0-9]*$/.test(value || '') || !Number.isSafeInteger(Number(value))) throw new Error('Invalid depth');
        depth = Number(value);
      } else if (!['thin-pack', 'no-progress', 'ofs-delta', 'done'].includes(key)) throw new Error('Invalid argument');
    }
    if (!wants.length) throw new Error('Missing want');
    const delivered = selectFetchObjects(wants, haves, depth, clientShallow);
    const objects = new Map(delivered.map(sha => [sha, readObject(sha)!]));
    const shallow = shallowLines(wants, haves, clientShallow, depth, new Set(delivered)).map(pktLine);
    const chunks = v2 ? [pktLine('acknowledgments\n'), pktLine('NAK\n'), pktDelim()] : [pktLine('NAK\n')];
    if (v2 && (depth != null || clientShallow.size)) chunks.push(pktLine('shallow-info\n'), ...shallow, pktDelim());
    if (!v2) chunks.push(...shallow);
    if (v2) chunks.push(pktLine('packfile\n'));
    chunks.push(sideband(buildPack(delivered, objects)), pktFlush());
    res.writeHead(200, { 'content-type': 'application/x-git-upload-pack-result' }); res.end(Buffer.concat(chunks));
  });

  route('POST', '/:owner/:repo/git-receive-pack', (repo, _req, res, _params, body) => {
    if (!Buffer.isBuffer(body)) throw new Error('Missing body');
    const commands: Array<{ old: string; next: string; ref: string }> = [];
    let offset = 0, capabilities: string[] = [];
    while (true) {
      const prefix = body.subarray(offset, offset + 4).toString();
      if (!/^[0-9a-fA-F]{4}$/.test(prefix)) throw new Error('Invalid packet');
      const length = parseInt(prefix, 16); offset += 4;
      if (!length) break;
      if (length < 4 || offset + length - 4 > body.length) throw new Error('Invalid packet');
      const parts = body.subarray(offset, offset + length - 4).toString().replace(/\n$/, '').split('\0'); offset += length - 4;
      if (parts.length > 2 || (commands.length && parts.length > 1)) throw new Error('Invalid capabilities');
      if (parts[1]) capabilities = parts[1].split(' ');
      const match = /^([0-9a-f]{40}) ([0-9a-f]{40}) ([^\s\0]+)$/.exec(parts[0]);
      if (!match) throw new Error('Invalid command');
      commands.push({ old: match[1], next: match[2], ref: match[3] });
    }
    let unpack = 'ok';
    if (offset < body.length) {
      try { parsePack(body.subarray(offset), new Map()); }
      catch (error) { unpack = error instanceof Error ? error.message.replace(/[\r\n]/g, ' ') : 'failed'; }
    }
    const report = [pktLine(`unpack ${unpack}\n`)];
    for (const command of commands) {
      const { old, next, ref } = command;
      let reason = unpack !== 'ok' ? 'unpack failed' : !ref.startsWith('refs/') ? 'invalid ref' :
        old !== (getRef(repo, ref) || ZERO) ? 'old oid does not match' : '';
      if (!reason && next !== ZERO) {
        if (!readObject(next)) reason = 'object not found';
        else if (old !== ZERO && readObject(old)?.type === 'commit' && readObject(next)?.type === 'commit' && !isAncestor(old, next)) reason = 'non-fast-forward';
      }
      if (!reason) { if (next === ZERO) deleteRef(repo, ref); else setRef(repo, ref, next); }
      report.push(pktLine(reason ? `ng ${ref} ${reason}\n` : `ok ${ref}\n`));
    }
    const data = Buffer.concat(report);
    res.writeHead(200, { 'content-type': 'application/x-git-receive-pack-result' });
    res.end(capabilities.includes('report-status') ? Buffer.concat([capabilities.includes('side-band-64k') ? sideband(data) : data, pktFlush()]) : Buffer.alloc(0));
  });
}
