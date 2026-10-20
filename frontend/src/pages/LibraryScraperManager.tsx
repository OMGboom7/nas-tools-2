import { useEffect, useRef, useState } from "react";
import { ApiError, getLibraryScraper, previewLibraryScrape, createLibraryScrape, getLibraryScrapeJob, cancelLibraryScrape, reviewLibraryScrapeOutput, recoverLibraryScrapeOutput, type LibraryScrapeInput, type LibraryScrapeStatus, type LibraryScrapePreview, type LibraryScrapeDetail, type LibraryScrapeRecovery } from "../api/client";

const states:Record<string,string>={ready:"等待处理",running:"执行中或中断（不自动重跑）",completed:"所选项目处理完成",partial:"部分完成／有缺口",needs_review:"需核查，禁止重发",cancelled:"已停止后续写入",pending:"待处理",blocked:"已阻止",writing:"写入意图待核查",created:"已生成",replaced:"已替换，原文件有备份",preserved:"已有文件保留（未验证内容）",uncertain:"结果不确定，目标仍保留占用",not_published:"核查未发布（暂存保留，不重跑）",restore_running:"恢复意图待核查",restored:"原文件已恢复，替换文件保留备份"};
const modes:Record<string,string>={no_force:"仅补齐缺失文件",force_nfo:"仅重写所选 NFO",force_all:"重写所选 NFO 与图片"};

