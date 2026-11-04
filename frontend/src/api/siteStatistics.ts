import { ApiError } from "./client";

export type StatsControl={enabled:boolean;epoch:string;actor:string;fingerprint:string;cron:string;location:string;readMessages:boolean;remote:boolean;nextRun:string;created:string};
export type StatsItem={site:string;url:string;username:string;userLevel:string;joinAt:string;lastSeen:string;updateAt:string;upload:string;download:string;ratio:number;bonus:number;seeding:number;leeching:number;seedingSize:string;messageUnread:number};
export type StatsJob={id:string;origin:string;actor:string;state:string;created:string;updated:string;requests:number;messageRequests:number;readMessages:boolean;results:{siteId:string;name:string;state:string;reason:string}[]};
export type StatsOverview={targets:{id:string;name:string;host:string;url?:string;messageEnabled:boolean}[];items:StatsItem[]};
export type StatsStatus=StatsOverview&{fingerprint:string;goOnly:boolean;cron:string;current:boolean;control:StatsControl;jobs:StatsJob[]};
export type StatsHistory={date:string;upload:string;download:string;bonus:number;seeding:number;seedingSize:string};
export type StatsDetail={siteId:string;history:StatsHistory[];seedingInfo:[string,string][]};
export const trafficRanges=[1,7,30,365] as const;
export type TrafficRange=(typeof trafficRanges)[number];
export type StatsTraffic={days:TrafficRange;endDay:string;today:string;items:{
  siteId:string;name:string;host:string;upload:string;download:string;
}[]};
export type SiteRanking={
  configured:boolean;site:string;current_level:string;next_level:string|null;overall_progress:number;
  requirements:{key:string;label:string;current:number;required:number;progress:number;met:boolean;need:number;unit:string}[];
  level_steps:{name:string;state:"locked"|"done"|"current"|"target";order:number;is_keep:boolean}[];
  keep_account_level:string;current_is_keep:boolean;rule_url:string;is_special_level:boolean;special_level_description:string;
  status_type:"not_configured"|"mapping_error"|"special_level"|"highest_level"|"normal";message:string;
};
export type StatsRanking={siteId:string;ranking:SiteRanking};
const object=(v:unknown):v is Record<string,unknown>=>!!v&&typeof v==="object"&&!Array.isArray(v);
const strings=(v:Record<string,unknown>,keys:string[])=>keys.every(k=>typeof v[k]==="string");
const numbers=(v:Record<string,unknown>,keys:string[])=>keys.every(k=>typeof v[k]==="number"&&Number.isFinite(v[k])&&Number(v[k])>=0);
const decimal=(v:unknown):v is string=>typeof v==="string"&&/^(0|[1-9]\d{0,18})$/.test(v)&&BigInt(v)<=9223372036854775807n;
const counts=(v:Record<string,unknown>,keys:string[])=>numbers(v,keys)&&keys.every(k=>Number.isSafeInteger(v[k]));
const bytes=(v:Record<string,unknown>)=>["upload","download","seedingSize"].every(k=>decimal(v[k]));
const hex=(v:unknown):v is string=>typeof v==="string"&&/^[a-f0-9]{64}$/.test(v);
const control=(v:unknown):v is StatsControl=>object(v)&&["enabled","readMessages","remote"].every(k=>typeof v[k]==="boolean")&&strings(v,["epoch","actor","fingerprint","cron","location","nextRun","created"]);
export const statsJobValid=(v:unknown):v is StatsJob=>object(v)&&hex(v.id)&&strings(v,["origin","actor","state","created","updated"])&&["queued","running","complete","partial","failed","stopped","unknown"].includes(String(v.state))&&numbers(v,["requests","messageRequests"])&&typeof v.readMessages==="boolean"&&Array.isArray(v.results)&&v.results.every(r=>object(r)&&strings(r,["siteId","name","state","reason"]));
export const statsOverviewValid=(v:unknown):v is StatsOverview=>object(v)&&Array.isArray(v.targets)&&v.targets.every(t=>object(t)&&strings(t,["id","name","host"])&&(t.url===undefined||typeof t.url==="string")&&typeof t.messageEnabled==="boolean")&&Array.isArray(v.items)&&v.items.every(i=>object(i)&&strings(i,["site","url","username","userLevel","joinAt","lastSeen","updateAt"])&&bytes(i)&&numbers(i,["ratio","bonus"])&&counts(i,["seeding","leeching","messageUnread"]));
export const statsStatusValid=(v:unknown):v is StatsStatus=>object(v)&&hex(v.fingerprint)&&typeof v.goOnly==="boolean"&&typeof v.current==="boolean"&&typeof v.cron==="string"&&control(v.control)&&Array.isArray(v.jobs)&&v.jobs.every(statsJobValid)&&statsOverviewValid(v);
export function statsDayValid(v:unknown):v is string{
  if(typeof v!=="string"||!/^\d{4}-\d{2}-\d{2}$/.test(v)||v.startsWith("0000"))return false;
  const date=new Date(v+"T00:00:00Z");
  return Number.isFinite(date.valueOf())&&date.toISOString().slice(0,10)===v;
}
export const statsDetailValid=(v:unknown):v is StatsDetail=>object(v)&&typeof v.siteId==="string"&&Array.isArray(v.history)&&v.history.every(h=>object(h)&&typeof h.date==="string"&&bytes(h)&&numbers(h,["bonus","seeding"]))&&Array.isArray(v.seedingInfo)&&v.seedingInfo.every(s=>Array.isArray(s)&&s.length===2&&s.every(decimal));
export function statsTrafficValid(v:unknown):v is StatsTraffic{
  return object(v)&&trafficRanges.some(days=>days===v.days)&&statsDayValid(v.today)&&statsDayValid(v.endDay)&&v.endDay<=v.today&&
    Array.isArray(v.items)&&v.items.every(row=>object(row)&&strings(row,["name","host"])&&decimal(row.siteId)&&row.siteId!=="0"&&
      decimal(row.upload)&&decimal(row.download))&&new Set(v.items.map(row=>row.siteId)).size===v.items.length;
}
const finite=(v:unknown):v is number=>typeof v==="number"&&Number.isFinite(v);
const percent=(v:unknown)=>finite(v)&&v>=0&&v<=100;
function rankRuleLink(v:unknown){
  if(v==="")return true;
  if(typeof v!=="string")return false;
  try{
    const link=new URL(v);
    return ["http:","https:"].includes(link.protocol)&&!!link.hostname&&!link.username&&!link.password;
  }catch{return false;}
}
export function statsRankingValid(v:unknown):v is StatsRanking{
  if(!object(v)||typeof v.siteId!=="string"||!object(v.ranking))return false;
  const r=v.ranking;
  return strings(r,["site","current_level","keep_account_level","special_level_description","message"])&&
    ["configured","current_is_keep","is_special_level"].every(k=>typeof r[k]==="boolean")&&
    (r.next_level===null||typeof r.next_level==="string")&&rankRuleLink(r.rule_url)&&percent(r.overall_progress)&&
    ["not_configured","mapping_error","special_level","highest_level","normal"].includes(String(r.status_type))&&
    Array.isArray(r.requirements)&&r.requirements.every(c=>object(c)&&strings(c,["key","label","unit"])&&
      finite(c.current)&&finite(c.required)&&finite(c.need)&&c.need>=0&&percent(c.progress)&&typeof c.met==="boolean")&&
    Array.isArray(r.level_steps)&&r.level_steps.every(l=>object(l)&&typeof l.name==="string"&&
      ["locked","done","current","target"].includes(String(l.state))&&Number.isSafeInteger(l.order)&&typeof l.is_keep==="boolean");
}
async function call<T>(token:string,path:string,body:unknown,valid:(v:unknown)=>v is T,signal?:AbortSignal):Promise<T>{
 const response=await fetch("/api/v1/site-statistics"+path,{method:body===undefined?"GET":"POST",headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},...(body===undefined?{}:{body:JSON.stringify(body)}),...(signal?{signal}:{})});
 let value:unknown;try{value=await response.json();}catch{throw new ApiError("统计应答未确认，请查询原回执",502);}
 if(!response.ok||!object(value)||value.code!==0||value.success!==true)throw new ApiError(object(value)&&typeof value.message==="string"?value.message:"统计应答未确认",response.status);
 if(!valid(value.data))throw new ApiError("统计应答格式无效，请查询原回执",502);return value.data;
}
export const getStatistics=(token:string)=>call(token,"",undefined,statsStatusValid);
export const getStatsOverview=(token:string)=>call(token,"/overview",undefined,statsOverviewValid);
export function getStatsTraffic(token:string,days:TrafficRange,endDay:string,signal?:AbortSignal){
  if(!trafficRanges.includes(days)||endDay&&!statsDayValid(endDay))throw new ApiError("统计日期或范围无效",400);
  const query=new URLSearchParams({days:String(days)});
  if(endDay)query.set("endDay",endDay);
  return call(token,"/traffic?"+query,undefined,(v):v is StatsTraffic=>statsTrafficValid(v)&&v.days===days&&
    v.endDay===(endDay||v.today),signal);
}
export function getStatsDetail(token:string,id:string){if(!/^[1-9]\d*$/.test(id))throw new ApiError("站点编号无效",400);return call(token,"/detail/"+id,undefined,(v):v is StatsDetail=>statsDetailValid(v)&&v.siteId===id);}
export function getStatsRanking(token:string,id:string,signal?:AbortSignal){
  if(!/^[1-9]\d{0,18}$/.test(id)||!decimal(id))throw new ApiError("站点编号无效",400);
  return call(token,"/ranking/"+id,undefined,(v):v is StatsRanking=>statsRankingValid(v)&&v.siteId===id,signal);
}
export const refreshStatistics=(token:string,fingerprint:string,requestId:string,siteIds:string[],readMessages:boolean)=>call(token,"/refresh",{fingerprint,requestId,siteIds,readMessages,confirm:true},statsJobValid);
export const activateStatistics=(token:string,fingerprint:string,expectedEpoch:string,readMessages:boolean,remote:boolean)=>call(token,"/activate",{fingerprint,expectedEpoch,readMessages,remote,confirm:true},control);
export const stopStatistics=(token:string,expectedEpoch:string)=>call(token,"/stop",{expectedEpoch,confirm:true},control);
