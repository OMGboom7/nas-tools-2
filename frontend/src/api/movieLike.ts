import {ApiError} from "./client";

export type FavoriteSpec={enabled:boolean;directory:string;mappings:{local:string;remote:string}[]};
export type FavoriteRoot={id:string;path:string;label:string;type:string};
export type FavoriteStatus={installed:boolean;active:boolean;current:boolean;ready:boolean;workerRunning:boolean;fingerprint:string;reason:string;spec:FavoriteSpec;roots:FavoriteRoot[];categories:string[]};
export type FavoriteReview={id:string;fingerprint:string;created:string;mediaId:string;http:boolean;spec:FavoriteSpec;roots:{id:string;path:string;identity:string;configuredPath:string}[];categories:string[]};
export type FavoriteReceipt={id:string;callbackId:string;event:{name:string;path:string;itemId:string};state:"pending"|"planned"|"running"|"needs_review"|"completed"|"ignored"|"blocked";phase:string;reason:string;actor:string;created:string;updated:string};
export type FavoriteConsent={confirm:boolean;move:boolean;whole:boolean;sourceRemoval:boolean;closeRelated:boolean;legacyStopped:boolean;refresh:boolean};
export type FavoriteMove={id:string;state:string;phase:string;rootId:string;source:string;target:string;files:number;directories:number;links:number;bytes:number;sample:string[];owners:{kind:string;id:string;state:string;fingerprint:string;batchId?:string;related:boolean}[];fingerprint:string;canContinue:boolean;evidence:string};
export type FavoriteExecution={receipt:FavoriteReceipt;move:FavoriteMove};
const base="/api/v1/movie-like";
const hex=(v:unknown,n=64):v is string=>typeof v==="string"&&new RegExp("^[a-f0-9]{"+n+"}$").test(v);
const text=(v:unknown,n=4096):v is string=>typeof v==="string"&&v.length<=n&&!/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(v);
const integer=(v:unknown,max:number):v is number=>typeof v==="number"&&Number.isSafeInteger(v)&&v>=0&&v<=max;
const date=(v:unknown)=>typeof v==="string"&&v.length<100&&Number.isFinite(Date.parse(v));
const component=(s:unknown):s is string=>text(s,255)&&!!s&&s!=="."&&s!==".."&&!/[\\/]/.test(s)&&!s.startsWith(".nastool-");
const relative=(s:unknown):s is string=>text(s)&&!!s&&!s.startsWith("/")&&!/[\\\x00\r\n]/.test(s)&&s.split("/").length===2&&s.split("/").every(component);
const specKey=(s:FavoriteSpec)=>JSON.stringify([s.enabled,s.directory,s.mappings]);
const phases=["","planned","preparing","rename","copying","publish","capture","cleaning","cleanup","moved","completed"];
export function validFavoriteSpec(s:FavoriteSpec):boolean{return !!s&&typeof s.enabled==="boolean"&&(s.directory===""||component(s.directory))&&Array.isArray(s.mappings)&&s.mappings.length<=3&&s.mappings.every(m=>!!m&&text(m.local)&&!!m.local&&text(m.remote)&&!!m.remote)}
export function validFavoriteStatus(s:FavoriteStatus):boolean{return !!s&&validFavoriteSpec(s.spec)&&[s.installed,s.active,s.current,s.ready,s.workerRunning].every(v=>typeof v==="boolean")&&(!s.active||s.current&&s.installed&&s.spec.enabled)&&hex(s.fingerprint)&&text(s.reason)&&Array.isArray(s.categories)&&s.categories.length<=1000&&s.categories.every(component)&&new Set(s.categories).size===s.categories.length&&Array.isArray(s.roots)&&s.roots.length<=128&&s.roots.every(r=>!!r&&text(r.id)&&!!r.id&&text(r.path)&&!!r.path&&text(r.label)&&r.type==="MOV")&&new Set(s.roots.map(r=>r?.id)).size===s.roots.length}
export function validFavoriteReview(p:FavoriteReview):boolean{return !!p&&hex(p.id,32)&&hex(p.fingerprint)&&date(p.created)&&text(p.mediaId,512)&&!!p.mediaId&&typeof p.http==="boolean"&&validFavoriteSpec(p.spec)&&p.spec.enabled&&component(p.spec.directory)&&Array.isArray(p.categories)&&p.categories.length>0&&p.categories.length<=1000&&p.categories.every(component)&&new Set(p.categories).size===p.categories.length&&Array.isArray(p.roots)&&p.roots.length>0&&p.roots.length<=128&&new Set(p.roots.map(r=>r?.id)).size===p.roots.length&&p.roots.every(r=>!!r&&text(r.id)&&!!r.id&&text(r.path)&&!!r.path&&text(r.identity)&&!!r.identity&&text(r.configuredPath)&&!!r.configuredPath)}
export function validFavoriteReceipt(p:FavoriteReceipt):boolean{return !!p&&hex(p.id)&&hex(p.callbackId,32)&&["pending","planned","running","needs_review","completed","ignored","blocked"].includes(p.state)&&phases.includes(p.phase)&&text(p.reason)&&text(p.actor,256)&&date(p.created)&&date(p.updated)&&!!p.event&&text(p.event.name,600)&&text(p.event.path)&&text(p.event.itemId,512)&&(p.state!=="completed"||p.phase==="completed")&&(!["planned","running","needs_review"].includes(p.state)||!!p.phase)}
export function validFavoriteExecution(p:FavoriteExecution):boolean{
 if(!p||!validFavoriteReceipt(p.receipt)||!p.move)return false;const v=p.move;
 return v.id===p.receipt.id&&v.state===p.receipt.state&&v.phase===p.receipt.phase&&text(v.rootId)&&!!v.rootId&&relative(v.source)&&relative(v.target)&&v.source!==v.target&&v.source.split("/")[1]===v.target.split("/")[1]&&[v.files,v.directories,v.links].every(n=>integer(n,20000))&&v.files+v.directories+v.links>0&&v.files+v.directories+v.links<=20000&&integer(v.bytes,2**50)&&Array.isArray(v.sample)&&v.sample.length<=100&&v.sample.every(s=>text(s))&&Array.isArray(v.owners)&&v.owners.length<=10000&&v.owners.every(o=>!!o&&["organization","library"].includes(o.kind)&&(hex(o.id,32)||hex(o.id))&&text(o.state)&&hex(o.fingerprint)&&typeof o.related==="boolean"&&(o.batchId===undefined||hex(o.batchId)))&&hex(v.fingerprint)&&typeof v.canContinue==="boolean"&&text(v.evidence)&&(!v.canContinue||!["completed","ignored"].includes(v.state));
}
const granted=(c:FavoriteConsent)=>!!c&&c.confirm&&c.move&&c.whole&&c.sourceRemoval&&c.closeRelated&&c.legacyStopped&&c.refresh;
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
 let r:Response;try{r=await fetch(base+path,{method:body===undefined?"GET":"POST",headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},body:body===undefined?undefined:JSON.stringify(body)})}catch{throw new ApiError("电影精选响应未确认，请只读查回原编号，不要重复移动",503)}
 const out=await r.json().catch(()=>{throw new ApiError("电影精选原回执未核实，请只读查回",503)}) as {code?:number;success?:boolean;message?:string;data:T};
 if(!r.ok||out.code!==0||out.success!==true)throw new ApiError(out.message||"原电影精选操作未确认",r.status===401?401:out.code||r.status||503);return out.data;
}
export async function getFavoriteStatus(token:string):Promise<FavoriteStatus>{const v=await request<FavoriteStatus>(token,"");if(!validFavoriteStatus(v))throw new ApiError("完整原电影精选状态未核实",503);return v}
export async function reviewFavorite(token:string,status:FavoriteStatus,roots:string[],http:boolean,confirmRead:boolean):Promise<FavoriteReview>{
 if(!validFavoriteStatus(status)||!status.ready||!confirmRead||!Array.isArray(roots)||roots.length===0||roots.length>128||new Set(roots).size!==roots.length||!roots.every(id=>status.roots.some(r=>r.id===id)))throw new ApiError("先核对设置，选择完整电影库并确认只读身份审核",400);
 const v=await request<FavoriteReview>(token,"/review",{fingerprint:status.fingerprint,rootIds:roots,http,confirmRead:true});
 if(!validFavoriteReview(v)||specKey(v.spec)!==specKey(status.spec)||JSON.stringify(v.categories)!==JSON.stringify(status.categories)||v.roots.length!==roots.length||v.roots.some(r=>!roots.includes(r.id)))throw new ApiError("完整原电影精选审核范围未核实",503);return v;
}
export async function authorizeFavorite(token:string,p:FavoriteReview,c:FavoriteConsent):Promise<FavoriteStatus>{
 if(!validFavoriteReview(p)||!granted(c))throw new ApiError("请分别确认未来整目录移动、源清理、原写入权关闭、旧执行器停止及独立刷新",400);
 const v=await request<FavoriteStatus>(token,"/authorize",{id:p.id,fingerprint:p.fingerprint,...c});if(!validFavoriteStatus(v)||!v.active||specKey(v.spec)!==specKey(p.spec))throw new ApiError("启用响应未核实，请只读核对，不要再次授权",503);return v;
}
export async function stopFavorite(token:string,p:FavoriteStatus,confirm:boolean):Promise<FavoriteStatus>{
 if(!validFavoriteStatus(p)||!confirm)throw new ApiError("先核对并明确停止未来电影精选移动",400);const v=await request<FavoriteStatus>(token,"/stop",{fingerprint:p.fingerprint,confirm:true});if(!validFavoriteStatus(v)||v.active)throw new ApiError("停止响应未核实，请只读查询",503);return v;
}
export async function getFavoriteReceipt(token:string,id:string):Promise<FavoriteReceipt>{if(!hex(id))throw new ApiError("需要完整原移动编号",400);const v=await request<FavoriteReceipt>(token,"/receipts/"+id);if(!validFavoriteReceipt(v)||v.id!==id)throw new ApiError("原移动回执未核实",503);return v}
export async function listFavoriteReceipts(token:string,page=1):Promise<FavoriteReceipt[]>{if(!Number.isSafeInteger(page)||page<1||page>10000)throw new ApiError("无效回执页码",400);const v=await request<FavoriteReceipt[]>(token,"/receipts?page="+page);if(!Array.isArray(v)||v.length>30||!v.every(validFavoriteReceipt))throw new ApiError("完整原移动回执页未核实",503);return v}
export async function reviewFavoriteExecution(token:string,p:FavoriteReceipt,confirmRead:boolean):Promise<FavoriteExecution>{
 if(!validFavoriteReceipt(p)||!p.phase||!confirmRead)throw new ApiError("先查回原移动编号，确认读取整目录及已有凭证",400);
 const v=await request<FavoriteExecution>(token,"/receipts/"+p.id+"/review",{confirmRead:true});if(!validFavoriteExecution(v)||v.receipt.id!==p.id)throw new ApiError("原完整目录审核未核实",503);return v;
}
export async function continueFavorite(token:string,p:FavoriteExecution,c:FavoriteConsent):Promise<FavoriteReceipt>{
 if(!validFavoriteExecution(p)||!p.move.canContinue||!granted(c))throw new ApiError("需要同一完整原目录的新审核及每项独立继续确认",400);
 const v=await request<FavoriteReceipt>(token,"/receipts/"+p.receipt.id+"/continue",{fingerprint:p.move.fingerprint,confirmRead:true,...c});if(!validFavoriteReceipt(v)||v.id!==p.receipt.id||v.state!=="completed")throw new ApiError("本次继续结果未核实，请只读查回，不要再次移动",503);return v;
}
export async function ignoreFavorite(token:string,p:FavoriteReceipt,reason:string,confirm:boolean):Promise<FavoriteReceipt>{
 if(!validFavoriteReceipt(p)||["completed","ignored"].includes(p.state)||!text(reason,1000)||!reason.trim()||!confirm)throw new ApiError("确认停止原未完成移动并填写原因",400);
 const v=await request<FavoriteReceipt>(token,"/receipts/"+p.id+"/ignore",{confirm:true,reason:reason.trim()});if(!validFavoriteReceipt(v)||v.id!==p.id||v.state!=="ignored")throw new ApiError("停止原回执未核实，请只读查回",503);return v;
}
