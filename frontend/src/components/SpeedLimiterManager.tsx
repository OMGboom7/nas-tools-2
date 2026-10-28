import {useEffect,useRef,useState} from "react";
import {ApiError} from "../api/client";
import {authorizeLimiter,getLimiterJob,getLimiterStatus,getLimiterTriggers,ignoreLimiterJob,listLimiterJobs,reviewLimiter,stopLimiter,verifyLimiterOperation,type LimiterJob,type LimiterReview,type LimiterStatus,type LimiterTrigger,type LimitRates} from "../api/speedLimiter";
const states:Record<string,string>={pending:"原计划尚未发送，不自动续跑",intent:"发送意图已保存，可能中断",confirmed:"已应答且当前数值核对一致",verified_current:"管理员只读核对当前值，不证明设置者",uncertain:"结果不确定，不自动重发",blocked:"执行前已阻止",ignored:"已明确忽略",not_finalized:"此批回执未结束，不自动续跑",partial:"此批仅部分完成",stopped:"原限速授权已停止或失效"};
const rate=(r:LimitRates)=>"上传 "+(r.uploadLimited?r.upload:"不限速")+"、下载 "+(r.downloadLimited?r.download:"不限速")+" "+r.unit+(r.alternative?"（备用模式）":"");
export function SpeedLimiterManager({token,onSessionExpired}:{token:string;onSessionExpired:()=>void}){
 const [status,setStatus]=useState<LimiterStatus|null>(null),[review,setReview]=useState<LimiterReview|null>(null),[jobs,setJobs]=useState<LimiterJob[]>([]),[job,setJob]=useState<LimiterJob|null>(null),[triggers,setTriggers]=useState<LimiterTrigger[]>([]);
 const [lookup,setLookup]=useState(""),[page,setPage]=useState("1"),[busy,setBusy]=useState(false),[notice,setNotice]=useState(""),[reason,setReason]=useState(""),[ordinal,setOrdinal]=useState("");
 const [http,setHTTP]=useState(false),[plex,setPlex]=useState(false),[plexHTTP,setPlexHTTP]=useState(false),[global,setGlobal]=useState(false),[unlimited,setUnlimited]=useState(false),[legacy,setLegacy]=useState(false),[notify,setNotify]=useState(false),[confirm,setConfirm]=useState(false),[disposition,setDisposition]=useState(false);
 const generation=useRef(0),inFlight=useRef(false);
 const clear=()=>{setHTTP(false);setPlex(false);setPlexHTTP(false);setGlobal(false);setUnlimited(false);setLegacy(false);setNotify(false);setConfirm(false);setDisposition(false)};
 useEffect(()=>{generation.current++;inFlight.current=false;setStatus(null);setReview(null);setJobs([]);setJob(null);setTriggers([]);setLookup("");setPage("1");setReason("");setOrdinal("");setBusy(false);setNotice("");clear();return()=>{generation.current++}},[token]);
 async function operate(action:"status"|"review"|"enable"|"stop"|"list"|"lookup"|"verify"|"ignore"|"triggers"){
  if(inFlight.current)return;
  if(action==="review"&&!status||action==="enable"&&(!review||!global||!unlimited||!legacy||!confirm||review.spec.notify&&!notify)||action==="stop"&&(!status||!confirm)||action==="verify"&&(!job||!disposition||ordinal==="")||action==="ignore"&&(!job||!disposition||!reason.trim()))return;
  const version=generation.current,readConsent={http,plexAccount:plex,plexHTTP},grant={confirm,global,unlimited,legacyStopped:legacy,notify};
  inFlight.current=true;setBusy(true);setNotice("");clear();
  try{
   if(action==="status"){setStatus(null);setReview(null);const v=await getLimiterStatus(token);if(version===generation.current)setStatus(v)}
   else if(action==="review"){setReview(null);const v=await reviewLimiter(token,status!,readConsent);if(version===generation.current)setReview(v)}
   else if(action==="enable"){setReview(null);const v=await authorizeLimiter(token,review!,grant);if(version===generation.current){setStatus(v);setNotice("已授权未来回调和定时检查；此回执不证明已经改变下载器速度。")}}
   else if(action==="stop"){setReview(null);const v=await stopLimiter(token,status!,true);if(version===generation.current){setStatus(v);setNotice("已停止后续自动限速；已有设置和回执保留，不自动恢复或取消限速。")}}
   else if(action==="list"){const v=await listLimiterJobs(token,Number(page));if(version===generation.current)setJobs(v)}
   else if(action==="lookup"){setJob(null);const v=await getLimiterJob(token,lookup.trim());if(version===generation.current){setJob(v);setOrdinal("");setReason("")}}
   else if(action==="triggers"){const v=await getLimiterTriggers(token);if(version===generation.current)setTriggers(v)}
   else{
    const v=action==="verify"?await verifyLimiterOperation(token,job!,Number(ordinal),true):await ignoreLimiterJob(token,job!,true,reason);
    if(version===generation.current){setJob(v);setJobs(current=>current.map(p=>p.id===v.id?v:p));setOrdinal("");setReason("");setNotice(action==="verify"?"仅核对原下载器当前数值；没有重发设置。":"已忽略原未结束设置并停止当前自动限速；保留已经发生的操作。");if(action==="ignore"){setStatus(null);setReview(null)}}
   }
  }catch(err){
   if(version!==generation.current)return;
   if(err instanceof ApiError&&err.code===401)onSessionExpired();else setNotice(err instanceof Error?err.message:"请只读核对原限速回执");
   // Lost mutations only permit a status read. Never replay enable, settings,
   // verification or disposition automatically.
   if(action==="enable"||action==="stop"||action==="ignore"){try{const v=await getLimiterStatus(token);if(version===generation.current)setStatus(v)}catch{/* retain uncertainty */}}
  }finally{if(version===generation.current){inFlight.current=false;setBusy(false)}}
 }
 const choose=(p:LimiterJob)=>{setLookup(p.id);setJob(p);setReason("");setOrdinal("");setDisposition(false)};
 return <section className="notification-receipt-panel" aria-label="播放联动下载器限速"><h2>播放联动下载器限速</h2>
  <p>先在“下载器限速”插件配置中保存播放／空闲速度、来源范围及下载器，再在这里单独审核启用。旧配置不会自动启用。回调只触发读取原媒体服务器的真实会话；读取失败不会当作无人播放。</p>
  <fieldset disabled={busy}><button type="button" onClick={()=>void operate("status")}>只读核对限速设置</button>
   {status&&<div><p>原媒体服务：{status.server||"不可用"} · {status.installed?"插件已安装":"插件未安装"} · {status.active?"未来检查已授权":"自动限速未启用或授权失效"} · {status.workerRunning?"后台已运行":"后台尚未运行"}。{status.reason}</p>
    <p>播放上传／下载：{status.spec.upload} / {status.spec.download}；空闲上传／下载：{status.spec.idleUpload} / {status.spec.idleDownload} KB/s；智能上行带宽 {status.spec.bandwidth} Mbps；每 {status.spec.interval} 秒检查。0 表示不限速。qBittorrent 以 1024 字节换算，Transmission 沿用原 RPC 数值单位。</p>
    <p>不限速来源：IPv4 {status.spec.ipv4}；IPv6 {status.spec.ipv6}。实际启用下载器：{status.targets.map(t=>t.name+"（权重 "+t.weight+"）").join("、")||"暂无"}。</p>
    <label><input type="checkbox" checked={http} onChange={e=>{setHTTP(e.target.checked);setReview(null)}}/>允许向以上原明文 HTTP 连接传送已有账户凭据和限速请求（HTTPS 不需要此项）</label>
    {status.cloud&&<><label><input type="checkbox" checked={plex} onChange={e=>{setPlex(e.target.checked);setReview(null)}}/>允许用原 Plex 账户登录 plex.tv，读取原指定服务器；不尝试其他账户或认证流程</label><label><input type="checkbox" checked={plexHTTP} onChange={e=>{setPlexHTTP(e.target.checked);setReview(null)}}/>可选：允许使用 Plex 原服务器提供的明文 HTTP 连接</label></>}
    <button type="button" disabled={!status.installed||status.cloud&&!plex} onClick={()=>void operate("review")}>只读读取当前会话与原全局速度</button>
    <label><input type="checkbox" checked={confirm} onChange={e=>setConfirm(e.target.checked)}/>确认按当前审核启用或停止这份自动限速</label><button type="button" disabled={!confirm} onClick={()=>void operate("stop")}>仅停止后续自动限速</button>
   </div>}
   {review&&<article aria-label="原下载器限速审核"><h3>原配置只读审核</h3><p>服务身份：{review.mediaId} · 非豁免播放 {review.observation.playing}，豁免 {review.observation.exempt} · 合计 {review.observation.bitrate} bit/s。审核有效期 10 分钟，启用前会再核对原速度。</p>
    {review.targets.map(t=><p key={t.id}>{t.name} · {t.kind} · 当前 {rate(t.before)}；按当前会话计算 {rate(t.desired)} · 权重 {t.weight}</p>)}
    <p>此授权覆盖未来会话变化，可能改变所选下载器内所有任务的全局速度。备用模式切换、外部改值和未确认的旧设置会阻止后续写入；停用不能撤回已经接收的操作。</p>
    <label><input type="checkbox" checked={global} onChange={e=>setGlobal(e.target.checked)}/>允许按以上播放、空闲与带宽分配规则调整所选下载器的全局速度</label>
    <label><input type="checkbox" checked={unlimited} onChange={e=>setUnlimited(e.target.checked)}/>理解并允许配置中的 0 表示不限速，停止插件不会自动恢复旧速度</label>
    <label><input type="checkbox" checked={legacy} onChange={e=>setLegacy(e.target.checked)}/>已停止旧 Python 限速插件及其他重复控制这些全局速度的执行器</label>
    {review.spec.notify&&<label><input type="checkbox" checked={notify} onChange={e=>setNotify(e.target.checked)}/>允许按原“自定义消息”通知开关发送已核实的限速状态</label>}
    <button type="button" disabled={!confirm||!global||!unlimited||!legacy||review.spec.notify&&!notify} onClick={()=>void operate("enable")}>启用这份原限速授权</button>
   </article>}
   <p>不确定设置、进程重启和刷新不自动重发。忽略会停止当前后台，不能回滚下载器上的设置；重新启用需要重新审核。</p>
   <button type="button" onClick={()=>void operate("triggers")}>只读查看最近触发与阻止原因</button>
   {triggers.map(p=><p key={p.id}>{p.source} · {p.state} · {p.reference} · {p.reason}</p>)}
   <label>限速回执页码<input type="number" min="1" max="10000" value={page} onChange={e=>setPage(e.target.value)}/></label><button type="button" onClick={()=>void operate("list")}>只读查看本页 30 条限速</button>
   <label>原限速稳定编号<input maxLength={64} value={lookup} onChange={e=>{setLookup(e.target.value);setJob(null);setDisposition(false)}}/></label><button type="button" disabled={!lookup.trim()} onClick={()=>void operate("lookup")}>按原限速编号只读查回</button>
   {job&&<article aria-label="原限速执行回执"><h3>{states[job.state]} · {job.id}</h3><p>{job.source} · {job.reference} · {job.created}</p>
    {job.operations.length===0&&<p>原当前速度已经符合这次计划，没有发送限速设置。</p>}
    {job.operations.map(op=><div key={op.ordinal}><h4>设置 {op.ordinal+1} · {op.name} · {op.dimension}</h4><p>{rate(op.before)} → {rate(op.after)}</p><p>{states[op.state]} · {op.reason}{op.actor&&" · "+op.actor}</p>{["intent","uncertain"].includes(op.state)&&<button type="button" onClick={()=>{setOrdinal(String(op.ordinal));setDisposition(false)}}>选择只读核对设置 {op.ordinal+1}</button>}</div>)}
    {ordinal!==""&&<p>已选原设置 {Number(ordinal)+1}；只读取原下载器当前值，不重发，不更换目标。值相同不证明是谁设置的。</p>}
    <label>忽略原因<input maxLength={1000} value={reason} onChange={e=>{setReason(e.target.value);setDisposition(false)}}/></label>
    <label><input type="checkbox" checked={disposition} onChange={e=>setDisposition(e.target.checked)}/>确认仅核对原设置，或按所填原因忽略此原未结束项并停止当前后台</label>
    <button type="button" disabled={!disposition||ordinal===""} onClick={()=>void operate("verify")}>仅只读核对所选原设置</button>
    <button type="button" disabled={!disposition||!reason.trim()||!job.operations.some(op=>!["confirmed","verified_current","ignored"].includes(op.state))} onClick={()=>void operate("ignore")}>仅忽略原未结束设置并停止后台</button>
   </article>}
   {jobs.map(p=><button type="button" key={p.id} onClick={()=>choose(p)}>{p.source} · {states[p.state]} · {p.id}</button>)}
  </fieldset>{notice&&<p role="status">{notice}</p>}
 </section>;
}
