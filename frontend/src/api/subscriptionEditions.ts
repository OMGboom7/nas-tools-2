import {ApiError} from "./client";
export type EditionProgress={previousOrder:number;order:number;topOrder:number;groupId:string;cycleId:string;target:number[];remaining:number[];cycleComplete:boolean};
export type EditionScope={type:"MOV"|"TV";id:string;name:string;scopeValid:boolean;upgrade:EditionProgress;revision:string};
export type EditionReceipt={id:string;type:"MOV"|"TV";subscriptionId:string;name:string;year:string;state:"active"|"completed"|"abandoned";sourceMatches:boolean;progress:EditionProgress;attempts:{title:string;order:number;episodes:number[];state:"intent"|"accepted"|"rejected"}[]};
const hash=(v:unknown):v is string=>typeof v==="string"&&/^[a-f0-9]{64}$/.test(v);
const id=(v:unknown):v is string=>typeof v==="string"&&/^[1-9][0-9]{0,18}$/.test(v)&&BigInt(v)<=9223372036854775807n;
const group=(v:unknown):v is string=>v==="0"||id(v);
const text=(v:unknown,max=4096):v is string=>typeof v==="string"&&v.length<=max&&!v.includes("\0");
const order=(v:unknown):v is number=>Number.isSafeInteger(v)&&Number(v)>=0&&Number(v)<=100;
const episodes=(v:unknown):v is number[]=>Array.isArray(v)&&v.length<=10000&&v.every((n,i)=>Number.isSafeInteger(n)&&n>=1&&n<=10000&&(i===0||n>v[i-1]));
export function validEditionProgress(v:EditionProgress):boolean{return !!v&&order(v.previousOrder)&&order(v.order)&&order(v.topOrder)&&group(v.groupId)&&text(v.cycleId,64)&&(v.cycleId===""||hash(v.cycleId))&&episodes(v.target)&&episodes(v.remaining)&&v.remaining.every(n=>v.target.includes(n))&&typeof v.cycleComplete==="boolean"&&(!v.cycleComplete||v.remaining.length===0)&&v.order<=v.topOrder;}
export function validEditionScope(v:EditionScope):boolean{return !!v&&(v.type==="MOV"||v.type==="TV")&&id(v.id)&&text(v.name)&&!!v.name&&typeof v.scopeValid==="boolean"&&hash(v.revision)&&validEditionProgress(v.upgrade);}
export function validEditionReceipt(v:EditionReceipt):boolean{return !!v&&hash(v.id)&&(v.type==="MOV"||v.type==="TV")&&id(v.subscriptionId)&&text(v.name)&&!!v.name&&text(v.year,4)&&["active","completed","abandoned"].includes(v.state)&&typeof v.sourceMatches==="boolean"&&validEditionProgress(v.progress)&&v.progress.cycleId===v.id&&v.progress.order>0&&v.progress.groupId!=="0"&&(v.state!=="completed"||v.progress.cycleComplete)&&Array.isArray(v.attempts)&&v.attempts.length<=1000&&v.attempts.every(a=>!!a&&text(a.title)&&!!a.title&&a.order===v.progress.order&&episodes(a.episodes)&&["intent","accepted","rejected"].includes(a.state));}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
  const r=await fetch(path,body===undefined?{headers:{Authorization:token}}:{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify(body)});
  let v:{code:number;success:boolean;message?:string;data:T};try{v=await r.json()}catch{throw new ApiError("洗版回执不完整，请只读查回，不要重发",r.status||503)}
  if(!r.ok||v?.code!==0||v.success!==true||v.data===undefined)throw new ApiError(v?.message||"洗版回执未核实",r.status===401?401:v?.code||r.status||503);return v.data;
}
function sourcePath(kind:"MOV"|"TV",value:string){if((kind!=="MOV"&&kind!=="TV")||!id(value))throw new ApiError("需要准确的订阅类型和完整编号",400);return "/api/v1/subscriptions/"+kind+"/"+value+"/upgrade";}
export async function getEditionScope(token:string,kind:"MOV"|"TV",value:string):Promise<EditionScope>{
  const v=await request<EditionScope>(token,sourcePath(kind,value));if(!validEditionScope(v)||v.type!==kind||v.id!==value)throw new ApiError("原洗版范围未核实",503);return v;
}
export async function getEditionReceipt(token:string,value:string):Promise<EditionReceipt>{
  if(!hash(value))throw new ApiError("需要完整洗版轮次编号",400);const v=await request<EditionReceipt>(token,"/api/v1/subscription-editions/"+value);if(!validEditionReceipt(v)||v.id!==value)throw new ApiError("原洗版轮次回执未核实",503);return v;
}
export async function getEditionReceipts(token:string):Promise<EditionReceipt[]>{
  const v=await request<EditionReceipt[]>(token,"/api/v1/subscription-editions");if(!Array.isArray(v)||v.length>30||!v.every(validEditionReceipt))throw new ApiError("洗版轮次清单未核实",503);return v;
}
export async function abandonEditionCycle(token:string,v:EditionScope,reason:string,confirm=false):Promise<void>{
  if(!validEditionScope(v)||!hash(v.upgrade.cycleId)||!confirm||!reason.trim()||reason.length>512)throw new ApiError("先核对完整轮次并明确封存原因；未知提交不能绕过",400);
  const out=await request<{cycleId:string;state:string;executionStarted:boolean}>(token,sourcePath(v.type,v.id)+"/reset",{revision:v.revision,reason,confirm:true});
  if(out?.cycleId!==v.upgrade.cycleId||out.state!=="abandoned"||out.executionStarted!==false)throw new ApiError("封存回执未核实，请保留原编号只读查回，不要重发",503);
}
