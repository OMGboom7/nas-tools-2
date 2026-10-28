import {useEffect,useRef,useState} from "react";
import {ApiError} from "../api/client";
import {changeMediaWebhooks,getMediaWebhookReceipt,getMediaWebhookReceipts,reviewMediaWebhooks,type MediaWebhookStatus,type MediaWebhookReceipt} from "../api/mediaWebhooks";

const actions:Record<string,string>={"playback.start":"开始播放","playback.stop":"停止播放","library.new":"新入库","system.webhooktest":"测试","user.authenticated":"登录成功","user.authenticationfailed":"登录失败","item.rate":"标记"};
const states:Record<string,string>={pending:"待发送",sending:"发送意图已保存，可能中断",sent:"渠道已确认接收",uncertain:"响应不确定，不自动重发",blocked:"已阻止，需核对原配置",dismissed:"已明确忽略"};
export function MediaWebhookManager({token,onSessionExpired}:{token:string;onSessionExpired:()=>void}){
 const [review,setReview]=useState<MediaWebhookStatus|null>(null),[rows,setRows]=useState<MediaWebhookReceipt[]>([]),[receipt,setReceipt]=useState<MediaWebhookReceipt|null>(null);
 const [lookup,setLookup]=useState(""),[page,setPage]=useState("1"),[busy,setBusy]=useState(false),[notice,setNotice]=useState("");
 const [receiver,setReceiver]=useState(false),[notifications,setNotifications]=useState(false),[legacy,setLegacy]=useState(false);
 const epoch=useRef(0),inFlight=useRef(false);
 const clear=()=>{setReceiver(false);setNotifications(false);setLegacy(false)};
 useEffect(()=>{epoch.current++;inFlight.current=false;setReview(null);setRows([]);setReceipt(null);setLookup("");setPage("1");setBusy(false);setNotice("");clear();return()=>{epoch.current++}},[token]);
 async function operate(action:"review"|"enable"|"stop"|"list"|"lookup"){
  if(inFlight.current)return;
  if((action==="enable"||action==="stop")&&(!review||!receiver||action==="enable"&&(!notifications||!legacy||!review.ready)))return;
  const current=++epoch.current;inFlight.current=true;setBusy(true);setNotice("");const original=review;clear();
  try{
   if(action==="review"){setReview(null);const v=await reviewMediaWebhooks(token);if(current===epoch.current)setReview(v)}
   else if(action==="enable"||action==="stop"){
    setReview(null);const v=await changeMediaWebhooks(token,original!,action==="enable",{receiver:true,notifications:action==="enable",legacyStopped:action==="enable"});
    if(current===epoch.current){setReview(v);setNotice(action==="enable"?"已启用接收；后续回调按此原设置处理。":"已停止接收新回调；原事件和通知回执保留。")}
   }else if(action==="list"){const v=await getMediaWebhookReceipts(token,Number(page));if(current===epoch.current)setRows(v)}
   else{setReceipt(null);const v=await getMediaWebhookReceipt(token,lookup.trim());if(current===epoch.current){setReceipt(v);setLookup(v.id)}}
  }catch(err){
   if(current!==epoch.current)return;
   if(err instanceof ApiError&&err.code===401)onSessionExpired();else setNotice(err instanceof Error?err.message:"请只读核对原设置或原回执");
   // A lost control response permits a read of current authority, never a write.
   if(action==="enable"||action==="stop"){try{const v=await reviewMediaWebhooks(token);if(current===epoch.current)setReview(v)}catch{/* keep the original uncertainty */}}
  }finally{if(current===epoch.current){inFlight.current=false;setBusy(false)}}
 }
 return <section className="notification-receipt-panel" aria-label="媒体服务器回调接收"><h2>媒体服务器回调接收</h2>
  <p>接收 Emby、Jellyfin、Plex 的播放、停止和媒体新增通知，按原渠道的“媒体服务”开关发送。原配置不会自动启用新接收器；此处不删除文件、不控制下载器，也不启动其他旧插件。</p>
  <button type="button" disabled={busy} onClick={()=>void operate("review")}>只读审核接收设置</button>
  {review&&<div><p>原服务：{review.server||"未配置"} · 地址路径：{review.endpoint||"未配置"} · {review.enabled?(review.current?"已启用且原配置有效":"原授权已失效"):"未启用"}</p>
   <p>原来源范围：IPv4 {review.ipv4||"未限定"}；IPv6 {review.ipv6||"未限定"}。只核对实际连接地址；代理转发头不作为来源证明。</p>
   <p>{review.apiKeyRequired?"沿用原 API 密钥校验；在媒体服务器回调地址添加原 apikey 参数，或使用原 Authorization 头。":"原配置未要求 API 密钥；启用后符合原来源范围的请求可以提交通知。"} 此页不显示密钥。</p>
   {review.reason&&<p role="status">{review.reason}</p>}
   <p>停止只阻止新接收，已提交消息仍按原渠道队列处理；要阻止尚未发送的消息，请核对通知渠道开关。丢失设置回执后只读查回，不重复授权。</p>
   <fieldset disabled={busy}><label><input type="checkbox" checked={receiver} onChange={e=>setReceiver(e.target.checked)}/>确认按以上原服务、来源范围和校验规则改变回调接收</label>
    <label><input type="checkbox" checked={notifications} onChange={e=>setNotifications(e.target.checked)}/>允许按原媒体服务通知开关发送事件中的用户、设备和播放信息</label>
    <label><input type="checkbox" checked={legacy} onChange={e=>setLegacy(e.target.checked)}/>已停止旧 Python 回调消费，避免两套运行时重复通知</label>
    <button type="button" disabled={!receiver||!notifications||!legacy||!review.ready} onClick={()=>void operate("enable")}>启用此原服务接收</button>
    <button type="button" disabled={!receiver||!review.revision} onClick={()=>void operate("stop")}>仅停止新回调接收</button>
   </fieldset><small>原设置版本：{review.revision||"尚无"} · 操作人：{review.actor||"尚无"} · {review.changed}</small>
  </div>}
  {notice&&<p role="status">{notice}</p>}
  <p>同一接收授权下，发送方提供事件唯一编号时，重复回调只查回同一接收；未提供编号时不猜测去重，以免丢弃再次播放。原通知发送意图、未知应答和重启不会自动重发。</p>
  <label>回调回执页码<input type="number" min="1" max="10000" value={page} onChange={e=>setPage(e.target.value)}/></label>
  <button type="button" disabled={busy} onClick={()=>void operate("list")}>只读查看本页 30 条回调</button>
  <label>原回调稳定编号<input value={lookup} maxLength={32} onChange={e=>{setLookup(e.target.value);setReceipt(null)}}/></label>
  <button type="button" disabled={busy||!lookup.trim()} onClick={()=>void operate("lookup")}>按原回调编号只读查回</button>
  {receipt&&<MediaWebhookReceiptCard value={receipt}/>}
  {rows.map(v=><button type="button" key={v.id} disabled={busy} onClick={()=>{setLookup(v.id);setReceipt(v)}}>{v.event.server} · {actions[v.event.action]||v.event.action} · {v.event.name} · {v.id}</button>)}
 </section>;
}
function MediaWebhookReceiptCard({value:v}:{value:MediaWebhookReceipt}){
 return <article aria-label="原媒体回调接收回执"><h3>{actions[v.event.action]||v.event.action} · {v.event.name}</h3><p>稳定编号：{v.id} · 原服务：{v.event.server} · 已接收并登记 · {v.received}</p>
  <p>用户：{v.event.user||"未提供"} · 设备：{v.event.device||v.event.client||"未提供"}{v.event.percentage!==undefined&&(" · 播放进度 "+v.event.percentage.toFixed(2)+"%")}</p><p>{v.event.overview}</p>
  <p>业务事件：{v.eventId}。已接收不代表渠道已送达、媒体已入库或播放已完成。</p>
  {v.deliveries.length===0?<p>仅站内记录；接收时没有匹配的启用渠道。</p>:<ul>{v.deliveries.map(d=><li key={d.id}>{d.channelName} · {states[d.state]||d.state} · {d.reason}</li>)}</ul>}
 </article>;
}
