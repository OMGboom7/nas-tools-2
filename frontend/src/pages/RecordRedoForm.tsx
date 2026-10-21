import { useEffect, useRef, useState } from "react";
import { ApiError, executeRecordRedo, organizationMovesSource, previewRecordRedo, type OrganizationRedoInput, type OrganizationRedoResult, type OrganizationRedoReview } from "../api/client";
import { RemoteTransferConsent } from "../components/RemoteTransferConsent";

type Props={token:string;enabled:boolean;onChanged:(id:string)=>Promise<void>;onSessionExpired:()=>void};

export function RecordRedoForm({token,enabled,onChanged,onSessionExpired}:Props){
  const [flag,setFlag]=useState<OrganizationRedoInput["flag"]>("unidentification");
  const [ids,setIDs]=useState("");
  const [review,setReview]=useState<OrganizationRedoReview|null>(null);
  const [results,setResults]=useState<OrganizationRedoResult[]>([]);
  const [confirmed,setConfirmed]=useState(false),[sourceRemoval,setSourceRemoval]=useState(false);
  const [exclusive,setExclusive]=useState(false),[recovery,setRecovery]=useState(false),[stopped,setStopped]=useState(false);
  const [busy,setBusy]=useState(false),[error,setError]=useState("");
  const generation=useRef(0),inFlight=useRef(false);
  const remote=!!review?.items.some(item=>item.plan?.remote);
  const multi=!!review?.items.some(item=>(item.plan?.partitions?.length || 0)>1);
  const stoppedRequired=remote||multi;
  const moves=!!review?.items.some(item=>organizationMovesSource(item.plan?.mode));
  const targets=review?.items.flatMap(item=>item.plan?.remote?[{targetId:item.id,path:item.targetRoot || "",remote:item.plan.remote}]:[]);
  function clearConsent(){setConfirmed(false);setSourceRemoval(false);setExclusive(false);setRecovery(false);setStopped(false);}
  function invalidate(){generation.current++;setReview(null);clearConsent();setError("");}
  useEffect(()=>{generation.current++;inFlight.current=false;setFlag("unidentification");setIDs("");setReview(null);setResults([]);clearConsent();setBusy(false);setError("");return()=>{generation.current++;};},[token]);
  async function operate(execute:boolean){
    if(!enabled || inFlight.current || execute&&(!review||!confirmed||moves&&!sourceRemoval||remote&&(!exclusive||!recovery)||stoppedRequired&&!stopped))return;
    inFlight.current=true;const version=++generation.current;setBusy(true);setError("");
    const input:OrganizationRedoInput={flag,ids:ids.trim().split(/[\s,，]+/).filter(Boolean)};
    try{
      if(!execute){setReview(null);setResults([]);clearConsent();const next=await previewRecordRedo(token,input);if(version===generation.current)setReview(next);}
      else{
        const outcome=await executeRecordRedo(token,input,review!,confirmed,sourceRemoval,stoppedRequired?{confirmRemoteExclusive:exclusive,confirmRemoteRecoveryCopy:recovery,confirmOldExecutorsStopped:stopped}:undefined);
        if(version!==generation.current)return;
        setResults(outcome.results);setReview(null);clearConsent();
        if(!outcome.success)setError(outcome.message || "部分记录未完成；核对逐记录结果，不重跑已保存任务。");
        await onChanged(outcome.results.find(item=>item.jobId)?.jobId || "");
      }
    }catch(err){
      if(version!==generation.current)return;
      if(err instanceof ApiError&&err.code===401)onSessionExpired();else setError(err instanceof Error?err.message:"请求未完成，请只读刷新任务账本");
      clearConsent();if(execute){setReview(null);try{await onChanged("");}catch{/* Never replay a lost mutation. */}}
    }finally{if(version===generation.current){inFlight.current=false;setBusy(false);}}
  }
  return <section aria-label="沿用旧方式重新识别">
    <h2>沿用旧方式重新识别</h2><p>按每条旧记录的原来源、明确目的与字面方式重新识别；无旧方式时使用当前默认方式。保留原历史和已发布媒体，原来源丢失不会改用目标。不重置已处理记录；不确定任务须在原账本恢复。</p>
    {error&&<p role="alert">{error}</p>}
    <fieldset disabled={!enabled||busy}>
      <label>记录类型<select value={flag} onChange={e=>{setFlag(e.target.value as OrganizationRedoInput["flag"]);invalidate();}}><option value="unidentification">未识别记录（含已处理）</option><option value="history">原历史</option></select></label>
      <label>完整记录 ID（空格或逗号分隔，最多 100 条）<input value={ids} maxLength={2100} onChange={e=>{setIDs(e.target.value);invalidate();}}/></label>
      <button type="button" onClick={()=>void operate(false)}>只读审核原记录与新计划</button>
    </fieldset>
    {review&&<div>
      <p>只有下列可用计划会执行；失败项保持原状，每条记录单独保留结果，不将部分成功当作整批完成。</p>
      <ul>{review.items.map(item=><li key={item.id}>#{item.id} · {item.plan?`${item.sourceRoot} → ${item.targetRoot} · ${item.plan.mode}`:`不执行：${item.message || item.code}`}<ul>{item.plan?.items.map(file=><li key={file.source}>{file.source} → {file.target} · {file.status}</li>)}</ul></li>)}</ul>
      {multi&&<div><p>完整多目标范围：任何子任务未完成都不标整条旧记录成功。</p>{review.items.flatMap(item=>(item.plan?.partitions || []).map(part=><div key={`${item.id}:${part.targetId}`}>#{item.id} → {part.targetRoot}<ul>{part.items.map(file=><li key={file.source}>{file.source} → {file.target}</li>)}</ul></div>))}</div>}
      <label><input type="checkbox" disabled={!enabled||busy} checked={confirmed} onChange={e=>setConfirmed(e.target.checked)}/>我已审核原记录与完整来源范围，确认文件下载完成且稳定，同意只执行以上可用计划。</label>
      {moves&&<label><input type="checkbox" disabled={!enabled||busy} checked={sourceRemoval} onChange={e=>setSourceRemoval(e.target.checked)}/>我另外同意移动模式在目标完整核验和持久回执后移除已确认的原源名与恢复备份，不删除其他文件。</label>}
      {stoppedRequired&&<><label><input type="checkbox" disabled={!enabled||busy} checked={stopped} onChange={e=>setStopped(e.target.checked)}/>我已实际停止旧执行器及会写本轮来源／远程范围的外部程序。</label>{remote&&<RemoteTransferConsent targets={targets} busy={!enabled||busy} exclusive={exclusive} recovery={recovery} onExclusive={setExclusive} onRecovery={setRecovery}/>}</>}
      <button type="button" disabled={!enabled||busy||!review.items.some(item=>item.plan)||!confirmed||moves&&!sourceRemoval||remote&&(!exclusive||!recovery)||stoppedRequired&&!stopped} onClick={()=>void operate(true)}>确认按审核快照重新识别并执行</button>
    </div>}
    {!!results.length&&<ul>{results.map(item=><li key={item.id}>#{item.id} · {item.state} · {item.message || (item.code?item.code:"回执已核实")}{item.jobId&&<button disabled={!enabled||busy} type="button" onClick={()=>{void onChanged(item.jobId!).catch(err=>{if(err instanceof ApiError&&err.code===401)onSessionExpired();else setError(err instanceof Error?err.message:"读取任务失败，请只读刷新");});}}>只读查看任务 {item.jobId}</button>}</li>)}</ul>}
  </section>;
}
