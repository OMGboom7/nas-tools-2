import {useEffect,useRef,useState} from "react";
import {ApiError} from "../api/client";
import {getOpenSubtitle,getOpenSubtitleStatus,listOpenSubtitles,stopOpenSubtitle,type OpenSubtitleReceipt,type OpenSubtitleStatus} from "../api/openSubtitles";
export const openSubtitleStates:Record<OpenSubtitleReceipt["state"],string>={
 not_queued:"只读审核，未提交",pending:"原请求待处理，尚未开始",running:"处理中或已中断，不会自动重跑",completed:"原字幕已新增或保留，详见逐文件回执",
 not_found:"未找到符合原条件的中文字幕",partial:"部分已处理，剩余步骤不重放",blocked:"原范围变化或服务拒绝，原事实保留",uncertain:"应答或发布结果不明，不会重发",stopped:"原尚未开始请求已明确停止",
};
export function OpenSubtitleFact({receipt:p}:{receipt:OpenSubtitleReceipt}){
 return <article aria-label="OpenSubtitles 原回执"><h3>OpenSubtitles · {p.name}</h3>
  <p>原编号：{p.id}；提供方 {p.origin}（{p.provider==="org"?"原网页":"官方 REST"}）。{openSubtitleStates[p.state]}</p>
  <p>已保存 {p.requests} 次请求意图，包括登录／检索／下载；不代表新增字幕数。{p.updated&&`回执更新时间 ${p.updated}`}</p>
  {p.reason&&<p>{p.reason}</p>}
  {p.libraryId&&<p>文件发布凭证编号：{p.libraryId}；原恢复副本与未知占用保留，按原凭证另外核对，不自动清理。</p>}
  {p.outputs.length>0&&<ul>{p.outputs.map(o=><li key={o.id}>{o.target} · {o.state==="created"?"已新增":o.state==="preserved"?"已有文件保留":o.state==="writing"?"写入意图已保存，须核对":o.state==="uncertain"?"结果不明，须核对":"未发布"} · {o.reason}</li>)}</ul>}
 </article>;
}
export function OpenSubtitleReceipts({token,onSessionExpired,enabled=true}:{token:string;onSessionExpired:()=>void;enabled?:boolean}){
 const [status,setStatus]=useState<OpenSubtitleStatus|null>(null),[rows,setRows]=useState<OpenSubtitleReceipt[]>([]),[receipt,setReceipt]=useState<OpenSubtitleReceipt|null>(null);
 const [page,setPage]=useState("1"),[id,setID]=useState(""),[reason,setReason]=useState(""),[confirm,setConfirm]=useState(false),[busy,setBusy]=useState(false),[notice,setNotice]=useState("");
 const generation=useRef(0),active=useRef(false);
 useEffect(()=>{generation.current++;active.current=false;setStatus(null);setRows([]);setReceipt(null);setPage("1");setID("");setReason("");setConfirm(false);setBusy(false);setNotice("");return()=>{generation.current++}},[token]);
 async function run(action:"status"|"list"|"get"|"stop"){
  if(active.current)return;active.current=true;const version=++generation.current,p=receipt,ok=confirm,why=reason;setBusy(true);setNotice("");setConfirm(false);
  try{
   if(action==="status"){const s=await getOpenSubtitleStatus(token);if(version===generation.current)setStatus(s)}
   else if(action==="list"){const values=await listOpenSubtitles(token,Number(page));if(version===generation.current)setRows(values)}
   else{let out:OpenSubtitleReceipt;if(action==="get"){out=await getOpenSubtitle(token,id.trim())}else{if(!p)throw Error("先只读查回原请求");out=await stopOpenSubtitle(token,p,ok,why)}
    if(version===generation.current){setReceipt(out);setID(out.id);setRows(rows=>rows.map(v=>v.id===out.id?out:v))}
   }
  }catch(e){
   if(version!==generation.current)return;if(e instanceof ApiError&&e.code===401)onSessionExpired();else setNotice(e instanceof Error?e.message:"请按原编号只读查回");
   if(action==="stop"&&p){try{const original=await getOpenSubtitle(token,p.id);if(version===generation.current)setReceipt(original)}catch{/* Never replay a mutation after an unknown response. */}}
  }finally{if(version===generation.current){setBusy(false);active.current=false}}
 }
 return <section className="notification-receipt-panel" aria-label="OpenSubtitles 回执管理"><h2>OpenSubtitles 原请求与文件回执</h2>
  <p>在插件管理中配置原提供方并独立授权；此处只读查看较早请求，或明确停止尚未开始的一项。未知应答、处理中的中断及进程重启均不重发；已有文件和恢复副本保留。</p>
  <fieldset disabled={busy||!enabled}>
   <button type="button" onClick={()=>void run("status")}>只读核对 OpenSubtitles 授权</button>
   {status&&<p>{status.ready?`原提供方 ${status.origin}；未来原入库远程写入${status.remote?"已单独授权":"未授权"}`:status.wanted?"原开关已启用，但当前原生授权／服务尚未就绪；到插件管理核对":"原 OpenSubtitles 消费者未启用"}</p>}
   <label>OpenSubtitles 回执页码<input type="number" min="1" max="10000" value={page} onChange={e=>setPage(e.target.value)}/></label>
   <button type="button" onClick={()=>void run("list")}>只读查看本页 30 条 OpenSubtitles 回执</button>
   <label>完整原 OpenSubtitles 编号<input value={id} maxLength={64} onChange={e=>{setID(e.target.value);setReceipt(null);setConfirm(false)}}/></label>
   <button type="button" disabled={!id.trim()} onClick={()=>void run("get")}>按原 OpenSubtitles 编号只读查回</button>
   {rows.map(p=><button key={p.id} type="button" onClick={()=>{setReceipt(p);setID(p.id);setReason("");setConfirm(false)}}>{p.name} · {openSubtitleStates[p.state]} · {p.id}</button>)}
   {receipt&&<><OpenSubtitleFact receipt={receipt}/>{receipt.state==="pending"&&receipt.requests===0&&!receipt.libraryId&&<>
    <label>停止这份 OpenSubtitles 请求的原因<input value={reason} maxLength={300} onChange={e=>{setReason(e.target.value);setConfirm(false)}}/></label>
    <label><input type="checkbox" checked={confirm} onChange={e=>setConfirm(e.target.checked)}/>确认仅停止这份尚未开始的 OpenSubtitles 项；ChineseSubFinder、独立事件外发和已有文件保留</label>
    <button type="button" disabled={!confirm||!reason.trim()} onClick={()=>void run("stop")}>停止这份尚未开始的 OpenSubtitles 请求</button>
   </>}</>}
  </fieldset>{notice&&<p role="status">{notice}</p>}
 </section>;
}
