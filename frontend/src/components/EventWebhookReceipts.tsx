import {useEffect,useRef,useState} from "react";
import {ApiError} from "../api/client";
import {dismissEventWebhook,getEventWebhook,listEventWebhooks,reviewEventWebhooks,type EventWebhookReceipt,type EventWebhookStatus} from "../api/eventWebhooks";
const states:Record<string,string>={pending:"等待外发",intent:"发送意图已保存，可能已送达",sent:"接收端已确认接收",uncertain:"响应不确定，不自动重发",blocked:"原配置或授权变化，已阻止",dismissed:"已明确忽略"};
export function EventWebhookReceipts({token,onSessionExpired}:{token:string;onSessionExpired:()=>void}){
 const [review,setReview]=useState<EventWebhookStatus|null>(null),[rows,setRows]=useState<EventWebhookReceipt[]>([]),[receipt,setReceipt]=useState<EventWebhookReceipt|null>(null);
 const [lookup,setLookup]=useState(""),[page,setPage]=useState("1"),[busy,setBusy]=useState(false),[notice,setNotice]=useState(""),[confirm,setConfirm]=useState(false),[reason,setReason]=useState("");
 const generation=useRef(0),inFlight=useRef(false);
 useEffect(()=>{generation.current++;inFlight.current=false;setReview(null);setRows([]);setReceipt(null);setLookup("");setBusy(false);setNotice("");setConfirm(false);setReason("");return()=>{generation.current++}},[token]);
 async function operate(action:"review"|"list"|"lookup"|"dismiss"){
  if(inFlight.current||action==="dismiss"&&(!receipt||!confirm||!reason.trim()))return;
  const version=generation.current;inFlight.current=true;setBusy(true);setNotice("");setConfirm(false);
  try{
   if(action==="review"){setReview(null);const v=await reviewEventWebhooks(token);if(version===generation.current)setReview(v)}
   else if(action==="list"){const v=await listEventWebhooks(token,Number(page));if(version===generation.current)setRows(v)}
   else if(action==="lookup"){setReceipt(null);const v=await getEventWebhook(token,lookup.trim());if(version===generation.current){setReceipt(v);setReason("")}}
   else{
    const v=await dismissEventWebhook(token,receipt!,true,reason);
    if(version===generation.current){setReceipt(v);setRows(current=>current.map(p=>p.id===v.id?v:p));setReason("");setNotice("已记录忽略原因；原事件与发送事实仍保留。")}
   }
  }catch(err){if(version===generation.current){if(err instanceof ApiError&&err.code===401)onSessionExpired();else setNotice(err instanceof Error?err.message:"请只读查回原外发回执")}}
  finally{if(version===generation.current){inFlight.current=false;setBusy(false)}}
 }
 const choose=(p:EventWebhookReceipt)=>{setLookup(p.id);setReceipt(p);setConfirm(false);setReason("")};
 return <section className="notification-receipt-panel" aria-label="Webhook 事件外发回执"><h2>Webhook 事件外发回执</h2>
  <p>在 Webhook 插件配置中单独启用未来新事件的外发。原地址不会自动启动发送；新授权不补发历史，响应不明、重启和刷新均不会重发。</p>
  <fieldset disabled={busy}><button type="button" onClick={()=>void operate("review")}>只读核对外发状态</button>
   {review&&<p>{review.active?"已授权接收未来新原生事件":"未启用或原授权失效"} · 原接收站点：{review.origin||"未显示，请从插件配置核对已保存内容"} · 方式：{review.method||"未显示"}{review.method==="get"&&(" / "+(review.encoding==="json"?"完整 JSON":"旧字典键展开"))} · {review.workerRunning?"后台已运行":"后台尚未运行"}。此处不回显完整地址、查询参数、凭据和原始事件内容，停用仍保留原配置。</p>}
   <label>外发回执页码<input type="number" min="1" max="10000" value={page} onChange={e=>setPage(e.target.value)}/></label><button type="button" onClick={()=>void operate("list")}>只读查看本页 30 条外发</button>
   <label>原外发稳定编号<input maxLength={64} value={lookup} onChange={e=>{setLookup(e.target.value);setReceipt(null);setConfirm(false);setReason("")}}/></label><button type="button" disabled={!lookup.trim()} onClick={()=>void operate("lookup")}>按原外发编号只读查回</button>
   {receipt&&<article aria-label="原事件外发回执"><h3>{receipt.type} · {states[receipt.state]}</h3><p>稳定编号：{receipt.id}</p><p>原事件：{receipt.eventId} · 业务引用：{receipt.reference}</p><p>接收状态码：{receipt.status||"尚无确认"} · {receipt.reason} · {receipt.updated}</p>
    {receipt.state==="dismissed"&&<p>忽略人：{receipt.actor}</p>}
    {!["sent","dismissed"].includes(receipt.state)&&<div><p>忽略只关闭这一条待核对事项，不能撤回接收端已执行的操作，也不会重发或删除原事件。</p><label>忽略原因<input maxLength={1000} value={reason} onChange={e=>{setReason(e.target.value);setConfirm(false)}}/></label><label><input type="checkbox" checked={confirm} onChange={e=>setConfirm(e.target.checked)}/>确认仅忽略此原外发回执，保留已发生的事实</label><button type="button" disabled={!confirm||!reason.trim()} onClick={()=>void operate("dismiss")}>仅忽略此原回执</button></div>}
   </article>}
   {rows.map(p=><button type="button" key={p.id} onClick={()=>choose(p)}>{p.type} · {states[p.state]} · {p.id}</button>)}
  </fieldset>{notice&&<p role="status">{notice}</p>}
 </section>;
}
