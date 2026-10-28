import {ApiError} from "./client";
export type LimitSpec={download:number;upload:number;idleDownload:number;idleUpload:number;bandwidth:number;interval:number;ipv4:string;ipv6:string;downloaders:string[];ratios:number[];notify:boolean};
export type LimiterTarget={id:string;name:string;kind:"qbittorrent"|"transmission";weight:number};
export type LimitRates={upload:number;download:number;otherUpload:number;otherDownload:number;uploadLimited:boolean;downloadLimited:boolean;alternative:boolean;scheduled:boolean;unit:"B/s"|"RPC KB/s"};
export type LimitObservation={playing:number;exempt:number;unknownBitrate:number;bitrate:number};
export type LimiterStatus={installed:boolean;active:boolean;current:boolean;server:string;cloud:boolean;workerRunning:boolean;fingerprint:string;spec:LimitSpec;reason:string;targets:LimiterTarget[]};
export type LimiterReview={id:string;fingerprint:string;created:string;server:string;mediaId:string;cloud:boolean;http:boolean;spec:LimitSpec;observation:LimitObservation;targets:(LimiterTarget&{before:LimitRates;desired:LimitRates})[]};
export type LimitOperation={ordinal:number;targetId:string;name:string;kind:"qbittorrent"|"transmission";dimension:"upload"|"download"|"pair";before:LimitRates;after:LimitRates;state:string;reason:string;actor:string;updated:string};
export type LimiterJob={id:string;source:"timer"|"webhook";reference:string;observation:LimitObservation;state:"not_finalized"|"confirmed"|"partial"|"stopped"|"ignored";created:string;updated:string;operations:LimitOperation[]};
export type LimiterTrigger={id:string;source:"timer"|"webhook";reference:string;due:string;state:"pending"|"planned"|"blocked";reason:string;created:string};
export type LimiterReadConsent={http:boolean;plexAccount:boolean;plexHTTP:boolean};
export type LimiterGrant={confirm:boolean;global:boolean;unlimited:boolean;legacyStopped:boolean;notify:boolean};
const hex=(v:unknown,n:number):v is string=>typeof v==="string"&&new RegExp("^[a-f0-9]{"+n+"}$").test(v);
const text=(v:unknown,n=4096):v is string=>typeof v==="string"&&v.length<=n&&!/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(v);
const integer=(v:unknown,max:number):v is number=>typeof v==="number"&&Number.isSafeInteger(v)&&v>=0&&v<=max;
const date=(v:unknown)=>typeof v==="string"&&v.length<100&&Number.isFinite(Date.parse(v));
const id=(v:unknown):v is string=>typeof v==="string"&&/^[1-9]\d{0,18}$/.test(v)&&BigInt(v)<=9223372036854775807n;
const specIdentity=(v:LimitSpec)=>JSON.stringify([v.download,v.upload,v.idleDownload,v.idleUpload,v.bandwidth,v.interval,v.ipv4,v.ipv6,v.downloaders,v.ratios,v.notify]);
export function validLimitSpec(v:LimitSpec):boolean{
 return !!v&&[v.download,v.upload,v.idleDownload,v.idleUpload].every(n=>integer(n,2**30))&&integer(v.bandwidth,1e6)&&integer(v.interval,86400)&&v.interval>0&&text(v.ipv4,16000)&&text(v.ipv6,16000)&&typeof v.notify==="boolean"&&Array.isArray(v.downloaders)&&v.downloaders.length<=64&&v.downloaders.every(id)&&new Set(v.downloaders).size===v.downloaders.length&&Array.isArray(v.ratios)&&(v.ratios.length===0||v.ratios.length===v.downloaders.length)&&v.ratios.every(n=>integer(n,1e6)&&n>0);
}
function validTarget(v:LimiterTarget):boolean{return !!v&&id(v.id)&&text(v.name)&&["qbittorrent","transmission"].includes(v.kind)&&integer(v.weight,1e6)&&v.weight>0}
export function validLimitRates(v:LimitRates,kind:string):boolean{
 if(!v||!["qbittorrent","transmission"].includes(kind)||v.unit!==(kind==="qbittorrent"?"B/s":"RPC KB/s"))return false;
 const max=kind==="qbittorrent"?2**40:2**30;
 if(![v.upload,v.download,v.otherUpload,v.otherDownload].every(n=>Number.isFinite(n)&&n>=0&&n<=max&&(kind!=="qbittorrent"||Number.isInteger(n))))return false;
 return [v.uploadLimited,v.downloadLimited,v.alternative,v.scheduled].every(n=>typeof n==="boolean")&&(kind!=="qbittorrent"||v.uploadLimited===(v.upload>0)&&v.downloadLimited===(v.download>0));
}
function validObservation(v:LimitObservation):boolean{return !!v&&[v.playing,v.exempt,v.unknownBitrate].every(n=>integer(n,1000))&&v.playing+v.exempt<=1000&&v.unknownBitrate<=v.playing&&integer(v.bitrate,2**50)}
export function validLimiterStatus(v:LimiterStatus):boolean{
 return !!v&&[v.installed,v.active,v.current,v.cloud,v.workerRunning].every(n=>typeof n==="boolean")&&(!v.active||v.installed&&v.current)&&["","emby","jellyfin","plex"].includes(v.server)&&hex(v.fingerprint,64)&&text(v.reason)&&validLimitSpec(v.spec)&&Array.isArray(v.targets)&&v.targets.length<=64&&v.targets.every(validTarget);
}
export function validLimiterReview(v:LimiterReview):boolean{
 return !!v&&hex(v.id,32)&&hex(v.fingerprint,64)&&date(v.created)&&["emby","jellyfin","plex"].includes(v.server)&&text(v.mediaId,512)&&!!v.mediaId&&typeof v.cloud==="boolean"&&typeof v.http==="boolean"&&validLimitSpec(v.spec)&&validObservation(v.observation)&&Array.isArray(v.targets)&&v.targets.length>0&&v.targets.length<=64&&new Set(v.targets.map(t=>t.id)).size===v.targets.length&&v.targets.every(t=>validTarget(t)&&v.spec.downloaders.includes(t.id)&&validLimitRates(t.before,t.kind)&&validLimitRates(t.desired,t.kind));
}
export function validLimiterJob(v:LimiterJob):boolean{
 if(!v||!hex(v.id,64)||!["timer","webhook"].includes(v.source)||!text(v.reference)||!["not_finalized","confirmed","partial","stopped","ignored"].includes(v.state)||!date(v.created)||!date(v.updated)||!validObservation(v.observation)||!Array.isArray(v.operations)||v.operations.length>128)return false;
 return v.operations.every((op,i)=>!!op&&op.ordinal===i&&id(op.targetId)&&text(op.name)&&["qbittorrent","transmission"].includes(op.kind)&&["upload","download","pair"].includes(op.dimension)&&validLimitRates(op.before,op.kind)&&validLimitRates(op.after,op.kind)&&["pending","intent","confirmed","verified_current","uncertain","blocked","ignored"].includes(op.state)&&text(op.reason)&&text(op.actor,256)&&date(op.updated)&&(op.state!=="verified_current"&&op.state!=="ignored"||!!op.actor));
}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
 let r:Response;
 try{r=await fetch(path,{method:body===undefined?"GET":"POST",headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},body:body===undefined?undefined:JSON.stringify(body)})}catch{throw new ApiError("限速响应未确认，请只读查回，不要重复操作",503)}
 const out=await r.json().catch(()=>{throw new ApiError("限速回执格式未核实，请只读查回",503)}) as {code?:number;success?:boolean;message?:string;data:T};
 if(!r.ok||out.code!==0||out.success!==true)throw new ApiError(out.message||"原限速操作未确认",r.status===401?401:out.code||r.status||503);return out.data;
}
const base="/api/v1/speed-limiter";
export async function getLimiterStatus(token:string):Promise<LimiterStatus>{
 const v=await request<LimiterStatus>(token,base);if(!validLimiterStatus(v))throw new ApiError("完整限速状态未核实",503);return v;
}
export async function reviewLimiter(token:string,status:LimiterStatus,consent:LimiterReadConsent):Promise<LimiterReview>{
 if(!validLimiterStatus(status)||!status.installed||status.cloud&&!consent.plexAccount||consent.plexHTTP&&!consent.plexAccount)throw new ApiError("先核对已保存设置与原连接读取范围",400);
 const v=await request<LimiterReview>(token,base+"/review",{fingerprint:status.fingerprint,confirm:true,...consent});if(!validLimiterReview(v)||specIdentity(v.spec)!==specIdentity(status.spec)||v.server!==status.server)throw new ApiError("原限速审核未核实，请只读核对状态",503);return v;
}
export async function authorizeLimiter(token:string,review:LimiterReview,consent:LimiterGrant):Promise<LimiterStatus>{
 if(!validLimiterReview(review)||!consent.confirm||!consent.global||!consent.unlimited||!consent.legacyStopped||review.spec.notify&&!consent.notify)throw new ApiError("需要独立确认未来全局限速、0 的含义、旧执行器停止及所选通知",400);
 const v=await request<LimiterStatus>(token,base+"/authorize",{id:review.id,fingerprint:review.fingerprint,...consent});if(!validLimiterStatus(v)||!v.active||v.server!==review.server)throw new ApiError("启用回执未核实，请只读查回，不要重启或重复授权",503);return v;
}
export async function stopLimiter(token:string,status:LimiterStatus,confirm:boolean):Promise<LimiterStatus>{
 if(!validLimiterStatus(status)||!confirm)throw new ApiError("先核对并确认停止原限速后台",400);
 const v=await request<LimiterStatus>(token,base+"/stop",{fingerprint:status.fingerprint,confirm:true});if(!validLimiterStatus(v)||v.active)throw new ApiError("停止回执未核实，请只读查回",503);return v;
}
export async function getLimiterJob(token:string,original:string):Promise<LimiterJob>{
 if(!hex(original,64))throw new ApiError("需要完整原限速稳定编号",400);
 const v=await request<LimiterJob>(token,base+"/jobs/"+original);if(!validLimiterJob(v)||v.id!==original)throw new ApiError("原限速回执未核实",503);return v;
}
export async function listLimiterJobs(token:string,page=1):Promise<LimiterJob[]>{
 if(!Number.isSafeInteger(page)||page<1||page>10000)throw new ApiError("无效限速页码",400);
 const v=await request<LimiterJob[]>(token,base+"/jobs?page="+page);if(!Array.isArray(v)||v.length>30||!v.every(validLimiterJob))throw new ApiError("完整限速回执页未核实",503);return v;
}
export async function getLimiterTriggers(token:string):Promise<LimiterTrigger[]>{
 const v=await request<LimiterTrigger[]>(token,base+"/triggers");
 if(!Array.isArray(v)||v.length>30||!v.every(p=>!!p&&hex(p.id,64)&&["timer","webhook"].includes(p.source)&&text(p.reference)&&date(p.due)&&date(p.created)&&text(p.reason)&&["pending","planned","blocked"].includes(p.state)))throw new ApiError("完整限速触发记录未核实",503);return v;
}
export async function verifyLimiterOperation(token:string,job:LimiterJob,ordinal:number,confirm:boolean):Promise<LimiterJob>{
 if(!validLimiterJob(job)||!confirm||!integer(ordinal,127)||!job.operations[ordinal]||!["intent","uncertain"].includes(job.operations[ordinal].state))throw new ApiError("先选原未确认设置并确认只读核对",400);
 const v=await request<LimiterJob>(token,base+"/jobs/"+job.id+"/verify",{confirm:true,ordinal});
 if(!validLimiterJob(v)||!sameOperations(v,job)||v.operations[ordinal]?.state!=="verified_current")throw new ApiError("只读核对回执未确认，没有重发设置",503);return v;
}
function sameOperations(a:LimiterJob,b:LimiterJob):boolean{
 return a.id===b.id&&a.operations.length===b.operations.length&&a.operations.every((op,i)=>{
  const old=b.operations[i];return op.targetId===old.targetId&&op.dimension===old.dimension&&op.kind===old.kind&&JSON.stringify([op.before.upload,op.before.download,op.before.otherUpload,op.before.otherDownload,op.before.uploadLimited,op.before.downloadLimited,op.before.alternative,op.before.scheduled,op.before.unit,op.after.upload,op.after.download,op.after.otherUpload,op.after.otherDownload,op.after.uploadLimited,op.after.downloadLimited,op.after.alternative,op.after.scheduled,op.after.unit])===JSON.stringify([old.before.upload,old.before.download,old.before.otherUpload,old.before.otherDownload,old.before.uploadLimited,old.before.downloadLimited,old.before.alternative,old.before.scheduled,old.before.unit,old.after.upload,old.after.download,old.after.otherUpload,old.after.otherDownload,old.after.uploadLimited,old.after.downloadLimited,old.after.alternative,old.after.scheduled,old.after.unit]);
 });
}
export async function ignoreLimiterJob(token:string,job:LimiterJob,confirm:boolean,reason:string):Promise<LimiterJob>{
 if(!validLimiterJob(job)||!confirm||!reason.trim()||reason.length>1000||!job.operations.some(op=>!["confirmed","verified_current","ignored"].includes(op.state)))throw new ApiError("先核对原回执，填写原因并确认停止后台",400);
 const v=await request<LimiterJob>(token,base+"/jobs/"+job.id+"/ignore",{confirm:true,reason});
 if(!validLimiterJob(v)||!sameOperations(v,job)||v.state!=="ignored"||v.operations.some((op,i)=>!["confirmed","verified_current","ignored"].includes(op.state)||["confirmed","verified_current","ignored"].includes(job.operations[i].state)&&op.state!==job.operations[i].state))throw new ApiError("忽略或停止回执未核实，请只读查回",503);return v;
}
