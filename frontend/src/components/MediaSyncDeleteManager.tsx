import {useEffect,useRef,useState} from "react";
import {ApiError} from "../api/client";
import {authorizeMediaDelete,continueMediaDelete,getMediaDeleteReceipt,getMediaDeleteStatus,ignoreMediaDelete,listMediaDeleteReceipts,reviewMediaDelete,reviewMediaDeleteExecution,stopMediaDelete,type MediaDeleteConsents,type MediaDeleteExecutionReview,type MediaDeleteReceipt,type MediaDeleteReview,type MediaDeleteStatus} from "../api/mediaSyncDelete";
const empty=():MediaDeleteConsents=>({confirm:false,history:false,dedupe:false,closeRelated:false,source:false,onlyCopy:false,expanded:false,legacyStopped:false,notify:false});
const states:Record<string,string>={pending:"待核对原完整范围",planned:"完整计划已保存，尚未执行",running:"已记录执行意图，可能中断，不自动续跑",needs_review:"原范围需要独立审核，不自动重做",completed:"完整原范围已核实完成",ignored:"已明确忽略",blocked:"执行前已阻止"};
export function MediaSyncDeleteManager({token,onSessionExpired}:{token:string;onSessionExpired:()=>void}){
 const [status,setStatus]=useState<MediaDeleteStatus|null>(null),[review,setReview]=useState<MediaDeleteReview|null>(null),[items,setItems]=useState<MediaDeleteReceipt[]>([]),[receipt,setReceipt]=useState<MediaDeleteReceipt|null>(null),[execution,setExecution]=useState<MediaDeleteExecutionReview|null>(null);
 const [selected,setSelected]=useState<string[]>([]),[consent,setConsent]=useState(empty),[http,setHTTP]=useState(false),[read,setRead]=useState(false),[dispose,setDispose]=useState(false),[reason,setReason]=useState(""),[lookup,setLookup]=useState(""),[page,setPage]=useState("1"),[unitPage,setUnitPage]=useState(1),[busy,setBusy]=useState(false),[notice,setNotice]=useState("");
 const generation=useRef(0),inFlight=useRef(false);
 const clear=()=>{setConsent(empty());setHTTP(false);setRead(false);setDispose(false)};
 const proofs=()=>{setReview(null);setExecution(null);clear()};
 useEffect(()=>{generation.current++;inFlight.current=false;setStatus(null);setReview(null);setItems([]);setReceipt(null);setExecution(null);setSelected([]);setLookup("");setPage("1");setUnitPage(1);setReason("");setBusy(false);setNotice("");clear();return()=>{generation.current++}},[token]);
 async function operate(action:"status"|"review"|"enable"|"stop"|"list"|"lookup"|"ignore"|"execution"|"continue"){
  if(inFlight.current)return;
  const s=status,v=review,p=receipt,x=execution,c={...consent},readOK=read,plain=http;
  inFlight.current=true;setBusy(true);setNotice("");clear();const version=generation.current;
  try{
   if(action==="status"){setReview(null);setExecution(null);setStatus(null);const out=await getMediaDeleteStatus(token);if(version===generation.current){setStatus(out);setSelected([])}}
   else if(action==="review"){setReview(null);setExecution(null);if(!s)throw Error("先只读核对原设置");const out=await reviewMediaDelete(token,s,selected,plain,readOK);if(version===generation.current)setReview(out)}
   else if(action==="enable"){setReview(null);setExecution(null);if(!v)throw Error("先读取原连接与源目录审核");const out=await authorizeMediaDelete(token,v,c);if(version===generation.current){setStatus(out);setNotice("已授权未来原回调；这不证明已经删除任何历史或文件。")}}
   else if(action==="stop"){proofs();if(!s)throw Error("先只读核对原设置");const out=await stopMediaDelete(token,s,c.confirm);if(version===generation.current){setStatus(out);setNotice("已停止未来自动同步删除；已有删除、捕获对象和回执保留，不恢复文件。")}}
   else if(action==="list"){setExecution(null);const out=await listMediaDeleteReceipts(token,Number(page));if(version===generation.current)setItems(out)}
   else if(action==="lookup"){setExecution(null);setReview(null);setReceipt(null);const out=await getMediaDeleteReceipt(token,lookup.trim());if(version===generation.current){setReceipt(out);setReason("");setUnitPage(1)}}
   else if(action==="ignore"){setExecution(null);setReview(null);if(!p)throw Error("先查回原未结束回执");const out=await ignoreMediaDelete(token,p,reason,dispose);if(version===generation.current){setReceipt(out);setStatus(null);setItems(rows=>rows.map(r=>r.id===out.id?out:r));setNotice("原未结束项已忽略，当前后台已停止；不会恢复已经删除的文件或重新执行原操作。")}}
   else if(action==="execution"){setExecution(null);setReview(null);if(!p)throw Error("先查回原关联回执");const out=await reviewMediaDeleteExecution(token,p,readOK);if(version===generation.current){setExecution(out);setReceipt(out.receipt);setUnitPage(1)}}
   else{setExecution(null);setReview(null);if(!x)throw Error("先重新只读审核原完整范围");const out=await continueMediaDelete(token,x,c);if(version===generation.current){setReceipt(out);setItems(rows=>rows.map(r=>r.id===out.id?out:r));setNotice("原完整范围本次继续结果已核实，保留同一原编号。")}}
  }catch(err){
   if(version!==generation.current)return;
   if(err instanceof ApiError&&err.code===401)onSessionExpired();else setNotice(err instanceof Error?err.message:"请只读查回原同步删除编号");
   if(["enable","stop","ignore","continue"].includes(action)){
    try{if(p&&(action==="ignore"||action==="continue")){const out=await getMediaDeleteReceipt(token,p.id);if(version===generation.current)setReceipt(out)}else{const out=await getMediaDeleteStatus(token);if(version===generation.current)setStatus(out)}}catch{/* only read after uncertainty */}
   }
  }finally{if(version===generation.current){inFlight.current=false;setBusy(false)}}
 }
 const grantFields=(files:boolean,notify:boolean)=>(
  <div><label><input type="checkbox" checked={consent.confirm} onChange={e=>setConsent(c=>({...c,confirm:e.target.checked}))}/>确认仅按这份原完整范围启用、停止或执行本次独立继续</label>
   <label><input type="checkbox" checked={consent.history} onChange={e=>setConsent(c=>({...c,history:e.target.checked}))}/>允许删除全部原匹配历史记录，保留独立操作回执</label>
   <label><input type="checkbox" checked={consent.dedupe} onChange={e=>setConsent(c=>({...c,dedupe:e.target.checked}))}/>允许清除对应原去重／同步记录，后续相同来源可能再次被识别</label>
   <label><input type="checkbox" checked={consent.closeRelated} onChange={e=>setConsent(c=>({...c,closeRelated:e.target.checked}))}/>允许关闭原关联整理执行器的旧写入权，保留其历史与剩余占用</label>
   {files&&<><label><input type="checkbox" checked={consent.source} onChange={e=>setConsent(c=>({...c,source:e.target.checked}))}/>允许永久删除批准范围内原历史登记的源文件与关联 NFO</label>
    <label><input type="checkbox" checked={consent.onlyCopy} onChange={e=>setConsent(c=>({...c,onlyCopy:e.target.checked}))}/>理解源文件可能是唯一副本，停止后台不会恢复已经删除的内容</label>
    <label><input type="checkbox" checked={consent.expanded} onChange={e=>setConsent(c=>({...c,expanded:e.target.checked}))}/>允许按原规则清理无媒体目录及其中其他内容，已核对整个展开范围</label></>}
   <label><input type="checkbox" checked={consent.legacyStopped} onChange={e=>setConsent(c=>({...c,legacyStopped:e.target.checked}))}/>已停止旧 Python 同步删除插件及其他重复处理同一源范围的执行器</label>
   {notify&&<label><input type="checkbox" checked={consent.notify} onChange={e=>setConsent(c=>({...c,notify:e.target.checked}))}/>允许按原“自定义消息”渠道开关发送完成通知及原 TMDB 图片</label>}
  </div>);
 const allowed=(files:boolean,notify:boolean)=>consent.confirm&&consent.history&&consent.dedupe&&consent.closeRelated&&consent.legacyStopped&&(!files||consent.source&&consent.onlyCopy&&consent.expanded)&&(!notify||consent.notify);
 const choose=(p:MediaDeleteReceipt)=>{proofs();setReceipt(p);setLookup(p.id);setReason("");setUnitPage(1)};
 return <section className="notification-receipt-panel" aria-label="Emby 同步删除管理"><h2>Emby 同步删除管理</h2>
  <p>先保存插件原开关，再在这里独立授权未来删除回调。支持电影、整剧、季和单集；电影／单集还按原目标文件名筛选版本。回调路径只用于匹配和排除，不直接作为删除路径；不会再次删除 Emby 库目标，也不会借此删除下载器任务。</p>
  <fieldset disabled={busy}><button type="button" onClick={()=>void operate("status")}>只读核对同步删除设置</button>
   {status&&<div><p>{status.installed?"插件已安装":"插件未安装"} · {status.active?"未来同步删除已授权":"自动同步删除未启用或原授权已失效"} · {status.workerRunning?"后台已运行":"后台尚未运行"}。{status.reason}</p>
    <p>原开关：{status.spec.enabled?"准备启用":"关闭"}；处理范围：{status.spec.deleteSource?"原历史、去重及单独批准的源文件／关联 NFO／无媒体目录":"仅原历史和对应去重／同步记录"}；通知：{status.spec.notify?"需要独立确认":"关闭"}。排除路径：{status.spec.exclude||"未设置"}。</p>
    {status.spec.deleteSource&&<div><p>逐项选择允许未来处理的本机源目录，初始均不选择：</p>{status.roots.map(root=><label key={root.id}><input type="checkbox" checked={selected.includes(root.id)} onChange={e=>{proofs();setSelected(ids=>e.target.checked?[...ids,root.id]:ids.filter(id=>id!==root.id))}}/>{root.label} · {root.path}</label>)}</div>}
    <label><input type="checkbox" checked={http} onChange={e=>{setHTTP(e.target.checked);setReview(null)}}/>允许向原明文 HTTP Emby 连接传送已有凭据（HTTPS 不需要此项）</label>
    <label><input type="checkbox" checked={read} onChange={e=>setRead(e.target.checked)}/>确认只读核对原 Emby 与所选源目录身份，不触发删除</label><button type="button" disabled={!read||!status.ready||status.spec.deleteSource&&selected.length===0} onClick={()=>void operate("review")}>只读审核未来同步删除范围</button>
    {!review&&<label><input type="checkbox" checked={consent.confirm} onChange={e=>setConsent(c=>({...c,confirm:e.target.checked}))}/>确认停止当前未来自动同步删除</label>}<button type="button" disabled={!consent.confirm} onClick={()=>void operate("stop")}>仅停止未来自动同步删除</button>
   </div>}
   {review&&<article aria-label="原同步删除授权审核"><h3>未来回调的原范围审核</h3><p>原服务身份 {review.mediaId}。此审核有效十分钟，不补处理旧接收回执。历史模式也会清除原去重记录；源模式还会永久处理批准源目录，不能由停止撤回。</p>{review.roots.map(root=><p key={root.id}>批准源目录：{root.path}</p>)}{grantFields(review.spec.deleteSource,review.spec.notify)}<button type="button" disabled={!allowed(review.spec.deleteSource,review.spec.notify)} onClick={()=>void operate("enable")}>启用这份未来同步删除授权</button></article>}
   <p>缺少明确虚拟媒体标志会停用插件；虚拟媒体不处理。新授权、刷新和进程重启不自动续跑旧计划，部分删除仍保留原事实。独立继续需要重新读取同一完整范围并逐项确认。</p>
   <label>同步删除回执页码<input type="number" min="1" max="10000" value={page} onChange={e=>setPage(e.target.value)}/></label><button type="button" onClick={()=>void operate("list")}>只读查看本页 30 条同步删除</button>
   <label>原同步删除稳定编号<input maxLength={64} value={lookup} onChange={e=>{setLookup(e.target.value);setReceipt(null);proofs()}}/></label><button type="button" disabled={!lookup.trim()} onClick={()=>void operate("lookup")}>按原同步删除编号只读查回</button>
   {receipt&&<article aria-label="原同步删除执行回执"><h3>{states[receipt.state]} · {receipt.id}</h3><p>{receipt.event.type} · {receipt.event.name} · TMDB {receipt.event.tmdbId} · 原季 {receipt.event.season}／集 {receipt.event.episode}</p><p>原回调路径：{receipt.event.path||"未提供明确有效路径"}。匹配原历史 {receipt.count} 条。{receipt.reason}{receipt.actor&&" · "+receipt.actor}</p>{receipt.parentId&&<p>关联完整删除编号：{receipt.parentId} · {receipt.parentState}。如有保留捕获对象，可在整理页“历史／媒体删除”区按此编号只读审核返还，不能冒充文件已恢复。</p>}
    {receipt.parentId&&!["completed","ignored"].includes(receipt.state)&&<><label><input type="checkbox" checked={read} onChange={e=>setRead(e.target.checked)}/>确认只读核对这份原完整删除范围及已有文件结果</label><button type="button" disabled={!read} onClick={()=>void operate("execution")}>只读审核原完整删除范围</button></>}
    {!["completed","ignored"].includes(receipt.state)&&<><label>忽略原因<input maxLength={1000} value={reason} onChange={e=>{setReason(e.target.value);setDispose(false)}}/></label><label><input type="checkbox" checked={dispose} onChange={e=>setDispose(e.target.checked)}/>确认忽略原未完成项并停止当前自动同步删除，保留已有事实与剩余对象</label><button type="button" disabled={!dispose||!reason.trim()} onClick={()=>void operate("ignore")}>仅忽略这份原未完成回执并停止后台</button></>}
   </article>}
   {execution&&<article aria-label="原完整删除独立继续审核"><h3>仅审核原完整范围的独立继续</h3><p>原历史 {execution.parent.historyIds.length} 条、去重记录 {execution.parent.dedupeRows} 条、展开范围 {execution.parent.units.length} 项。{execution.parent.reason}</p>
    {execution.parent.units.slice((unitPage-1)*30,unitPage*30).map(u=><p key={u.ordinal}>范围 {u.ordinal+1} · {u.rootId} / {u.path} · {u.absent?"已知原对象不存在":"原文件／目录范围"} · 文件 {u.files}、目录 {u.directories}、链接 {u.links}、{u.bytes} 字节 · {u.sample.join("、")}</p>)}
    {execution.parent.units.length>30&&<div><button type="button" disabled={unitPage<=1} onClick={()=>{setUnitPage(p=>p-1);setConsent(empty())}}>查看前 30 项原范围</button><span>范围页 {unitPage} / {Math.ceil(execution.parent.units.length/30)}</span><button type="button" disabled={unitPage*30>=execution.parent.units.length} onClick={()=>{setUnitPage(p=>p+1);setConsent(empty())}}>查看后 30 项原范围</button></div>}
    {grantFields(execution.parent.units.length>0,execution.receipt.policy.notify)}<button type="button" disabled={!execution.parent.canContinue||!allowed(execution.parent.units.length>0,execution.receipt.policy.notify)} onClick={()=>void operate("continue")}>仅按原完整范围执行本次独立继续</button>
   </article>}
   {items.map(p=><button type="button" key={p.id} onClick={()=>choose(p)}>{p.event.name||"原回调"} · {states[p.state]} · {p.id}</button>)}
  </fieldset>{notice&&<p role="status">{notice}</p>}
 </section>;
}
