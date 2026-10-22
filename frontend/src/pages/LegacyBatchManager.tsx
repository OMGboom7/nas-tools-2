import {useEffect,useRef,useState} from "react";
import {ApiError,controlLegacyOrganizationBatch,getLegacyOrganizationBatch,organizationMovesSource,organizationIsRemote,type OrganizationLegacyBatch} from "../api/client";
import {RemoteTransferConsent} from "../components/RemoteTransferConsent";
type Props={token:string;batchId:string;enabled:boolean;onChanged:(id:string)=>Promise<void>;onSessionExpired:()=>void};
export function LegacyBatchManager({token,batchId,enabled,onChanged,onSessionExpired}:Props){
  const [data,setData]=useState<OrganizationLegacyBatch|null>(null),[error,setError]=useState("");
  const [action,setAction]=useState<"execute"|"reconcile"|"stop"|"resume"|"close">("reconcile");
  const [confirmed,setConfirmed]=useState(false),[stopped,setStopped]=useState(false),[removal,setRemoval]=useState(false),[exclusive,setExclusive]=useState(false),[recovery,setRecovery]=useState(false),[reason,setReason]=useState("");
  const [busy,setBusy]=useState(false);const generation=useRef(0),inFlight=useRef(false);
  const remote=!!data&&organizationIsRemote(data.mode)&&["execute","resume"].includes(action);
  function clear(){setConfirmed(false);setStopped(false);setRemoval(false);setExclusive(false);setRecovery(false);}
  useEffect(()=>{generation.current++;setData(null);setError("");setReason("");clear();inFlight.current=false;setBusy(false);return()=>{generation.current++;};},[token,batchId]);
  async function load(){
    if(!enabled||inFlight.current)return;inFlight.current=true;const version=++generation.current;setBusy(true);setError("");clear();
    try{const next=await getLegacyOrganizationBatch(token,batchId);if(version===generation.current)setData(next);}catch(err){if(version!==generation.current)return;if(err instanceof ApiError&&err.code===401)onSessionExpired();else setError(err instanceof Error?err.message:"读取批次失败");}finally{if(version===generation.current){setBusy(false);inFlight.current=false;}}
  }
  async function operate(){
    if(!enabled||inFlight.current||!data||!confirmed)return;inFlight.current=true;const version=++generation.current;setBusy(true);setError("");
    try{const next=await controlLegacyOrganizationBatch(token,data,action,{confirm:confirmed,confirmOldExecutorsStopped:stopped,confirmSourceRemoval:removal,...(remote?{confirmRemoteExclusive:exclusive,confirmRemoteRecoveryCopy:recovery}:{}),...(reason.trim()?{reason:reason.trim()}:{})});if(version!==generation.current)return;setData(next);clear();await onChanged(next.jobs[0] || "");}
    catch(err){if(version!==generation.current)return;if(err instanceof ApiError&&err.code===401)onSessionExpired();else setError(err instanceof Error?err.message:"批次操作未核实；不自动重试");clear();try{const next=await getLegacyOrganizationBatch(token,batchId);if(version===generation.current)setData(next);}catch{/* No replay after a lost response. */}}
    finally{if(version===generation.current){setBusy(false);inFlight.current=false;}}
  }
  return <section aria-label="旧记录多目标批次">
    <h3>旧记录多目标批次 {batchId}</h3><p>单个子任务完成不代表整条记录完成。不得在某个子任务上解除整批关联；需先停止整批，再按原任务凭证处置未完成对象。取消／恢复原源／远程停止都不记新成功。</p>
    {error&&<p role="alert">{error}</p>}
    <button type="button" disabled={!enabled||busy} onClick={()=>void load()}>只读刷新完整批次与审核快照</button>
    {data&&<div><p>状态 {data.state} · {data.reason || "无异常说明"} · 原关联状态 {data.unknownState || "不关联未知行"}</p><p>来源 {data.sourceRoot}/{data.scope} · 方式 {data.mode}</p><ul>{data.targets.map((root,i)=><li key={`${i}:${root}`}>{root} · 子任务 {data.jobs[i]}</li>)}</ul>
      <fieldset disabled={!enabled||busy}><label>明确操作<select value={action} onChange={e=>{setAction(e.target.value as typeof action);clear();}}><option value="reconcile">只读核验全部目标和来源，补完整批次回执</option><option value="execute">仅执行从未启动过的 ready 批次</option><option value="stop">停止整批后续写入（当前步骤须核对）</option><option value="resume">重新授权逐任务人工恢复，不自动续跑</option><option value="close">关闭整批：仅取消从未开始项，保留文件与历史</option></select></label>
      {["stop","resume","close"].includes(action)&&<label>原因<input maxLength={512} value={reason} onChange={e=>{setReason(e.target.value);clear();}}/></label>}
      {["execute","resume","close"].includes(action)&&<label><input type="checkbox" checked={stopped} onChange={e=>setStopped(e.target.checked)}/>我已实际停止旧执行器及会修改本轮来源／目标的外部程序。</label>}
      {action==="execute"&&organizationMovesSource(data.mode)&&<label><input type="checkbox" checked={removal} onChange={e=>setRemoval(e.target.checked)}/>我另外授权目标核验后移除所列来源与核验备份。</label>}
      {remote&&<RemoteTransferConsent busy={busy||!enabled} exclusive={exclusive} recovery={recovery} onExclusive={setExclusive} onRecovery={setRecovery}/>}
      <p>“核验”不重传、不继续发布或清理；“重新授权”只开放原任务的独立人工恢复入口，仍需逐任务新确认。关闭前，已执行但未完成的子任务必须先凭证恢复／放弃；关闭不标旧行成功，不删除已发布媒体或云对象。</p>
      <label><input type="checkbox" checked={confirmed} onChange={e=>setConfirmed(e.target.checked)}/>我已审核所有目标和子任务，明确确认上述操作；这不是把部分成功当整条完成。</label><button type="button" disabled={!confirmed} onClick={()=>void operate()}>确认本次批次操作</button>
      </fieldset>
    </div>}
  </section>;
}
