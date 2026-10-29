import {useEffect,useRef,useState} from "react";
import {ApiError} from "../api/client";
import {authorizeFavorite,continueFavorite,getFavoriteReceipt,getFavoriteStatus,ignoreFavorite,listFavoriteReceipts,reviewFavorite,reviewFavoriteExecution,stopFavorite,type FavoriteConsent,type FavoriteExecution,type FavoriteReceipt,type FavoriteReview,type FavoriteStatus} from "../api/movieLike";

const empty=():FavoriteConsent=>({confirm:false,move:false,whole:false,sourceRemoval:false,closeRelated:false,legacyStopped:false,refresh:false});
const states:Record<FavoriteReceipt["state"],string>={pending:"新回调待核对",planned:"原完整目录已冻结，尚未移动",running:"已记录本次移动意图",needs_review:"原移动中断，需要重新审核",completed:"完整目录移动已核实完成",ignored:"原后续工作已停止，对象保留",blocked:"原范围未核实，未自动移动"};
export function MovieLikeManager({token,onSessionExpired}:{token:string;onSessionExpired:()=>void}){
 const [status,setStatus]=useState<FavoriteStatus|null>(null),[review,setReview]=useState<FavoriteReview|null>(null),[execution,setExecution]=useState<FavoriteExecution|null>(null);
 const [selected,setSelected]=useState<string[]>([]),[http,setHTTP]=useState(false),[read,setRead]=useState(false),[consent,setConsent]=useState(empty),[stop,setStop]=useState(false);
 const [rows,setRows]=useState<FavoriteReceipt[]>([]),[receipt,setReceipt]=useState<FavoriteReceipt|null>(null),[page,setPage]=useState("1"),[lookup,setLookup]=useState(""),[reason,setReason]=useState(""),[dispose,setDispose]=useState(false),[busy,setBusy]=useState(false),[notice,setNotice]=useState("");
 const version=useRef(0),inFlight=useRef(false);
 function proofs(){setReview(null);setExecution(null);setConsent(empty());setHTTP(false);setRead(false);setStop(false);setDispose(false)}
 useEffect(()=>{version.current++;inFlight.current=false;setStatus(null);setSelected([]);setRows([]);setReceipt(null);setPage("1");setLookup("");setReason("");proofs();setBusy(false);setNotice("");return()=>{version.current++}},[token]);
 async function operate(action:"status"|"review"|"enable"|"stop"|"list"|"lookup"|"execution"|"continue"|"ignore"){
  if(inFlight.current)return;const generation=++version.current;inFlight.current=true;setBusy(true);setNotice("");
  const s=status,r=review,x=execution,p=receipt,c={...consent},rootIds=[...selected],httpOK=http,readOK=read,stopOK=stop,disposeOK=dispose;proofs();
  try{
   if(action==="status"){const out=await getFavoriteStatus(token);if(generation===version.current){setStatus(out);setSelected([])}}
   else if(action==="review"){if(!s)throw Error("先只读核对原设置");const out=await reviewFavorite(token,s,rootIds,httpOK,readOK);if(generation===version.current){setReview(out);setExecution(null)}}
   else if(action==="enable"){if(!r)throw Error("先读取完整原范围");const out=await authorizeFavorite(token,r,c);if(generation===version.current){setStatus(out);setNotice("仅未来新回调获得原完整范围授权，不补处理旧事件。")}}
   else if(action==="stop"){if(!s)throw Error("先核对原设置");const out=await stopFavorite(token,s,stopOK);if(generation===version.current){setStatus(out);setNotice("未来自动移动已停止，不撤回已经完成的移动，原回执保留。")}}
   else if(action==="list"){const out=await listFavoriteReceipts(token,Number(page));if(generation===version.current)setRows(out)}
   else if(action==="lookup"){const out=await getFavoriteReceipt(token,lookup.trim());if(generation===version.current){setReceipt(out);setLookup(out.id)}}
   else if(action==="execution"){if(!p)throw Error("先查回完整原编号");const out=await reviewFavoriteExecution(token,p,readOK);if(generation===version.current){setReceipt(out.receipt);setExecution(out)}}
   else if(action==="continue"){if(!x)throw Error("先重新核对原完整目录");const out=await continueFavorite(token,x,c);if(generation===version.current){setReceipt(out);setRows(v=>v.map(r=>r.id===out.id?out:r));setNotice("同一原目录的本次移动结果已核实，不新增整理历史。")}}
   else{if(!p)throw Error("先查回原编号");const out=await ignoreFavorite(token,p,reason,disposeOK);if(generation===version.current){setReceipt(out);setStatus(null);setRows(v=>v.map(r=>r.id===out.id?out:r));setNotice("原未完成任务和当前后台均已停止；保留现有目录、暂存、捕获对象及其占用。")}}
  }catch(e){
   if(generation!==version.current)return;if(e instanceof ApiError&&e.code===401)onSessionExpired();else setNotice(e instanceof Error?e.message:"请只读核对原移动编号");
   if(["enable","stop","continue","ignore"].includes(action)){
    try{if(p&&(action==="continue"||action==="ignore")){const out=await getFavoriteReceipt(token,p.id);if(generation===version.current)setReceipt(out)}else{const out=await getFavoriteStatus(token);if(generation===version.current)setStatus(out)}}catch{/* only read after an uncertain mutation */}
   }
  }finally{if(generation===version.current){setBusy(false);inFlight.current=false}}
 }
 const granted=()=>consent.confirm&&consent.move&&consent.whole&&consent.sourceRemoval&&consent.closeRelated&&consent.legacyStopped&&consent.refresh;
 const grants=()=>{const fields:[keyof FavoriteConsent,string][]=[
  ["confirm","我已核对原电影分类、电影库范围与完整原移动内容"],
  ["move","允许将电影的整个原目录移动到精选分类；不覆盖、不合并已有目标"],
  ["whole","确认整目录包含字幕、封面、NFO、空目录、链接及其他附件，样例不是全部范围"],
  ["sourceRemoval","确认移动会移除原名字；跨文件系统须完整副本验证后清理原来源，停止不能撤回已完成动作"],
  ["closeRelated","允许关闭整个关联旧批次和原写入权限，保留原历史及旧结果，不重开旧任务"],
  ["legacyStopped","旧 Python 精选插件、其他实例和外部写入器已实际停止，勾选不会代为停止"],
  ["refresh","允许完成后产生原媒体刷新事件；实际扫描仍需要刷新插件自己的有效授权"]
 ];return <div>{fields.map(([key,label])=><label key={key}><input type="checkbox" checked={consent[key]} onChange={e=>setConsent(v=>({...v,[key]:e.target.checked}))}/>{label}</label>)}</div>};
 const choose=(p:FavoriteReceipt)=>{proofs();setReceipt(p);setLookup(p.id);setReason("")};
 return <section className="notification-receipt-panel" aria-label="电影精选移动管理"><h2>电影精选移动管理</h2>
  <p>保留原 Emby 电影 item.rate 回调及三组路径映射。整部电影目录连同所有附件一起移动到原精选分类；原历史不改、不新增入库记录，不把回调路径当作任意移动指令。已有同名目标不会覆盖或合并。</p>
  <fieldset disabled={busy}><button type="button" onClick={()=>void operate("status")}>只读核对电影精选设置</button>
   {status&&<div><p>{status.installed?"插件已安装":"插件未安装"} · {status.active?"未来精选移动已授权":"自动精选移动未启用或原授权已失效"} · {status.workerRunning?"后台已运行":"后台尚未运行"}。{status.reason}</p><p>原开关：{status.spec.enabled?"准备启用":"关闭"}；精选目录：{status.spec.directory||"未设置"}；原电影分类：{status.categories.join("、")||"未配置"}。</p>
    <p>逐项选择允许处理的本机电影库，初始均未选择：</p>{status.roots.map(root=><label key={root.id}><input type="checkbox" checked={selected.includes(root.id)} onChange={e=>{proofs();setSelected(v=>e.target.checked?[...v,root.id]:v.filter(id=>id!==root.id))}}/>{root.label} · {root.path}</label>)}
    <label><input type="checkbox" checked={http} onChange={e=>{setHTTP(e.target.checked);setReview(null)}}/>允许向原明文 HTTP Emby 连接传送已有凭据（HTTPS 不需要此项）</label>
    <label><input type="checkbox" checked={read} onChange={e=>setRead(e.target.checked)}/>确认只读核对原 Emby、分类和所选电影库身份，不触发移动</label><button type="button" disabled={!status.ready||!read||selected.length===0} onClick={()=>void operate("review")}>只读审核未来精选移动范围</button>
    <label><input type="checkbox" checked={stop} onChange={e=>setStop(e.target.checked)}/>确认仅停止未来自动精选移动</label><button type="button" disabled={!stop} onClick={()=>void operate("stop")}>仅停止未来自动精选移动</button>
   </div>}
   {review&&<article aria-label="原电影精选授权审核"><h3>未来回调的完整原范围</h3><p>原 Emby 身份 {review.mediaId}；原分类 {review.categories.join("、")}；精选目录 {review.spec.directory}。审核有效十分钟，不补领此前接收的事件。</p>{review.roots.map(r=><p key={r.id}>批准电影库：{r.path}</p>)}{grants()}<button type="button" disabled={!granted()} onClick={()=>void operate("enable")}>启用这份未来精选移动授权</button></article>}
   <p>后台只处理新回调，中断计划与进程重启不自动继续。设置应答不明时只读查询，不能重新发送移动；按原完整编号重新审核后才可独立继续。</p>
   <label>精选回执页码<input type="number" min="1" max="10000" value={page} onChange={e=>setPage(e.target.value)}/></label><button type="button" onClick={()=>void operate("list")}>只读查看本页 30 条精选移动</button>
   <label>原精选移动稳定编号<input maxLength={64} value={lookup} onChange={e=>{setLookup(e.target.value);setReceipt(null);proofs()}}/></label><button type="button" disabled={!lookup.trim()} onClick={()=>void operate("lookup")}>按原精选移动编号只读查回</button>
   {receipt&&<article aria-label="原精选移动回执"><h3>{states[receipt.state]} · {receipt.id}</h3><p>{receipt.event.name||"原电影回调"} · 原路径 {receipt.event.path||"未核实"} · 原阶段 {receipt.phase||"未建立移动计划"}。{receipt.reason}</p>
    {receipt.phase&&!["completed","ignored"].includes(receipt.state)&&<><label><input type="checkbox" checked={read} onChange={e=>setRead(e.target.checked)}/>确认读取同一完整原目录、暂存、捕获与已移动事实</label><button type="button" disabled={!read} onClick={()=>void operate("execution")}>只读审核原完整移动范围</button></>}
    {!["completed","ignored"].includes(receipt.state)&&<><label>停止原任务的原因<input maxLength={1000} value={reason} onChange={e=>{setReason(e.target.value);setDispose(false)}}/></label><label><input type="checkbox" checked={dispose} onChange={e=>setDispose(e.target.checked)}/>确认停止这份原未完成移动及当前后台，保留所有对象与占用</label><button type="button" disabled={!dispose||!reason.trim()} onClick={()=>void operate("ignore")}>停止原未完成移动并保留对象</button></>}
   </article>}
   {execution&&<article aria-label="原完整电影目录独立继续"><h3>仅原完整目录的本次继续</h3><p>{execution.move.rootId} / {execution.move.source} → {execution.move.target}</p><p>文件 {execution.move.files}、目录 {execution.move.directories}、链接 {execution.move.links}、{execution.move.bytes} 字节。{execution.move.evidence}</p><details><summary>最多 100 项路径样例；确认范围始终是整个电影目录</summary><ul>{execution.move.sample.map(p=><li key={p}>{p}</li>)}</ul></details>
    <details><summary>完整关联旧任务与批次，共 {execution.move.owners.length} 项</summary><ul>{execution.move.owners.map(o=><li key={o.kind+o.id}>{o.kind} · {o.id} · {o.state}{o.batchId&&" · 批次 "+o.batchId}{o.related&&" · 关联兄弟"}</li>)}</ul></details>
    {grants()}<button type="button" disabled={!execution.move.canContinue||!granted()} onClick={()=>void operate("continue")}>仅按原完整范围执行本次继续</button>
   </article>}
   {rows.map(p=><button type="button" key={p.id} onClick={()=>choose(p)}>{p.event.name||"原回调"} · {states[p.state]} · {p.id}</button>)}
  </fieldset>{notice&&<p role="status">{notice}</p>}
 </section>;
}
