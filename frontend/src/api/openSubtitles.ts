import {ApiError} from "./client";
export type OpenSubtitleOutput={id:string;jobId:string;target:string;state:"writing"|"created"|"preserved"|"blocked"|"uncertain";reason:string};
export type OpenSubtitleReceipt={
 id:string;reference:string;name:string;state:"not_queued"|"pending"|"running"|"completed"|"not_found"|"partial"|"blocked"|"uncertain"|"stopped";
 provider:"org"|"com";origin:string;created:string;updated:string;reason:string;libraryId:string;requests:number;outputs:OpenSubtitleOutput[];
};
export type OpenSubtitleService={provider:"org"|"com";origin:string;remote:boolean};
export type OpenSubtitleStatus={wanted:boolean;ready:boolean;provider:""|"org"|"com";origin:string;remote:boolean};
const base="/api/v1/open-subtitles",encoder=new TextEncoder();
const text=(v:unknown,n=1000):v is string=>typeof v==="string"&&encoder.encode(v).length<=n&&!/[\x00-\x1f\x7f]/.test(v);
const hash=(v:unknown):v is string=>typeof v==="string"&&/^[a-f0-9]{64}$/.test(v);
const date=(v:unknown)=>typeof v==="string"&&v.length<100&&Number.isFinite(Date.parse(v));
export function validOpenSubtitleService(v:OpenSubtitleService):boolean{
 return !!v&&typeof v.remote==="boolean"&&(v.provider==="org"&&v.origin==="https://www.opensubtitles.org"||v.provider==="com"&&v.origin==="https://api.opensubtitles.com");
}
export function validOpenSubtitleReceipt(v:OpenSubtitleReceipt):boolean{
 if(!v||!hash(v.id)||typeof v.reference!=="string"||!/^(?:[a-f0-9]{32}|[a-f0-9]{64})$/.test(v.reference)||!text(v.name,600)||!v.name||
  !validOpenSubtitleService({...v,remote:false})||!["not_queued","pending","running","completed","not_found","partial","blocked","uncertain","stopped"].includes(v.state)||
  !date(v.created)||!(v.state==="not_queued"?v.updated==="":date(v.updated))||!text(v.reason)||!(v.libraryId===""||hash(v.libraryId))||
  !Number.isSafeInteger(v.requests)||v.requests<0||v.requests>256||!Array.isArray(v.outputs)||v.outputs.length>3)return false;
 if(v.state==="not_queued"&&(v.libraryId||v.requests||v.outputs.length)||v.state==="pending"&&(v.libraryId||v.requests||v.outputs.length))return false;
 if(["running","completed","not_found","partial","uncertain"].includes(v.state)&&!v.libraryId)return false;
 if(v.state==="stopped"&&(v.libraryId||v.requests||v.outputs.length)||
  v.state==="completed"&&(!v.requests||!v.outputs.length||v.outputs.some(o=>!o||!["created","preserved"].includes(o.state)))||
  v.state==="not_found"&&(!v.requests||v.outputs.length)||v.state==="partial"&&!v.outputs.length)return false;
 return v.outputs.every(o=>!!o&&hash(o.id)&&o.jobId===v.libraryId&&text(o.target,4096)&&!!o.target&&!o.target.startsWith("/")&&!o.target.includes("\\")&&
  o.target.split("/").every(p=>p&&p!=="."&&p!=="..")&&/\.(?:srt|ass|ssa)$/.test(o.target)&&["writing","created","preserved","blocked","uncertain"].includes(o.state)&&text(o.reason,2000))&&
  new Set(v.outputs.map(o=>o.id)).size===v.outputs.length&&new Set(v.outputs.map(o=>o.target)).size===v.outputs.length;
}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
 const response=await fetch(base+path,{method:body===undefined?"GET":"POST",headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},...(body===undefined?{}:{body:JSON.stringify(body)})});
 const value=await response.json().catch(()=>null) as {code?:number;success?:boolean;data?:T;message?:string}|null;
 if(!response.ok||value?.code!==0||value.success!==true||value.data===undefined)throw new ApiError(value?.message||"原字幕操作未获明确应答；只读查回，不自动重试",response.status===401?401:value?.code||response.status);
 return value.data;
}
export async function getOpenSubtitleStatus(token:string):Promise<OpenSubtitleStatus>{
 const s=await request<OpenSubtitleStatus>(token,"");
 if(!s||typeof s.wanted!=="boolean"||typeof s.ready!=="boolean"||typeof s.remote!=="boolean"||
  (s.ready?!s.wanted||!validOpenSubtitleService(s as OpenSubtitleService):s.provider!==""||s.origin!==""||s.remote))throw new ApiError("字幕授权状态未核实",503);
 return s;
}
function checked(p:OpenSubtitleReceipt):OpenSubtitleReceipt{if(!validOpenSubtitleReceipt(p)||p.state==="not_queued")throw new ApiError("原 OpenSubtitles 回执未核实",503);return p}
export async function getOpenSubtitle(token:string,id:string):Promise<OpenSubtitleReceipt>{
 if(!hash(id))throw new ApiError("请输入完整原 OpenSubtitles 编号",400);
 const out=checked(await request<OpenSubtitleReceipt>(token,"/requests/"+id));if(out.id!==id)throw new ApiError("返回了另一份字幕回执",503);return out;
}
export async function listOpenSubtitles(token:string,page:number):Promise<OpenSubtitleReceipt[]>{
 if(!Number.isInteger(page)||page<1||page>10000)throw new ApiError("无效字幕回执页码",400);
 const rows=await request<OpenSubtitleReceipt[]>(token,"/requests?page="+page);
 if(!Array.isArray(rows)||rows.length>30||rows.some(p=>!validOpenSubtitleReceipt(p)||p.state==="not_queued")||new Set(rows.map(p=>p.id)).size!==rows.length)throw new ApiError("原字幕回执列表未核实",503);
 return rows;
}
export async function stopOpenSubtitle(token:string,p:OpenSubtitleReceipt,confirm:boolean,reason:string):Promise<OpenSubtitleReceipt>{
 checked(p);if(p.state!=="pending"||p.requests!==0||p.libraryId||!confirm||!text(reason,600)||!reason.trim())throw new ApiError("只可明确停止尚未开始的原请求，并填写原因",400);
 const out=checked(await request<OpenSubtitleReceipt>(token,"/requests/"+p.id+"/stop",{confirm:true,reason}));
 for(const key of ["id","reference","name","provider","origin","created"] as const)if(out[key]!==p[key])throw new ApiError("停止结果不属于同一原请求，请只读查回",503);
 if(out.state!=="stopped"||out.requests!==0||out.libraryId)throw new ApiError("原停止结果未核实，请只读查回",503);return out;
}
