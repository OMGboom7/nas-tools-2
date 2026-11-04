import { useEffect, useRef, useState } from "react";
import { ApiError } from "../api/client";

type Run={id:string;kind:string;state:string;created:string;updated:string;message:string;added:number;updatedSites:number;kept:number;skipped:number;stopRequested:boolean};
type Status={
  goOnly:boolean;fingerprint:string;receiver:string;current:boolean;settingsInvalid:boolean;
  settings:{server:string;cron:string;synchronousMode:string;black_list:string;white_list:string;notify:boolean;onlyonce:boolean;keyConfigured:boolean;passwordConfigured:boolean};
  control:{epoch:string;nextRun:string;nextOnce:string};
  runs:Run[];
};
const labels:Record<string,string>={test:"连接测试",manual:"手动同步",once:"一次运行",cron:"定时同步",running:"执行中",complete:"已完成",failed:"失败",unknown:"结果未知",stopped:"已停止"};
const nonce=()=>Array.from(crypto.getRandomValues(new Uint8Array(32)),b=>b.toString(16).padStart(2,"0")).join("");

async function cookieCloudRequest<T>(token:string,path:string,body?:unknown,signal?:AbortSignal):Promise<T>{
  const response=await fetch("/api/v1/cookie-cloud"+path,{
    method:body===undefined?"GET":"POST",signal,
    headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},
    ...(body===undefined?{}:{body:JSON.stringify(body)}),
  });
  const envelope=await response.json().catch(()=>null);
  if(!response.ok||envelope?.code!==0||envelope?.success!==true||typeof envelope.data!=="object"||envelope.data===null){
    throw new ApiError(typeof envelope?.message==="string"?envelope.message:"应答未确认，请查看原记录",response.status);
  }
  return envelope.data as T;
}

export function CookieCloudManager({token,onSessionExpired}:{token:string;onSessionExpired:()=>void}){
  const [data,setData]=useState<Status>();
  const [approved,setApproved]=useState("");
  const [busy,setBusy]=useState(false);
  const [notice,setNotice]=useState("");
  const [error,setError]=useState("");
  const pending=useRef<{action:string;requestId:string}|undefined>(undefined);
  const running=useRef(false);
  const life=useRef<AbortController|null>(null);

  function failed(e:unknown){
    if(e instanceof ApiError&&e.code===401)onSessionExpired();
    else setError(e instanceof Error?e.message:"无法读取 CookieCloud");
  }
  async function load(signal?:AbortSignal){
    try{
      const result=await cookieCloudRequest<Status>(token,"",undefined,signal);
      if(signal?.aborted)return;
      setData(result);
      setApproved(value=>value===result.fingerprint?value:"");
    }catch(e){if(!signal?.aborted)failed(e);}
  }
  useEffect(()=>{
    const controller=new AbortController();
    life.current=controller;
    void load(controller.signal);
    const timer=window.setInterval(()=>void load(controller.signal),4000);
    return ()=>{controller.abort();window.clearInterval(timer);};
  },[token]);

  async function perform(action:"run"|"test"|"activate"|"stop"){
    if(!data||running.current||!data.goOnly||action!=="stop"&&approved!==data.fingerprint)return;
    const controller=life.current;
    if(!controller||controller.signal.aborted)return;
    running.current=true;
    setBusy(true);
    setError("");
    setNotice("");
    try{
      const body={fingerprint:data.fingerprint,epoch:data.control.epoch,confirm:true,requestId:"",activeRunId:active?.id||""};
      if(action==="run"||action==="test"){
        if(pending.current?.action!==action)pending.current={action,requestId:nonce()};
        body.requestId=pending.current.requestId;
      }
      const result=await cookieCloudRequest<Partial<Run>>(token,"/"+action,body,controller.signal);
      if(controller.signal.aborted)return;
      pending.current=undefined;
      setApproved("");
      setNotice(result.id?"已登记原记录 "+result.id:action==="stop"?"已撤销同步授权":"已确认原定时设置");
      await load(controller.signal);
    }catch(e){
      if(!controller.signal.aborted){
        failed(e);
        await load(controller.signal);
      }
    }finally{
      running.current=false;
      if(!controller.signal.aborted)setBusy(false);
    }
  }

  const active=data?.runs.find(run=>run.state==="running");
  const ready=!!data&&data.goOnly&&!!data.receiver&&data.settings.keyConfigured&&data.settings.passwordConfigured;
  return <section className="signin-manager" aria-label="CookieCloud 同步管理">
    <header><div><h2>CookieCloud 同步管理</h2><p>在插件卡片的配置中保存服务器、过滤方式和周期。确认范围后再执行。</p></div>
      <button type="button" disabled={busy} onClick={()=>void load(life.current?.signal)}>重新读取 CookieCloud</button></header>
    {error&&<p role="alert" className="data-warning">{error}</p>}
    {notice&&<p role="status">{notice}</p>}
    {!data?<p>正在读取原同步设置…</p>:<>
      <p>原接收方 <code>{data.receiver||"尚未配置"}</code></p>
      {data.settingsInvalid&&<p role="alert">已保存设置无效，请在插件配置中核对。</p>}
      <p>用户标识 {data.settings.keyConfigured?"已保存":"未保存"} · 解密密码 {data.settings.passwordConfigured?"已保存":"未保存"}</p>
      <p>过滤方式 {({all_mode:"全部",black_mode:"黑名单",white_mode:"白名单"} as Record<string,string>)[data.settings.synchronousMode]||"待核对"}</p>
      {data.settings.synchronousMode==="black_mode"&&<p>黑名单 {data.settings.black_list||"无"}</p>}
      {data.settings.synchronousMode==="white_mode"&&<p>白名单 {data.settings.white_list||"无"}</p>}
      <p>周期 {data.settings.cron||"未设置"} · {data.current?"已启用":data.control.epoch?"授权需要重新核对或一次运行已领取":"未启用"}</p>
      {data.current&&data.control.nextRun&&<p>下次周期 {new Date(data.control.nextRun).toLocaleString()}</p>}
      {data.current&&data.control.nextOnce&&<p>一次运行 {new Date(data.control.nextOnce).toLocaleString()}</p>}
      <p>测试只读取原服务。同步还会检查已配置站点、更新适用的 Cookie，并新增目录支持的站点。</p>
      <label><input type="checkbox" disabled={!ready||busy} checked={approved===data.fingerprint} onChange={e=>setApproved(e.target.checked?data.fingerprint:"")}/>确认上述接收方及执行范围</label>
      <div className="site-statistics__actions">
        <button type="button" disabled={!ready||busy||!!active||approved!==data.fingerprint} onClick={()=>void perform("test")}>测试原服务连接</button>
        <button type="button" disabled={!ready||busy||!!active||approved!==data.fingerprint} onClick={()=>void perform("run")}>同步一次</button>
        <button type="button" disabled={!ready||busy||!!active||approved!==data.fingerprint||!data.settings.cron&&!data.settings.onlyonce} onClick={()=>void perform("activate")}>启用保存的周期或一次运行</button>
        <button type="button" disabled={busy||!data.goOnly||!active&&!data.control.epoch} onClick={()=>void perform("stop")}>停止同步和定时</button>
      </div>
      <h3>同步记录</h3>
      {!data.runs.length?<p>暂无同步记录。</p>:<ul>{data.runs.map(run=><li key={run.id}>
        <strong>{labels[run.kind]||run.kind} · {labels[run.state]||run.state}</strong>
        <p>{run.message} · {new Date(run.updated).toLocaleString()}</p><small>编号 {run.id}</small>
      </li>)}</ul>}
    </>}
  </section>;
}
