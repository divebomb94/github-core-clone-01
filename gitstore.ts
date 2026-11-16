import * as crypto from 'node:crypto';

export type ObjectType = 'blob' | 'tree' | 'commit' | 'tag';
const objects = new Map<string, { type: ObjectType, data: Buffer }>();
export function hashObject(type: ObjectType, data: Buffer): string { return crypto.createHash('sha1').update(Buffer.concat([Buffer.from(`${type} ${data.length}\0`), data])).digest('hex'); }
export function writeObject(type: ObjectType, data: Buffer): string { const sha = hashObject(type, data); objects.set(sha, { type, data: Buffer.from(data) }); return sha; }
export const writeBlob = (bytes: Buffer) => writeObject('blob', bytes);
export function readObject(sha: string) { return objects.get(sha); }
export function writeTree(entries: Array<{mode:string,name:string,sha:string}>): string {
  const sorted = [...entries].sort((a,b) => Buffer.compare(Buffer.from(a.mode==='40000'?a.name+'/':a.name), Buffer.from(b.mode==='40000'?b.name+'/':b.name)));
  const chunks: Buffer[] = []; for (const e of sorted) chunks.push(Buffer.from(`${e.mode} ${e.name}\0`), Buffer.from(e.sha, 'hex')); return writeObject('tree', Buffer.concat(chunks));
}
export function parseTree(data: Buffer) { const out: Array<{mode:string,name:string,sha:string}> = []; let i=0; while(i<data.length){ const z=data.indexOf(0,i); if(z<0) break; const space=data.indexOf(32,i); const mode=data.slice(i,space).toString(), name=data.slice(space+1,z).toString(); const sha=data.slice(z+1,z+21).toString('hex'); out.push({mode,name,sha}); i=z+21; } return out; }
export function parseCommit(data: Buffer) { const [head,...rest]=data.toString().split('\n\n'); const lines=head.split('\n'); const result:any={tree:'',parents:[],author:'',committer:'',message:rest.join('\n\n')}; for(const l of lines){ const [k,...v]=l.split(' '); if(k==='tree') result.tree=v.join(' '); else if(k==='parent') result.parents.push(v.join(' ')); else if(k==='author'||k==='committer') result[k]=v.join(' '); } return result; }
export function writeCommit(c:{tree:string,parents:string[],author:string,committer:string,message:string}) { const h=[`tree ${c.tree}`,...c.parents.map(p=>`parent ${p}`),`author ${c.author}`,`committer ${c.committer}`].join('\n'); return writeObject('commit',Buffer.from(`${h}\n\n${c.message}`)); }
export function flattenTree(treeSha:string,prefix=''):Map<string,{mode:string,sha:string}> { const out=new Map<string,{mode:string,sha:string}>(); for(const e of parseTree(readObject(treeSha)?.data||Buffer.alloc(0))){ const p=prefix?`${prefix}/${e.name}`:e.name; if(e.mode==='40000') for(const [k,v] of flattenTree(e.sha,p)) out.set(k,v); else out.set(p,{mode:e.mode,sha:e.sha}); } return out; }
export function buildTree(files:Map<string,{mode:string,sha:string}>):string { const groups=new Map<string,Map<string,{mode:string,sha:string}>>(); for(const [p,v] of files){ const [head,...tail]=p.split('/'); const m=groups.get(head)||new Map(); m.set(tail.join('/'),v); groups.set(head,m); } const entries:any[]=[]; for(const [name,m] of groups){ if(m.has('')) entries.push({mode:m.get('')!.mode,name,sha:m.get('')!.sha}); else entries.push({mode:'40000',name,sha:buildTree(new Map([...m].map(([k,v])=>[k,v])))}); } return writeTree(entries); }
export const getRef=(repo:any,ref:string)=>repo.refs.get(ref); export const setRef=(repo:any,ref:string,sha:string)=>repo.refs.set(ref,sha); export const deleteRef=(repo:any,ref:string)=>repo.refs.delete(ref); export const listRefs=(repo:any,prefix:string)=>[...repo.refs].filter(([r])=>r.startsWith(prefix));
