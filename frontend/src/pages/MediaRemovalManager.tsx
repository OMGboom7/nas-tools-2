import { useEffect, useRef, useState } from "react";
import { ApiError, type LibraryStorage } from "../api/client";
import { browseMediaRemoval, continueMediaRemoval, createMediaRemoval, getMediaRemovals, mediaRemovalGranted, previewMediaRemoval, returnMediaRemovalCapture, reviewMediaRemoval, stopMediaRemoval, type MediaRemoval, type MediaRemovalConsent, type MediaRemovalPath, type MediaRemovalSelection, type MediaRemovalStatus } from "../api/mediaRemoval";
import type { RecoveryBrowse } from "../api/recoveryObjects";

const emptyConsent=():MediaRemovalConsent=>({confirm:false,confirmHistoryDeletion:false,confirmDedupeReset:false,confirmPermanentDeletion:false,confirmOnlyCopyRisk:false,confirmExpandedScope:false,confirmOldExecutorsStopped:false,confirmCloseRelated:false,confirmRemoteExclusive:false});
const labels:Record<MediaRemoval["state"],string>={preview:"只读审核，尚未保存",ready:"已保存，尚未执行",running:"删除意图已保存／正在执行或已中断",needs_review:"中断或证据变化，必须重新审核",completed:"完整删除及历史回执已提交",closed:"整批后续工作已停止，剩余对象和历史保留"};
export function MediaRemovalConsentFields({value,onChange,review,disabled}:{value:MediaRemovalConsent;onChange:(v:MediaRemovalConsent)=>void;review:MediaRemoval;disabled:boolean}) {
  const box=(key:keyof MediaRemovalConsent,label:string)=><label key={key}><input type="checkbox" checked={value[key]} onChange={e=>onChange({...value,[key]:e.target.checked})}/>{label}</label>;
  return <fieldset disabled={disabled}><legend>本次删除的独立确认</legend>
    {box("confirm","我已核对完整批次、所有历史编号和全部所选范围，不把文件名当作内容归属证明")}
    {!!review.historyIds.length&&box("confirmHistoryDeletion","我明确同意删除这些历史记录；完整原始行会留在处置账本，不会先删记录再假报文件成功")}
    {!!review.historyIds.length&&box("confirmDedupeReset","我同意清除所列原来源的对应转移黑名单／同步去重行，理解这可能允许未来重新处理；不会清空整张表")}
    {!!review.units.length&&box("confirmPermanentDeletion","我明确授权永久删除列出的媒体／源文件，而不只是数据库记录；删除后不能通过本操作撤销")}
    {!!review.units.length&&box("confirmOnlyCopyRisk","这些对象可能是唯一副本；本机符号链接只删链接本身，硬链接只移除选中的名字，我已核实风险")}
    {!!review.units.length&&box("confirmExpandedScope","我同意完整审核展示的 NFO、父目录、整盘及其他目录成员；样例不是全部清单，不会只处理成功子集")}
    {box("confirmOldExecutorsStopped","我已实际停止旧 Python、其他实例、下载器和全部外部写入器；勾选不会替我停止它们")}
    {box("confirmCloseRelated","我同意永久关闭列出的关联任务及整个旧多库批次，不伪造整理成功、不重开任务；旧目标预留保留")}
    {review.units.some(u=>u.driver!=="")&&box("confirmRemoteExclusive","我独占远程命名空间，承担重复完整读取费用，接受非原子删除；不清桶、不绕保留策略、不强制清版本")}
  </fieldset>;
}

