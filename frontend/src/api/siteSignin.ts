import { ApiError } from "./client";

export type SigninSettings={enabled:boolean;cron:string;sign_sites:string[];special_sites:string[];retry_keyword:string;queue_cnt:number;notify:boolean;onlyonce:boolean;clean:boolean;auto_cf:number;missed_detection:boolean;missed_schedule:string};
export type SigninOptions={ocr:boolean;douban:boolean;lens:boolean;ai:boolean;savedLogin:boolean;remote:boolean};
export type SigninTarget={id:string;name:string;scheme:string;host:string;path:string;adapter:string;browser:boolean};
export type SigninOutcome={state:string;reason:string;authenticated:boolean;retryable:boolean;cacheState?:string};
export type SigninJob={id:string;batch:string;siteId:string;name:string;host:string;actor:string;state:string;day:string;created:string;updated:string;outcome:SigninOutcome;requests:number;actions:number;canStop:boolean;runtimeActive:boolean;challenge?:{kind:string;nonce:string;deadline:string;question:string;image?:string;choices:{value:string;text:string}[];multiple:boolean}};
export type SigninControl={id:string;enabled:boolean;current:boolean;reason:string;nextRun:string;nextOnce?:string;nextMissed:string;location:string;remote:boolean};
export type SigninRemote={id:string;name:string;type:string;mode:string;receiver:string;permission:string;state?:string;reason?:string;updated?:string};
export type SigninStatus={installed:boolean;goOnly:boolean;browserConfigured:boolean;savedConfigured:boolean;fingerprint:string;maxConcurrency:number;settings:SigninSettings;control:SigninControl;sites:SigninTarget[];jobs:SigninJob[];dependencies:{batch:string;plugin:string;state:string;reason:string}[];signals:{id:string;state:string;reason:string;created:string;batch:string}[];remote:SigninRemote[]};
export type SigninReview={id:string;fingerprint:string;mode:"manual"|"activate";expires:string;location:string;settings:SigninSettings;targets:SigninTarget[];helpers:{kind:string;scheme:string;host:string;path:string;data:string}[];remote:SigninRemote[]};
export type SigninBatch={id:string;items:SigninJob[];summary:string;notified:boolean};
const object=(v:unknown):v is Record<string,unknown>=>!!v&&typeof v==="object"&&!Array.isArray(v);
const hex=(v:unknown):v is string=>typeof v==="string"&&/^[a-f0-9]{64}$/.test(v);
const id=(v:unknown):v is string=>typeof v==="string"&&/^[1-9][0-9]*$/.test(v);
const fields=(v:Record<string,unknown>,names:string[])=>names.every(k=>typeof v[k]==="string");
export const signinActive=(state:string)=>["queued","running","waiting_input"].includes(state);
export function signinSettingsValid(v:unknown):v is SigninSettings{return object(v)&&["enabled","notify","onlyonce","clean","missed_detection"].every(k=>typeof v[k]==="boolean")&&fields(v,["cron","retry_keyword","missed_schedule"])&&Array.isArray(v.sign_sites)&&v.sign_sites.every(id)&&Array.isArray(v.special_sites)&&v.special_sites.every(id)&&Number.isSafeInteger(v.queue_cnt)&&Number(v.queue_cnt)>0&&Number.isSafeInteger(v.auto_cf)&&Number(v.auto_cf)>=0;}
const targetValid=(v:unknown):v is SigninTarget=>object(v)&&id(v.id)&&fields(v,["name","scheme","host","path","adapter"])&&["http","https"].includes(String(v.scheme))&&typeof v.browser==="boolean";
export function signinJobValid(v:unknown):v is SigninJob{
 if(!object(v)||!hex(v.id)||!hex(v.batch)||!id(v.siteId)||!fields(v,["name","host","actor","state","day","created","updated"])||!["queued","running","waiting_input","finished"].includes(String(v.state))||!object(v.outcome)||!fields(v.outcome,["state","reason"])||typeof v.outcome.authenticated!=="boolean"||typeof v.outcome.retryable!=="boolean"||v.state==="finished"&&!["signed","already","authenticated","failed","deferred","unknown","blocked","stopped"].includes(String(v.outcome.state)))return false;
 if(!Number.isSafeInteger(v.requests)||Number(v.requests)<0||!Number.isSafeInteger(v.actions)||Number(v.actions)<0||typeof v.canStop!=="boolean"||typeof v.runtimeActive!=="boolean")return false;
 const c=v.challenge;if(c===undefined)return true;
 return v.state==="waiting_input"&&object(c)&&hex(c.nonce)&&fields(c,["kind","deadline","question"])&&["captcha","choice","two_step","email","email_code"].includes(String(c.kind))&&typeof c.multiple==="boolean"&&Array.isArray(c.choices)&&c.choices.length<=64&&c.choices.every(o=>object(o)&&fields(o,["value","text"]))&&(c.image===undefined||typeof c.image==="string"&&c.image.length<3_000_000&&/^data:image\/(png|jpeg|gif);base64,[A-Za-z0-9+/=]+$/.test(c.image));
}
const remoteValid=(v:unknown):v is SigninRemote=>object(v)&&id(v.id)&&fields(v,["name","type","mode","receiver","permission"]);
export function signinStatusValid(v:unknown):v is SigninStatus{
 return object(v)&&["installed","goOnly","browserConfigured","savedConfigured"].every(k=>typeof v[k]==="boolean")&&hex(v.fingerprint)&&Number.isSafeInteger(v.maxConcurrency)&&signinSettingsValid(v.settings)&&object(v.control)&&fields(v.control,["id","reason","nextRun","nextMissed","location"])&&["enabled","current","remote"].every(k=>typeof (v.control as Record<string,unknown>)[k]==="boolean")&&(v.control.id===""||hex(v.control.id))&&Array.isArray(v.sites)&&v.sites.every(targetValid)&&Array.isArray(v.jobs)&&v.jobs.every(signinJobValid)&&Array.isArray(v.dependencies)&&v.dependencies.every(d=>object(d)&&hex(d.batch)&&fields(d,["plugin","state","reason"]))&&Array.isArray(v.signals)&&v.signals.every(s=>object(s)&&hex(s.id)&&fields(s,["state","reason","created","batch"]))&&Array.isArray(v.remote)&&v.remote.every(remoteValid);
}
const reviewValid=(v:unknown):v is SigninReview=>object(v)&&hex(v.id)&&hex(v.fingerprint)&&["manual","activate"].includes(String(v.mode))&&fields(v,["expires","location"])&&signinSettingsValid(v.settings)&&Array.isArray(v.targets)&&v.targets.length>0&&v.targets.length<=1000&&v.targets.every(targetValid)&&Array.isArray(v.helpers)&&v.helpers.every(h=>object(h)&&fields(h,["kind","scheme","host","path","data"])&&["http","https"].includes(String(h.scheme)))&&Array.isArray(v.remote)&&v.remote.every(remoteValid);
export const signinBatchValid=(v:unknown):v is SigninBatch=>object(v)&&hex(v.id)&&Array.isArray(v.items)&&v.items.length<=1000&&v.items.every(j=>signinJobValid(j)&&j.batch===v.id)&&typeof v.summary==="string"&&typeof v.notified==="boolean";
async function data<T>(token:string,path:string,method:string,body:unknown,valid:(v:unknown)=>v is T):Promise<T>{
 const response=await fetch("/api/v1/site-signin"+path,{method,headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},...(body===undefined?{}:{body:JSON.stringify(body)})});
 let result:unknown;try{result=await response.json();}catch{throw new ApiError("签到应答未确认，请按原编号只读查询",response.status||502);}
 if(!object(result)||!response.ok||result.code!==0||!result.success)throw new ApiError(object(result)&&typeof result.message==="string"?result.message:"签到应答未确认",object(result)&&typeof result.code==="number"?result.code:response.status);
 if(!valid(result.data))throw new ApiError("签到应答格式无效，请按原编号只读查询",502);return result.data;
}
const empty=(v:unknown):v is Record<string,unknown>=>object(v);
export const getSigninStatus=(token:string,page=1)=>data(token,"?page="+page,"GET",undefined,signinStatusValid);
export const reviewSignin=(token:string,mode:"manual"|"activate",siteIds:string[],options:SigninOptions)=>data(token,"/review","POST",{mode,siteIds,options},reviewValid);
export async function getSigninBatch(token:string,id:string){if(!hex(id))throw new ApiError("批次编号无效",400);const result=await data(token,"/batches/"+id,"GET",undefined,signinBatchValid);if(result.id!==id)throw new ApiError("应答不是原批次",502);return result;}
export const startSignin=(token:string,r:SigninReview)=>data(token,"/start","POST",{id:r.id,fingerprint:r.fingerprint,confirm:true},signinBatchValid);
export const activateSignin=(token:string,r:SigninReview)=>data(token,"/activate","POST",{id:r.id,fingerprint:r.fingerprint,confirm:true},empty);
export const stopSigninControl=(token:string,id:string)=>data(token,"/stop","POST",{id,confirm:true},empty);
export const stopSigninJob=(token:string,id:string)=>data(token,"/jobs/"+id+"/stop","POST",{confirm:true},empty);
export const inputSignin=(token:string,id:string,nonce:string,values:string[])=>data(token,"/jobs/"+id+"/input","POST",{nonce,values,confirm:true},empty);
export function saveSigninSettings(token:string,fingerprint:string,settings:SigninSettings){return pluginAction(token,"PUT","/api/v1/plugins/AutoSignIn",{fingerprint,values:settings});}
export function installSignin(token:string){return pluginAction(token,"POST","/api/v1/plugins/AutoSignIn/install",{});}
async function pluginAction(token:string,method:string,path:string,body:unknown){const response=await fetch(path,{method,headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify(body)});let r:unknown;try{r=await response.json();}catch{throw new ApiError("设置应答未确认，请只读刷新",502)};if(!response.ok||!object(r)||r.code!==0||!r.success)throw new ApiError(object(r)&&typeof r.message==="string"?r.message:"签到设置未保存",object(r)&&typeof r.code==="number"?r.code:response.status);}
