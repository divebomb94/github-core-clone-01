import type { ServerResponse } from 'node:http';
import { VIEWER, type Store, type RepoRecord, type LabelRecord, type IssueRecord, type CommentRecord } from './store.ts';
import { sendJson, type Router } from './app.ts';

const nf = (res:ServerResponse) => sendJson(res,404,{message:'Not Found'});
const vf = (res:ServerResponse) => sendJson(res,422,{message:'Validation Failed'});
const user = () => ({login:VIEWER.login,id:VIEWER.id,type:'User'});
function labelJson(label:LabelRecord){ return {...label,default:false,url:''}; }
function commentJson(comment:CommentRecord){ return {id:comment.id,body:comment.body,user:user(),created_at:comment.createdAt,updated_at:comment.updatedAt,html_url:'',url:''}; }
function issueJson(issue:IssueRecord,repo:RepoRecord){
 return {id:issue.number,number:issue.number,title:issue.title,body:issue.body,state:issue.state,labels:issue.labels.flatMap(name=>{const label=repo.labels.get(name);return label?[labelJson(label)]:[];}),user:user(),author_association:'OWNER',comments:issue.comments.length,created_at:issue.createdAt,updated_at:issue.updatedAt,closed_at:issue.closedAt,html_url:'',url:'',repository_url:''};
}
function validLabels(labels:unknown,repo:RepoRecord):labels is string[]{ return Array.isArray(labels)&&labels.every(name=>typeof name==='string'&&repo.labels.has(name)); }

export function registerIssueRoutes(router:Router,store:Store){
 const base='/api/v3/repos/:owner/:repo';
 const find=(p:Record<string,string>)=>p.owner===VIEWER.login?store.repos.get(store.repoKey(p.owner,p.repo)):undefined;
 const findIssue=(repo:RepoRecord,p:Record<string,string>)=>{const issue=/^[1-9]\d*$/.test(p.number)?repo.items.get(Number(p.number)):undefined;return issue?.kind==='issue'?issue:undefined;};
 router.register('POST',`${base}/labels`,(_q,res,p,b:any)=>{
  const repo=find(p);if(!repo)return nf(res);
  if(!b||typeof b.name!=='string'||!b.name||repo.labels.has(b.name))return vf(res);
  const label:LabelRecord={name:b.name,color:typeof b.color==='string'?b.color.replace(/^#/,''):'ededed',description:typeof b.description==='string'?b.description:''};
  repo.labels.set(label.name,label);sendJson(res,201,labelJson(label));
 });
 router.register('GET',`${base}/labels`,(_q,res,p)=>{const repo=find(p);if(!repo)return nf(res);sendJson(res,200,[...repo.labels.values()].map(labelJson));});
 router.register('GET',`${base}/labels/:name`,(_q,res,p)=>{const label=find(p)?.labels.get(p.name);label?sendJson(res,200,labelJson(label)):nf(res);});
 router.register('POST',`${base}/issues`,(_q,res,p,b:any)=>{
  const repo=find(p);if(!repo)return nf(res);
  if(!b||typeof b.title!=='string'||!b.title.trim()||!validLabels(b.labels===undefined?[]:b.labels,repo)||('body' in b&&typeof b.body!=='string'))return vf(res);
  const now=new Date().toISOString(),number=++repo.numberSeq;
  const issue:IssueRecord={number,kind:'issue',title:b.title,body:b.body??'',state:'open',labels:[...(b.labels??[])],createdAt:now,updatedAt:now,closedAt:null,seq:number,comments:[]};
  repo.items.set(number,issue);sendJson(res,201,issueJson(issue,repo));
 });
 router.register('GET',`${base}/issues`,(req,res,p)=>{
  const repo=find(p);if(!repo)return nf(res);
  const u=new URL(req.url||'',`http://${req.headers.host||'localhost'}`),state=u.searchParams.get('state')||'open',direction=u.searchParams.get('direction')==='asc'?1:-1;
  const n=Math.min(100,Math.max(1,Number(u.searchParams.get('per_page')||30))),page=Math.max(1,Number(u.searchParams.get('page')||1));
  const issues=[...repo.items.values()].filter((i):i is IssueRecord=>i.kind==='issue'&&(state==='all'||i.state===state)).sort((a,b)=>direction*(a.createdAt.localeCompare(b.createdAt)||a.seq-b.seq));
  sendJson(res,200,issues.slice((page-1)*n,page*n).map(i=>issueJson(i,repo)));
 });
 router.register('GET',`${base}/issues/:number`,(_q,res,p)=>{const repo=find(p),issue=repo&&findIssue(repo,p);if(!repo||!issue)return nf(res);sendJson(res,200,issueJson(issue,repo));});
 router.register('PATCH',`${base}/issues/:number`,(_q,res,p,b:any)=>{
  const repo=find(p),issue=repo&&findIssue(repo,p);if(!repo||!issue)return nf(res);
  if(!b||typeof b!=='object'||('title' in b&&(typeof b.title!=='string'||!b.title.trim()))||('body' in b&&typeof b.body!=='string')||('state' in b&&b.state!=='open'&&b.state!=='closed')||('labels' in b&&!validLabels(b.labels,repo)))return vf(res);
  const now=new Date().toISOString();
  if('title' in b)issue.title=b.title;if('body' in b)issue.body=b.body;if('labels' in b)issue.labels=[...b.labels];
  if('state' in b){if(b.state==='closed'&&issue.state!=='closed')issue.closedAt=now;if(b.state==='open')issue.closedAt=null;issue.state=b.state;}
  issue.updatedAt=now;sendJson(res,200,issueJson(issue,repo));
 });
 router.register('POST',`${base}/issues/:number/comments`,(_q,res,p,b:any)=>{
  const repo=find(p),issue=repo&&findIssue(repo,p);if(!issue)return nf(res);
  if(!b||typeof b.body!=='string'||!b.body.trim())return vf(res);
  const now=new Date().toISOString(),comment:CommentRecord={id:store.nextId(),body:b.body,createdAt:now,updatedAt:now};
  issue.comments.push(comment);issue.updatedAt=now;sendJson(res,201,commentJson(comment));
 });
 router.register('GET',`${base}/issues/:number/comments`,(_q,res,p)=>{const repo=find(p),issue=repo&&findIssue(repo,p);if(!issue)return nf(res);sendJson(res,200,issue.comments.map(commentJson));});
}
