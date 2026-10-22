import { useEffect, useRef, useState } from "react";
import { ApiError, type LibraryStorage } from "../api/client";
import { browseRecoveryObjects, continueRecoveryDisposal, createRecoveryDisposal, getRecoveryObjects, previewRecoveryObjects, returnRecoveryCapture, reviewRecoveryDisposal, stopRecoveryDisposal, type RecoveryBrowse, type RecoveryConsent, type RecoveryDisposal, type RecoveryPreview, type RecoveryReview, type RecoverySelection, type RecoveryStatus } from "../api/recoveryObjects";

const emptyConsent=():RecoveryConsent=>({confirm:false,confirmPermanentDeletion:false,confirmOnlyCopyRisk:false,confirmOldExecutorsStopped:false,confirmCloseRelated:false,confirmRemoteExclusive:false});
const labels:Record<string,string>={ready:"已保存审核，尚未删除",capturing:"捕获意图已保存",deleting:"逐项删除意图已保存",needs_review:"中断／变化，必须新审核",returning:"捕获归还待核验",returned:"整个捕获已归还，旧任务未重开",completed:"所选恢复对象已不在原范围",closed:"已停止后续处置，剩余对象保留"};
export function RecoveryDisposalConsent({value,onChange,remote,disabled}:{value:RecoveryConsent;onChange:(v:RecoveryConsent)=>void;remote:boolean;disabled:boolean}){
  const box=(key:keyof RecoveryConsent,text:string)=><label><input type="checkbox" checked={value[key]} onChange={e=>onChange({...value,[key]:e.target.checked})}/>{text}</label>;
  return <fieldset disabled={disabled} aria-label="独立永久处置授权"><legend>本次独立处置授权（不会继承旧整理／清理授权）</legend>
    {box("confirm","我确认下面审核的完整恢复单元及全部内容，不以名称或关联记录认定其归属")}
    {box("confirmPermanentDeletion","我明确授权永久删除所选恢复对象；这不是只删数据库记录，删除后不能通过本操作撤销")}
    {box("confirmOnlyCopyRisk","我已核实这些对象可能包含原文件或唯一副本，接受失去旧任务恢复／继续发布能力的风险")}
    {box("confirmOldExecutorsStopped","我已实际停止旧 Python、其他实例、下载器和全部外部写入器；勾选不会替我停止它们")}
    {box("confirmCloseRelated","我同意永久停止列出的关联整理任务及整个关联旧记录批次，不重开、不伪造成功；旧历史、目标及预留保留")}
    {remote&&box("confirmRemoteExclusive","我独占本轮远程命名空间，接受非原子删除、完整内容重复读取的费用；不绕过保留策略、不强制清除旧版本")}
  </fieldset>;
}
const consentReady=(v:RecoveryConsent,remote:boolean)=>v.confirm&&v.confirmPermanentDeletion&&v.confirmOnlyCopyRisk&&v.confirmOldExecutorsStopped&&v.confirmCloseRelated&&(!remote||v.confirmRemoteExclusive);
export function RecoveryObjectManager({token,enabled,onSessionExpired,onChanged}:{token:string;enabled:boolean;onSessionExpired:()=>void;onChanged:()=>void}){
  const [status,setStatus]=useState<RecoveryStatus|null>(null),[rootId,setRoot]=useState(""),[driver,setDriver]=useState<LibraryStorage>(""),[directory,setDirectory]=useState("."),[selected,setSelected]=useState("");
  const [decision,setDecision]=useState(""),[read,setRead]=useState(false),[browse,setBrowse]=useState<RecoveryBrowse|null>(null),[preview,setPreview]=useState<RecoveryPreview|null>(null),[review,setReview]=useState<RecoveryReview|null>(null);
  const [consent,setConsent]=useState(emptyConsent),[busy,setBusy]=useState(false),[error,setError]=useState(""),[unknown,setUnknown]=useState(false),[stopConfirm,setStopConfirm]=useState(false),[returnConfirm,setReturnConfirm]=useState(false),[returnStopped,setReturnStopped]=useState(false),[stopping,setStopping]=useState(false);
  const epoch=useRef(0),inFlight=useRef(false),stopFlight=useRef(false);
  function invalidate(){setPreview(null);setReview(null);setConsent(emptyConsent());setStopConfirm(false);setReturnConfirm(false);setReturnStopped(false);}
  useEffect(()=>{epoch.current++;inFlight.current=false;stopFlight.current=false;setStatus(null);setRoot("");setDriver("");setDirectory(".");setSelected("");setDecision("");setRead(false);setBrowse(null);invalidate();setBusy(false);setStopping(false);setError("");setUnknown(false);return()=>{epoch.current++;};},[token]);
  async function operate(action:()=>Promise<void>,mutation=false){if(!enabled||inFlight.current)return;const version=++epoch.current;inFlight.current=true;setBusy(true);setError("");try{await action();}catch(e){if(version!==epoch.current)return;if(e instanceof ApiError&&e.code===401)onSessionExpired();else setError((e instanceof Error?e.message:"操作未核实")+(mutation?"。请只读刷新，不要重复提交。":""));invalidate();if(mutation)setUnknown(true);}finally{if(version===epoch.current){inFlight.current=false;setBusy(false);setConsent(emptyConsent());setReturnConfirm(false);setReturnStopped(false);}}}
  async function load(){const v=epoch.current;const next=await getRecoveryObjects(token);if(v!==epoch.current)return;setStatus(next);setUnknown(false);invalidate();}
  async function list(path=directory){const v=epoch.current;invalidate();const next=await browseRecoveryObjects(token,rootId,driver,path);if(v!==epoch.current)return;setDirectory(path);setBrowse(next);setSelected("");}
  const selection=():RecoverySelection=>({rootId,driver,path:selected,decision,confirmRead:read});
  async function inspect(){const v=epoch.current;invalidate();const result=await previewRecoveryObjects(token,selection());if(v===epoch.current)setPreview(result);}
  function updateRecord(record:RecoveryDisposal){setStatus(s=>s?{...s,disposals:[record,...s.disposals.filter(r=>r.id!==record.id)]}:s);}
  async function create(){if(!preview)return;const v=epoch.current;const saved=await createRecoveryDisposal(token,selection(),preview,consent);if(v!==epoch.current)return;updateRecord(saved);invalidate();onChanged();}
  async function inspectRecord(id:string){const v=epoch.current;invalidate();const next=await reviewRecoveryDisposal(token,id);if(v===epoch.current){setReview(next);updateRecord(next.disposal);}}
  async function execute(){if(!review)return;const current=review;const v=epoch.current;const saved=await continueRecoveryDisposal(token,current,consent);if(v!==epoch.current)return;updateRecord(saved);invalidate();onChanged();}
  async function giveBack(){if(!review)return;const current=review,v=epoch.current;const saved=await returnRecoveryCapture(token,current,returnConfirm,returnConfirm,returnStopped);if(v!==epoch.current)return;updateRecord(saved);invalidate();onChanged();}
  // A stop is intentionally allowed while a long explicit continuation is in
  // flight. It cancels subsequent work, never retries or rolls back that call.
  async function stop(id:string){if(!enabled||!stopConfirm||stopFlight.current)return;const version=++epoch.current;stopFlight.current=true;setStopping(true);setError("");try{const saved=await stopRecoveryDisposal(token,id,true,true);if(version!==epoch.current)return;inFlight.current=false;setBusy(false);updateRecord(saved);invalidate();setUnknown(true);onChanged();}catch(e){if(version===epoch.current){inFlight.current=false;setBusy(false);invalidate();setError((e instanceof Error?e.message:"停止回执未核实")+"。请只读刷新，不要重发。");setUnknown(true);}}finally{if(version===epoch.current){stopFlight.current=false;setStopping(false);setStopConfirm(false);}}}
  return <section aria-label="恢复对象独立处置"><h2>恢复对象独立处置</h2>
    <p>用于配置目录内完整的 .nastool 恢复单元，包括未入账、含未知文件的暂存和长期保留副本。先只读浏览并完整读取内容审核，再保存稳定任务，最后新审核后单次执行；没有自动清理或重启续跑。不删除已发布媒体，不把目录名称或关联任务当作旧凭证，不改旧历史或把未识别记录标为成功。</p>
    <p>永久删除可能失去唯一原件。所选完整目录内的所有文件和链接都会列入审核，最多 20,000 个对象；不会跟随本机链接，远程危险／链接清单继续拒绝。本机先同目录捕获、核验，再逐项删除；捕获不符则保留。远程没有通用原子比较删除，旧版本／保留策略可能仍占空间。审核及删除前会重复完整读取，不能按一次读取估算流量。</p>
    {error&&<p role="alert">{error}</p>}<button type="button" disabled={!enabled||busy||stopping} onClick={()=>void operate(load)}>只读刷新处置任务与配置范围</button>
    {status&&<><fieldset disabled={!enabled||busy||stopping||unknown}><legend>仅浏览已配置目录</legend>
      <label>存储<select value={driver} onChange={e=>{setDriver(e.target.value as LibraryStorage);setBrowse(null);setSelected("");invalidate();}}>{status.drivers.map(d=><option key={d||"local"} value={d}>{d||"本机文件系统"}</option>)}</select></label>
      <label>配置目录<select value={rootId} onChange={e=>{setRoot(e.target.value);setDirectory(".");setBrowse(null);setSelected("");invalidate();}}><option value="">请选择</option>{status.roots.map(r=><option key={r.id} value={r.id}>{r.label} · {r.path}</option>)}</select></label>
      <label>相对浏览目录<input value={directory} onChange={e=>{setDirectory(e.target.value);setBrowse(null);setSelected("");invalidate();}}/></label><button type="button" disabled={!rootId} onClick={()=>void operate(()=>list())}>只读浏览此层</button>
      {browse&&<ul>{browse.entries.map(e=><li key={e.path}>{e.path}{e.directory&&<button type="button" onClick={()=>void operate(()=>list(e.path))}>进入目录</button>}{e.candidate&&<button type="button" onClick={()=>{setSelected(e.path);setRead(false);invalidate();}}>选择这个完整恢复单元</button>}</li>)}</ul>}
      <label>完整恢复单元相对路径<input value={selected} onChange={e=>{setSelected(e.target.value);setRead(false);invalidate();}} placeholder=".nastool-copy-… 或 .nastool-remote/…"/></label>
      <label>本次独立处置原因<textarea maxLength={512} value={decision} onChange={e=>{setDecision(e.target.value);invalidate();}}/></label>
      <label><input type="checkbox" checked={read} onChange={e=>{setRead(e.target.checked);invalidate();}}/>我同意完整读取所选对象以建立本次新审核，承担远程重复读取费用；这不是旧任务归属证明</label>
      <button type="button" disabled={!rootId||!selected||!decision.trim()||!read} onClick={()=>void operate(inspect)}>只读审核完整内容与关联影响</button>
    </fieldset>
    {preview&&<fieldset disabled={busy||stopping}><legend>本次独立范围</legend><p>{preview.driver||"本机"} · {preview.path}：文件 {preview.files}，目录 {preview.directories}，链接 {preview.links}，完整读取字节 {preview.bytes}。{preview.unknown?"未找到关联账本。":"存在关联记录，但不代表已证明这些对象归属。"}</p><ul>{preview.sample.map(p=><li key={p}>{p}</li>)}</ul><p>会永久停止的关联范围：</p><ul>{preview.owners.map(o=><li key={o.kind+o.id}>{o.kind} · {o.id} · {o.state}{o.related?"（整个旧记录批次的其他任务）":""}</li>)}</ul><RecoveryDisposalConsent value={consent} onChange={setConsent} remote={!!preview.driver} disabled={busy||stopping}/><button type="button" disabled={!consentReady(consent,!!preview.driver)} onClick={()=>void operate(create,true)}>保存独立审核任务（此步不删除对象）</button></fieldset>}
    <p>最近 50 条处置任务；刷新和查看不会执行删除。停止后剩余对象、捕获目录及旧预留保留，不表示在途单次请求已经撤回。</p><label><input type="checkbox" checked={stopConfirm} onChange={e=>setStopConfirm(e.target.checked)}/>我确认只停止所点击任务的后续处置，不删除剩余对象、不重开旧任务</label>
    <ul>{status.disposals.map(d=><li key={d.id}>{d.id.slice(0,12)} · {d.driver||"本机"} · {d.path} · {labels[d.state]} · 回执 {d.done}/{d.total}{d.capture&&<span> · 捕获：{d.capture}</span>}<button type="button" disabled={!enabled||busy||stopping} onClick={()=>void operate(()=>inspectRecord(d.id))}>只读完整核验此任务</button>{!["closed","completed","returned"].includes(d.state)&&<button type="button" disabled={!enabled||stopping||!stopConfirm} onClick={()=>void stop(d.id)}>只停止后续处置，保留剩余对象</button>}</li>)}</ul></>}
    {review&&<fieldset disabled={!enabled||stopping}><legend>单次执行／续办审核 · {review.disposal.path}</legend><p>{review.evidence}；{labels[review.disposal.state]}。旧历史、目标、预留不因本操作而清空。错误或响应丢失后先只读刷新，不重发。</p><RecoveryDisposalConsent value={consent} onChange={setConsent} remote={!!review.disposal.driver} disabled={busy||stopping}/><button type="button" disabled={busy||!review.canContinue||!consentReady(consent,!!review.disposal.driver)} onClick={()=>void operate(execute,true)}>确认仅按本次新审核执行一次永久处置</button>
      {review.canReturn&&<fieldset disabled={busy}><legend>只归还尚未删除的整个捕获对象</legend><label><input type="checkbox" checked={returnConfirm} onChange={e=>setReturnConfirm(e.target.checked)}/>我确认仅将整个捕获对象归还到原空位置，不覆盖、不重新启用旧任务</label><label><input type="checkbox" checked={returnStopped} onChange={e=>setReturnStopped(e.target.checked)}/>我已实际停止全部外部写入器</label><button type="button" disabled={!returnConfirm||!returnStopped} onClick={()=>void operate(giveBack,true)}>单独确认归还整个捕获对象</button></fieldset>}
    </fieldset>}
  </section>;
}
