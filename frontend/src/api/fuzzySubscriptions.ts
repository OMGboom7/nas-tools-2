import { ApiError } from "./client";

export type FuzzyKind="MOV"|"TV";
export type FuzzyConsent={confirm:boolean;confirmWhole:boolean;confirmOldExecutorsStopped:boolean};
export type FuzzyCandidate={title:string;name:string;year:string;kind:FuzzyKind;season:string;episodes:string;site:string;priority:number;topOrder:number;groupId:string};
export type FuzzyGrantReview={kind:FuzzyKind;id:string;name:string;year:string;season:string;overEdition:boolean;actor:string;active:boolean;fingerprint:string;sites:string[];workerRunning:boolean;targets:{site:string;downloaderId:string;downloader:string;type:string;directory:string;paused:boolean}[]};
export type FuzzyReview={id:string;requestId:string;fingerprint:string;kind:FuzzyKind;subscriptionId:string;name:string;actor:string;candidates:FuzzyCandidate[];overEdition:boolean};
export type FuzzyBatch={id:string;kind:FuzzyKind;subscriptionId:string;name:string;actor:string;requestId:string;fingerprint:string;revision:string;authorizedBy:string;state:"pending"|"running"|"completed"|"blocked"|"uncertain"|"stopped";reason:string;created:string;sourceResult:"none"|"retained"|"finished";items:{index:number;candidate:FuzzyCandidate;state:"pending"|"intent"|"accepted"|"uncertain"|"rejected";reason:string}[]};
export type FuzzyContinuation={batch:FuzzyBatch;fingerprint:string;actor:string;canResume:boolean};
export type FuzzyStatus={workerRunning:boolean;jobs:FuzzyBatch[]};
export type FuzzySource={review:FuzzyGrantReview;jobs:FuzzyBatch[]};
export type FuzzyRestore={id:string;batchId:string;requestId:string;executionAuthorized:false};
const base="/api/v1/fuzzy-subscriptions";
const hash=(v:unknown,n=64):v is string=>typeof v==="string"&&new RegExp("^[a-f0-9]{"+n+"}$").test(v);
const subID=(v:unknown,zero=false):v is string=>typeof v==="string"&&(zero&&v==="0"||/^[1-9][0-9]{0,18}$/.test(v)&&BigInt(v)<=9223372036854775807n);
const text=(v:unknown,max=4096):v is string=>typeof v==="string"&&v.length<=max&&!v.includes("\0");
const kind=(v:unknown):v is FuzzyKind=>v==="MOV"||v==="TV";
const approved=(c:FuzzyConsent)=>!!c&&c.confirm===true&&c.confirmWhole===true&&c.confirmOldExecutorsStopped===true;
const sameCandidate=(a:FuzzyCandidate,b:FuzzyCandidate)=>a.title===b.title&&a.name===b.name&&a.year===b.year&&a.kind===b.kind&&a.season===b.season&&a.episodes===b.episodes&&a.site===b.site&&a.priority===b.priority&&a.topOrder===b.topOrder&&a.groupId===b.groupId;
export function validFuzzyCandidate(v:FuzzyCandidate):boolean{
  return !!v&&text(v.title)&&!!v.title&&text(v.name,1024)&&!!v.name&&text(v.year,4)&&kind(v.kind)&&text(v.season,32)&&text(v.episodes,64<<10)&&text(v.site)&&Number.isSafeInteger(v.priority)&&Number.isSafeInteger(v.topOrder)&&subID(v.groupId,true);
}
export function validFuzzyReview(v:FuzzyReview):boolean{
  return !!v&&hash(v.id)&&hash(v.requestId,32)&&hash(v.fingerprint)&&kind(v.kind)&&subID(v.subscriptionId)&&text(v.name,16<<10)&&!!v.name&&text(v.actor,128)&&!!v.actor&&typeof v.overEdition==="boolean"&&Array.isArray(v.candidates)&&v.candidates.length<=200&&v.candidates.every(c=>validFuzzyCandidate(c)&&c.kind===v.kind)&&new Set(v.candidates.map(c=>[c.kind,c.name,c.year,c.season,c.episodes].join("\0"))).size===v.candidates.length;
}
export function validFuzzyBatch(v:FuzzyBatch):boolean{
  if(!v||!Array.isArray(v.items)||v.items.length>200)return false;
  if(v.state==="completed"&&(v.items.some(i=>i?.state!=="accepted")||v.sourceResult==="none"||v.sourceResult==="finished"&&v.items.length===0))return false;
  if(v.state!=="completed"&&v.sourceResult!=="none")return false;
  if(v.state==="pending"&&v.items.some(i=>i?.state!=="pending"&&i?.state!=="accepted"))return false;
  return hash(v.id)&&kind(v.kind)&&subID(v.subscriptionId)&&text(v.name,16<<10)&&!!v.name&&text(v.actor,128)&&!!v.actor&&hash(v.requestId,32)&&hash(v.fingerprint)&&hash(v.revision)&&text(v.authorizedBy,128)&&!!v.authorizedBy&&["pending","running","completed","blocked","uncertain","stopped"].includes(v.state)&&text(v.reason)&&text(v.created,128)&&["none","retained","finished"].includes(v.sourceResult)&&v.items.every((i,n)=>!!i&&i.index===n&&validFuzzyCandidate(i.candidate)&&i.candidate.kind===v.kind&&["pending","intent","accepted","uncertain","rejected"].includes(i.state)&&text(i.reason));
}
function validGrant(v:FuzzyGrantReview):boolean{
  return !!v&&kind(v.kind)&&subID(v.id)&&text(v.name,16<<10)&&!!v.name&&text(v.year,4)&&text(v.season,32)&&typeof v.overEdition==="boolean"&&text(v.actor,128)&&!!v.actor&&typeof v.active==="boolean"&&typeof v.workerRunning==="boolean"&&hash(v.fingerprint)&&Array.isArray(v.sites)&&v.sites.length<=100&&v.sites.every(s=>text(s))&&Array.isArray(v.targets)&&v.targets.length===v.sites.length&&v.targets.every((t,i)=>!!t&&t.site===v.sites[i]&&subID(t.downloaderId)&&text(t.downloader)&&text(t.type,128)&&text(t.directory)&&typeof t.paused==="boolean");
}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
  const response=await fetch(base+path,body===undefined?{headers:{Authorization:token}}:{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify(body)});
  let v:{code:number;success:boolean;message?:string;data:T};
  try{v=await response.json()}catch{throw new ApiError("模糊订阅回执不完整，请按稳定编号只读查回，不要重发",response.status||503)}
  if(!response.ok||v?.code!==0||v.success!==true||!v.data)throw new ApiError(v?.message||"模糊订阅结果未核实",response.status===401?401:v?.code||response.status||503);
  return v.data;
}
const selector=(k:FuzzyKind,id:string)=>{if(!kind(k)||!subID(id))throw new ApiError("需要准确的原生订阅类型及完整编号",400);return "/"+k+"/"+id};
export async function getFuzzyStatus(token:string):Promise<FuzzyStatus>{
  const v=await request<FuzzyStatus>(token,"");
  if(!v||typeof v.workerRunning!=="boolean"||!Array.isArray(v.jobs)||v.jobs.length>30||!v.jobs.every(validFuzzyBatch))throw new ApiError("模糊批次清单未核实",503);return v;
}
export async function getFuzzySource(token:string,k:FuzzyKind,id:string):Promise<FuzzySource>{
  const v=await request<FuzzySource>(token,selector(k,id));
  if(!v||!validGrant(v.review)||v.review.kind!==k||v.review.id!==id||!Array.isArray(v.jobs)||v.jobs.length>30||!v.jobs.every(j=>validFuzzyBatch(j)&&j.kind===k&&j.subscriptionId===id))throw new ApiError("模糊规则授权范围未核实",503);return v;
}
export async function setFuzzyGrant(token:string,v:FuzzyGrantReview,enable:boolean,c:FuzzyConsent):Promise<void>{
  if(!validGrant(v)||!c?.confirm||enable&&!approved(c))throw new ApiError("须重新核对规则、整种子范围及旧执行器停止的独立确认",400);
  const out=await request<{active:boolean}>(token,selector(v.kind,v.id)+(enable?"/authorize":"/revoke"),{fingerprint:v.fingerprint,...c});
  if(out?.active!==enable)throw new ApiError("模糊规则授权回执未核实，请只读刷新，不要重发",503);
}
export async function previewFuzzyBatch(token:string,k:FuzzyKind,id:string,confirmRead=false):Promise<FuzzyReview>{
  if(!confirmRead)throw new ApiError("须同意只读访问已审核的站点并读取完整 RSS 候选",400);
  const v=await request<FuzzyReview>(token,selector(k,id)+"/preview",{confirmRead:true});
  if(!validFuzzyReview(v)||v.kind!==k||v.subscriptionId!==id)throw new ApiError("模糊 RSS 候选范围未核实",503);return v;
}
export async function saveFuzzyBatch(token:string,v:FuzzyReview,c:FuzzyConsent):Promise<FuzzyBatch>{
  if(!validFuzzyReview(v)||!approved(c))throw new ApiError("需要完整审核与三项独立提交确认",400);
  const out=await request<FuzzyBatch>(token,selector(v.kind,v.subscriptionId)+"/save",{requestId:v.requestId,fingerprint:v.fingerprint,confirmRead:true,...c});
  if(!validFuzzyBatch(out)||out.id!==v.id||out.requestId!==v.requestId||out.fingerprint!==v.fingerprint||out.actor!==v.actor||out.kind!==v.kind||out.subscriptionId!==v.subscriptionId||out.items.length!==v.candidates.length||out.items.some((i,n)=>!sameCandidate(i.candidate,v.candidates[n])))throw new ApiError("模糊批次保存回执范围未核实，请保留编号只读查回，不要重发",503);return out;
}
export async function getFuzzyBatch(token:string,id:string):Promise<FuzzyBatch>{
  if(!hash(id))throw new ApiError("需要完整批次稳定编号",400);
  const v=await request<FuzzyBatch>(token,"/jobs/"+id);
  if(!validFuzzyBatch(v)||v.id!==id)throw new ApiError("原模糊批次回执未核实",503);return v;
}
async function mutateBatch(token:string,v:FuzzyBatch,action:string,body:unknown):Promise<FuzzyBatch>{
  if(!validFuzzyBatch(v))throw new ApiError("需要已核实的原批次回执",400);
  const out=await request<FuzzyBatch>(token,"/jobs/"+v.id+"/"+action,body);
  if(!validFuzzyBatch(out)||out.id!==v.id||out.fingerprint!==v.fingerprint||out.items.length!==v.items.length||out.items.some((i,n)=>!sameCandidate(i.candidate,v.items[n].candidate)))throw new ApiError("批次操作回执未核实，请按原编号只读核对，不要重发",503);return out;
}
export async function stopFuzzyBatch(token:string,v:FuzzyBatch,confirm=false):Promise<FuzzyBatch>{
  if(!confirm)throw new ApiError("须明确停止本批并暂停对应原规则；已接收下载不撤回",400);
  const out=await mutateBatch(token,v,"stop",{confirm:true});
  if(out.state!=="stopped")throw new ApiError("停止回执未核实",503);return out;
}
export async function reviewFuzzyContinuation(token:string,v:FuzzyBatch,confirmRead=false):Promise<FuzzyContinuation>{
  if(!validFuzzyBatch(v)||!confirmRead)throw new ApiError("须明确只读核对原批次、剩余目标及当前授权",400);
  const out=await request<FuzzyContinuation>(token,"/jobs/"+v.id+"/review",{confirmRead:true});
  if(!out||!validFuzzyBatch(out.batch)||out.batch.id!==v.id||out.batch.fingerprint!==v.fingerprint||!hash(out.fingerprint)||typeof out.canResume!=="boolean"||!text(out.actor,128)||!out.actor||out.canResume&&out.batch.items.some(i=>i.state==="intent"||i.state==="uncertain"))throw new ApiError("原批次续办审核未核实",503);return out;
}
export async function resumeFuzzyBatch(token:string,v:FuzzyContinuation,c:FuzzyConsent):Promise<FuzzyBatch>{
  if(!v?.canResume||!hash(v.fingerprint)||!validFuzzyBatch(v.batch)||!approved(c))throw new ApiError("只能在新审核及三项确认后继续已证明未提交的目标",400);
  return mutateBatch(token,v.batch,"resume",{revision:v.batch.revision,fingerprint:v.fingerprint,confirmRead:true,...c});
}
export async function executeFuzzyBatch(token:string,v:FuzzyBatch,c:FuzzyConsent):Promise<FuzzyBatch>{
  if(v?.state!=="pending"||!approved(c))throw new ApiError("只能明确执行原待办批次；未知意图不能重发",400);
  return mutateBatch(token,v,"execute",{revision:v.revision,...c});
}
export async function verifyFuzzyItem(token:string,v:FuzzyBatch,index:number,confirmRead=false,confirm=false):Promise<FuzzyBatch>{
  if(!validFuzzyBatch(v)||!Number.isSafeInteger(index)||!["intent","uncertain"].includes(v.items[index]?.state)||!confirmRead||!confirm)throw new ApiError("须明确只读核对这一个原提交并采用正向证据；未找到不能释放",400);
  const out=await mutateBatch(token,v,"verify",{revision:v.revision,index,confirmRead:true,confirm:true});
  if(out.items[index]?.state!=="accepted")throw new ApiError("原下载器接收证据仍未核实",503);return out;
}
export async function restoreFuzzyRule(token:string,v:FuzzyBatch,requestId:string,confirm=false):Promise<FuzzyRestore>{
  if(!validFuzzyBatch(v)||v.state!=="completed"||v.sourceResult!=="finished"||!hash(requestId,32)||!confirm)throw new ApiError("须明确恢复原规则副本；原执行授权不会恢复",400);
  const out=await request<FuzzyRestore>(token,"/jobs/"+v.id+"/restore",{revision:v.revision,requestId,confirm:true});
  if(!out||!subID(out.id)||out.batchId!==v.id||out.requestId!==requestId||out.executionAuthorized!==false)throw new ApiError("规则恢复回执未核实，请保留恢复请求编号，不要重发",503);return out;
}
