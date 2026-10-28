import {ApiError} from "./client";
export type MediaWebhookStatus={server:string;endpoint:string;revision:string;enabled:boolean;current:boolean;ready:boolean;actor:string;changed:string;apiKeyRequired:boolean;ipv4:string;ipv6:string;fingerprint:string;reason:string};
export type MediaWebhookEvent={version:1;server:"emby"|"jellyfin"|"plex";action:string;mediaType:string;itemId:string;name:string;user:string;client:string;device:string;address:string;overview:string;percentage?:number;image:string;link:string};
export type MediaWebhookDelivery={id:string;eventId:string;channelId:string;channelName:string;channelType:string;state:string;reason?:string;actor?:string;updated:string};
export type MediaWebhookReceipt={id:string;received:string;event:MediaWebhookEvent;eventId:string;deduplicated:boolean;deliveries:MediaWebhookDelivery[]};
const hex=(s:unknown,n:number):s is string=>typeof s==="string"&&new RegExp("^[a-f0-9]{"+n+"}$").test(s);
const text=(s:unknown,n=4096):s is string=>typeof s==="string"&&s.length<=n&&!/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(s);
const date=(s:unknown)=>typeof s==="string"&&s.length<100&&Number.isFinite(Date.parse(s));
export function validMediaWebhookStatus(v:MediaWebhookStatus):boolean{
 return !!v&&["","emby","jellyfin","plex"].includes(v.server)&&v.endpoint===(v.server?"/"+v.server:"")&&(v.revision===""||hex(v.revision,32))&&typeof v.enabled==="boolean"&&typeof v.current==="boolean"&&(!v.current||v.enabled)&&typeof v.ready==="boolean"&&text(v.actor,256)&&(v.changed===""||date(v.changed))&&typeof v.apiKeyRequired==="boolean"&&text(v.ipv4,16000)&&text(v.ipv6,16000)&&hex(v.fingerprint,64)&&text(v.reason)&&(!v.enabled||hex(v.revision,32))&&(!v.ready||!!v.server);
}
export function validMediaWebhookReceipt(v:MediaWebhookReceipt):boolean{
 if(!v||!hex(v.id,32)||!hex(v.eventId,64)||!date(v.received)||typeof v.deduplicated!=="boolean"||!v.event||v.event.version!==1)return false;
 const e=v.event;
 if(!["emby","jellyfin","plex"].includes(e.server)||!["library.new","system.webhooktest","playback.start","playback.stop","user.authenticated","user.authenticationfailed","item.rate"].includes(e.action)||!["","TV","SHOW","MOV","AUD"].includes(e.mediaType))return false;
 for(const [s,n]of [[e.itemId,256],[e.name,4096],[e.user,256],[e.client,256],[e.device,256],[e.address,512],[e.overview,1000]] as const)if(!text(s,n))return false;
 if(e.percentage!==undefined&&(!Number.isFinite(e.percentage)||e.percentage<0||e.percentage>100))return false;
 if(!text(e.image)||!text(e.link))return false;
 if(e.link){try{const u=new URL(e.link);if(!["http:","https:"].includes(u.protocol)||u.username||u.password||u.search)return false}catch{return false}}
 return Array.isArray(v.deliveries)&&v.deliveries.length<=1000&&v.deliveries.every(d=>!!d&&hex(d.id,64)&&d.eventId===v.eventId&&typeof d.channelId==="string"&&/^[1-9]\d*$/.test(d.channelId)&&d.channelId.length<=19&&BigInt(d.channelId)<=9223372036854775807n&&text(d.channelName)&&text(d.channelType)&&["pending","sending","sent","uncertain","blocked","dismissed"].includes(d.state)&&text(d.reason||"")&&text(d.actor||"")&&date(d.updated));
}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
 let r:Response;
 try{r=await fetch(path,{method:body===undefined?"GET":"POST",headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},body:body===undefined?undefined:JSON.stringify(body)})}catch{throw new ApiError("回调设置或回执未核实，请只读查回，不要重发",503)}
 const out=await r.json().catch(()=>{throw new ApiError("回执格式未核实，请只读查回",503)}) as {code?:number;success?:boolean;message?:string;data:T};
 if(!r.ok||out.success!==true||out.code!==0)throw new ApiError(out.message||"回调操作未确认，请只读查回",r.status===401?401:out.code||r.status||503);
 return out.data;
}
const base="/api/v1/media-webhooks";
export async function reviewMediaWebhooks(token:string):Promise<MediaWebhookStatus>{
 const out=await request<MediaWebhookStatus>(token,base);
 if(!validMediaWebhookStatus(out))throw new ApiError("原媒体回调设置未核实",503);return out;
}
export async function changeMediaWebhooks(token:string,review:MediaWebhookStatus,enabled:boolean,consent:{receiver?:boolean;notifications?:boolean;legacyStopped?:boolean}={}):Promise<MediaWebhookStatus>{
 if(!validMediaWebhookStatus(review)||!consent.receiver||enabled&&(!review.ready||!consent.notifications||!consent.legacyStopped))throw new ApiError("先审核原设置并分别确认接收与通知范围",400);
 const out=await request<MediaWebhookStatus>(token,base+(enabled?"/authorize":"/stop"),{fingerprint:review.fingerprint,confirm:true,confirmNotifications:consent.notifications===true,legacyStopped:consent.legacyStopped===true});
 if(!validMediaWebhookStatus(out)||out.enabled!==enabled||out.revision===review.revision||enabled&&(!out.current||out.server!==review.server))throw new ApiError("接收器回执未核实，请只读查回，不要重发",503);return out;
}
export async function getMediaWebhookReceipt(token:string,id:string):Promise<MediaWebhookReceipt>{
 if(!hex(id,32))throw new ApiError("需要完整原回调稳定编号",400);
 const out=await request<MediaWebhookReceipt>(token,base+"/receipts/"+id);
 if(!validMediaWebhookReceipt(out)||out.id!==id)throw new ApiError("原回调完整回执未核实",503);return out;
}
export async function getMediaWebhookReceipts(token:string,page=1):Promise<MediaWebhookReceipt[]>{
 if(!Number.isSafeInteger(page)||page<1||page>10000)throw new ApiError("无效回调页码",400);
 const out=await request<MediaWebhookReceipt[]>(token,base+"/receipts?page="+page);
 if(!Array.isArray(out)||out.length>30||!out.every(validMediaWebhookReceipt))throw new ApiError("完整回调清单未核实",503);return out;
}
