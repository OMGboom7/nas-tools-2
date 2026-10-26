import { useEffect,useRef,useState } from "react";
import { ApiError } from "../api/client";
import { getMediaRefreshJob,getMediaRefreshStatus,previewMediaRefresh,stopMediaRefreshJob,submitMediaRefresh,type MediaRefreshConsent,type MediaRefreshJob,type MediaRefreshReview,type MediaRefreshStatus } from "../api/mediaRefresh";

const emptyConsent=():MediaRefreshConsent=>({confirm:false,confirmScan:false,confirmOldExecutorsStopped:false});
export function MediaRefreshConsentFields({value,onChange,disabled}:{value:MediaRefreshConsent;onChange:(v:MediaRefreshConsent)=>void;disabled:boolean}){
  return <fieldset disabled={disabled}><legend>本次独立刷新授权</legend>
    <label><input type="checkbox" checked={value.confirm} onChange={e=>onChange({...value,confirm:e.target.checked})}/>我已核对当前服务器身份和全部目标，明确同意本次刷新请求</label>
    <label><input type="checkbox" checked={value.confirmScan} onChange={e=>onChange({...value,confirmScan:e.target.checked})}/>独立同意定向或回退整库扫描、服务器元数据变化及 I/O 开销；已接收不等于扫描完成</label>
    <label><input type="checkbox" checked={value.confirmOldExecutorsStopped} onChange={e=>onChange({...value,confirmOldExecutorsStopped:e.target.checked})}/>我已实际停止旧 Python 刷新插件及重复事件执行器，勾选不会代为停止它们</label>
  </fieldset>;
}
function PlanView({value}:{value:MediaRefreshReview["plan"]}){
  return <><p>服务器：{value.server} · 身份：{value.serverId} · 完整范围共 {value.targets.length} 项</p><details><summary>展开全部刷新目标（不是样例）</summary><ul>{value.targets.map((t,i)=><li key={i}>{t.kind} · {t.id} · {t.label}{t.path&&" · "+t.path}</li>)}</ul></details></>;
}
function JobView({value}:{value:MediaRefreshJob}){
  return <section aria-label="已保存媒体刷新回执"><p>稳定编号：{value.id}</p><p>状态：{value.state} · 来源：{value.source} · 原任务：{value.reference||"整库明确请求"}</p><p>{value.reason}</p>{value.plan&&<PlanView value={value.plan}/>}<ul>{value.actions.map(a=><li key={a.index}>{a.target.label||a.target.id} · {a.state} · {a.reason}</li>)}</ul></section>;
}
export function MediaRefreshManager({token,onSessionExpired}:{token:string;onSessionExpired:()=>void}){
  const [status,setStatus]=useState<MediaRefreshStatus|null>(null),[whole,setWhole]=useState(false),[jobId,setJobId]=useState(""),[read,setRead]=useState(false),[lookupId,setLookupId]=useState("");
  const [review,setReview]=useState<MediaRefreshReview|null>(null),[receipt,setReceipt]=useState<MediaRefreshJob|null>(null),[consent,setConsent]=useState(emptyConsent),[stop,setStop]=useState(false),[busy,setBusy]=useState(false),[error,setError]=useState("");
  const epoch=useRef(0),inFlight=useRef(false);
  function invalidate(){setReview(null);setReceipt(null);setConsent(emptyConsent());setStop(false)}
  useEffect(()=>{epoch.current++;inFlight.current=false;setStatus(null);setWhole(false);setJobId("");setRead(false);setLookupId("");invalidate();setBusy(false);setError("");return()=>{epoch.current++}},[token]);
  async function operate(action:()=>Promise<void>,mutation=false){
    if(inFlight.current)return;const version=++epoch.current;inFlight.current=true;setBusy(true);setError("");
    try{await action()}catch(e){if(version!==epoch.current)return;invalidate();if(e instanceof ApiError&&e.code===401)onSessionExpired();else setError((e instanceof Error?e.message:"刷新结果未核实")+(mutation?"。请保留稳定编号，只读查回，不要重复提交。":""))}
    finally{if(version===epoch.current){inFlight.current=false;setBusy(false);setConsent(emptyConsent());setStop(false)}}
  }
  async function load(){const version=epoch.current;const v=await getMediaRefreshStatus(token);if(version===epoch.current){setStatus(v);invalidate()}}
  async function inspect(){const version=epoch.current;invalidate();const v=await previewMediaRefresh(token,whole?"":jobId.trim(),whole,read);if(version===epoch.current)setReview(v)}
  async function apply(){if(!review)return;const version=epoch.current;setLookupId(review.id);const v=await submitMediaRefresh(token,review,consent);if(version===epoch.current){invalidate();setReceipt(v)}}
  async function lookup(id=lookupId){const version=epoch.current;invalidate();const v=await getMediaRefreshJob(token,id);if(version===epoch.current){setLookupId(v.id);setReceipt(v)}}
  async function stopJob(){if(!receipt)return;const version=epoch.current;const v=await stopMediaRefreshJob(token,receipt.id,stop);if(version===epoch.current){invalidate();setReceipt(v)}}
  function changed(){invalidate();setRead(false)}
  return <section className="media-refresh-panel" aria-label="媒体服务器原生刷新"><h2>入库后刷新媒体服务器</h2>
    <p>仅管理员使用。先在插件管理安装并明确授权“刷新媒体库”，再接收新原生入库事件；不从历史通知补扫。这里不是把媒体服务器清单同步到本地，也不整理或删除文件。</p>
    <p>Emby 保留既有电影跳过、电视剧与分类定向刷新，Plex 按映射路径刷新；无法唯一映射时可能整库扫描，Jellyfin 沿用整库扫描。不强制重写全部元数据、不清空回收站。服务器按自身设置运行，accepted 仅表示接收，不证明扫描完成。</p>
    <button type="button" disabled={busy} onClick={()=>void operate(load)}>只读刷新授权及最近 30 条队列</button>
    {status&&<p>已安装：{status.installed?"是":"否"} · 当前授权有效：{status.active?"是":"否"} · 服务器：{status.server||"未授权"} · 自动延迟：{status.delay} 秒 · 后台：{status.workerRunning?"运行中":"未启动，保存不会启动服务器"}</p>}
    <div className="media-refresh-fields"><label>已知刷新稳定编号（提交前保留，支持查找较早回执）<input value={lookupId} disabled={busy} onChange={e=>{setLookupId(e.target.value);invalidate()}}/></label><button type="button" disabled={busy||!/^[a-f0-9]{64}$/.test(lookupId)} onClick={()=>void operate(()=>lookup())}>按稳定编号只读查回</button></div>
    <fieldset disabled={busy}><legend>明确刷新范围</legend>
      <label><input type="checkbox" checked={whole} onChange={e=>{setWhole(e.target.checked);changed()}}/>明确审核整库（不从某条历史推断范围）</label>
      {!whole&&<label>已经完整入库的原生整理任务编号<input value={jobId} onChange={e=>{setJobId(e.target.value);changed()}} placeholder="32 位完整编号"/></label>}
      <label><input type="checkbox" checked={read} onChange={e=>{setRead(e.target.checked);invalidate()}}/>同意只读访问已配置媒体服务器并审核完整目标；这不授权扫描</label>
      <button type="button" disabled={!read||!whole&&!/^[a-f0-9]{32}$/.test(jobId.trim())||status?.active!==true} onClick={()=>void operate(inspect)}>只读审核服务器及全部目标</button>
    </fieldset>
    {review&&<section aria-label="只读媒体刷新审核"><p>尚未提交。稳定编号：{review.id} · 原生媒体项：{review.itemCount}</p><PlanView value={review.plan}/><MediaRefreshConsentFields value={consent} onChange={setConsent} disabled={busy}/><button type="button" disabled={busy||!consent.confirm||!consent.confirmScan||!consent.confirmOldExecutorsStopped} onClick={()=>void operate(apply,true)}>明确提交本次刷新任务（单次，不重放）</button></section>}
    {receipt&&<><JobView value={receipt}/>{["pending","planning","running"].includes(receipt.state)&&<fieldset disabled={busy}><legend>仅停止这个刷新任务</legend><label><input type="checkbox" checked={stop} onChange={e=>setStop(e.target.checked)}/>确认阻止此任务后续请求；不能撤回服务器已接收的扫描，也不撤销未来事件授权</label><button type="button" disabled={!stop} onClick={()=>void operate(stopJob,true)}>请求停止后续刷新</button></fieldset>}</>}
    {status&&<details><summary>最近 30 条队列与回执</summary><ul>{status.jobs.map(j=><li key={j.id}>{j.id} · {j.state} · {j.due} <button type="button" disabled={busy} onClick={()=>void operate(()=>lookup(j.id))}>只读查看</button></li>)}</ul></details>}
    {error&&<p role="alert" className="form-error">{error}</p>}
    <p>网络错误、进程中断或回执丢失后，不自动重发刷新。更换配置、停用或卸载不会转用新账户继续旧任务；已经进入服务器的扫描仍可能完成。</p>
  </section>;
}
