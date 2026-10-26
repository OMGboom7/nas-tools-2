import { useEffect,useRef,useState } from "react";
import { ApiError,createOperationId } from "../api/client";
import { executeFuzzyBatch,getFuzzyBatch,getFuzzySource,getFuzzyStatus,previewFuzzyBatch,restoreFuzzyRule,resumeFuzzyBatch,reviewFuzzyContinuation,saveFuzzyBatch,setFuzzyGrant,stopFuzzyBatch,verifyFuzzyItem,type FuzzyBatch,type FuzzyCandidate,type FuzzyConsent,type FuzzyContinuation,type FuzzyKind,type FuzzyReview,type FuzzySource,type FuzzyStatus } from "../api/fuzzySubscriptions";

const blank=():FuzzyConsent=>({confirm:false,confirmWhole:false,confirmOldExecutorsStopped:false});
export function FuzzyConsentFields({value,onChange,disabled,future=false}:{value:FuzzyConsent;onChange:(v:FuzzyConsent)=>void;disabled:boolean;future?:boolean}){
  return <fieldset disabled={disabled}><legend>本次独立确认（不会保存为下次默认值）</legend>
    <label><input type="checkbox" checked={value.confirm} onChange={e=>onChange({...value,confirm:e.target.checked})}/>{future?"同意本规则扫描当前及后续 RSS，可能匹配多个不同媒体，不检查库存":"已核对完整批次、原规则及全部目标，只授权本次明确的批次操作"}</label>
    <label><input type="checkbox" checked={value.confirmWhole} onChange={e=>onChange({...value,confirmWhole:e.target.checked})}/>独立同意下载整个匹配种子及流量／磁盘开销，不按库存或缺失单集筛选；已接收不等于下载完成</label>
    <label><input type="checkbox" checked={value.confirmOldExecutorsStopped} onChange={e=>onChange({...value,confirmOldExecutorsStopped:e.target.checked})}/>旧 Python／重复下载执行器已实际停止，勾选不会代为停止它们</label>
  </fieldset>;
}
const ready=(v:FuzzyConsent)=>v.confirm&&v.confirmWhole&&v.confirmOldExecutorsStopped;
function Candidates({items}:{items:FuzzyCandidate[]}){
  return <details><summary>全部最终候选（{items.length} 项，不是样例）</summary><ol>{items.map((v,n)=><li key={n}>{v.title} · {v.name} {v.year} · {v.kind} {v.season} {v.episodes&&"集："+v.episodes} · {v.site} · 优先级 {v.priority}／组最高 {v.topOrder} · 组 {v.groupId} · 整个种子</li>)}</ol></details>;
}
const stateName:Record<FuzzyBatch["state"],string>={pending:"待后台领取",running:"执行中（中断后须审核）",completed:"本批处理已结束",blocked:"已拦截",uncertain:"待核实，禁止重发",stopped:"已停止"};
export function FuzzySubscriptionManager({token,selected,onClose,onSessionExpired,onChanged}:{token:string;selected:{type:FuzzyKind;id:string;name:string}|null;onClose:()=>void;onSessionExpired:()=>void;onChanged:()=>void}){
  const [status,setStatus]=useState<FuzzyStatus|null>(null),[source,setSource]=useState<FuzzySource|null>(null),[review,setReview]=useState<FuzzyReview|null>(null),[batch,setBatch]=useState<FuzzyBatch|null>(null),[continuation,setContinuation]=useState<FuzzyContinuation|null>(null);
  const [consent,setConsent]=useState(blank),[futureConsent,setFutureConsent]=useState(blank),[read,setRead]=useState(false),[pause,setPause]=useState(false),[stop,setStop]=useState(false),[verify,setVerify]=useState(false),[restore,setRestore]=useState(false);
  const [lookup,setLookup]=useState(""),[restoreID,setRestoreID]=useState(""),[notice,setNotice]=useState(""),[error,setError]=useState(""),[busy,setBusy]=useState(false);
  const epoch=useRef(0),inFlight=useRef(false),restoreTarget=useRef<{batch:string;id:string}|null>(null);
  function clearConfirmation(){setConsent(blank());setFutureConsent(blank());setPause(false);setStop(false);setVerify(false);setRestore(false)}
  function invalidate(){setReview(null);setBatch(null);setContinuation(null);clearConfirmation()}
  useEffect(()=>{epoch.current++;inFlight.current=false;setBusy(false);setSource(null);setRead(false);invalidate();setNotice("");setError("")},[selected]);
  useEffect(()=>{epoch.current++;inFlight.current=false;restoreTarget.current=null;setBusy(false);setStatus(null);setSource(null);setLookup("");setRestoreID("");setRead(false);invalidate();setNotice("");setError("")},[token]);
  async function operate(action:()=>Promise<void>,mutation=false){
    if(inFlight.current)return;const version=++epoch.current;inFlight.current=true;setBusy(true);setError("");setNotice("");
    try{await action()}catch(e){if(version!==epoch.current)return;invalidate();setSource(null);if(e instanceof ApiError&&e.code===401)onSessionExpired();else setError((e instanceof Error?e.message:"模糊订阅结果未核实")+(mutation?"。请保存稳定编号，只读查回，不要重发。":""))}
    finally{if(version===epoch.current){inFlight.current=false;setBusy(false);clearConfirmation()}}
  }
  async function loadStatus(){const version=epoch.current;const v=await getFuzzyStatus(token);if(version===epoch.current){setStatus(v);invalidate()}}
  async function loadSource(){if(!selected)return;const version=epoch.current;invalidate();const v=await getFuzzySource(token,selected.type,selected.id);if(version===epoch.current)setSource(v)}
  async function grant(enable:boolean){if(!source)return;const version=epoch.current;const prior=source.review;setSource(null);invalidate();await setFuzzyGrant(token,prior,enable,enable?futureConsent:{...blank(),confirm:pause});if(version===epoch.current){setNotice(enable?"本规则已明确授权；旧批次不会由新授权接管。":"本规则已暂停；已经进入下载器的请求不能保证撤回。");await loadSource()}}
  async function inspect(){if(!selected)return;const version=epoch.current;invalidate();const v=await previewFuzzyBatch(token,selected.type,selected.id,read);if(version===epoch.current)setReview(v)}
  async function save(){if(!review)return;const version=epoch.current;setLookup(review.id);const v=await saveFuzzyBatch(token,review,consent);if(version===epoch.current){invalidate();setBatch(v);setNotice("批次已保存，下载器接收情况以逐项回执为准。")}}
  async function find(id=lookup){const version=epoch.current;invalidate();const v=await getFuzzyBatch(token,id);if(version===epoch.current){setLookup(v.id);setBatch(v);if(v.state==="completed"||v.state==="stopped"){setSource(null);onChanged()}}}
  async function stopBatch(){if(!batch)return;const version=epoch.current;const v=await stopFuzzyBatch(token,batch,stop);if(version===epoch.current){invalidate();setBatch(v);setSource(null);onChanged()}}
  async function inspectContinuation(){if(!batch)return;const version=epoch.current;setContinuation(null);const v=await reviewFuzzyContinuation(token,batch,read);if(version===epoch.current){setBatch(v.batch);setContinuation(v)}}
  async function continueBatch(){if(!continuation)return;const version=epoch.current;const v=await resumeFuzzyBatch(token,continuation,consent);if(version===epoch.current){invalidate();setBatch(v)}}
  async function execute(){if(!batch)return;const version=epoch.current;const v=await executeFuzzyBatch(token,batch,consent);if(version===epoch.current){invalidate();setBatch(v);onChanged()}}
  async function verifyItem(index:number){if(!batch)return;const version=epoch.current;const v=await verifyFuzzyItem(token,batch,index,read,verify);if(version===epoch.current){invalidate();setBatch(v);setNotice("仅采用了原提交的正向证据，没有重新提交，也没有自动续办。")}}
  async function restoreRule(){if(!batch)return;const version=epoch.current;const requestId=restoreTarget.current?.batch===batch.id?restoreTarget.current.id:createOperationId();restoreTarget.current={batch:batch.id,id:requestId};setRestoreID(requestId);const v=await restoreFuzzyRule(token,batch,requestId,restore);if(version===epoch.current){clearConfirmation();setSource(null);setNotice("已恢复规则副本 "+v.id+"；旧历史及批次保留，执行授权保持关闭。");onChanged()}}
  const worker=source?.review.workerRunning??status?.workerRunning;
  return <section className="fuzzy-subscription-panel" aria-label="模糊 RSS 订阅与持久批次">
    <h2>模糊 RSS 订阅与批次回执</h2>
    <p>仅管理员可授权执行。模糊规则通过关键词／正则、年份和季号匹配 RSS，不是精确媒体搜索，不查库存，不按缺集挑文件。一条规则可命中多个媒体，所有候选按原优先级和站点／做种规则择优。</p>
    <p>普通模糊规则在本批完整提交后收尾，不是永久关键词监控；洗版组合按优先级保留或结束，没有有效优先级或仍为明确单集时不会冒充整季洗版完成。未知义务仍受 PT 保护，不会借此解除 HR。</p>
    <button type="button" disabled={busy} onClick={()=>void operate(loadStatus)}>只读刷新最近 30 条批次</button>
    <label>已知批次稳定编号（保存前保留，支持查找较早回执）<input disabled={busy} value={lookup} onChange={e=>{setLookup(e.target.value);invalidate()}}/></label>
    <button type="button" disabled={busy||!/^[a-f0-9]{64}$/.test(lookup)} onClick={()=>void operate(()=>find())}>按稳定编号只读查回</button>
    {selected&&<section aria-label="当前模糊规则"><h3>{selected.name} · {selected.type}/{selected.id}</h3><button type="button" disabled={busy} onClick={onClose}>关闭规则审核（保留回执编号）</button>
      <button type="button" disabled={busy} onClick={()=>void operate(loadSource)}>只读审核本规则授权范围</button>
      {source&&<><p>规则：{source.review.name} {source.review.year} {source.review.season} · 当前授权：{source.review.active?"有效":"未启用或已失效"} · 后台：{source.review.workerRunning?"运行":"未启动"}</p>
        <details><summary>当前完整站点及下载器范围（{source.review.targets.length} 项）</summary><ul>{source.review.targets.map((v,i)=><li key={i}>{v.site} → {v.downloader}（{v.type} / {v.downloaderId}）· {v.directory||"下载器默认目录（远端修改默认值也会影响目的地）"} · {v.paused?"暂停添加":"按原设置添加"}</li>)}</ul></details>
        <FuzzyConsentFields value={futureConsent} onChange={setFutureConsent} disabled={busy} future/>
        <button type="button" disabled={busy||!ready(futureConsent)} onClick={()=>void operate(()=>grant(true),true)}>明确授权此规则的当前及后续 RSS 执行</button>
        <label><input type="checkbox" checked={pause} disabled={busy} onChange={e=>setPause(e.target.checked)}/>明确暂停此规则的未来执行；旧批次及提交事实保留</label>
        <button type="button" disabled={busy||!pause} onClick={()=>void operate(()=>grant(false),true)}>暂停本规则授权</button>
        <label><input type="checkbox" checked={read} disabled={busy} onChange={e=>{setRead(e.target.checked);setReview(null);clearConfirmation()}}/>同意只读访问上述站点并审核全部 RSS 目标；这不授权添加下载</label>
        <button type="button" disabled={busy||!read||!source.review.active} onClick={()=>void operate(inspect)}>只读审核完整模糊 RSS 候选</button>
        {source.jobs.length>0&&<details><summary>本规则最近批次</summary><ul>{source.jobs.map(v=><li key={v.id}>{v.id} · {stateName[v.state]} <button type="button" disabled={busy} onClick={()=>void operate(()=>find(v.id))}>只读查看</button></li>)}</ul></details>}
      </>}
    </section>}
    {review&&<section aria-label="完整模糊下载审核"><p>尚未保存。稳定编号：{review.id} · 规则：{review.name} · {review.overEdition?"洗版组合":"普通模糊"} · {review.candidates.length} 个目标</p><Candidates items={review.candidates}/><FuzzyConsentFields value={consent} onChange={setConsent} disabled={busy}/><button type="button" disabled={busy||!ready(consent)} onClick={()=>void operate(save,true)}>明确保存完整批次（单次，不重发）</button></section>}
    {batch&&<section aria-label="已保存模糊批次回执"><h3>{batch.name}</h3><p>稳定编号：{batch.id}</p><p>状态：{stateName[batch.state]} · 原规则：{batch.kind}/{batch.subscriptionId} · 原操作者：{batch.actor} · 当前批次授权者：{batch.authorizedBy}</p><p>{batch.reason}</p><p>{batch.sourceResult==="finished"?"原订阅计划已收尾，历史与完整原行已保留，不代表媒体下载或入库完成":batch.sourceResult==="retained"?"原规则保留；本轮未达到订阅结束条件":"原订阅尚未收尾"}</p><Candidates items={batch.items.map(v=>v.candidate)}/>
      <ol>{batch.items.map(v=><li key={v.index}>{v.candidate.title} · {v.state} · {v.reason}{["intent","uncertain"].includes(v.state)&&<button type="button" disabled={busy||!read||!verify} onClick={()=>void operate(()=>verifyItem(v.index),true)}>只读核实此项并采用正向接收证据</button>}</li>)}</ol>
      {batch.items.some(v=>["intent","uncertain"].includes(v.state))&&<><label><input type="checkbox" disabled={busy} checked={read} onChange={e=>setRead(e.target.checked)}/>同意只读访问原下载器及准确 hash，不改用新账户</label><label><input type="checkbox" disabled={busy} checked={verify} onChange={e=>setVerify(e.target.checked)}/>仅采用原提交的正向证据；未找到不能释放、重下或补造成功</label></>}
      {["running","blocked","uncertain"].includes(batch.state)&&<><label><input type="checkbox" disabled={busy} checked={read} onChange={e=>setRead(e.target.checked)}/>同意只读审核原完整批次、当前授权和剩余未提交项</label><button type="button" disabled={busy||!read} onClick={()=>void operate(inspectContinuation)}>只读审核原批次后续续办</button></>}
      {continuation&&<section><p>当前审核允许续办：{continuation.canResume?"是，只包含已证明未提交的剩余目标":"否，尚有未知意图须先核实"}</p><FuzzyConsentFields value={consent} onChange={setConsent} disabled={busy}/><button type="button" disabled={busy||!continuation.canResume||!ready(consent)} onClick={()=>void operate(continueBatch,true)}>明确继续剩余未提交目标</button></section>}
      {batch.state==="pending"&&worker===false&&<><p>当前实例没有后台执行器，保存不会自动启动服务。</p><FuzzyConsentFields value={consent} onChange={setConsent} disabled={busy}/><button type="button" disabled={busy||!ready(consent)} onClick={()=>void operate(execute,true)}>明确执行此待办批次</button></>}
      {!["completed","stopped"].includes(batch.state)&&<><label><input type="checkbox" disabled={busy} checked={stop} onChange={e=>setStop(e.target.checked)}/>停止本批后续请求并暂停匹配的原规则；不撤回已接收任务、不删除下载器数据</label><button type="button" disabled={busy||!stop} onClick={()=>void operate(stopBatch,true)}>停止本批并暂停原规则</button></>}
      {batch.state==="completed"&&batch.sourceResult==="finished"&&<><label><input type="checkbox" disabled={busy} checked={restore} onChange={e=>setRestore(e.target.checked)}/>恢复完整原规则的一个新副本，旧历史／批次保留，执行授权保持关闭</label><button type="button" disabled={busy||!restore} onClick={()=>void operate(restoreRule,true)}>按原配置恢复模糊规则副本</button>{restoreID&&<p>规则恢复请求编号：{restoreID}（响应丢失保留，不重发）</p>}</>}
    </section>}
    {status&&<details><summary>最近 30 条全局批次</summary><ul>{status.jobs.map(v=><li key={v.id}>{v.id} · {v.name} · {stateName[v.state]} <button type="button" disabled={busy} onClick={()=>void operate(()=>find(v.id))}>只读查看</button></li>)}</ul></details>}
    {notice&&<p role="status">{notice}</p>}{error&&<p role="alert" className="form-error">{error}</p>}
    <p>切换规则或页面不会撤回已经发出的请求。网络错误、停止、进程中断及回执丢失后不自动重发；新授权不接管旧批次，未确认的旧配置不自动启动。</p>
  </section>;
}
