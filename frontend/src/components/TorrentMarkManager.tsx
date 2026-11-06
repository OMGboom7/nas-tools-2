import { useEffect, useRef, useState } from "react";
import { ApiError } from "../api/client";

type Run={id:string;kind:string;state:string;created:string;changed:number;skipped:number;unknown:number};
type Status={fingerprint:string;goOnly:boolean;active:boolean;exclusiveRequired:boolean;settings:{enable:boolean;onlyonce:boolean;cron:string};control:{Epoch:string;Next:string;Once:string};targets:{id:string;name:string;type:string;receiver:string}[];runs:Run[];pending:{id:string;downloader:string;hash:string;goal:string}[]};
const states:Record<string,string>={complete:"已完成",partial:"部分未完成",unknown:"结果待核对",running:"执行中",stopped:"已停止",archived:"原记录已归档",manual:"手动",once:"一次运行",cron:"定时"};
const hash=(s:unknown)=>typeof s==="string"&&/^[a-f0-9]{64}$/.test(s);
export function validTorrentMarkStatus(s:Status):boolean{
 return !!s&&hash(s.fingerprint)&&typeof s.goOnly==="boolean"&&typeof s.active==="boolean"&&typeof s.exclusiveRequired==="boolean"&&typeof s.settings?.enable==="boolean"&&typeof s.settings.onlyonce==="boolean"&&typeof s.settings.cron==="string"&&typeof s.control?.Epoch==="string"&&typeof s.control.Next==="string"&&typeof s.control.Once==="string"&&Array.isArray(s.targets)&&s.targets.every(d=>/^[1-9][0-9]*$/.test(d.id)&&typeof d.name==="string"&&["qbittorrent","transmission"].includes(d.type)&&typeof d.receiver==="string")&&Array.isArray(s.runs)&&s.runs.length<=30&&s.runs.every(r=>hash(r.id)&&Object.hasOwn(states,r.state)&&[r.changed,r.skipped,r.unknown].every(n=>Number.isSafeInteger(n)&&n>=0))&&Array.isArray(s.pending)&&s.pending.length<=500&&s.pending.every(p=>hash(p.id)&&/^[a-f0-9]{40}$|^[a-f0-9]{64}$/.test(p.hash)&&["PT","BT"].includes(p.goal));
}
async function request<T>(token:string,path:string,signal:AbortSignal,body?:unknown):Promise<T>{
 const response=await fetch("/api/v1/torrent-mark"+path,{method:body===undefined?"GET":"POST",signal,headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},...(body===undefined?{}:{body:JSON.stringify(body)})});
 const e=await response.json().catch(()=>null);
 if(!response.ok||e?.code!==0||e?.success!==true||!e.data||typeof e.data!=="object")throw new ApiError(typeof e?.message==="string"?e.message:"应答未确认，请回查原编号",response.status);
 return e.data as T;
}
export function TorrentMarkManager({token,onSessionExpired}:{token:string;onSessionExpired:()=>void}){
 const [data,setData]=useState<Status>();const [approved,setApproved]=useState("");const [exclusive,setExclusive]=useState("");const [busy,setBusy]=useState(false);const [error,setError]=useState("");const [notice,setNotice]=useState("");const [pending,setPending]=useState("");
 const life=useRef<AbortController|null>(null);const mutex=useRef(false);const version=useRef(0);
 const stopping=useRef(false);const [stopBusy,setStopBusy]=useState(false);
 function failed(e:unknown){if(e instanceof ApiError&&e.code===401)onSessionExpired();else setError(e instanceof Error?e.message:"种子标记不可用")}
 async function load(signal:AbortSignal){const v=++version.current;try{const result=await request<Status>(token,"",signal);if(signal.aborted||v!==version.current)return;if(!validTorrentMarkStatus(result))throw new Error("种子标记响应不完整");setData(result);setApproved(x=>x===result.fingerprint?x:"");setExclusive(x=>x===result.fingerprint?x:"");}catch(e){if(!signal.aborted)failed(e)}}
 useEffect(()=>{const c=new AbortController();life.current=c;void load(c.signal);return()=>{c.abort();version.current++}},[token]);
 async function stopNow(){
  const c=life.current;if(!data||!c||c.signal.aborted||stopping.current||!data.goOnly)return;
  stopping.current=true;setStopBusy(true);setError("");
  try{const result=await request<{stopped:boolean}>(token,"/stop",c.signal,{confirm:true,epoch:data.control.Epoch});if(c.signal.aborted)return;if(result.stopped!==true)throw new Error("停止回执未确认，请只读刷新");setNotice("已停止后续执行，已发送的标签变更仍需核对。");setApproved("");setExclusive("");await load(c.signal)}catch(e){if(!c.signal.aborted)failed(e)}finally{stopping.current=false;if(!c.signal.aborted)setStopBusy(false)}
 }
 async function perform(action:"run"|"activate"|"reconcile"){
  const c=life.current;if(!data||!c||c.signal.aborted||mutex.current||stopping.current||!data.goOnly||approved!==data.fingerprint||action!=="reconcile"&&data.exclusiveRequired&&exclusive!==data.fingerprint)return;
  mutex.current=true;setBusy(true);setError("");setNotice("");let requestId=pending;
  if(action==="run"&&!requestId){requestId=Array.from(crypto.getRandomValues(new Uint8Array(32)),b=>b.toString(16).padStart(2,"0")).join("");setPending(requestId)}
  try{const result=await request<Run|Record<string,unknown>>(token,"/"+action,c.signal,{confirm:true,fingerprint:data.fingerprint,epoch:data.control.Epoch,exclusive:exclusive===data.fingerprint,...(action==="run"?{requestId}:{})});if(c.signal.aborted)return;if(action==="run"&&(!hash((result as Run).id)||!Object.hasOwn(states,(result as Run).state)))throw new Error("执行回执不完整，请保留编号并回查");if(action==="run")setPending("");setNotice(action==="reconcile"?"已完成只读核对，请查看尚待核对的记录。":"操作已有回执，请查看当前记录。");setApproved("");setExclusive("");await load(c.signal)}catch(e){if(!c.signal.aborted)failed(e)}finally{mutex.current=false;if(!c.signal.aborted)setBusy(false)}
 }
 return <section className="service-panel" aria-label="种子标记"><h2>种子标记</h2><p>按已完成任务的 Tracker 和私有属性设置 PT／BT，并保留其他标签。证据不完整时跳过；未知结果保留原意图供只读核对。</p>
 {error&&<p className="data-warning" role="alert">{error}</p>}{notice&&<p role="status">{notice}</p>}
 {data&&<><p>当前：{data.active?"已启用":"未启用"}；周期：{data.settings.cron||"未设置"}；下次：{data.control.Once||data.control.Next||"无"}</p><ul>{data.targets.map(d=><li key={d.id}>{d.name} · {d.type} · {d.receiver}</li>)}</ul>
 {!data.goOnly&&<p>请在 Go 独立运行模式执行。</p>}
 <label><input type="checkbox" checked={approved===data.fingerprint} disabled={busy} onChange={e=>setApproved(e.target.checked?data.fingerprint:"")}/>确认以上下载器范围，已停止旧种子标记执行器</label>
 {data.exclusiveRequired&&<label><input type="checkbox" checked={exclusive===data.fingerprint} disabled={busy} onChange={e=>setExclusive(e.target.checked?data.fingerprint:"")}/>确认 Transmission 标签由本应用独占写入，已停止外部标签修改</label>}
 {pending&&<p role="status">原请求编号：{pending}。回查继续使用此编号。</p>}
 <div className="services-heading__actions"><button className="secondary-button" disabled={busy} onClick={()=>life.current&&void load(life.current.signal)}>只读刷新</button>
 <button className="secondary-button" disabled={busy||!data.goOnly||!data.settings.enable||data.targets.length===0||approved!==data.fingerprint||data.exclusiveRequired&&exclusive!==data.fingerprint} onClick={()=>void perform("run")}>{pending?"回查原请求":"执行一次"}</button>
 <button className="secondary-button" disabled={busy||!!pending||!data.goOnly||!data.settings.enable||data.targets.length===0||approved!==data.fingerprint||data.exclusiveRequired&&exclusive!==data.fingerprint||!data.settings.cron&&!data.settings.onlyonce} onClick={()=>void perform("activate")}>启用周期／一次运行</button>
 <button className="secondary-button" disabled={stopBusy||!data.goOnly} onClick={()=>void stopNow()}>停止后续执行</button>
 <button className="secondary-button" disabled={busy||!data.goOnly||!data.pending.length||approved!==data.fingerprint} onClick={()=>void perform("reconcile")}>只读核对未知结果</button></div>
 <p>待核对写入：{data.pending.length}。停止会阻止后续请求，已发送的标签变更需读取下载器确认。</p>
 <table><thead><tr><th>执行编号</th><th>触发</th><th>状态</th><th>标记／跳过／未知</th></tr></thead><tbody>{[...data.runs].reverse().map(r=><tr key={r.id}><td>{r.id}</td><td>{states[r.kind]||r.kind}</td><td>{states[r.state]}</td><td>{r.changed}／{r.skipped}／{r.unknown}</td></tr>)}</tbody></table>
 {data.pending.length>0&&<ul>{data.pending.map(p=><li key={p.id}>{p.downloader} · {p.hash} · {p.goal}</li>)}</ul>}</>}
 </section>
}
