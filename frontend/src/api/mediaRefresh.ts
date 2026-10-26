import { ApiError } from "./client";

export type RefreshTarget={kind:"root"|"item"|"section";id:string;path:string;label:string};
export type RefreshPlan={serverId:string;server:"emby"|"jellyfin"|"plex";targets:RefreshTarget[]};
export type MediaRefreshReview={id:string;requestId:string;jobId:string;whole:boolean;actor:string;itemCount:number;fingerprint:string;plan:RefreshPlan};
export type MediaRefreshJob={id:string;state:"pending"|"planning"|"running"|"accepted"|"not_needed"|"blocked"|"uncertain"|"stopped";reason:string;created:string;due:string;actor:string;source:"transfer_finished"|"refresh_media_server";reference:string;server:RefreshPlan["server"];fingerprint:string;plan:RefreshPlan|null;actions:{index:number;target:RefreshTarget;state:"pending"|"intent"|"accepted"|"uncertain";reason:string}[]};
export type MediaRefreshStatus={installed:boolean;enabled:boolean;active:boolean;server:string;serverId:string;actor:string;delay:number;workerRunning:boolean;jobs:MediaRefreshJob[]};
export type MediaRefreshConsent={confirm:boolean;confirmScan:boolean;confirmOldExecutorsStopped:boolean};
const base="/api/v1/library/refresh";
const hash=(v:unknown,n=64):v is string=>typeof v==="string"&&new RegExp("^[a-f0-9]{"+n+"}$").test(v);
const safe=(v:unknown,max=4096):v is string=>typeof v==="string"&&v.length<=max&&!/[\0\r\n]/.test(v);
const identifier=(v:unknown):v is string=>typeof v==="string"&&/^[a-zA-Z0-9_-]{1,128}$/.test(v);
const targetSame=(a:RefreshTarget,b:RefreshTarget)=>a.kind===b.kind&&a.id===b.id&&a.path===b.path&&a.label===b.label;
export function validRefreshPlan(v:RefreshPlan):boolean{
  return !!v&&identifier(v.serverId)&&["emby","jellyfin","plex"].includes(v.server)&&Array.isArray(v.targets)&&v.targets.length<=4096&&new Set(v.targets.map(t=>t?.kind+":"+t?.id+":"+t?.path)).size===v.targets.length&&v.targets.every(t=>!!t&&identifier(t.id)&&safe(t.path)&&safe(t.label)&&(v.server==="plex"?t.kind==="section"&&/^[0-9]+$/.test(t.id):(t.kind==="root"&&t.id==="root"&&t.path===""||v.server==="emby"&&t.kind==="item")));
}
export function validMediaRefreshReview(v:MediaRefreshReview):boolean{
  return !!v&&hash(v.id)&&hash(v.requestId,32)&&hash(v.fingerprint)&&typeof v.whole==="boolean"&&(v.whole?v.jobId==="":hash(v.jobId,32))&&safe(v.actor,128)&&!!v.actor&&Number.isSafeInteger(v.itemCount)&&v.itemCount>=0&&v.itemCount<=1000&&(v.whole?v.itemCount===0:v.itemCount>0)&&validRefreshPlan(v.plan);
}
export function validMediaRefreshJob(v:MediaRefreshJob):boolean{
  if(!v||!Array.isArray(v.actions)||(v.plan!==null&&!validRefreshPlan(v.plan)))return false;
  if(v&&["running","accepted","not_needed","uncertain"].includes(v.state)&&v.plan===null)return false;
  if(v&&["pending","planning"].includes(v.state)&&v.plan!==null)return false;
  if(v?.state==="accepted"&&(!v.actions?.length||v.actions.some(a=>a.state!=="accepted")))return false;
  if(v?.state==="not_needed"&&(v.plan?.targets.length!==0||v.actions?.length!==0))return false;
  return !!v&&hash(v.id)&&hash(v.fingerprint)&&["pending","planning","running","accepted","not_needed","blocked","uncertain","stopped"].includes(v.state)&&safe(v.reason)&&safe(v.created)&&safe(v.due)&&safe(v.actor,128)&&!!v.actor&&["transfer_finished","refresh_media_server"].includes(v.source)&&(v.reference===""||hash(v.reference,32))&&["emby","jellyfin","plex"].includes(v.server)&&(v.plan===null||validRefreshPlan(v.plan)&&v.plan.server===v.server)&&Array.isArray(v.actions)&&v.actions.length<=4096&&v.actions.length===(v.plan?.targets.length||0)&&v.actions.every((a,i)=>!!a&&a.index===i&&["pending","intent","accepted","uncertain"].includes(a.state)&&safe(a.reason)&&!!v.plan&&targetSame(a.target,v.plan.targets[i]));
}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
  const response=await fetch(base+path,body===undefined?{headers:{Authorization:token}}:{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify(body)});
  let v:{code:number;success:boolean;data:T;message?:string};
  try{v=await response.json()}catch{throw new ApiError("刷新回执不完整，请按稳定编号只读核验，不要重发",response.status||503)}
  if(!response.ok||v?.code!==0||!v.success||!v.data)throw new ApiError(v?.message||"媒体服务器刷新未核实",response.status===401?401:v?.code||response.status||503);
  return v.data;
}
export async function getMediaRefreshStatus(token:string):Promise<MediaRefreshStatus>{
  const v=await request<MediaRefreshStatus>(token,"");
  if(!v||![v.installed,v.enabled,v.active,v.workerRunning].every(b=>typeof b==="boolean")||!safe(v.server)||!safe(v.serverId)||!safe(v.actor)||!Number.isSafeInteger(v.delay)||v.delay<0||v.delay>2147483647||!Array.isArray(v.jobs)||v.jobs.length>30||!v.jobs.every(validMediaRefreshJob))throw new ApiError("刷新队列状态未核实",503);
  return v;
}
export async function previewMediaRefresh(token:string,jobId:string,whole:boolean,confirmRead=false):Promise<MediaRefreshReview>{
  if(!confirmRead||(whole?jobId!=="":!hash(jobId,32)))throw new ApiError("须明确整库或准确的原生入库任务，并同意只读访问服务器",400);
  const v=await request<MediaRefreshReview>(token,"/preview",{jobId,whole,confirmRead:true});
  if(!validMediaRefreshReview(v)||v.whole!==whole||v.jobId!==jobId)throw new ApiError("媒体服务器刷新审核范围未核实",503);
  return v;
}
export async function submitMediaRefresh(token:string,v:MediaRefreshReview,c:MediaRefreshConsent):Promise<MediaRefreshJob>{
  if(!validMediaRefreshReview(v)||!c?.confirm||!c.confirmScan||!c.confirmOldExecutorsStopped)throw new ApiError("需要完整新审核及三项独立刷新确认",400);
  const out=await request<MediaRefreshJob>(token,"/submit",{jobId:v.jobId,whole:v.whole,requestId:v.requestId,fingerprint:v.fingerprint,confirmRead:true,...c});
  if(!validMediaRefreshJob(out)||out.id!==v.id||out.fingerprint!==v.fingerprint||out.source!=="refresh_media_server"||out.actor!==v.actor||out.reference!==v.jobId||out.server!==v.plan.server||out.plan&&(out.plan.serverId!==v.plan.serverId||out.plan.targets.length!==v.plan.targets.length||out.plan.targets.some((t,i)=>!targetSame(t,v.plan.targets[i]))))throw new ApiError("刷新保存回执范围未核实，请按稳定编号只读查询，不要重发",503);
  return out;
}
export async function getMediaRefreshJob(token:string,id:string):Promise<MediaRefreshJob>{
  if(!hash(id))throw new ApiError("需要完整的刷新稳定编号",400);
  const v=await request<MediaRefreshJob>(token,"/jobs/"+id);
  if(!validMediaRefreshJob(v)||v.id!==id)throw new ApiError("原刷新回执未核实",503);return v;
}
export async function stopMediaRefreshJob(token:string,id:string,confirm=false):Promise<MediaRefreshJob>{
  if(!hash(id)||!confirm)throw new ApiError("须明确确认停止这一个刷新任务",400);
  const v=await request<MediaRefreshJob>(token,"/jobs/"+id+"/stop",{confirm:true});
  if(!validMediaRefreshJob(v)||v.id!==id||v.state!=="stopped")throw new ApiError("停止回执未核实，请只读查询，已进入服务器的扫描不能撤回",503);return v;
}
