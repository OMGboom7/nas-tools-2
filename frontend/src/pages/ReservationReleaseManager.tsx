import { useEffect, useRef, useState } from "react";
import { ApiError } from "../api/client";
import { getReservationRelease, getReservationStatus, previewReservationRelease, releaseReservations, reservationReleaseGranted, validReservationRef, type ReservationConsent, type ReservationOwner, type ReservationRef, type ReservationRelease, type ReservationStatus } from "../api/reservationRelease";

const emptyConsent=():ReservationConsent=>({confirm:false,confirmReleaseReservations:false,confirmCloseRelated:false,confirmNoContentProof:false,confirmFutureOperations:false,confirmOldExecutorsStopped:false});
const labels:Record<ReservationRef["kind"],string>={organization:"整理任务",library:"已有库刮削",recovery:"恢复对象处置", "media-removal":"历史及媒体删除批次"};
export function ReservationConsentFields({value,onChange,disabled}:{value:ReservationConsent;onChange:(v:ReservationConsent)=>void;disabled:boolean}){
  const box=(key:keyof ReservationConsent,text:string)=><label key={key}><input type="checkbox" checked={value[key]} onChange={e=>onChange({...value,[key]:e.target.checked})}/>{text}</label>;
  return <fieldset disabled={disabled}><legend>独立释放授权，不是文件删除或成功核对</legend>
    {box("confirm","我已核对完整关联范围、所有旧任务和全部占用；展示的路径样例不是工作子集")}
    {box("confirmReleaseReservations","我独立同意释放列出的目标、整盘、远程、元数据及已停止删除批次的占用，不只停止旧任务")}
    {box("confirmCloseRelated","我同意永久关闭全部列出的旧任务、整个旧多库／当前刮削批次及相关删除任务，取消待处理的未知记录关联；不能重新续办或复用旧恢复授权")}
    {box("confirmNoContentProof","本次只核对账本，不读取媒体内容，不证明文件存在、缺失或整理成功；原文件、恢复副本、历史和原始凭据保留，不会自动删除或采用未知内容")}
    {box("confirmFutureOperations","解除占用可能允许未来已授权的自动任务或新任务处理这些位置；它们仍须重新核验内容，不会重开旧任务或取得覆盖未知文件的许可")}
    {box("confirmOldExecutorsStopped","我已实际停止旧 Python、其他实例、下载器及全部外部写入器；勾选不会替我停止它们")}
  </fieldset>;
}
function OwnerDetails({owner}:{owner:ReservationOwner}){
  return <li>{labels[owner.kind]} · {owner.id} · 原状态 {owner.state}{owner.related?"（完整关联范围）":"（所选起点）"}{owner.batchId&&` · 批次 ${owner.batchId}`}
    <details><summary>该范围的全部 {owner.claims.length} 项占用／执行授权</summary><ul>{owner.claims.map((c,i)=><li key={i}>{c.kind} · {c.key}{c.path&&` · ${c.path}`}</li>)}</ul></details>
    <details><summary>历史路径共 {owner.pathCount} 个，最多 100 项展示；不读取或操作这些文件</summary><ul>{owner.paths.map(p=><li key={p}>{p}</li>)}</ul></details>
  </li>;
}
function ReleaseDetails({value}:{value:ReservationRelease}){return <><p>{value.state==="preview"?"只读审核，尚未释放":"已提交释放回执，旧任务保持关闭"} · 稳定编号 {value.id}</p><p>完整范围 {value.owners.length} 个旧任务／批次，{value.claimCount} 项占用／授权，{value.rowCount} 行原始账本留档。原因：{value.decision}</p><ul>{value.owners.map(o=><OwnerDetails key={o.kind+o.id} owner={o}/>)}</ul></>;}
export function ReservationReleaseManager({token,enabled,onSessionExpired,onChanged}:{token:string;enabled:boolean;onSessionExpired:()=>void;onChanged:()=>void}){
  const [status,setStatus]=useState<ReservationStatus|null>(null),[kind,setKind]=useState<ReservationRef["kind"]>("organization"),[ownerId,setOwnerId]=useState(""),[decision,setDecision]=useState(""),[read,setRead]=useState(false);
  const [preview,setPreview]=useState<ReservationRelease|null>(null),[receipt,setReceipt]=useState<ReservationRelease|null>(null),[consent,setConsent]=useState(emptyConsent),[lookupId,setLookupId]=useState(""),[busy,setBusy]=useState(false),[error,setError]=useState(""),[uncertain,setUncertain]=useState(false);
  const epoch=useRef(0),inFlight=useRef(false);
  function invalidate(){setPreview(null);setReceipt(null);setConsent(emptyConsent());}
  function changed(){invalidate();setRead(false);}
  useEffect(()=>{epoch.current++;inFlight.current=false;setStatus(null);setKind("organization");setOwnerId("");setDecision("");setRead(false);setLookupId("");invalidate();setBusy(false);setError("");setUncertain(false);return()=>{epoch.current++;};},[token]);
  async function operate(action:()=>Promise<void>,mutation=false){if(!enabled||inFlight.current)return;const version=++epoch.current;inFlight.current=true;setBusy(true);setError("");try{await action();}catch(e){if(version!==epoch.current)return;invalidate();if(e instanceof ApiError&&e.code===401)onSessionExpired();else setError((e instanceof Error?e.message:"操作未核实")+(mutation?"。请保留稳定编号，只读查询，不要重复提交释放。":""));if(mutation)setUncertain(true);}finally{if(version===epoch.current){inFlight.current=false;setBusy(false);setConsent(emptyConsent());}}}
  async function load(after=""){const version=epoch.current;const v=await getReservationStatus(token,after);if(version!==epoch.current)return;setStatus(v);invalidate();setUncertain(false);}
  async function inspect(){const version=epoch.current;invalidate();const v=await previewReservationRelease(token,{kind,id:ownerId.trim()},decision,read);if(version===epoch.current)setPreview(v);}
  async function apply(){if(!preview)return;const version=epoch.current;setLookupId(preview.id);const v=await releaseReservations(token,preview,consent);if(version!==epoch.current)return;invalidate();setReceipt(v);setUncertain(false);setStatus(s=>s?{...s,owners:s.owners.filter(o=>!v.owners.some(n=>n.kind===o.kind&&n.id===o.id)),releases:[v,...s.releases.filter(r=>r.id!==v.id)].slice(0,30)}:s);onChanged();}
  async function lookup(id=lookupId){const version=epoch.current;invalidate();const v=await getReservationRelease(token,id);if(version===epoch.current){setLookupId(v.id);setReceipt(v);setUncertain(false);}}
  function choose(ref:ReservationRef){setKind(ref.kind);setOwnerId(ref.id);changed();}
  return <section aria-label="旧目标预留独立释放"><h2>旧目标预留独立释放</h2>
    <p>这是独立的账本处置，不删除媒体、不清理副本、不写整理成功或历史，不操作下载器。先完整只读审核，再分别确认；一次事务关闭全部关联旧任务、释放精确预留并保存原始账本。没有后台 worker、启动重放或错误重试。</p>
    <p>不要求旧根目录或云账户在线，也不读取媒体内容。释放不证明文件存在或缺失；未知内容仍不能当作旧凭据。未来已授权的自动流程可能重新处理这些位置，旧任务的继续／恢复授权则永久关闭。</p>
    {error&&<p role="alert">{error}</p>}
    <button type="button" disabled={!enabled||busy} onClick={()=>void operate(()=>load())}>只读刷新预留账本与释放回执</button>
    <label>已知释放回执编号（提交前保留；响应丢失时不要重复释放）<input value={lookupId} disabled={busy} maxLength={64} onChange={e=>{setLookupId(e.target.value);invalidate();}}/></label><button type="button" disabled={!enabled||busy||!/^[a-f0-9]{64}$/.test(lookupId)} onClick={()=>void operate(()=>lookup())}>按稳定编号只读查回释放结果</button>
    <fieldset disabled={!enabled||busy||uncertain}><legend>选择旧任务作为完整关联范围的起点</legend>
      <label>旧任务类型<select value={kind} onChange={e=>{setKind(e.target.value as ReservationRef["kind"]);setOwnerId("");changed();}}>{Object.entries(labels).map(([k,label])=><option key={k} value={k}>{label}</option>)}</select></label>
      <label>精确旧任务／批次编号<input value={ownerId} maxLength={64} onChange={e=>{setOwnerId(e.target.value);changed();}}/></label>
      <label>独立释放原因<textarea value={decision} maxLength={512} onChange={e=>{setDecision(e.target.value);changed();}}/></label>
      <label><input type="checkbox" checked={read} onChange={e=>{setRead(e.target.checked);invalidate();}}/>同意读取完整关联账本；不读取媒体内容，不授权释放</label>
      <button type="button" disabled={!read||!decision.trim()||!validReservationRef({kind,id:ownerId.trim()})} onClick={()=>void operate(inspect)}>只读审核全部关联任务与占用</button>
    </fieldset>
    {status&&<><p>本页最多 30 个起点；审核会计算完整关联范围，不只释放本页。也可直接输入较早任务编号。</p><ul>{status.owners.map(o=><li key={o.kind+o.id}>{labels[o.kind]} · {o.id} · {o.state} · {o.claims.length} 项占用／授权<button type="button" disabled={busy||uncertain} onClick={()=>choose(o)}>选择为审核起点</button></li>)}</ul>{status.next&&<button type="button" disabled={busy} onClick={()=>void operate(()=>load(status.next))}>只读读取后续账本</button>}<p>最近 30 个已提交释放回执</p><ul>{status.releases.map(v=><li key={v.id}>{v.id} · {v.created}<button type="button" disabled={busy} onClick={()=>void operate(()=>lookup(v.id))}>只读查看完整释放回执</button></li>)}</ul></>}
    {preview&&<fieldset disabled={!enabled||busy}><legend>完整新审核，尚未修改账本</legend><ReleaseDetails value={preview}/><ReservationConsentFields value={consent} onChange={setConsent} disabled={busy}/><button type="button" disabled={!reservationReleaseGranted(consent)} onClick={()=>void operate(apply,true)}>按此新审核关闭旧任务并释放预留（不删除文件）</button></fieldset>}
    {receipt&&<section aria-label="已提交预留释放回执"><ReleaseDetails value={receipt}/></section>}
  </section>;
}
