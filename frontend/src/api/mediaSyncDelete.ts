import {ApiError} from "./client";
export type MediaDeleteSpec={enabled:boolean;deleteSource:boolean;notify:boolean;exclude:string};
export type MediaDeleteRoot={id:string;path:string;label:string;type:string};
export type MediaDeleteStatus={installed:boolean;active:boolean;current:boolean;ready:boolean;workerRunning:boolean;fingerprint:string;reason:string;spec:MediaDeleteSpec;roots:MediaDeleteRoot[]};
export type MediaDeleteReview={id:string;fingerprint:string;created:string;mediaId:string;http:boolean;spec:MediaDeleteSpec;roots:{id:string;path:string;identity:string}[]};
export type MediaDeleteEvent={type:""|"Movie"|"Series"|"Season"|"Episode";name:string;path:string;tmdbId:string;season:number;episode:number;virtual:boolean;missingVirtual:boolean};
export type MediaDeleteReceipt={id:string;callbackId:string;event:MediaDeleteEvent;state:"pending"|"planned"|"running"|"needs_review"|"completed"|"ignored"|"blocked";parentId:string;parentState:string;parentFingerprint:string;count:number;reason:string;actor:string;created:string;updated:string;policy:MediaDeleteSpec};
export type MediaDeleteConsents={confirm:boolean;history:boolean;dedupe:boolean;closeRelated:boolean;source:boolean;onlyCopy:boolean;expanded:boolean;legacyStopped:boolean;notify:boolean};
export type MediaDeleteUnit={ordinal:number;rootId:string;driver:string;path:string;role:string;absent:boolean;files:number;directories:number;links:number;bytes:number;sample:string[]};
export type MediaDeleteExecutionReview={receipt:MediaDeleteReceipt;parent:{id:string;state:string;fingerprint:string;creationFingerprint:string;canContinue:boolean;historyIds:string[];units:MediaDeleteUnit[];dedupeRows:number;reason:string}};
const hex=(v:unknown,n:number):v is string=>typeof v==="string"&&new RegExp("^[a-f0-9]{"+n+"}$").test(v);
const text=(v:unknown,n=4096):v is string=>typeof v==="string"&&v.length<=n&&!/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(v);
const integer=(v:unknown,max:number):v is number=>typeof v==="number"&&Number.isSafeInteger(v)&&v>=0&&v<=max;
const id=(v:unknown):v is string=>typeof v==="string"&&/^[1-9]\d{0,18}$/.test(v)&&BigInt(v)<=9223372036854775807n;
const date=(v:unknown)=>typeof v==="string"&&v.length<100&&Number.isFinite(Date.parse(v));
const parentStates=["","ready","running","needs_review","completed","closed"];
const specKey=(s:MediaDeleteSpec)=>JSON.stringify([s.enabled,s.deleteSource,s.notify,s.exclude]);
export const validMediaDeleteSpec=(s:MediaDeleteSpec):boolean=>!!s&&[s.enabled,s.deleteSource,s.notify].every(v=>typeof v==="boolean")&&text(s.exclude,16000);
export function validMediaDeleteStatus(s:MediaDeleteStatus):boolean{return !!s&&validMediaDeleteSpec(s.spec)&&[s.installed,s.active,s.current,s.ready,s.workerRunning].every(v=>typeof v==="boolean")&&(!s.active||s.current&&s.installed&&s.spec.enabled)&&hex(s.fingerprint,64)&&text(s.reason)&&Array.isArray(s.roots)&&s.roots.length<=256&&new Set(s.roots.map(r=>r?.id)).size===s.roots.length&&s.roots.every(r=>!!r&&text(r.id)&&!!r.id&&text(r.path)&&!!r.path&&text(r.label)&&text(r.type))}
export function validMediaDeleteReview(s:MediaDeleteReview):boolean{return !!s&&hex(s.id,32)&&hex(s.fingerprint,64)&&date(s.created)&&text(s.mediaId,512)&&!!s.mediaId&&typeof s.http==="boolean"&&validMediaDeleteSpec(s.spec)&&s.spec.enabled&&Array.isArray(s.roots)&&s.roots.length<=256&&new Set(s.roots.map(r=>r?.id)).size===s.roots.length&&s.roots.every(r=>!!r&&text(r.id)&&!!r.id&&text(r.path)&&!!r.path&&text(r.identity)&&!!r.identity)&&(s.spec.deleteSource?s.roots.length>0:s.roots.length===0)}
export function validMediaDeleteReceipt(p:MediaDeleteReceipt):boolean{
 if(!p||!hex(p.id,64)||!hex(p.callbackId,32)||!["pending","planned","running","needs_review","completed","ignored","blocked"].includes(p.state)||!text(p.reason)||!text(p.actor,256)||!date(p.created)||!date(p.updated)||!integer(p.count,20000)||!validMediaDeleteSpec(p.policy)||!parentStates.includes(p.parentState))return false;
 const e=p.event;if(!e||!["","Movie","Series","Season","Episode"].includes(e.type)||!text(e.name,2000)||!text(e.path)||typeof e.virtual!=="boolean"||typeof e.missingVirtual!=="boolean"||!integer(e.season,99999)||!integer(e.episode,99999)||!(e.tmdbId===""||id(e.tmdbId)))return false;
 if(p.parentId==="")return p.parentFingerprint===""&&p.parentState===""&&p.count===0&&!["planned","running","needs_review","completed"].includes(p.state);
 return hex(p.parentId,64)&&hex(p.parentFingerprint,64)&&p.count>0&&!!p.parentState&&(p.state!=="completed"||p.parentState==="completed");
}
export function validMediaDeleteExecution(v:MediaDeleteExecutionReview):boolean{
 if(!v||!validMediaDeleteReceipt(v.receipt)||!v.receipt.parentId||!v.parent)return false;
 const p=v.parent;if(p.id!==v.receipt.parentId||p.creationFingerprint!==v.receipt.parentFingerprint||!parentStates.includes(p.state)||!p.state||!hex(p.fingerprint,64)||typeof p.canContinue!=="boolean"||!text(p.reason)||!integer(p.dedupeRows,2000000)||!Array.isArray(p.historyIds)||p.historyIds.length!==v.receipt.count||!p.historyIds.every(id)||new Set(p.historyIds).size!==p.historyIds.length||!Array.isArray(p.units)||p.units.length>20000)return false;
 return p.units.every((u,i)=>!!u&&u.ordinal===i&&text(u.rootId)&&!!u.rootId&&u.driver===""&&text(u.path)&&!!u.path&&["source","cleanup"].includes(u.role)&&typeof u.absent==="boolean"&&[u.files,u.directories,u.links].every(n=>integer(n,20000))&&integer(u.bytes,2**50)&&Array.isArray(u.sample)&&u.sample.length<=20000&&u.sample.every(s=>text(s)));
}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
 let r:Response;try{r=await fetch(path,{method:body===undefined?"GET":"POST",headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},body:body===undefined?undefined:JSON.stringify(body)})}catch{throw new ApiError("同步删除响应未确认，请只读查回原编号，不要重复操作",503)}
 const out=await r.json().catch(()=>{throw new ApiError("原同步删除回执未核实，请只读查回",503)}) as {code?:number;success?:boolean;message?:string;data:T};
 if(!r.ok||out.code!==0||out.success!==true)throw new ApiError(out.message||"原同步删除操作未确认",r.status===401?401:out.code||r.status||503);return out.data;
}
const base="/api/v1/media-sync-delete";
export async function getMediaDeleteStatus(token:string):Promise<MediaDeleteStatus>{const s=await request<MediaDeleteStatus>(token,base);if(!validMediaDeleteStatus(s))throw new ApiError("完整同步删除状态未核实",503);return s}
export async function reviewMediaDelete(token:string,status:MediaDeleteStatus,rootIds:string[],http:boolean,confirmRead:boolean):Promise<MediaDeleteReview>{
 if(!validMediaDeleteStatus(status)||!status.ready||!confirmRead||!Array.isArray(rootIds)||rootIds.length>256||new Set(rootIds).size!==rootIds.length||!rootIds.every(id=>status.roots.some(r=>r.id===id))||(status.spec.deleteSource?rootIds.length===0:rootIds.length!==0))throw new ApiError("先核对已保存设置并选择明确源目录，确认只读审核",400);
 const v=await request<MediaDeleteReview>(token,base+"/review",{fingerprint:status.fingerprint,rootIds,http,confirmRead:true});if(!validMediaDeleteReview(v)||specKey(v.spec)!==specKey(status.spec)||v.roots.length!==rootIds.length||v.roots.some(r=>!rootIds.includes(r.id)))throw new ApiError("原同步删除审核范围未核实",503);return v;
}
const allowed=(c:MediaDeleteConsents,spec:MediaDeleteSpec,files:boolean)=>c.confirm&&c.history&&c.dedupe&&c.closeRelated&&c.legacyStopped&&(!files||c.source&&c.onlyCopy&&c.expanded)&&(!spec.notify||c.notify);
export async function authorizeMediaDelete(token:string,review:MediaDeleteReview,c:MediaDeleteConsents):Promise<MediaDeleteStatus>{
 if(!validMediaDeleteReview(review)||!allowed(c,review.spec,review.spec.deleteSource))throw new ApiError("需要分别确认未来原历史、去重、关联执行器、源文件和所选通知",400);
 const s=await request<MediaDeleteStatus>(token,base+"/authorize",{id:review.id,fingerprint:review.fingerprint,...c});if(!validMediaDeleteStatus(s)||!s.active||specKey(s.spec)!==specKey(review.spec))throw new ApiError("启用响应未核实，请只读查回，不要再次授权",503);return s;
}
export async function stopMediaDelete(token:string,status:MediaDeleteStatus,confirm:boolean):Promise<MediaDeleteStatus>{
 if(!validMediaDeleteStatus(status)||!confirm)throw new ApiError("先核对并确认停止未来同步删除",400);const s=await request<MediaDeleteStatus>(token,base+"/stop",{fingerprint:status.fingerprint,confirm:true});if(!validMediaDeleteStatus(s)||s.active)throw new ApiError("停止响应未核实，请只读查回",503);return s;
}
export async function getMediaDeleteReceipt(token:string,original:string):Promise<MediaDeleteReceipt>{
 if(!hex(original,64))throw new ApiError("需要完整原同步删除编号",400);const p=await request<MediaDeleteReceipt>(token,base+"/receipts/"+original);if(!validMediaDeleteReceipt(p)||p.id!==original)throw new ApiError("原同步删除回执未核实",503);return p;
}
export async function listMediaDeleteReceipts(token:string,page=1):Promise<MediaDeleteReceipt[]>{
 if(!Number.isSafeInteger(page)||page<1||page>10000)throw new ApiError("无效回执页码",400);const items=await request<MediaDeleteReceipt[]>(token,base+"/receipts?page="+page);if(!Array.isArray(items)||items.length>30||!items.every(validMediaDeleteReceipt))throw new ApiError("完整原同步删除回执页未核实",503);return items;
}
export async function ignoreMediaDelete(token:string,p:MediaDeleteReceipt,reason:string,confirm:boolean):Promise<MediaDeleteReceipt>{
 if(!validMediaDeleteReceipt(p)||!confirm||!reason.trim()||reason.length>1000||["completed","ignored"].includes(p.state))throw new ApiError("先核对原未结束回执，填写原因并确认停止后台",400);
 const v=await request<MediaDeleteReceipt>(token,base+"/receipts/"+p.id+"/ignore",{confirm:true,reason});if(!validMediaDeleteReceipt(v)||v.id!==p.id||v.parentId!==p.parentId||v.state!=="ignored"||v.reason!==reason||!v.actor)throw new ApiError("忽略响应未核实，请只读查回，没有重做删除",503);return v;
}
export async function reviewMediaDeleteExecution(token:string,p:MediaDeleteReceipt,confirmRead:boolean):Promise<MediaDeleteExecutionReview>{
 if(!validMediaDeleteReceipt(p)||!p.parentId||!confirmRead)throw new ApiError("先确认只读读取原完整删除范围",400);
 const v=await request<MediaDeleteExecutionReview>(token,base+"/receipts/"+p.id+"/review",{confirmRead:true});if(!validMediaDeleteExecution(v)||v.receipt.id!==p.id||v.parent.id!==p.parentId)throw new ApiError("原完整删除审核未核实",503);return v;
}
export async function continueMediaDelete(token:string,v:MediaDeleteExecutionReview,c:MediaDeleteConsents):Promise<MediaDeleteReceipt>{
 if(!validMediaDeleteExecution(v)||!v.parent.canContinue||!allowed(c,v.receipt.policy,v.parent.units.length>0))throw new ApiError("需要原完整范围的新只读审核与各项独立继续确认",400);
 const p=await request<MediaDeleteReceipt>(token,base+"/receipts/"+v.receipt.id+"/continue",{confirmRead:true,fingerprint:v.parent.fingerprint,...c});if(!validMediaDeleteReceipt(p)||p.id!==v.receipt.id||p.parentId!==v.parent.id||p.state!=="completed")throw new ApiError("原继续结果未核实，请只读查回，不要再次执行",503);return p;
}
