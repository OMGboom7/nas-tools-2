import {useEffect,useRef,useState} from "react";
import {ApiError} from "../api/client";
import {cancelSubtitleRequest,getSubtitleRequest,getSubtitleRequestStatus,listSubtitleRequests,queueSubtitleRequest,reviewSubtitleRequest,type SubtitleDriver,type SubtitleRequest,type SubtitleRequestConsent,type SubtitleRequestIdentity,type SubtitleRequestStatus} from "../api/subtitleRequests";

const emptyConsent=():SubtitleRequestConsent=>({confirm:false,confirmPath:false,confirmExternalWrite:false,confirmOldExecutorsStopped:false,http:false,confirmRemoteRead:false});
const states:Record<SubtitleRequest["serviceState"],string>={
  not_queued:"只读审核，未提交",pending:"原请求已排队，尚未发送",sending:"发送意图已保存，处理中或已中断",
  submitted:"字幕服务已接收任务，尚不证明字幕已下载",uncertain:"服务应答不明，不会重发",blocked:"原范围已变化或已停止，未发送",
};
export function SubtitleRequestManager({token,onSessionExpired,enabled=true}:{token:string;onSessionExpired:()=>void;enabled?:boolean}){
  const [status,setStatus]=useState<SubtitleRequestStatus|null>(null),[receipt,setReceipt]=useState<SubtitleRequest|null>(null),[rows,setRows]=useState<SubtitleRequest[]>([]);
  const [root,setRoot]=useState(""),[driver,setDriver]=useState<SubtitleDriver>(""),[path,setPath]=useState(""),[name,setName]=useState("");
  const [kind,setKind]=useState<""|SubtitleRequestIdentity["kind"]>(""),[tmdb,setTMDB]=useState(""),[season,setSeason]=useState(""),[episode,setEpisode]=useState(""),[endEpisode,setEndEpisode]=useState("");
  const [read,setRead]=useState(false),[remoteRead,setRemoteRead]=useState(false),[consent,setConsent]=useState(emptyConsent),[lookup,setLookup]=useState(""),[page,setPage]=useState("1");
  const [cancelOK,setCancelOK]=useState(false),[reason,setReason]=useState(""),[busy,setBusy]=useState(false),[notice,setNotice]=useState("");
  const version=useRef(0),inFlight=useRef(false);
  function proofs(){setRead(false);setRemoteRead(false);setConsent(emptyConsent());setCancelOK(false)}
  function changed(){version.current++;setReceipt(null);proofs();setNotice("")}
  useEffect(()=>{
    version.current++;inFlight.current=false;setStatus(null);setReceipt(null);setRows([]);setRoot("");setDriver("");setPath("");setName("");
    setKind("");setTMDB("");setSeason("");setEpisode("");setEndEpisode("");setLookup("");setPage("1");setReason("");proofs();setBusy(false);setNotice("");
    return()=>{version.current++};
  },[token]);
  async function operate(action:"status"|"review"|"queue"|"lookup"|"list"|"cancel"){
    if(inFlight.current)return;const generation=++version.current;inFlight.current=true;setBusy(true);setNotice("");
    const s=status,p=receipt,c={...consent},readOK=read,remoteOK=remoteRead,stop=cancelOK,why=reason;proofs();
    try{
      let out:SubtitleRequest|undefined;
      if(action==="status"){
        const data=await getSubtitleRequestStatus(token);
        if(generation===version.current){setStatus(data);setRoot("");setDriver("");setReceipt(null)}
      }else if(action==="review"){
        if(!s)throw Error("先只读核对字幕服务和配置目录");
        let identity:SubtitleRequestIdentity|undefined;
        if(kind){
          identity={kind,tmdbId:tmdb,...(season!==""?{season:Number(season)}:{}),...(episode!==""?{episode:Number(episode)}:{}),...(endEpisode!==""?{endEpisode:Number(endEpisode)}:{})};
        }else if(tmdb||season||episode||endEpisode)throw Error("指定身份时先选择电影或剧集类型");
        out=await reviewSubtitleRequest(token,s,{rootId:root,driver,path,name,identity,confirmRead:readOK,confirmRemoteRead:remoteOK});
      }else if(action==="queue"){
        if(!p)throw Error("先审核这份原文件");out=await queueSubtitleRequest(token,p,c);
        if(generation===version.current)setNotice("仅这份原字幕请求已排队；字幕服务是否接收及是否完成分别查看，不会重发应答不明的请求。");
      }else if(action==="lookup"){out=await getSubtitleRequest(token,lookup.trim())}
      else if(action==="list"){const data=await listSubtitleRequests(token,Number(page));if(generation===version.current)setRows(data)}
      else{if(!p)throw Error("先查回原请求");out=await cancelSubtitleRequest(token,p,stop,why)}
      if(out&&generation===version.current){const saved=out;setReceipt(saved);setLookup(saved.id);setRows(v=>v.map(p=>p.id===saved.id?saved:p))}
    }catch(e){
      if(generation!==version.current)return;
      if(e instanceof ApiError&&e.code===401)onSessionExpired();else setNotice(e instanceof Error?e.message:"请按原编号只读核对");
      if(p&&(action==="queue"||action==="cancel")){
        try{const original=await getSubtitleRequest(token,p.id);if(generation===version.current){setReceipt(original);setLookup(original.id)}}catch{/* Readonly recovery never retries a submission. */}
      }
    }finally{if(generation===version.current){setBusy(false);inFlight.current=false}}
  }
  function choose(p:SubtitleRequest){proofs();setReceipt(p);setLookup(p.id);setReason("");setNotice("")}
  const confirmed=consent.confirm&&consent.confirmPath&&consent.confirmExternalWrite&&consent.confirmOldExecutorsStopped&&(!receipt?.http||consent.http)&&(!receipt?.driver||consent.confirmRemoteRead);
  return <section className="notification-receipt-panel" aria-label="单文件字幕请求">
    <h2>为一个现有视频请求字幕</h2>
    <p>读取已配置目录里的原视频，核对媒体身份后提交到原 ChineseSubFinder。此处不搬动视频、不新增整理历史；任务被接收不代表字幕已经下载。服务可能按自己的规则写入或更新字幕。</p>
    <fieldset disabled={busy||!enabled}>
      <button type="button" onClick={()=>void operate("status")}>只读核对字幕服务与目录</button>
      {status&&<div>
        <p>{status.ready?`字幕服务：${status.origin}`:status.reason}</p>
        <label>原配置目录<select value={root} onChange={e=>{setRoot(e.target.value);changed()}}><option value="">请选择一个目录</option>{status.roots.map(r=><option key={r.id} value={r.id}>{r.label} · {r.path}</option>)}</select></label>
        <label>文件存储<select value={driver} onChange={e=>{setDriver(e.target.value as SubtitleDriver);changed()}}>{status.drivers.map(d=><option key={d||"local"} value={d}>{d==="rclone"?"Rclone":d==="minio"?"MinIO":"本机文件"}</option>)}</select></label>
        <label>目录内单个视频路径<input value={path} maxLength={4096} placeholder="影片/影片.2026.mkv" onChange={e=>{setPath(e.target.value);changed()}}/></label>
        <label>识别用名称（留空使用文件名）<input value={name} maxLength={200} onChange={e=>{setName(e.target.value);changed()}}/></label>
        <details><summary>可选：指定原媒体身份</summary>
          <label>媒体类型<select value={kind} onChange={e=>{setKind(e.target.value as typeof kind);setSeason("");setEpisode("");setEndEpisode("");changed()}}><option value="">按名称识别</option><option value="movie">电影</option><option value="tv">剧集</option><option value="anime">动漫剧集</option></select></label>
          <label>TMDB 编号<input value={tmdb} maxLength={19} onChange={e=>{setTMDB(e.target.value);changed()}}/></label>
          {kind&&kind!=="movie"&&<><label>季<input type="number" min="0" max="99" value={season} onChange={e=>{setSeason(e.target.value);changed()}}/></label><label>起始集（可选）<input type="number" min="1" max="9999" value={episode} onChange={e=>{setEpisode(e.target.value);changed()}}/></label><label>结束集（可选）<input type="number" min="1" max="9999" value={endEpisode} onChange={e=>{setEndEpisode(e.target.value);changed()}}/></label></>}
        </details>
        <p>只审核这个视频；会完整读取内容并计算摘要。链接只采用已配置目录内的已知目标，不扩大成目录或蓝光片段。读取可能需要较长时间。</p>
        <label><input type="checkbox" checked={read} onChange={e=>setRead(e.target.checked)}/>确认只读核对原文件及媒体身份，不提交字幕任务</label>
        {driver&&<label><input type="checkbox" checked={remoteRead} onChange={e=>setRemoteRead(e.target.checked)}/>允许使用这份原远程账户读取所选视频的完整内容</label>}
        <button type="button" disabled={!status.ready||!root||!path||!read||!!driver&&!remoteRead} onClick={()=>void operate("review")}>只读审核这个字幕请求</button>
      </div>}
      <label>原字幕请求稳定编号<input value={lookup} maxLength={64} onChange={e=>{setLookup(e.target.value);setReceipt(null);proofs()}}/></label>
      <button type="button" disabled={!lookup.trim()} onClick={()=>void operate("lookup")}>按原字幕编号只读查回</button>
      <label>字幕回执页码<input type="number" min="1" max="10000" value={page} onChange={e=>setPage(e.target.value)}/></label>
      <button type="button" onClick={()=>void operate("list")}>只读查看本页 30 条字幕请求</button>
      {receipt&&<article aria-label="原单文件字幕请求">
        <h3>{receipt.title} {receipt.year} · {receipt.mediaType}</h3>
        <p>原编号：{receipt.id}；TMDB {receipt.tmdbId}；原目录 {receipt.rootId} / {receipt.path}；{receipt.bytes} 字节。</p>
        <p>原服务：{receipt.serviceOrigin}；映射后文件：{receipt.mappedPath}。{receipt.state==="cancelled"?"原尚未发送请求已停止":states[receipt.serviceState]}{receipt.externalId&&`；字幕服务任务编号 ${receipt.externalId}`}</p>
        {receipt.serviceUpdated&&<p>服务回执更新时间：{receipt.serviceUpdated}</p>}
        {receipt.reason&&<p>{receipt.reason} · {receipt.stoppedBy}</p>}
        {receipt.state==="review"&&<>
          <p>审核有效十分钟。仅将下面确认过的这一份原文件提交一次；应答不明和进程重启不会重发。</p>
          <label><input type="checkbox" checked={consent.confirm} onChange={e=>setConsent(v=>({...v,confirm:e.target.checked}))}/>我已核对这份原媒体身份和本次请求</label>
          <label><input type="checkbox" checked={consent.confirmPath} onChange={e=>setConsent(v=>({...v,confirmPath:e.target.checked}))}/>确认原视频路径及原服务路径映射，不扩大到其他文件</label>
          <label><input type="checkbox" checked={consent.confirmExternalWrite} onChange={e=>setConsent(v=>({...v,confirmExternalWrite:e.target.checked}))}/>允许原字幕服务按其自身规则下载、写入或更新这个视频的字幕</label>
          <label><input type="checkbox" checked={consent.confirmOldExecutorsStopped} onChange={e=>setConsent(v=>({...v,confirmOldExecutorsStopped:e.target.checked}))}/>原字幕执行器和其他重复提交器已实际停止</label>
          {receipt.http&&<label><input type="checkbox" checked={consent.http} onChange={e=>setConsent(v=>({...v,http:e.target.checked}))}/>允许向原明文 HTTP 字幕服务传送已有凭据与这个文件路径</label>}
          {receipt.driver&&<label><input type="checkbox" checked={consent.confirmRemoteRead} onChange={e=>setConsent(v=>({...v,confirmRemoteRead:e.target.checked}))}/>允许提交前及后台发送前重新完整读取原远程视频以核对摘要</label>}
          <button type="button" disabled={!confirmed} onClick={()=>void operate("queue")}>仅提交这份原字幕请求</button>
        </>}
        {receipt.state!=="cancelled"&&(!receipt.queued||receipt.serviceState==="pending")&&<>
          <label>停止原请求的原因<input value={reason} maxLength={300} onChange={e=>{setReason(e.target.value);setCancelOK(false)}}/></label>
          <label><input type="checkbox" checked={cancelOK} onChange={e=>setCancelOK(e.target.checked)}/>确认仅停止 ChineseSubFinder 尚未发送的原请求；独立事件外发及服务已接收的任务保留</label>
          <button type="button" disabled={!cancelOK||!reason.trim()} onClick={()=>void operate("cancel")}>停止原尚未发送的字幕请求</button>
        </>}
      </article>}
      {rows.map(p=><button type="button" key={p.id} onClick={()=>choose(p)}>{p.title} · {p.state==="cancelled"?"原请求已停止":states[p.serviceState]} · {p.id}</button>)}
    </fieldset>
    {notice&&<p role="status">{notice}</p>}
  </section>;
}
