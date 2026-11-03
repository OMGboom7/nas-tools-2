import { ApiError } from "./client";

export type StatsControl={enabled:boolean;epoch:string;actor:string;fingerprint:string;cron:string;location:string;readMessages:boolean;remote:boolean;nextRun:string;created:string};
export type StatsItem={site:string;url:string;username:string;userLevel:string;joinAt:string;lastSeen:string;updateAt:string;upload:string;download:string;ratio:number;bonus:number;seeding:number;leeching:number;seedingSize:string;messageUnread:number};
export type StatsJob={id:string;origin:string;actor:string;state:string;created:string;updated:string;requests:number;messageRequests:number;readMessages:boolean;results:{siteId:string;name:string;state:string;reason:string}[]};
export type StatsStatus={fingerprint:string;goOnly:boolean;cron:string;current:boolean;control:StatsControl;targets:{id:string;name:string;host:string;messageEnabled:boolean}[];items:StatsItem[];jobs:StatsJob[]};
export type StatsHistory={date:string;upload:string;download:string;bonus:number;seeding:number;seedingSize:string};
export type StatsDetail={siteId:string;history:StatsHistory[];seedingInfo:[string,string][]};
const object=(v:unknown):v is Record<string,unknown>=>!!v&&typeof v==="object"&&!Array.isArray(v);
const strings=(v:Record<string,unknown>,keys:string[])=>keys.every(k=>typeof v[k]==="string");
const numbers=(v:Record<string,unknown>,keys:string[])=>keys.every(k=>typeof v[k]==="number"&&Number.isFinite(v[k])&&Number(v[k])>=0);
const decimal=(v:unknown):v is string=>typeof v==="string"&&/^(0|[1-9]\d{0,18})$/.test(v)&&BigInt(v)<=9223372036854775807n;
const bytes=(v:Record<string,unknown>)=>["upload","download","seedingSize"].every(k=>decimal(v[k]));
const hex=(v:unknown):v is string=>typeof v==="string"&&/^[a-f0-9]{64}$/.test(v);
const control=(v:unknown):v is StatsControl=>object(v)&&["enabled","readMessages","remote"].every(k=>typeof v[k]==="boolean")&&strings(v,["epoch","actor","fingerprint","cron","location","nextRun","created"]);
export const statsJobValid=(v:unknown):v is StatsJob=>object(v)&&hex(v.id)&&strings(v,["origin","actor","state","created","updated"])&&["queued","running","complete","partial","failed","stopped","unknown"].includes(String(v.state))&&numbers(v,["requests","messageRequests"])&&typeof v.readMessages==="boolean"&&Array.isArray(v.results)&&v.results.every(r=>object(r)&&strings(r,["siteId","name","state","reason"]));
export const statsStatusValid=(v:unknown):v is StatsStatus=>object(v)&&hex(v.fingerprint)&&typeof v.goOnly==="boolean"&&typeof v.current==="boolean"&&typeof v.cron==="string"&&control(v.control)&&Array.isArray(v.targets)&&v.targets.every(t=>object(t)&&strings(t,["id","name","host"])&&typeof t.messageEnabled==="boolean")&&Array.isArray(v.items)&&v.items.every(i=>object(i)&&strings(i,["site","url","username","userLevel","joinAt","lastSeen","updateAt"])&&bytes(i)&&numbers(i,["ratio","bonus","seeding","leeching","messageUnread"]))&&Array.isArray(v.jobs)&&v.jobs.every(statsJobValid);
export const statsDetailValid=(v:unknown):v is StatsDetail=>object(v)&&typeof v.siteId==="string"&&Array.isArray(v.history)&&v.history.every(h=>object(h)&&typeof h.date==="string"&&bytes(h)&&numbers(h,["bonus","seeding"]))&&Array.isArray(v.seedingInfo)&&v.seedingInfo.every(s=>Array.isArray(s)&&s.length===2&&s.every(decimal));
async function call<T>(token:string,path:string,body:unknown,valid:(v:unknown)=>v is T):Promise<T>{
 const response=await fetch("/api/v1/site-statistics"+path,{method:body===undefined?"GET":"POST",headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},...(body===undefined?{}:{body:JSON.stringify(body)})});
 let value:unknown;try{value=await response.json();}catch{throw new ApiError("统计应答未确认，请查询原回执",502);}
 if(!response.ok||!object(value)||value.code!==0||value.success!==true)throw new ApiError(object(value)&&typeof value.message==="string"?value.message:"统计应答未确认",response.status);
 if(!valid(value.data))throw new ApiError("统计应答格式无效，请查询原回执",502);return value.data;
}
export const getStatistics=(token:string)=>call(token,"",undefined,statsStatusValid);
export function getStatsDetail(token:string,id:string){if(!/^[1-9]\d*$/.test(id))throw new ApiError("站点编号无效",400);return call(token,"/detail/"+id,undefined,(v):v is StatsDetail=>statsDetailValid(v)&&v.siteId===id);}
export const refreshStatistics=(token:string,fingerprint:string,requestId:string,siteIds:string[],readMessages:boolean)=>call(token,"/refresh",{fingerprint,requestId,siteIds,readMessages,confirm:true},statsJobValid);
export const activateStatistics=(token:string,fingerprint:string,expectedEpoch:string,readMessages:boolean,remote:boolean)=>call(token,"/activate",{fingerprint,expectedEpoch,readMessages,remote,confirm:true},control);
export const stopStatistics=(token:string,expectedEpoch:string)=>call(token,"/stop",{expectedEpoch,confirm:true},control);