export function LibraryScraperManager({token,enabled,onSessionExpired}:{token:string;enabled:boolean;onSessionExpired:()=>void}){
  const [status,setStatus]=useState<LibraryScrapeStatus|null>(null),[rootId,setRoot]=useState(""),[path,setPath]=useState("."),[mode,setMode]=useState<LibraryScrapeInput["mode"]>("no_force"),[exclude,setExclude]=useState("");
  const [preview,setPreview]=useState<LibraryScrapePreview|null>(null),[detail,setDetail]=useState<LibraryScrapeDetail|null>(null),[recovery,setRecovery]=useState<LibraryScrapeRecovery|null>(null);
  const [consent,setConsent]=useState(false),[stopped,setStopped]=useState(false),[overwrite,setOverwrite]=useState(false),[recoverConsent,setRecoverConsent]=useState(false),[recoverStopped,setRecoverStopped]=useState(false);
  const [cancelConsent,setCancelConsent]=useState(false);
  const [busy,setBusy]=useState(false),[error,setError]=useState(""),[unknownSubmit,setUnknownSubmit]=useState(false);
  const generation=useRef(0),inFlight=useRef(false);
  useEffect(()=>{generation.current++;inFlight.current=false;setStatus(null);setRoot("");setPath(".");setMode("no_force");setExclude("");setPreview(null);setDetail(null);setRecovery(null);setConsent(false);setStopped(false);setOverwrite(false);setRecoverConsent(false);setRecoverStopped(false);setCancelConsent(false);setUnknownSubmit(false);setError("");setBusy(false);return()=>{generation.current++;};},[token]);
  function invalidate(){setPreview(null);setConsent(false);setStopped(false);setOverwrite(false);}
  async function operate(action:()=>Promise<void>,mutation=false){
    if(!enabled || inFlight.current)return;const version=++generation.current;inFlight.current=true;setBusy(true);setError("");
    // Each action checks the generation before changing state. Mutations are
    // sent exactly once; a lost response never triggers recovery or resubmit.
    try{await action();}catch(err){if(version!==generation.current)return;if(err instanceof ApiError && err.code===401)onSessionExpired();else setError((err instanceof Error?err.message:"操作失败")+(mutation?"。请只读刷新核查，不要重复提交。":""));if(mutation){invalidate();setRecovery(null);setRecoverConsent(false);setRecoverStopped(false);setCancelConsent(false);setUnknownSubmit(true);}}finally{if(version===generation.current){inFlight.current=false;setBusy(false);}}
  }
  async function load(){const v=generation.current;const next=await getLibraryScraper(token);if(v!==generation.current)return;setStatus(next);setUnknownSubmit(false);invalidate();setRecovery(null);setRecoverConsent(false);setRecoverStopped(false);}
  async function view(id:string,page=1,outputPage=1){const v=generation.current;const d=await getLibraryScrapeJob(token,id,page,outputPage);if(v!==generation.current)return;setDetail(d);setCancelConsent(false);setRecovery(null);setRecoverConsent(false);setRecoverStopped(false);}
  async function makePreview(){const v=generation.current;invalidate();const p=await previewLibraryScrape(token,{rootId,path,mode,excludes:exclude.split("\n").map(s=>s.trim()).filter(Boolean)});if(v===generation.current)setPreview(p);}
  async function submit(){if(!preview)return;const v=generation.current;const saved=await createLibraryScrape(token,preview,consent,stopped,overwrite);if(v!==generation.current)return;invalidate();setDetail(null);setStatus(s=>s?{...s,jobs:[saved,...s.jobs.filter(j=>j.id!==saved.id)]}:s);}
  async function stop(){if(!detail)return;const v=generation.current;const d=await cancelLibraryScrape(token,detail.job.id,cancelConsent);if(v!==generation.current)return;setDetail(d);setCancelConsent(false);setRecovery(null);}
  async function inspect(id:string){const v=generation.current;setRecovery(null);setRecoverConsent(false);setRecoverStopped(false);const r=await reviewLibraryScrapeOutput(token,id);if(v===generation.current)setRecovery(r);}
  async function recover(action:"restore"|"reconcile"){if(!recovery)return;const v=generation.current;await recoverLibraryScrapeOutput(token,recovery,action,recoverConsent,recoverStopped);if(v!==generation.current)return;setRecovery(null);setRecoverConsent(false);setRecoverStopped(false);if(detail)await view(detail.job.id,detail.page,detail.outputPage);}
  const active=detail && ["ready","running"].includes(detail.job.state);
  return <section aria-label="已有媒体库手动刮削"><h2>已有媒体库手动刮削</h2>
    <p>只修改配置媒体库范围内的所选 NFO／图片，不整理、删除或移动视频，不新增入库历史。先只读扫描并审核，再单独提交一次后台任务（通常 15 秒内开始）。不依赖入库刮削开关；选项沿用上方配置。</p>
    <p>剧集／动漫会按选项处理关联的剧、季共享元数据，可能位于所选目录的上一级，但不会越出媒体库。仅补缺失时优先读取已有 NFO 的明确 TMDB ID；两种重写模式会忽略旧 NFO ID，重新识别名称。</p>
    <p>覆盖前将原文件交换到同目录的私有备份中，占用额外磁盘；备份不会自动删除。恢复须逐个核查并确认，不会强行覆盖后来改动的文件。中断任务不自动续跑；定时插件调度尚未迁移。</p>
    {error && <p role="alert">{error}</p>}
    <button type="button" disabled={!enabled || busy} onClick={()=>void operate(load)}>只读刷新媒体库与任务</button>
    {status && <><fieldset disabled={!enabled || busy || unknownSubmit}><legend>本次相对范围（最多 50,000 个媒体文件）</legend>
      <label>媒体库<select value={rootId} onChange={e=>{setRoot(e.target.value);invalidate();}}><option value="">请选择</option>{status.roots.map(r=><option key={r.id} value={r.id}>{r.label} · {r.path}</option>)}</select></label>
      <label>相对目录或单个媒体文件<input value={path} onChange={e=>{setPath(e.target.value);invalidate();}} placeholder=". 或 影片/影片.mkv"/></label>
      <label>模式<select value={mode} onChange={e=>{setMode(e.target.value as LibraryScrapeInput["mode"]);invalidate();}}>{Object.entries(modes).map(([value,label])=><option key={value} value={value}>{label}</option>)}</select></label>
      <label>排除的相对路径（每行一个，按完整路径边界匹配）<textarea value={exclude} onChange={e=>{setExclude(e.target.value);invalidate();}}/></label>
      <button type="button" disabled={!rootId || !path} onClick={()=>void operate(makePreview)}>只读扫描并生成新审核</button>
    </fieldset>
    {preview && <fieldset disabled={!enabled || busy || unknownSubmit}><legend>本次审核 · {modes[preview.input.mode]}</legend><p>范围：{preview.input.path}；媒体 {preview.total} 个，阻止 {preview.blocked} 个，跳过 {preview.skipped} 个对象。仅展示前 200 个，提交会再次核对全部文件。新增文件不自动扩入已保存任务。</p><ul>{preview.sample.map(m=><li key={m.path}>{m.path}{m.blocked && <small> · {m.blocked}</small>}</li>)}</ul>
      <label><input type="checkbox" checked={consent} onChange={e=>setConsent(e.target.checked)}/>确认此范围按上方已保存选项进行一次刮削，在线识别失败保留失败回执</label>
      <label><input type="checkbox" checked={stopped} onChange={e=>setStopped(e.target.checked)}/>确认旧 Python 刮削、其他实例及外部元数据写入器已实际停止</label>
      {preview.input.mode!=="no_force" && <label><input type="checkbox" checked={overwrite} onChange={e=>setOverwrite(e.target.checked)}/>独立同意覆盖本范围内的所选元数据，并保留占用磁盘的原文件备份</label>}
      <button type="button" disabled={!preview.total || !consent || !stopped || preview.input.mode!=="no_force" && !overwrite} onClick={()=>void operate(submit,true)}>确认创建并执行一次刮削任务</button>
    </fieldset>}
    <p>最近 50 条任务，未完成优先；刷新不启动旧任务。</p><ul>{status.jobs.map(j=><li key={j.id}>{j.id.slice(0,12)} · {status.roots.find(r=>r.id===j.rootId)?.label||"原媒体库"} / {j.path} · {modes[j.mode]} · {states[j.state]||j.state} · 完成 {j.done}/{j.total}，缺口 {j.blocked}<button type="button" disabled={!enabled || busy} onClick={()=>void operate(()=>view(j.id))}>只读查看进度与回执</button></li>)}</ul>
    </>}
    {detail && <><h3>任务 {detail.job.id.slice(0,12)} · {states[detail.job.state]||detail.job.state}</h3><p>{detail.job.reason} · 完成 {detail.job.done}/{detail.job.total}，缺口 {detail.job.blocked}</p>
      <button type="button" disabled={!enabled || busy} onClick={()=>void operate(()=>view(detail.job.id,detail.page,detail.outputPage))}>只读刷新当前任务</button>
      {["ready","running","needs_review"].includes(detail.job.state) && <fieldset disabled={!enabled || busy}><legend>独立停止</legend><label><input type="checkbox" checked={cancelConsent} onChange={e=>setCancelConsent(e.target.checked)}/>确认停止后续写入，已生成文件、备份和不确定占用均保留</label><button type="button" disabled={!cancelConsent} onClick={()=>void operate(stop,true)}>确认停止此任务</button></fieldset>}
      <p>媒体回执第 {detail.page} 页，每页 100 条</p><ul>{detail.items.map(i=><li key={i.ordinal}>{i.path} · {states[i.state]||i.state} · {i.reason}</li>)}</ul>
      <button type="button" disabled={!enabled || busy || detail.page<=1} onClick={()=>void operate(()=>view(detail.job.id,detail.page-1,detail.outputPage))}>上一页媒体</button><button type="button" disabled={!enabled || busy || detail.page*100>=detail.job.total} onClick={()=>void operate(()=>view(detail.job.id,detail.page+1,detail.outputPage))}>下一页媒体</button>
      <p>元数据回执第 {detail.outputPage} 页，每页 100 条。恢复或核对前先停止任务；只核对不会重写文件。</p><ul>{detail.outputs.map(o=><li key={o.id}>{o.target} · {states[o.state]||o.state} · {o.reason}{["replaced","writing","uncertain","restore_running"].includes(o.state) && <button type="button" disabled={!enabled || busy || !!active} onClick={()=>void operate(()=>inspect(o.id))}>只读核查此文件的恢复证据</button>}</li>)}</ul>
      <button type="button" disabled={!enabled || busy || detail.outputPage<=1} onClick={()=>void operate(()=>view(detail.job.id,detail.page,detail.outputPage-1))}>上一页元数据</button><button type="button" disabled={!enabled || busy || detail.outputs.length<100} onClick={()=>void operate(()=>view(detail.job.id,detail.page,detail.outputPage+1))}>下一页元数据</button>
    </>}
    {recovery && <fieldset disabled={!enabled || busy}><legend>单个元数据恢复审核 · {recovery.output.target}</legend><p>当前证据：{states[recovery.evidence]||recovery.evidence}。恢复只交换回原文件并保留替换文件备份，不改视频、不重跑刮削。</p><label><input type="checkbox" checked={recoverConsent} onChange={e=>setRecoverConsent(e.target.checked)}/>确认下面所选单次动作</label><label><input type="checkbox" checked={recoverStopped} onChange={e=>setRecoverStopped(e.target.checked)}/>确认旧执行器及其他元数据写入器已实际停止</label><button type="button" disabled={!recoverConsent || !recoverStopped || !recovery.canReconcile} onClick={()=>void operate(()=>recover("reconcile"),true)}>只核对证据并补记回执（不操作文件）</button><button type="button" disabled={!recoverConsent || !recoverStopped || !recovery.canRestore} onClick={()=>void operate(()=>recover("restore"),true)}>确认恢复这个原文件</button></fieldset>}
  </section>;
}