function RemovalDetails({value}:{value:MediaRemoval}) {
  return <><p>{labels[value.state]} · 历史 {value.historyIds.join("、")||"无（仅所选文件）"} · 对应去重行 {value.dedupeRows}。{value.reason}</p>
    <ul>{value.records.map(r=><li key={r.id}>历史 #{r.id} · {r.title} ({r.year}) · {r.type} · TMDB {r.tmdbId} · {r.mode}<p>原来源：{r.sourcePath}/{r.sourceFilename}；原媒体库：{r.destPath}/{r.destFilename}</p></li>)}</ul>
    <ul>{value.units.map(u=><li key={u.ordinal}>#{u.ordinal+1} · {u.driver||"本机"} · {u.rootId} / {u.path} · {u.role} · {u.absent?"原本不存在：仅记录已核验缺失，不发送删除事件":`文件 ${u.files}，目录 ${u.directories}，链接 ${u.links}，载荷 ${u.bytes} 字节`}
      {u.disposal&&<p>逐项回执 {u.disposal.done}/{u.disposal.total} · {u.disposal.state}{u.disposal.capture&&` · 捕获位置：${u.disposal.capture}`}</p>}
      <details><summary>最多 100 项样例；确认范围是整个对象，不是样例</summary><ul>{u.sample.map(p=><li key={p}>{p}</li>)}</ul></details>
    </li>)}</ul>
    {!!value.owners.length&&<><p>本次会永久关闭的关联范围（包括旧批次全部兄弟任务）：</p><ul>{value.owners.map(o=><li key={o.kind+o.id}>{o.kind} · {o.id} · {o.state}{o.related?"（关联兄弟任务）":""}</li>)}</ul></>}
  </>;
}

export function MediaRemovalManager({token,enabled,onSessionExpired,onChanged,historySeed}:{token:string;enabled:boolean;onSessionExpired:()=>void;onChanged:()=>void;historySeed?:{id:string;nonce:number}}) {
  const [status,setStatus]=useState<MediaRemovalStatus|null>(null),[ids,setIds]=useState(""),[flag,setFlag]=useState<MediaRemovalSelection["flag"]>("");
  const [paths,setPaths]=useState<MediaRemovalPath[]>([]),[rootId,setRootId]=useState(""),[driver,setDriver]=useState<LibraryStorage>(""),[directory,setDirectory]=useState("."),[selectedPath,setSelectedPath]=useState("");
  const [attachId,setAttachId]=useState(""),[role,setRole]=useState<"file"|"source"|"library">("file"),[browse,setBrowse]=useState<RecoveryBrowse|null>(null);
  const [nfo,setNfo]=useState(true),[cleanup,setCleanup]=useState(false),[reason,setReason]=useState(""),[read,setRead]=useState(false),[preview,setPreview]=useState<MediaRemoval|null>(null),[review,setReview]=useState<MediaRemoval|null>(null);
  const [consent,setConsent]=useState(emptyConsent),[busy,setBusy]=useState(false),[stopping,setStopping]=useState(false),[uncertain,setUncertain]=useState(false),[stopConfirm,setStopConfirm]=useState(false),[returnConfirm,setReturnConfirm]=useState(false),[returnStopped,setReturnStopped]=useState(false),[error,setError]=useState("");
  const [lookupId,setLookupId]=useState("");
  const epoch=useRef(0),inFlight=useRef(false),stopFlight=useRef(false),seedSeen=useRef(-1);
  function invalidate(){setPreview(null);setReview(null);setConsent(emptyConsent());setStopConfirm(false);setReturnConfirm(false);setReturnStopped(false);}
  function scopeChanged(){invalidate();setRead(false);}
  useEffect(()=>{epoch.current++;inFlight.current=false;stopFlight.current=false;setStatus(null);setIds("");setFlag("");setPaths([]);setRootId("");setDriver("");setDirectory(".");setSelectedPath("");setAttachId("");setRole("file");setBrowse(null);setReason("");setRead(false);setLookupId("");invalidate();setBusy(false);setStopping(false);setUncertain(false);setError("");seedSeen.current=historySeed?.nonce??-1;return()=>{epoch.current++;};},[token]);
  useEffect(()=>{if(!historySeed||historySeed.nonce===seedSeen.current||busy||stopping)return;seedSeen.current=historySeed.nonce;setIds(historySeed.id);setFlag("");setPaths([]);setUncertain(false);scopeChanged();},[historySeed,busy,stopping]);
  async function operate(action:()=>Promise<void>,mutation=false){
    if(!enabled||inFlight.current)return;const version=++epoch.current;inFlight.current=true;setBusy(true);setError("");
    try{await action();}catch(e){if(version!==epoch.current)return;if(e instanceof ApiError&&e.code===401)onSessionExpired();else setError((e instanceof Error?e.message:"操作未核实")+(mutation?"。请只读刷新，不要重复提交删除。":""));invalidate();if(mutation)setUncertain(true);}
    finally{if(version===epoch.current){inFlight.current=false;setBusy(false);setConsent(emptyConsent());setReturnConfirm(false);setReturnStopped(false);}}
  }
  function update(v:MediaRemoval){setStatus(s=>s?{...s,removals:[v,...s.removals.filter(x=>x.id!==v.id)]}:s);}
  const selection=():MediaRemovalSelection=>({historyIds:ids.split(/[,，\s]+/).filter(Boolean),flag,paths,includeNfo:nfo,cleanupDirectories:cleanup,decision:reason,confirmRead:read});
  async function load(){const version=epoch.current;const v=await getMediaRemovals(token);if(version!==epoch.current)return;setStatus(v);setUncertain(false);invalidate();}
  async function list(path=directory){const version=epoch.current;scopeChanged();const v=await browseMediaRemoval(token,rootId,driver,path);if(version!==epoch.current)return;setBrowse(v);setDirectory(path);setSelectedPath("");}
  function addPath(path=selectedPath){if(!rootId||!path)return;setPaths(p=>[...p,{rootId,driver,path,...(role==="file"?{}:{historyId:attachId.trim(),role})}]);setSelectedPath("");scopeChanged();}
  async function inspect(){const version=epoch.current;invalidate();const v=await previewMediaRemoval(token,selection());if(version===epoch.current)setPreview(v);}
  async function save(){if(!preview)return;const version=epoch.current;setLookupId(preview.id);const v=await createMediaRemoval(token,selection(),preview,consent);if(version!==epoch.current)return;update(v);invalidate();onChanged();}
  async function inspectRecord(id:string){const version=epoch.current;invalidate();const v=await reviewMediaRemoval(token,id);if(version===epoch.current){setLookupId(v.id);update(v);setReview(v);}}
  async function execute(){if(!review)return;const version=epoch.current;const v=await continueMediaRemoval(token,review,consent);if(version!==epoch.current)return;update(v);invalidate();onChanged();}
  async function giveBack(ordinal:number){if(!review)return;const version=epoch.current;const v=await returnMediaRemovalCapture(token,review,ordinal,returnConfirm,returnStopped);if(version!==epoch.current)return;update(v);invalidate();onChanged();}
  async function stop(v:MediaRemoval){
    if(!enabled||!stopConfirm||stopFlight.current)return;const version=++epoch.current;stopFlight.current=true;setStopping(true);setError("");
    try{const next=await stopMediaRemoval(token,v,true);if(version!==epoch.current)return;update(next);invalidate();setUncertain(true);onChanged();}
    catch(e){if(version===epoch.current){if(e instanceof ApiError&&e.code===401)onSessionExpired();else setError((e instanceof Error?e.message:"停止回执未核实")+"。请只读刷新，不要重发。");invalidate();setUncertain(true);}}
    finally{if(version===epoch.current){inFlight.current=false;stopFlight.current=false;setBusy(false);setStopping(false);setStopConfirm(false);}}
  }
  return <section aria-label="历史与媒体永久删除"><h2>历史与媒体永久删除</h2>
    <p>先只读审核并保存稳定批次，再新审核后明确执行一次。历史、源、媒体库和附件不是同一种授权。全部所选范围完成后才删除对应历史、转移黑名单和同步去重行；部分失败保留原记录。不会操作下载器、启动 Python 插件或清空其他历史。</p>
    <p>完整目录、蓝光载荷、链接及未知成员均属于所展示范围，最多 20,000 个对象。审核和执行前重复完整读取；远程没有通用原子删除，不保证旧版本物理容量已回收。没有自动重启续办或错误重试。</p>
    {error&&<p role="alert">{error}</p>}<button type="button" disabled={!enabled||busy||stopping} onClick={()=>void operate(load)}>只读刷新删除批次与配置范围</button>
    <label>已保存批次编号（保存响应丢失时会保留已知编号，也可查找较早批次）<input value={lookupId} disabled={busy||stopping} maxLength={64} onChange={e=>{setLookupId(e.target.value);invalidate();}}/></label><button type="button" disabled={!enabled||busy||stopping||!/^[a-f0-9]{64}$/.test(lookupId)} onClick={()=>void operate(()=>inspectRecord(lookupId))}>只读核验这个稳定编号</button>
    {status&&<><fieldset disabled={!enabled||busy||stopping||uncertain}><legend>历史选择与可选文件范围</legend>
      <label>精确历史编号（最多 100 个，以逗号或空格分隔；不选择历史时只处理下面明确选择的文件）<input value={ids} onChange={e=>{setIds(e.target.value);scopeChanged();}}/></label>
      <label>历史操作<select value={flag} onChange={e=>{setFlag(e.target.value as MediaRemovalSelection["flag"]);scopeChanged();}}><option value="">只删除历史及其对应去重记录</option><option value="del_source">同时删除原来源</option><option value="del_dest">同时删除媒体库文件</option><option value="del_all">同时删除原来源和媒体库文件</option></select></label>
      <fieldset><legend>直接选择文件／完整目录，或补充旧记录缺失的实际布局</legend>
        <label>配置根<select value={rootId} onChange={e=>{setRootId(e.target.value);setBrowse(null);setDirectory(".");scopeChanged();}}><option value="">请选择</option>{status.roots.map(r=><option key={r.id} value={r.id}>{r.label} · {r.path}</option>)}</select></label>
        <label>存储<select value={driver} onChange={e=>{setDriver(e.target.value as LibraryStorage);setBrowse(null);scopeChanged();}}>{status.drivers.map(d=><option key={d||"local"} value={d}>{d||"本机"}</option>)}</select></label>
        <label>用途<select value={role} onChange={e=>{setRole(e.target.value as typeof role);scopeChanged();}}><option value="file">独立文件／目录，不冒充历史删除事件</option><option value="source">某条历史的明确原来源位置</option><option value="library">某条历史的明确媒体库位置</option></select></label>
        {role!=="file"&&<label>关联历史编号（须在上方选择且包含对应删除操作）<input value={attachId} onChange={e=>{setAttachId(e.target.value);scopeChanged();}}/></label>}
        <label>相对浏览目录<input value={directory} onChange={e=>{setDirectory(e.target.value);setBrowse(null);scopeChanged();}}/></label><button type="button" disabled={!rootId} onClick={()=>void operate(()=>list())}>只读浏览此层</button>
        {browse&&<ul>{browse.entries.map(e=><li key={e.path}>{e.path}{e.directory&&<button type="button" onClick={()=>void operate(()=>list(e.path))}>进入</button>}<button type="button" onClick={()=>addPath(e.path)}>加入完整选择</button></li>)}</ul>}
        <label>完整对象相对路径（不能选择配置根）<input value={selectedPath} onChange={e=>{setSelectedPath(e.target.value);scopeChanged();}}/></label><button type="button" disabled={!rootId||!selectedPath||role!=="file"&&!attachId.trim()} onClick={()=>addPath()}>加入此完整路径</button>
        <ul>{paths.map((p,i)=><li key={i}>{p.rootId} · {p.driver||"本机"} · {p.path}{p.historyId&&` · 历史 ${p.historyId} / ${p.role}`}<button type="button" onClick={()=>{setPaths(v=>v.filter((_,n)=>i!==n));scopeChanged();}}>从本次选择移除</button></li>)}</ul>
      </fieldset>
      <label><input type="checkbox" checked={nfo} onChange={e=>{setNfo(e.target.checked);scopeChanged();}}/>将实际存在的同名 NFO 加入新审核</label>
      <label><input type="checkbox" checked={cleanup} onChange={e=>{setCleanup(e.target.checked);scopeChanged();}}/>审核删除后不再含媒体的直接父目录／季与媒体目录；完整目录的所有其他成员也须明确确认，不凭媒体后缀直接删除</label>
      <label>本次删除原因<textarea value={reason} maxLength={512} onChange={e=>{setReason(e.target.value);scopeChanged();}}/></label>
      <label><input type="checkbox" checked={read} onChange={e=>{setRead(e.target.checked);invalidate();}}/>我同意完整读取所选范围、关联账本及潜在目录清理范围，承担远程重复读取费用；审核不执行删除</label>
      <button type="button" disabled={!read||!reason.trim()||!ids.trim()&&!paths.length} onClick={()=>void operate(inspect)}>只读审核全部范围与关联影响</button>
    </fieldset>
    {preview&&<fieldset disabled={busy||stopping}><legend>完整新审核</legend><RemovalDetails value={preview}/><MediaRemovalConsentFields review={preview} value={consent} onChange={setConsent} disabled={busy||stopping}/><button type="button" disabled={!mediaRemovalGranted(preview,consent)} onClick={()=>void operate(save,true)}>只保存稳定批次（尚不删除任何文件或历史）</button></fieldset>}
    <p>最近 30 个批次。停止请求可在长操作进行中发送，不代表已送出的单次删除已撤回。停止后未删文件、捕获对象、原历史及旧目标预留保留。</p><label><input type="checkbox" checked={stopConfirm} onChange={e=>setStopConfirm(e.target.checked)}/>我确认只停止所点击批次的后续工作，不删除剩余对象，不重开旧任务</label>
    <ul>{status.removals.map(v=><li key={v.id}>{v.id.slice(0,12)} · {labels[v.state]} · 历史 {v.historyIds.join("、")||"无"}<button type="button" disabled={busy||stopping} onClick={()=>void operate(()=>inspectRecord(v.id))}>只读完整审核／查看回执</button>{!["closed","completed"].includes(v.state)&&<button type="button" disabled={!enabled||stopping||!stopConfirm} onClick={()=>void stop(v)}>停止整批后续工作</button>}</li>)}</ul>
    </>}
    {review&&<fieldset disabled={!enabled||stopping}><legend>仅本次单次执行／续办</legend><RemovalDetails value={review}/><MediaRemovalConsentFields review={review} value={consent} onChange={setConsent} disabled={busy||stopping}/><button type="button" disabled={busy||!review.canContinue||!mediaRemovalGranted(review,consent)} onClick={()=>void operate(execute,true)}>按此新审核执行一次；不自动重发删除</button>
      {review.units.some(u=>u.review?.canReturn)&&<fieldset disabled={busy}><legend>归还尚未删除的本机捕获对象</legend><p>只能防覆盖归还该完整捕获到原空位置；不能恢复已经删除的兄弟范围。随后整批停止，旧任务不重开。</p><label><input type="checkbox" checked={returnConfirm} onChange={e=>setReturnConfirm(e.target.checked)}/>我独立确认上述整对象防覆盖归还和整批停止</label><label><input type="checkbox" checked={returnStopped} onChange={e=>setReturnStopped(e.target.checked)}/>全部外部写入器已实际停止</label>{review.units.filter(u=>u.review?.canReturn).map(u=><button key={u.ordinal} type="button" disabled={!returnConfirm||!returnStopped} onClick={()=>void operate(()=>giveBack(u.ordinal),true)}>只归还 {u.path}，然后停止整批</button>)}</fieldset>}
    </fieldset>}
  </section>;
}
