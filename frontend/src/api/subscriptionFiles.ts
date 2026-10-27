import {ApiError} from "./client";
export type TorrentFile={index:number;path:string;size:string;selected:boolean;padding?:boolean};
export type FileSeason={subscriptionId:string;season:number;total:number;episodes:number[]};
export type FileSelection={id:string;downloaderId:string;downloaderType:"qbittorrent"|"transmission";title:string;subscriptionId:string;season:number;episodes:number[];infoHash:string;state:"adding"|"added"|"unwanting"|"unwanted"|"wanting"|"selected"|"starting"|"ready"|"rejected";sourceRecorded:boolean;files:TorrentFile[];seasons?:FileSeason[]};
export type FileReview={receipt:FileSelection;current:{hash:string;directory:string;labels:string[];state:string;paused:boolean;files:TorrentFile[]}|null;next:"none"|"adopt-add"|"settings"|"adopt-settings"|"select"|"adopt-unwanted"|"adopt-selected"|"start"|"acknowledge"|"record";actor:string;presetEvidence:string;fingerprint:string};
const hex=(v:unknown,n:number):v is string=>typeof v==="string"&&new RegExp("^[a-f0-9]{"+n+"}$").test(v);
const bounded=(v:unknown,n=4096):v is string=>typeof v==="string"&&v.length<=n&&!/[\x00-\x1f\x7f]/.test(v);
const integer=(v:unknown,zero=false):v is string=>typeof v==="string"&&(zero?/^(0|[1-9][0-9]{0,18})$/:/^[1-9][0-9]{0,18}$/).test(v)&&BigInt(v)<=9223372036854775807n;
const eps=(v:unknown):v is number[]=>Array.isArray(v)&&v.length>0&&v.length<=10000&&v.every((n,i)=>Number.isSafeInteger(n)&&n>0&&n<=10000&&(i===0||n>v[i-1]));
export function validTorrentFiles(v:TorrentFile[]):boolean{
 if(!Array.isArray(v)||v.length===0||v.length>10000)return false;const indexes=new Set<number>(),paths=new Set<string>();
 return v.every(f=>{if(!f||!Number.isSafeInteger(f.index)||f.index<0||f.index>1000000||indexes.has(f.index)||!bounded(f.path)||!f.path||f.path.includes("\\")||f.path.includes(":")||f.path.split("/").some(p=>!p||p==="."||p==="..")||paths.has(f.path.toLowerCase())||!integer(f.size,true)||typeof f.selected!=="boolean"||f.padding!==undefined&&typeof f.padding!=="boolean"||f.padding&&f.selected)return false;indexes.add(f.index);paths.add(f.path.toLowerCase());return true});
}
export function validFileSelection(v:FileSelection):boolean{
 if(v?.seasons!==undefined){
  const sources=v.seasons;
  if(!Array.isArray(sources)||sources.length<1||sources.length>100||!sources.every((s,i)=>!!s&&integer(s.subscriptionId)&&(!i||BigInt(s.subscriptionId)>BigInt(sources[i-1].subscriptionId))&&Number.isSafeInteger(s.season)&&s.season>=0&&s.season<=1000&&Number.isSafeInteger(s.total)&&s.total>0&&s.total<=10000&&eps(s.episodes)&&s.episodes.every(n=>n<=s.total))||!sources.some(s=>s.subscriptionId===v.subscriptionId&&s.season===v.season&&JSON.stringify(s.episodes)===JSON.stringify(v.episodes)))return false;
 }
 return !!v&&hex(v.id,32)&&integer(v.downloaderId)&&integer(v.subscriptionId)&&["qbittorrent","transmission"].includes(v.downloaderType)&&bounded(v.title)&&!!v.title&&Number.isSafeInteger(v.season)&&v.season>=0&&v.season<=1000&&eps(v.episodes)&&(hex(v.infoHash,40)||v.downloaderType==="qbittorrent"&&hex(v.infoHash,64))&&["adding","added","unwanting","unwanted","wanting","selected","starting","ready","rejected"].includes(v.state)&&typeof v.sourceRecorded==="boolean"&&(!v.sourceRecorded||v.state==="ready")&&validTorrentFiles(v.files)&&v.files.some(f=>f.selected);
}
export function validFileReview(v:FileReview):boolean{
 if(!v||!validFileSelection(v.receipt)||!["none","adopt-add","settings","adopt-settings","select","adopt-unwanted","adopt-selected","start","acknowledge","record"].includes(v.next)||!bounded(v.actor,256)||!v.actor||!hex(v.fingerprint,64)||v.presetEvidence!==""&&!hex(v.presetEvidence,64))return false;
 if(v.next==="none")return v.current===null&&(v.receipt.sourceRecorded||v.receipt.state==="rejected");
 const s=v.current;if(!s||s.hash!==v.receipt.infoHash||!bounded(s.directory)||!s.directory||!bounded(s.state,64)||typeof s.paused!=="boolean"||!Array.isArray(s.labels)||s.labels.length>65||!s.labels.every(x=>bounded(x,256))||!s.labels.includes("NASTOOL-FILES-"+v.receipt.id)||!validTorrentFiles(s.files))return false;
 const expected=new Map(v.receipt.files.map(f=>[f.path,f]));for(const f of s.files){const e=expected.get(f.path);if(!e||e.size!==f.size)return false;expected.delete(f.path)}if([...expected.values()].some(f=>!f.padding))return false;
 const wants=new Map(v.receipt.files.map(f=>[f.path,f.selected]));
 const state=v.receipt.state,exact=s.files.every(f=>f.selected===wants.get(f.path));
 const noExtras=s.files.every(f=>!f.selected||wants.get(f.path)===true);
 switch(v.next){
  case "adopt-add":return state==="adding"&&s.paused;
  case "settings":case "adopt-settings":return state==="added"&&s.paused;
  case "select":return (state==="added"||state==="unwanted")&&s.paused;
  case "adopt-unwanted":return state==="unwanting"&&s.paused&&noExtras;
  case "adopt-selected":return state==="wanting"&&s.paused&&exact;
  case "start":return state==="selected"&&s.paused&&exact;
  case "acknowledge":return (state==="starting"&&!s.paused||state==="selected"&&s.paused)&&exact;
  case "record":return state==="ready"&&!v.receipt.sourceRecorded&&exact;
  default:return false;
 }
}
async function request<T>(token:string,path:string,body?:unknown):Promise<T>{
 const r=await fetch(path,body===undefined?{headers:{Authorization:token}}:{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify(body)});let v:{code:number;success:boolean;message?:string;data:T};
 try{v=await r.json()}catch{throw new ApiError("文件选择回执不完整，请保留原编号只读查回，不要重发",r.status||503)}
 if(!r.ok||v?.code!==0||v.success!==true||v.data===undefined)throw new ApiError(v?.message||"原文件选择未核实",r.status===401?401:v?.code||r.status||503);return v.data;
}
function endpoint(id:string){if(!hex(id,32))throw new ApiError("需要完整文件选择编号",400);return "/api/v1/subscription-file-selections/"+id}
export async function getFileSelection(token:string,id:string):Promise<FileSelection>{const v=await request<FileSelection>(token,endpoint(id));if(!validFileSelection(v)||v.id!==id)throw new ApiError("原文件选择回执未核实",503);return v}
export async function getFileSelections(token:string,page=1):Promise<FileSelection[]>{if(!Number.isSafeInteger(page)||page<1||page>10000)throw new ApiError("无效回执页码",400);const v=await request<FileSelection[]>(token,"/api/v1/subscription-file-selections?page="+page);if(!Array.isArray(v)||v.length>30||!v.every(validFileSelection))throw new ApiError("文件选择清单未核实",503);return v}
export async function reviewFileSelection(token:string,id:string):Promise<FileReview>{const v=await request<FileReview>(token,endpoint(id)+"/review",{});if(!validFileReview(v)||v.receipt.id!==id)throw new ApiError("原文件选择审核未核实",503);return v}
export async function recoverFileSelection(token:string,v:FileReview,confirm=false,flags:{files?:boolean;settings?:boolean;start?:boolean;record?:boolean;seasons?:boolean}={}):Promise<FileSelection>{
 if(!validFileReview(v)||!!v.receipt.seasons?.length&&!flags.seasons||v.next==="none"||!confirm||v.next==="select"&&!flags.files||v.next==="settings"&&!flags.settings||v.next==="start"&&!flags.start||v.next==="record"&&!flags.record)throw new ApiError("先重新审核原范围并独立确认下一阶段",400);
 const out=await request<FileSelection>(token,endpoint(v.receipt.id)+"/recover",{fingerprint:v.fingerprint,confirm:true,confirmFiles:flags.files===true,confirmSettings:flags.settings===true,confirmStart:flags.start===true,confirmRecord:flags.record===true,confirmSeasons:flags.seasons===true});
 const states:Record<string,FileSelection["state"]>={"adopt-add":"added","adopt-unwanted":"unwanted","adopt-selected":"selected","adopt-settings":"added",settings:"added",select:"selected",start:"ready",acknowledge:"ready",record:"ready"};
 if(!validFileSelection(out)||out.state!==states[v.next]||v.next==="record"&&!out.sourceRecorded||JSON.stringify(out.seasons)!==JSON.stringify(v.receipt.seasons)||out.id!==v.receipt.id||out.infoHash!==v.receipt.infoHash||out.subscriptionId!==v.receipt.subscriptionId||out.downloaderId!==v.receipt.downloaderId||JSON.stringify(out.files)!==JSON.stringify(v.receipt.files))throw new ApiError("恢复回执未核实，只读查回原编号，不要重发",503);return out;
}
