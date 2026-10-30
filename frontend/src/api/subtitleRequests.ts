import {ApiError} from "./client";
import {validOpenSubtitleReceipt,validOpenSubtitleService,type OpenSubtitleReceipt,type OpenSubtitleService} from "./openSubtitles";

export type SubtitleDriver=""|"rclone"|"minio";
export type SubtitleRequestStatus={
  ready:boolean;reason:string;fingerprint:string;service:"ChineseSubFinder"|"OpenSubtitles"|"ChineseSubFinder+OpenSubtitles";origin:string;http:boolean;openSubtitles?:OpenSubtitleService;
  roots:{id:string;path:string;label:string;type:string}[];drivers:SubtitleDriver[];
};
export type SubtitleRequest={
  id:string;state:"review"|"queued"|"cancelled";queued:boolean;rootId:string;driver:SubtitleDriver;path:string;name:string;
  title:string;year:string;mediaType:"电影"|"电视剧"|"动漫";tmdbId:string;service:"ChineseSubFinder"|"OpenSubtitles"|"ChineseSubFinder+OpenSubtitles";serviceOrigin:string;openSubtitles?:OpenSubtitleReceipt;
  mappedPath:string;http:boolean;bytes:number;created:string;updated:string;fingerprint:string;reason:string;
  serviceState:"not_queued"|"pending"|"sending"|"submitted"|"uncertain"|"blocked"|"completed"|"not_found"|"partial";serviceUpdated:string;externalId:string;stoppedBy:string;
};
export type SubtitleRequestIdentity={kind:"movie"|"tv"|"anime";tmdbId:string;season?:number;episode?:number;endEpisode?:number};
export type SubtitleRequestSelection={rootId:string;driver:SubtitleDriver;path:string;name:string;identity?:SubtitleRequestIdentity;confirmRead:boolean;confirmRemoteRead:boolean};
export type SubtitleRequestConsent={confirm:boolean;confirmPath:boolean;confirmExternalWrite:boolean;confirmOldExecutorsStopped:boolean;http:boolean;confirmRemoteRead:boolean;confirmOpenSubtitles?:boolean;confirmRemoteSubtitleWrite?:boolean};
const base="/api/v1/organization/subtitles";
const encoder=new TextEncoder();
const text=(v:unknown,n=4096):v is string=>typeof v==="string"&&encoder.encode(v).length<=n&&!/[\x00-\x1f\x7f]/.test(v);
const hash=(v:unknown):v is string=>typeof v==="string"&&/^[a-f0-9]{64}$/.test(v);
const decimal=(v:unknown):v is string=>typeof v==="string"&&/^[1-9]\d{0,18}$/.test(v)&&BigInt(v)<=9223372036854775807n;
const date=(v:unknown):v is string=>typeof v==="string"&&v.length<100&&Number.isFinite(Date.parse(v));
const relative=(v:unknown):v is string=>text(v)&&!!v&&!v.startsWith("/")&&!v.includes("\\")&&v.split("/").every(p=>!!p&&p!=="."&&p!=="..");
const absolute=(v:unknown):v is string=>text(v)&&!!v&&(v.startsWith("/")||/^[A-Za-z]:\//.test(v))&&!v.includes("\\")&&!v.split("/").some(p=>p==="."||p==="..");
function origin(v:unknown,http:boolean):boolean{
  if(!text(v,2048)||!v)return false;
  try{const u=new URL(v);return !!u.hostname&&!u.username&&!u.password&&!u.search&&!u.hash&&u.pathname==="/"&&(http?u.protocol==="http:":u.protocol==="https:")}catch{return false}
}
function integer(v:unknown,max:number):v is number{return typeof v==="number"&&Number.isSafeInteger(v)&&v>=0&&v<=max}
export function validSubtitleRequestStatus(s:SubtitleRequestStatus):boolean{
  return !!s&&typeof s.ready==="boolean"&&typeof s.http==="boolean"&&["ChineseSubFinder","OpenSubtitles","ChineseSubFinder+OpenSubtitles"].includes(s.service)&&hash(s.fingerprint)&&text(s.reason)&&
    (s.service==="ChineseSubFinder"?s.openSubtitles===undefined:!!s.openSubtitles&&validOpenSubtitleService(s.openSubtitles))&&
    (s.service!=="OpenSubtitles"||!s.http&&s.origin===s.openSubtitles?.origin)&&
    (s.ready?origin(s.origin,s.http):s.origin===""&&!s.http)&&Array.isArray(s.roots)&&s.roots.length<=512&&
    s.roots.every(r=>!!r&&text(r.id,256)&&!!r.id&&absolute(r.path)&&text(r.label,600)&&text(r.type,100))&&
    new Set(s.roots.map(r=>r.id)).size===s.roots.length&&Array.isArray(s.drivers)&&s.drivers.length>0&&s.drivers.length<=3&&
    s.drivers.includes("")&&s.drivers.every(v=>["","rclone","minio"].includes(v))&&new Set(s.drivers).size===s.drivers.length;
}
export function validSubtitleRequest(p:SubtitleRequest):boolean{
  if(!p||!hash(p.id)||!hash(p.fingerprint)||!["review","queued","cancelled"].includes(p.state)||typeof p.queued!=="boolean"||
    !text(p.rootId,256)||!p.rootId||!["","rclone","minio"].includes(p.driver)||!relative(p.path)||!text(p.name,600)||!p.name||
    !text(p.title,600)||!p.title||!text(p.year,4)||!["电影","电视剧","动漫"].includes(p.mediaType)||!decimal(p.tmdbId)||
    !["ChineseSubFinder","OpenSubtitles","ChineseSubFinder+OpenSubtitles"].includes(p.service)||typeof p.http!=="boolean"||!origin(p.serviceOrigin,p.http)||!absolute(p.mappedPath)||
    !integer(p.bytes,2**50)||!date(p.created)||!date(p.updated)||!text(p.reason,1000)||!text(p.stoppedBy,256)||
    !["not_queued","pending","sending","submitted","uncertain","blocked","completed","not_found","partial"].includes(p.serviceState)||!text(p.externalId,512))return false;
  if(p.service==="ChineseSubFinder"?p.openSubtitles!==undefined:!p.openSubtitles||!validOpenSubtitleReceipt(p.openSubtitles)||p.openSubtitles.reference!==p.id)return false;
  if(p.service==="OpenSubtitles"&&(p.serviceOrigin!==p.openSubtitles?.origin||p.http||p.externalId))return false;
  if(p.service!=="OpenSubtitles"&&["completed","not_found","partial"].includes(p.serviceState))return false;
  if(p.service==="OpenSubtitles"&&p.queued&&p.openSubtitles){
    const expected=p.openSubtitles.state==="running"?"sending":p.openSubtitles.state==="stopped"?"blocked":p.openSubtitles.state;
    if(p.serviceState!==expected)return false;
  }
  if(p.openSubtitles&&(p.queued?p.openSubtitles.state==="not_queued":p.openSubtitles.state!=="not_queued"))return false;
  return (p.state!=="review"||!p.queued)&&(p.state!=="queued"||p.queued)&&
    (p.queued?p.serviceState!=="not_queued":p.serviceState==="not_queued")&&
    (p.queued?date(p.serviceUpdated):p.serviceUpdated==="")&&
    (p.state!=="cancelled"||!p.queued||p.serviceState==="blocked")&&
    (p.state==="cancelled"?!!p.stoppedBy:p.stoppedBy==="")&&
    (p.serviceState==="submitted"?!!p.externalId:p.externalId==="");
}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
  const r=await fetch(base+path,{method:body===undefined?"GET":"POST",headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},...(body===undefined?{}:{body:JSON.stringify(body)})});
  const value=await r.json().catch(()=>null) as {code?:number;success?:boolean;message?:string;data?:T}|null;
  if(!r.ok||value?.code!==0||value.success!==true||value.data===undefined)throw new ApiError(value?.message||"字幕请求未获明确应答，请按原编号只读核对",r.status===401?401:value?.code||r.status);
  return value.data;
}
function checked(p:SubtitleRequest):SubtitleRequest{if(!validSubtitleRequest(p))throw new ApiError("原字幕回执字段未核实，请只读查询",503);return p}
function original(p:SubtitleRequest,ref:SubtitleRequest):SubtitleRequest{
  checked(p);
  for(const key of ["id","fingerprint","rootId","driver","path","name","title","year","mediaType","tmdbId","service","serviceOrigin","mappedPath","http","bytes","created"] as const){
    if(p[key]!==ref[key])throw new ApiError("响应没有对应同一原文件及字幕请求，请只读查询原编号",503);
  }
  if(!!p.openSubtitles!==!!ref.openSubtitles||p.openSubtitles&&ref.openSubtitles&&["id","reference","provider","origin"].some(k=>p.openSubtitles?.[k as keyof OpenSubtitleReceipt]!==ref.openSubtitles?.[k as keyof OpenSubtitleReceipt]))throw new ApiError("返回了另一份 OpenSubtitles 范围，请只读查回",503);
  return p;
}
export async function getSubtitleRequestStatus(token:string):Promise<SubtitleRequestStatus>{
  const out=await request<SubtitleRequestStatus>(token,"");if(!validSubtitleRequestStatus(out))throw new ApiError("字幕原设置未核实",503);return out;
}
export async function reviewSubtitleRequest(token:string,status:SubtitleRequestStatus,incoming:SubtitleRequestSelection):Promise<SubtitleRequest>{
  if(!validSubtitleRequestStatus(status)||!status.ready||!status.roots.some(r=>r.id===incoming.rootId)||!status.drivers.includes(incoming.driver)||
    !relative(incoming.path)||!text(incoming.name,600)||!incoming.confirmRead||!!incoming.driver&&!incoming.confirmRemoteRead)throw new ApiError("先选择一个原文件并确认只读核验",400);
  if(incoming.identity){
    const i=incoming.identity;
    if(!["movie","tv","anime"].includes(i.kind)||!decimal(i.tmdbId)||[i.season,i.episode,i.endEpisode].some(v=>v!==undefined&&!integer(v,9999))||
      i.kind==="movie"&&[i.season,i.episode,i.endEpisode].some(v=>v!==undefined)||i.kind!=="movie"&&i.season===undefined||
      i.endEpisode!==undefined&&(i.episode===undefined||i.endEpisode<i.episode))throw new ApiError("请核对指定的媒体身份及季集",400);
  }
  const out=checked(await request<SubtitleRequest>(token,"/review",{...incoming,fingerprint:status.fingerprint}));
  if(out.state!=="review"||out.queued||out.rootId!==incoming.rootId||out.driver!==incoming.driver||out.path!==incoming.path||
    out.name!==(incoming.name||incoming.path.split("/").at(-1))||out.serviceOrigin!==status.origin||out.http!==status.http)throw new ApiError("审核没有对应所选原文件，请重新读取设置",503);
  if(incoming.identity&&(out.tmdbId!==incoming.identity.tmdbId||(incoming.identity.kind==="movie")!==(out.mediaType==="电影")))throw new ApiError("返回的媒体身份与本次指定不同",503);
  if(!!out.openSubtitles!==!!status.openSubtitles||out.openSubtitles&&status.openSubtitles&&(out.openSubtitles.provider!==status.openSubtitles.provider||out.openSubtitles.origin!==status.openSubtitles.origin))throw new ApiError("字幕提供方与本次原审核不同",503);
  return out;
}
export async function queueSubtitleRequest(token:string,review:SubtitleRequest,consent:SubtitleRequestConsent):Promise<SubtitleRequest>{
  checked(review);const age=Date.now()-Date.parse(review.created);
  if(review.state!=="review"||age<0||age>600000||!consent.confirm||!consent.confirmPath||!consent.confirmExternalWrite||
    !consent.confirmOldExecutorsStopped||review.http&&!consent.http||!!review.driver&&!consent.confirmRemoteRead||review.openSubtitles&&(!consent.confirmOpenSubtitles||!!review.driver&&!consent.confirmRemoteSubtitleWrite))throw new ApiError("重新审核原文件并逐项确认本次字幕提交",400);
  const out=original(await request<SubtitleRequest>(token,"/requests",{reviewId:review.id,fingerprint:review.fingerprint,...consent}),review);
  if(out.state!=="queued"||!out.queued)throw new ApiError("原字幕请求状态未核实，请只读查回",503);return out;
}
export async function getSubtitleRequest(token:string,id:string):Promise<SubtitleRequest>{
  if(!hash(id))throw new ApiError("请输入完整原字幕请求编号",400);
  const out=checked(await request<SubtitleRequest>(token,"/requests/"+id));
  if(out.id!==id)throw new ApiError("返回了另一份字幕请求，请只读查回原编号",503);return out;
}
export async function listSubtitleRequests(token:string,page:number):Promise<SubtitleRequest[]>{
  if(!integer(page,10000)||page<1)throw new ApiError("无效字幕回执页码",400);
  const out=await request<SubtitleRequest[]>(token,"/requests?page="+page);
  if(!Array.isArray(out)||out.length>30||out.some(p=>!validSubtitleRequest(p))||new Set(out.map(p=>p.id)).size!==out.length)throw new ApiError("原字幕回执列表未核实",503);return out;
}
export function canStopSubtitleRequest(p:SubtitleRequest):boolean{
  return p.state!=="cancelled"&&(!p.queued||
    (p.serviceState==="pending"||p.service==="OpenSubtitles"&&p.openSubtitles?.state==="stopped")&&
    (!p.openSubtitles||["pending","stopped"].includes(p.openSubtitles.state)));
}
export async function cancelSubtitleRequest(token:string,p:SubtitleRequest,confirm:boolean,reason:string):Promise<SubtitleRequest>{
  checked(p);
  if(!confirm||!text(reason,1000)||!reason.trim()||!canStopSubtitleRequest(p))throw new ApiError("只能明确停止尚未发送的原请求，并填写原因",400);
  const out=original(await request<SubtitleRequest>(token,"/requests/"+p.id+"/cancel",{confirm:true,reason}),p);
  if(out.state!=="cancelled")throw new ApiError("原停止结果未核实，请只读查回",503);return out;
}
