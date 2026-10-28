import {ApiError} from "./client";
export type EventWebhookStatus={active:boolean;epoch:string;actor:string;created:string;origin:string;method:string;encoding:string;current:boolean;workerRunning:boolean};
export type EventWebhookReceipt={id:string;eventId:string;type:string;reference:string;state:string;reason:string;actor:string;created:string;updated:string;status:number};
const hex=(v:unknown,n:number):v is string=>typeof v==="string"&&new RegExp("^[a-f0-9]{"+n+"}$").test(v);
const text=(v:unknown,n=4096):v is string=>typeof v==="string"&&v.length<=n&&!/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(v);
const date=(v:unknown)=>typeof v==="string"&&v.length<100&&Number.isFinite(Date.parse(v));
const types=new Set(["emby.webhook","jellyfin.webhook","plex.webhook","download.add","download.fail","transfer.finished","transfer.fail","subtitle.download","subscribe.add","subscribe.finished","message.incoming","search.start","sourcefile.deleted","libraryfile.deleted","media.scrap.start","plugin.reload","douban.sync","autoseed.start","refresh.mediaserver","site.signin"]);
export function validEventWebhookReceipt(v:EventWebhookReceipt):boolean {
 return !!v&&hex(v.id,64)&&hex(v.eventId,64)&&types.has(v.type)&&text(v.reference)&&["pending","intent","sent","uncertain","blocked","dismissed"].includes(v.state)&&text(v.reason,1000)&&text(v.actor,256)&&date(v.created)&&date(v.updated)&&Number.isInteger(v.status)&&v.status>=0&&v.status<=599&&(v.state!=="sent"||v.status>=200&&v.status<=299)&&(v.state!=="dismissed"||!!v.actor&&!!v.reason);
}
export function validEventWebhookStatus(v:EventWebhookStatus):boolean {
 if(!v||typeof v.active!=="boolean"||typeof v.current!=="boolean"||typeof v.workerRunning!=="boolean"||v.active&&!v.current||v.epoch!==""&&!hex(v.epoch,32)||!text(v.actor,256)||v.created!==""&&!date(v.created)||!["","post","get"].includes(v.method)||!["","legacy","json"].includes(v.encoding))return false;
 if(!v.origin)return !v.active;
 try{const u=new URL(v.origin);return ["http:","https:"].includes(u.protocol)&&!u.username&&!u.password&&!u.search&&!u.hash&&u.pathname==="/"&&(!v.active||!!v.actor&&hex(v.epoch,32)&&date(v.created)&&!!v.method)}catch{return false}
}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
 let r:Response;
 try{r=await fetch(path,{method:body===undefined?"GET":"POST",headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},body:body===undefined?undefined:JSON.stringify(body)})}catch{throw new ApiError("外发回执未确认，请只读查回，不要重复操作",503)}
 const out=await r.json().catch(()=>{throw new ApiError("外发回执格式未核实，请只读查回",503)}) as {code?:number;success?:boolean;message?:string;data:T};
 if(!r.ok||out.success!==true||out.code!==0)throw new ApiError(out.message||"外发操作未确认，请只读查回",r.status===401?401:out.code||r.status||503);
 return out.data;
}
const base="/api/v1/event-webhooks";
export async function reviewEventWebhooks(token:string):Promise<EventWebhookStatus>{
 const v=await request<EventWebhookStatus>(token,base);if(!validEventWebhookStatus(v))throw new ApiError("原事件外发设置未核实",503);return v;
}
export async function listEventWebhooks(token:string,page=1):Promise<EventWebhookReceipt[]>{
 if(!Number.isSafeInteger(page)||page<1||page>10000)throw new ApiError("无效外发回执页码",400);
 const v=await request<EventWebhookReceipt[]>(token,base+"/receipts?page="+page);if(!Array.isArray(v)||v.length>30||!v.every(validEventWebhookReceipt))throw new ApiError("完整外发回执页未核实",503);return v;
}
export async function getEventWebhook(token:string,id:string):Promise<EventWebhookReceipt>{
 if(!hex(id,64))throw new ApiError("需要完整原外发稳定编号",400);
 const v=await request<EventWebhookReceipt>(token,base+"/receipts/"+id);if(!validEventWebhookReceipt(v)||v.id!==id)throw new ApiError("原外发回执未核实",503);return v;
}
export async function dismissEventWebhook(token:string,p:EventWebhookReceipt,confirmed:boolean,reason:string):Promise<EventWebhookReceipt>{
 if(!validEventWebhookReceipt(p)||!confirmed||!reason.trim()||reason.length>1000||["sent","dismissed"].includes(p.state))throw new ApiError("先查回原回执并确认忽略原因",400);
 const v=await request<EventWebhookReceipt>(token,base+"/receipts/"+p.id+"/dismiss",{confirm:true,reason});
 if(!validEventWebhookReceipt(v)||v.id!==p.id||v.eventId!==p.eventId||v.type!==p.type||v.reference!==p.reference||v.state!=="dismissed"||v.reason!==reason)throw new ApiError("忽略回执未核实，请只读查回，不要重新提交",503);return v;
}
