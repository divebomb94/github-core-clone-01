import { createApp } from './app.ts';
import { VIEWER } from './store.ts';
import { writeBlob, writeTree, writeCommit, writeObject, readObject } from './gitstore.ts';
import { pktLine, pktFlush, pktDelim, parsePktLines, buildPack, parsePack, collectFetchObjects } from './pack.ts';

const check = (ok: unknown, message: string) => { if (!ok) throw new Error(message); };
process.env.MAKEFUN_SHOP_TOKEN = 'git-self-test';
const server = createApp();
let inProcess = false;
await new Promise<void>((resolve, reject) => {
  server.once('error', (error: NodeJS.ErrnoException) => { if (error.code === 'EPERM') { inProcess = true; resolve(); } else reject(error); });
  server.listen(0, '127.0.0.1', resolve);
});
const address = server.address();
const base = `http://127.0.0.1:${address && typeof address !== 'string' ? address.port : 0}`;
async function request(path: string, body?: Buffer, v2 = false) {
  if (inProcess) {
    return new Promise<{ status: number; data: Buffer }>(resolve => {
      const listeners = new Map<string, (chunk?: Buffer) => void>();
      let status = 200;
      const req = { url: path, method: body ? 'POST' : 'GET', headers: { authorization: 'Bearer git-self-test', ...(v2 ? { 'git-protocol': 'version=2' } : {}) }, on: (event: string, callback: (chunk?: Buffer) => void) => listeners.set(event, callback) };
      const res = { setHeader() {}, writeHead(code: number) { status = code; }, end(data?: string | Buffer) { resolve({ status, data: Buffer.from(data || '') }); } };
      server.emit('request', req, res);
      if (body) listeners.get('data')!(body);
      listeners.get('end')!();
    });
  }
  const response = await fetch(base + path, { method: body ? 'POST' : 'GET', headers: { authorization: 'Bearer git-self-test', ...(v2 ? { 'Git-Protocol': 'version=2' } : {}) }, body });
  return { status: response.status, data: Buffer.from(await response.arrayBuffer()) };
}
const root = `/${VIEWER.login}/wire-test.git`;
const zero = '0'.repeat(40);
const p = (...lines: string[]) => Buffer.concat(lines.map(line => pktLine(line + '\n')));
const author = 'Test <test@example.invalid> 1 +0000';
function commit(text: string, parents: string[]) {
  const blob = writeBlob(Buffer.from(text));
  const tree = writeTree([{ mode: '100644', name: 'file', sha: blob }]);
  return writeCommit({ tree, parents, author, committer: author, message: text });
}
const first = commit('first', []), tip = commit('second', [first]);
const tag = writeObject('tag', Buffer.from(`object ${tip}\ntype commit\ntag v1\ntagger ${author}\n\nrelease\n`));
const all = collectFetchObjects([tip, tag], new Set(), new Set());
const expected = new Map(all.map(sha => [sha, { ...readObject(sha)!, data: Buffer.from(readObject(sha)!.data) }]));
async function push(old: string, next: string, ref: string, pack?: Buffer) {
  const response = await request(root + '/git-receive-pack', Buffer.concat([pktLine(`${old} ${next} ${ref}\0report-status side-band-64k\n`), pktFlush(), pack || Buffer.alloc(0)]));
  check(response.status === 200, 'push status');
  const report = Buffer.concat(parsePktLines(response.data).flatMap(item => item.type === 'line' ? [item.data.subarray(1)] : []));
  return parsePktLines(report).flatMap(item => item.type === 'line' ? [item.data.toString()] : []).join('');
}
async function fetchObjects(args: string[], v2 = true) {
  const body = v2 ? Buffer.concat([p('command=fetch'), pktDelim(), p(...args, 'done'), pktFlush()]) : Buffer.concat([p(...args), pktFlush(), p('done'), pktFlush()]);
  const response = await request(root + '/git-upload-pack', body, v2);
  check(response.status === 200, 'fetch status');
  const items = parsePktLines(response.data);
  const pack = Buffer.concat(items.flatMap(item => item.type === 'line' && item.data[0] === 1 ? [item.data.subarray(1)] : []));
  const objects = new Map();
  const parsed = parsePack(pack, objects);
  return { shas: parsed.shas, objects, text: items.flatMap(item => item.type === 'line' && item.data[0] !== 1 ? [item.data.toString()] : []).join(''), pack };
}
try {
  check((await request('/api/v3/user/repos', Buffer.from(JSON.stringify({ name: 'wire-test' })))).status === 201, 'REST create');
  check((await request(root + '/info/refs?service=git-upload-pack', undefined, true)).data.includes(Buffer.from('version 2')), 'v2 advertisement');
  check((await push(zero, tip, 'refs/heads/main', buildPack(all, expected))).includes('ok refs/heads/main'), 'initial push');
  check((await push(zero, tag, 'refs/tags/v1')).includes('ok refs/tags/v1'), 'tag push');
  const refs = await request(root + '/git-upload-pack', Buffer.concat([p('command=ls-refs'), pktDelim(), p('peel', 'symrefs'), pktFlush()]), true);
  check(refs.data.includes(Buffer.from(`peeled:${tip}`)) && refs.data.includes(Buffer.from('HEAD symref-target:refs/heads/main')), 'ls-refs');
  const clone = await fetchObjects([`want ${tip}`, `want ${tag}`]);
  check(clone.shas.length === all.length, 'clone count');
  for (const [sha, object] of expected) check(clone.objects.get(sha)?.type === object.type && clone.objects.get(sha)?.data.equals(object.data), 'clone bytes ' + sha);
  check((await push(tip, first, 'refs/heads/main')).includes('ng refs/heads/main non-fast-forward'), 'non-fast-forward');
  const after = await request(root + '/info/refs?service=git-upload-pack');
  check(after.data.includes(Buffer.from(`${tip} HEAD`)), 'unchanged tip');
  check((await push(tag, zero, 'refs/tags/v1')).includes('ok refs/tags/v1'), 'delete');
  const shallow = await fetchObjects([`want ${tip}`, 'deepen 1']);
  check(shallow.text.includes(`shallow ${tip}\n`) && shallow.shas.includes(tip) && !shallow.shas.includes(first), 'shallow clone');
  const deepened = await fetchObjects([`want ${tip}`, `shallow ${tip}`, 'deepen 2']);
  check(deepened.text.includes(`unshallow ${tip}`), 'unshallow');
  const oldClosure = new Set(collectFetchObjects([first], new Set(), new Set()));
  const incremental = await fetchObjects([`want ${tip}`, `have ${first}`]);
  check(incremental.shas.length === all.filter(sha => sha !== tag && !oldClosure.has(sha)).length && incremental.shas.every(sha => !oldClosure.has(sha)), 'incremental objects');
  const empty = await fetchObjects([`want ${tip}`, `have ${tip}`]);
  check(empty.pack.length === 32 && !empty.shas.length, 'empty pack');
  check((await fetchObjects([`want ${tip}`], false)).shas.includes(first), 'v0 fetch');
  check((await request(root + '/git-upload-pack', Buffer.from('garbage'), true)).status === 400, 'malformed fetch');
  check((await request(root + '/git-receive-pack', Buffer.from('0008bad'))).status === 400, 'malformed push');
  check((await request('/api/v3/user')).status === 200, 'server still serving REST');
  console.log('Git smart-HTTP self-test passes');
} finally { if (!inProcess) await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve())); }
