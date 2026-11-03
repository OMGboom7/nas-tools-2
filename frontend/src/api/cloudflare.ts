import { ApiError } from "./client";

export type CFJob={id:string;origin:string;state:string;reason:string;created:string;updated:string;canReconcile:boolean;canClose:boolean;bestIP?:string;version?:string;beforeIP?:string;afterIP?:string};
export type CFValues={cf_ip:string;cron:string;version:string;additional_args:string;ipv4:boolean;ipv6:boolean;check:boolean;onlyonce:boolean;re_install:boolean;notify:boolean};
export type CFStatus={fingerprint:string;ready:boolean;enabled:boolean;current:boolean;fromSignin:boolean;nextRun:string;nextOnce:string;values:CFValues;jobs:CFJob[];requestID?:string};
const object=(v:unknown):v is Record<string,unknown>=>!!v&&typeof v==="object"&&!Array.isArray(v);
const hex=(v:unknown):v is string=>typeof v==="string"&&/^[a-f0-9]{64}$/.test(v);
const strings=(v:Record<string,unknown>,keys:string[])=>keys.every(k=>typeof v[k]==="string");
export function cfJobValid(v:unknown):v is CFJob{return object(v)&&hex(v.id)&&strings(v,["origin","state","reason","created","updated"])&&["queued","running","applying","complete","failed","blocked","unknown","closed"].includes(String(v.state))&&typeof v.canReconcile==="boolean"&&typeof v.canClose==="boolean"&&["bestIP","version","beforeIP","afterIP"].every(k=>v[k]===undefined||typeof v[k]==="string");}
export function cfStatusValid(v:unknown):v is CFStatus{return object(v)&&hex(v.fingerprint)&&["ready","enabled","current","fromSignin"].every(k=>typeof v[k]==="boolean")&&strings(v,["nextRun","nextOnce"])&&object(v.values)&&strings(v.values,["cf_ip","cron","version","additional_args"])&&["ipv4","ipv6","check","onlyonce","re_install","notify"].every(k=>typeof (v.values as Record<string,unknown>)[k]==="boolean")&&Array.isArray(v.jobs)&&v.jobs.length<=50&&v.jobs.every(cfJobValid)&&(v.requestID===undefined||hex(v.requestID));}
async function call<T>(token:string,path:string,body:unknown,valid:(v:unknown)=>v is T):Promise<T>{
 const response=await fetch("/api/v1/cloudflare"+path,{method:body===undefined?"GET":"POST",headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},...(body===undefined?{}:{body:JSON.stringify(body)})});
 let data:unknown;try{data=await response.json();}catch{throw new ApiError("优选应答未确认，请查询原回执",502);}
 if(!response.ok||!object(data)||data.code!==0||data.success!==true)throw new ApiError(object(data)&&typeof data.message==="string"?data.message:"优选应答未确认",response.status);
 if(!valid(data.data))throw new ApiError("优选应答格式无效，请查询原回执",502);return data.data;
}
const record=(v:unknown):v is Record<string,unknown>=>object(v);
export const getCF=(token:string)=>call(token,"",undefined,cfStatusValid);
export const reviewCF=(token:string,nonce:string)=>call(token,"/review",{nonce},cfStatusValid);
export const activateCF=(token:string,fingerprint:string,fromSignin:boolean)=>call(token,"/activate",{fingerprint,fromSignin,confirm:true},record);
export const runCF=(token:string,fingerprint:string,nonce:string)=>call(token,"/run",{fingerprint,nonce,confirm:true},cfJobValid);
export function getCFJob(token:string,id:string){if(!hex(id))throw new ApiError("原优选编号无效",400);return call(token,"/jobs/"+id,undefined,(v):v is CFJob=>cfJobValid(v)&&v.id===id);}
export const stopCF=(token:string)=>call(token,"/stop",{confirm:true},record);
export const reconcileCF=(token:string,id:string)=>call(token,"/reconcile",{id},cfJobValid);
export const closeCF=(token:string,id:string)=>call(token,"/close",{id,confirm:true},cfJobValid);
