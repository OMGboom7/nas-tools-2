import {useEffect,useRef,useState} from "react";
import {ApiError} from "../api/client";
import {abandonEditionCycle,getEditionReceipt,getEditionReceipts,getEditionScope,type EditionReceipt,type EditionScope} from "../api/subscriptionEditions";

export function SubscriptionEditionManager({token,selected,lastCycleId,onClose,onChanged,onSessionExpired}:{token:string;selected:{type:"MOV"|"TV";id:string;name:string}|null;lastCycleId:string;onClose:()=>void;onChanged:()=>void;onSessionExpired:()=>void}){
  const [scope,setScope]=useState<EditionScope|null>(null),[receipt,setReceipt]=useState<EditionReceipt|null>(null),[recent,setRecent]=useState<EditionReceipt[]>([]);
  const [lookup,setLookup]=useState(""),[reason,setReason]=useState(""),[confirm,setConfirm]=useState(false),[busy,setBusy]=useState(false),[error,setError]=useState(""),[notice,setNotice]=useState("");
  const epoch=useRef(0),inFlight=useRef(false);
  function clear(){setScope(null);setReceipt(null);setConfirm(false);setReason("")}
  useEffect(()=>{epoch.current++;inFlight.current=false;setBusy(false);clear();setError("");setNotice("")},[selected]);
  useEffect(()=>{epoch.current++;inFlight.current=false;setBusy(false);clear();setRecent([]);setLookup("");setError("");setNotice("")},[token]);
  useEffect(()=>{if(/^[a-f0-9]{64}$/.test(lastCycleId))setLookup(lastCycleId)},[lastCycleId]);
  async function run(action:()=>Promise<void>,mutation=false){if(inFlight.current)return;const version=++epoch.current;inFlight.current=true;setBusy(true);setError("");setNotice("");try{await action()}catch(e){if(version!==epoch.current)return;clear();if(e instanceof ApiError&&e.code===401)onSessionExpired();else setError((e instanceof Error?e.message:"洗版回执未核实")+(mutation?"。请保留稳定轮次编号，只读查回，不要重发。":""))}finally{if(version===epoch.current){inFlight.current=false;setBusy(false);setConfirm(false)}}}
  async function inspect(){if(!selected)return;const version=epoch.current;clear();const v=await getEditionScope(token,selected.type,selected.id);if(version===epoch.current)setScope(v)}
  async function find(id=lookup){const version=epoch.current;setReceipt(null);setConfirm(false);const v=await getEditionReceipt(token,id);if(version===epoch.current){setLookup(v.id);setReceipt(v)}}
  async function list(){const version=epoch.current;const v=await getEditionReceipts(token);if(version===epoch.current)setRecent(v)}
  async function abandon(){if(!scope)return;const version=epoch.current;setLookup(scope.upgrade.cycleId);const saved=scope;setScope(null);setConfirm(false);await abandonEditionCycle(token,saved,reason,confirm);if(version===epoch.current){setNotice("本轮已封存，原接收事实保留；没有提交、撤回或删除下载。后续现有订阅周期可重新规划新的完整质量轮次。");await find(saved.upgrade.cycleId);onChanged()}}
  return <section className="fuzzy-subscription-panel" aria-label="精确订阅洗版轮次与回执"><h2>精确订阅洗版轮次与回执</h2>
    <p>洗版不把已有库存当作完成，只接收比已记录优先级更高的资源。电视剧同一轮固定过滤组和优先级，全部目标集获得接收回执后才推进；不混用不同质量的部分集冒充完成，不修改原普通缺集记录。</p>
    <p>跨季包仍待订阅协调模块。整季包和需拆选的资源由文件选择模块核验完整清单、暂停添加并实际选择，只登记实际覆盖的目标集；不能因规划交集就下载整个包。接收不等于下载或入库完成。</p>
    <button type="button" disabled={busy} onClick={()=>void run(list)}>只读刷新最近 30 条洗版轮次</button>
    <label>稳定轮次编号（来源已收尾也可只读找回）<input disabled={busy} value={lookup} onChange={e=>{setLookup(e.target.value);setReceipt(null);setConfirm(false)}}/></label><button type="button" disabled={busy||!/^[a-f0-9]{64}$/.test(lookup)} onClick={()=>void run(()=>find())}>按轮次编号只读查回</button>
    {selected&&<section aria-label="当前精确洗版规则"><h3>{selected.name} · {selected.type}/{selected.id}</h3><button type="button" disabled={busy} onClick={onClose}>关闭当前规则（保留轮次编号）</button><button type="button" disabled={busy} onClick={()=>void run(inspect)}>只读审核当前洗版轮次</button>
      {scope&&<><p>上次完整质量优先级：{scope.upgrade.previousOrder} · 当前轮次：{scope.upgrade.cycleId||"尚无进行中的原生轮次"} · 当前配置：{scope.scopeValid?"匹配":"已变化，禁止沿用旧执行范围"}</p>
        {scope.upgrade.cycleId&&<><p>本轮优先级 {scope.upgrade.order}／组最高 {scope.upgrade.topOrder} · 过滤组 {scope.upgrade.groupId}</p><p>完整目标：{scope.upgrade.target.join(", ")||"电影单资源"}；尚待接收：{scope.upgrade.remaining.join(", ")||"无集数目标"}</p>
          <label>封存原因<input disabled={busy} value={reason} maxLength={512} onChange={e=>{setReason(e.target.value);setConfirm(false)}}/></label>
          <label><input type="checkbox" disabled={busy} checked={confirm} onChange={e=>setConfirm(e.target.checked)}/>明确封存整个未完成轮次，不清除接收事实、不撤回或删除下载；后续周期可能重新规划同质量的新完整轮次并产生额外流量。未知提交必须先核实，不能通过封存绕过。</label>
          <button type="button" disabled={busy||!confirm||!reason.trim()} onClick={()=>void run(abandon,true)}>明确封存本轮（不执行新下载）</button></>}
      </>}
    </section>}
    {receipt&&<section aria-label="原洗版轮次回执"><h3>{receipt.name} {receipt.year}</h3><p>稳定轮次：{receipt.id} · {receipt.type}/{receipt.subscriptionId} · {receipt.state==="completed"?"本轮质量接收已完整":receipt.state==="abandoned"?"已明确封存":"进行中"}</p><p>原来源：{receipt.sourceMatches?"仍匹配":"已变化或已收尾，原事实仍保留"}；优先级 {receipt.progress.order}／组最高 {receipt.progress.topOrder}；过滤组 {receipt.progress.groupId}</p><p>完整目标：{receipt.progress.target.join(", ")||"电影单资源"}；原轮剩余：{receipt.progress.remaining.join(", ")||"无"}</p>
      <details><summary>全部提交尝试（{receipt.attempts.length}）</summary><ol>{receipt.attempts.map((a,i)=><li key={i}>{a.title} · 优先级 {a.order} · 集 {a.episodes.join(", ")||"电影"} · {a.state==="accepted"?"下载器已接收":a.state==="rejected"?"已明确拒绝且未提交":"意图待核实，禁止重发"}</li>)}</ol></details>
    </section>}
    {recent.length>0&&<details><summary>最近洗版轮次</summary><ul>{recent.map(v=><li key={v.id}>{v.name} · {v.state} · {v.id} <button type="button" disabled={busy} onClick={()=>void run(()=>find(v.id))}>只读查看原轮次</button></li>)}</ul></details>}
    {notice&&<p role="status">{notice}</p>}{error&&<p role="alert">{error}</p>}
    <p>此面板不自动轮询、不自动重发。网络错误、未知回执或进程重启后先查询原提交；重新授权或编辑不能清除原共享预留。</p>
  </section>;
}
