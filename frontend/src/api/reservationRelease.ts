import { ApiError } from "./client";

export type ReservationRef = { kind:"organization"|"library"|"recovery"|"media-removal"; id:string };
export type ReservationClaim = { kind:"target"|"disc-target"|"remote-target"|"library-output"|"intake-output"|"unknown-association"|"media-range"|"history-association"|"disposal-authority"|"task-authority"; key:string; path:string };
export type ReservationOwner = ReservationRef & { state:string; batchId?:string; related:boolean; pathCount:number; paths:string[]; claims:ReservationClaim[] };
export type ReservationRelease = { id:string; state:"preview"|"released"; seed:ReservationRef; actor:string; requestId:string; decision:string; fingerprint:string; created:string; owners:ReservationOwner[]; claimCount:number; rowCount:number };
export type ReservationStatus = { owners:ReservationOwner[]; next:string; releases:ReservationRelease[] };
export type ReservationConsent = { confirm:boolean; confirmReleaseReservations:boolean; confirmCloseRelated:boolean; confirmNoContentProof:boolean; confirmFutureOperations:boolean; confirmOldExecutorsStopped:boolean };
const base="/api/v1/organization/reservation-releases";
const hash=(v:unknown,n=64):v is string=>typeof v==="string"&&new RegExp(`^[a-f0-9]{${n}}$`).test(v);
const integer=(v:unknown):v is number=>Number.isSafeInteger(v)&&Number(v)>=0;
const text=(v:unknown):v is string=>typeof v==="string"&&v.length<=4096&&!/[\0\r\n]/.test(v);
const kinds=["target","disc-target","remote-target","library-output","intake-output","unknown-association","media-range","history-association","disposal-authority","task-authority"];
export function validReservationRef(v:ReservationRef):boolean{return !!v&&["organization","library","recovery","media-removal"].includes(v.kind)&&hash(v.id,v.kind==="organization"?32:64);}
function owner(v:ReservationOwner):boolean{return validReservationRef(v)&&text(v.state)&&!!v.state&&(v.batchId===undefined||hash(v.batchId)||hash(v.batchId,32))&&typeof v.related==="boolean"&&integer(v.pathCount)&&v.pathCount<=200000&&Array.isArray(v.paths)&&v.paths.length<=100&&v.paths.length<=v.pathCount&&v.paths.every(p=>text(p)&&p.startsWith("/"))&&Array.isArray(v.claims)&&v.claims.length<=50000&&v.claims.every(c=>!!c&&kinds.includes(c.kind)&&text(c.key)&&!!c.key&&text(c.path));}
export function validReservationRelease(v:ReservationRelease):boolean{return !!v&&hash(v.id)&&["preview","released"].includes(v.state)&&validReservationRef(v.seed)&&typeof v.actor==="string"&&!!v.actor&&hash(v.requestId,32)&&typeof v.decision==="string"&&!!v.decision.trim()&&v.decision.length<=512&&hash(v.fingerprint)&&typeof v.created==="string"&&Array.isArray(v.owners)&&v.owners.length>0&&v.owners.length<=10000&&v.owners.every(owner)&&new Set(v.owners.map(o=>o.kind+":"+o.id)).size===v.owners.length&&v.owners.some(o=>o.kind===v.seed.kind&&o.id===v.seed.id)&&integer(v.claimCount)&&v.claimCount>0&&v.claimCount<=50000&&v.claimCount===v.owners.reduce((n,o)=>n+o.claims.length,0)&&integer(v.rowCount)&&v.rowCount>0&&v.rowCount<=50000;}
export function reservationReleaseGranted(c:ReservationConsent):boolean{return !!c&&c.confirm&&c.confirmReleaseReservations&&c.confirmCloseRelated&&c.confirmNoContentProof&&c.confirmFutureOperations&&c.confirmOldExecutorsStopped;}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
  const r=await fetch(base+path,body===undefined?{headers:{Authorization:token}}:{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify(body)});
  let v:{code:number;success:boolean;data:T;message?:string};try{v=await r.json();}catch{throw new ApiError("预留释放回执不完整，请按已知编号只读核验，不要重发",r.status||503);}
  if(!r.ok||v.code!==0||!v.success||!v.data)throw new ApiError(v.message||"预留释放未核实，请只读刷新",r.status===401?401:v.code||r.status||503);return v.data;
}
export async function getReservationStatus(token:string,after=""):Promise<ReservationStatus>{
  if(after&&!/^(organization:[a-f0-9]{32}|(?:library|recovery|media-removal):[a-f0-9]{64})$/.test(after))throw new ApiError("无效的账本分页位置",400);
  const v=await request<ReservationStatus>(token,after?"?after="+encodeURIComponent(after):"");if(!v||!Array.isArray(v.owners)||v.owners.length>30||!v.owners.every(owner)||typeof v.next!=="string"||(v.next!==""&&!/^(organization:[a-f0-9]{32}|(?:library|recovery|media-removal):[a-f0-9]{64})$/.test(v.next))||!Array.isArray(v.releases)||v.releases.length>30||!v.releases.every(r=>validReservationRelease(r)&&r.state==="released"))throw new ApiError("预留账本列表未核实",503);return v;
}
export async function previewReservationRelease(token:string,seed:ReservationRef,decision:string,confirmRead=false):Promise<ReservationRelease>{
  if(!validReservationRef(seed)||!decision.trim()||decision.length>512||!confirmRead)throw new ApiError("须明确选择旧任务、填写原因并同意完整账本审核",400);
  const v=await request<ReservationRelease>(token,"/preview",{seed,decision:decision.trim(),confirmRead:true});if(!validReservationRelease(v)||v.state!=="preview"||v.seed.kind!==seed.kind||v.seed.id!==seed.id||v.decision!==decision.trim())throw new ApiError("完整预留释放审核未核实",503);return v;
}
export async function releaseReservations(token:string,v:ReservationRelease,c:ReservationConsent):Promise<ReservationRelease>{
  if(!validReservationRelease(v)||v.state!=="preview"||!reservationReleaseGranted(c))throw new ApiError("必须重新审核完整关联范围并分别确认释放、关闭旧任务和风险",400);
  const out=await request<ReservationRelease>(token,"",{seed:v.seed,requestId:v.requestId,decision:v.decision,fingerprint:v.fingerprint,confirmRead:true,...c});
  if(!validReservationRelease(out)||out.state!=="released"||out.id!==v.id||out.fingerprint!==v.fingerprint||out.actor!==v.actor||out.requestId!==v.requestId||out.decision!==v.decision||out.seed.kind!==v.seed.kind||out.seed.id!==v.seed.id||JSON.stringify(out.owners)!==JSON.stringify(v.owners)||out.rowCount!==v.rowCount||out.claimCount!==v.claimCount)throw new ApiError("预留释放回执范围未核实，请按稳定编号只读查询，不要重发",503);return out;
}
export async function getReservationRelease(token:string,id:string):Promise<ReservationRelease>{if(!hash(id))throw new ApiError("无效的预留释放回执编号",400);const v=await request<ReservationRelease>(token,"/"+id);if(!validReservationRelease(v)||v.state!=="released"||v.id!==id)throw new ApiError("已保存预留释放回执未核实",503);return v;}
