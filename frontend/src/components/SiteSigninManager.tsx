import { useEffect, useRef, useState } from "react";
import { ApiError, type AuthSession } from "../api/client";
import { activateSignin, getSigninBatch, getSigninStatus, inputSignin, installSignin, reviewSignin, saveSigninSettings, signinActive, signinInputError, signinInputRejected, signinSettingsValid, startSignin, stopSigninControl, stopSigninJob, type SigninBatch, type SigninJob, type SigninOptions, type SigninReview, type SigninSettings, type SigninStatus } from "../api/siteSignin";
import "./SiteSigninManager.css";

type Props={session:AuthSession;onSessionExpired:()=>void;onClose?:()=>void;initialSites?:string[]};
const initialOptions:SigninOptions={ocr:false,douban:false,lens:false,ai:false,savedLogin:false,remote:false};
const labels:Record<string,string>={queued:"排队中",running:"执行中",waiting_input:"等待本次输入",finished:"已结束",signed:"本次签到成功",already:"今日已签到",authenticated:"登录有效",deferred:"等待可签到时间",failed:"已确认失败",unknown:"结果未知",blocked:"需要重新核对",stopped:"已停止"};
const savedId=(key:string)=>{try{const id=localStorage.getItem(key)||"";return /^[a-f0-9]{64}$/.test(id)?id:""}catch{return ""}};
const rememberId=(key:string,id:string)=>{try{localStorage.setItem(key,id)}catch{/* The server history remains available. */}};

export function SiteSigninManager({session,onSessionExpired,onClose,initialSites=[]}:Props){
 const [status,setStatus]=useState<SigninStatus>();const [draft,setDraft]=useState<SigninSettings>();const [error,setError]=useState("");const [notice,setNotice]=useState("");const [busy,setBusy]=useState(false);const [mode,setMode]=useState<"manual"|"activate">("manual");const [selected,setSelected]=useState(initialSites);const [options,setOptions]=useState<SigninOptions>(initialOptions);const [review,setReview]=useState<SigninReview>();const [batch,setBatch]=useState<SigninBatch>();const [batchId,setBatchId]=useState("");const [lookup,setLookup]=useState("");const [uncertain,setUncertain]=useState(false);const [page,setPage]=useState(1);
 const generation=useRef(0),revision=useRef(0),batchReads=useRef(0),inFlight=useRef(false),initialized=useRef(false),mounted=useRef(true);const storageKey="nastool.site-signin.batch:"+session.user.username;
 const fail=(e:unknown)=>{if(!mounted.current)return;if(e instanceof ApiError&&e.code===401){onSessionExpired();return}setError(e instanceof Error?e.message:"签到操作未完成")};
 async function load(reset=false){if(!mounted.current)return;const version=generation.current;try{const data=await getSigninStatus(session.token,page);if(version!==generation.current)return;setStatus(data);if(reset||!initialized.current){setDraft(data.settings);initialized.current=true}}catch(e){if(version===generation.current)fail(e)}}
 async function refresh(id=batchId){if(!id)return;const version=generation.current,read=++batchReads.current;try{const data=await getSigninBatch(session.token,id);if(version!==generation.current||read!==batchReads.current)return;setBatch(data);setBatchId(id);setLookup(id);rememberId(storageKey,id);setUncertain(false)}catch(e){if(version===generation.current&&read===batchReads.current)fail(e)}}
 useEffect(()=>{mounted.current=true;generation.current++;initialized.current=false;inFlight.current=false;setStatus(undefined);setDraft(undefined);setBatch(undefined);setReview(undefined);setOptions(initialOptions);setError("");setNotice("");setBusy(false);const id=savedId(storageKey);setBatchId(id);setLookup(id);setUncertain(!!id);void load(true);if(id)void refresh(id);return()=>{mounted.current=false;generation.current++}},[session.token,storageKey]);
 useEffect(()=>{void load()},[page]);
 const active=batch?.items.some(j=>signinActive(j.state))||status?.jobs.some(j=>signinActive(j.state));
 useEffect(()=>{if(!active)return;const timer=setInterval(()=>{void load();if(batchId)void refresh(batchId)},3000);return()=>clearInterval(timer)},[active,batchId,session.token,page]);
 function changed(){revision.current++;setReview(undefined);setNotice("")}
 function edit<K extends keyof SigninSettings>(key:K,value:SigninSettings[K]){setDraft(current=>current?{...current,[key]:value}:current);changed()}
 async function action(run:()=>Promise<void>){if(inFlight.current)return;const version=generation.current;inFlight.current=true;setBusy(true);setError("");try{await run()}catch(e){if(version===generation.current)fail(e)}finally{if(version===generation.current){inFlight.current=false;setBusy(false)}}}
 function toggle(list:string[],id:string,on:boolean){return on?[...new Set([...list,id])]:list.filter(v=>v!==id)}
 async function inspect(){const version=generation.current,editRevision=revision.current;await action(async()=>{const data=await reviewSignin(session.token,mode,selected,{...options,remote:mode==="activate"&&options.remote});if(version===generation.current&&editRevision===revision.current)setReview(data)})}
 async function begin(){const r=review;if(!r)return;await action(async()=>{
  const version=generation.current;setUncertain(true);setReview(undefined);
  if(r.mode==="manual"){batchReads.current++;setBatch(undefined);setBatchId(r.id);setLookup(r.id);rememberId(storageKey,r.id);const data=await startSignin(session.token,r);if(version!==generation.current)return;batchReads.current++;setBatch(data);setUncertain(false);setNotice("本批次已保存，可按原编号刷新查看")}
  else{await activateSignin(session.token,r);if(version!==generation.current)return;setUncertain(false);setNotice("已按核对的设置启用；旧的一次运行和清理开关已消耗，不会因重启再次触发")}
  await load(true)
 })}
 const allSites=[...(status?.sites||[])];for(const id of new Set([...(draft?.sign_sites||[]),...(draft?.special_sites||[])])){if(!allSites.some(s=>s.id===id))allSites.push({id,name:"原站点 "+id+"（当前不存在）",scheme:"http",host:"",path:"",adapter:"",browser:false})}
 async function readChallenge(job:SigninJob){
	const version=generation.current,read=++batchReads.current;const controller=new AbortController();const timer=setTimeout(()=>controller.abort(),15000);
	let data:SigninBatch;try{data=await getSigninBatch(session.token,job.batch,controller.signal)}finally{clearTimeout(timer)}
  if(!mounted.current||version!==generation.current||read!==batchReads.current)return false;
  setBatch(data);setBatchId(data.id);setLookup(data.id);rememberId(storageKey,data.id);
  const current=data.items.find(v=>v.id===job.id);
  return !!current&&current.state==="waiting_input"&&current.runtimeActive&&current.challenge?.nonce===job.challenge?.nonce&&Date.parse(current.challenge?.deadline||"")>Date.now();
 }
 async function sendChallenge(job:SigninJob,values:string[]){
  if(!mounted.current||!job.challenge||inFlight.current)throw new ApiError("请等待上一操作完成后核对本次输入",409);
  const version=generation.current;inFlight.current=true;setBusy(true);setError("");
  try{
   await inputSignin(session.token,job.id,job.challenge.nonce,values);
   if(version===generation.current){void readChallenge(job).catch(e=>{if(version===generation.current)fail(e)});void load();}
  }
  catch(e){if(version===generation.current&&e instanceof ApiError&&e.code===401)fail(e);throw e;}
  finally{if(mounted.current&&version===generation.current){inFlight.current=false;setBusy(false);}}
 }
 const history=(batch?.items||status?.jobs||[]);
 return <section className="signin-manager" aria-label="站点签到管理">
  <header><div><h2>站点签到管理</h2><p>按原站点执行签到，保留每个请求和结果。登录有效、今日已签与本次签到成功分别显示。</p></div><div><button type="button" disabled={busy} onClick={()=>{changed();void load(true)}}>刷新原设置</button>{onClose&&<button type="button" onClick={onClose}>收起</button>}</div></header>
  {error&&<p role="alert" className="form-error">{error}</p>}{notice&&<p role="status">{notice}</p>}
  {!status&&<p>正在读取签到设置与历史…</p>}
  {status&&!status.goOnly&&<p role="alert">当前运行模式仅查看历史，签到执行需要独立运行模式。</p>}
  {status&&!status.installed&&<p>自动签到插件尚未安装。<button type="button" disabled={busy||!status.goOnly} onClick={()=>void action(async()=>{await installSignin(session.token);await load(true)})}>安装自动签到</button></p>}
  {status&&draft&&status.installed&&<>
   <details><summary>原签到设置</summary><form aria-label="签到设置" onSubmit={e=>{e.preventDefault();void action(async()=>{if(!signinSettingsValid(draft))throw new Error("请填写有效的并发数和阈值");await saveSigninSettings(session.token,status.fingerprint,draft);changed();await load(true);setNotice("设置已保存；核对目标后再执行")})}}>
    <fieldset disabled={busy||!status.goOnly}>
     <legend>时间与行为</legend><label><input type="checkbox" checked={draft.enabled} onChange={e=>edit("enabled",e.target.checked)}/>准备启用周期签到</label><label>时间或周期<input value={draft.cron} onChange={e=>edit("cron",e.target.value)} placeholder="08:00、08:00-09:00、23.5 小时或五段 cron"/></label>
     <label>原并发数<input type="number" min="1" max="1000" value={Number.isFinite(draft.queue_cnt)?draft.queue_cnt:""} onChange={e=>edit("queue_cnt",Number(e.target.value))}/></label><p>当前最多同时处理 {status.maxConcurrency} 个站点，其余站点排队。</p>
     <label>已确认失败的重试关键词<input value={draft.retry_keyword} onChange={e=>edit("retry_keyword",e.target.value)}/></label><label><input type="checkbox" checked={draft.notify} onChange={e=>edit("notify",e.target.checked)}/>按现有“站点签到”通知开关发送汇总</label>
     <label><input type="checkbox" checked={draft.onlyonce} onChange={e=>edit("onlyonce",e.target.checked)}/>启用本份设置后运行一次</label><label><input type="checkbox" checked={draft.clean} onChange={e=>edit("clean",e.target.checked)}/>重新核对今日已确认的站点；保留未知动作与旧历史</label>
     <label><input type="checkbox" checked={draft.missed_detection} onChange={e=>edit("missed_detection",e.target.checked)}/>检测漏签</label><label>漏签检测时间段<input value={draft.missed_schedule} onChange={e=>edit("missed_schedule",e.target.value)} placeholder="09:00-23:59"/></label>
     <label>原 CF 优选阈值<input type="number" min="0" max="1000" value={Number.isFinite(draft.auto_cf)?draft.auto_cf:""} onChange={e=>edit("auto_cf",Number(e.target.value))}/></label><p>命中失败阈值后，使用已接管且允许签到触发的 CF 优选设置；同一签到批次只排入一次。</p>
    </fieldset>
    <fieldset disabled={busy||!status.goOnly}><legend>周期签到站点</legend><div className="signin-site-choices">{allSites.map(site=><label key={site.id}><input type="checkbox" checked={draft.sign_sites.includes(site.id)} onChange={e=>edit("sign_sites",toggle(draft.sign_sites,site.id,e.target.checked))}/>{site.name}</label>)}</div></fieldset>
    <fieldset disabled={busy||!status.goOnly}><legend>特殊站点（周期运行时重复核对）</legend><div className="signin-site-choices">{allSites.map(site=><label key={site.id}><input type="checkbox" checked={draft.special_sites.includes(site.id)} onChange={e=>edit("special_sites",toggle(draft.special_sites,site.id,e.target.checked))}/>{site.name}</label>)}</div><p>结果未知、已停止或仍在执行的动作不会自动重做。</p></fieldset>
    <button type="submit" disabled={busy||!status.goOnly}>保存签到设置</button>
   </form></details>
   <section aria-label="当前自动签到"><h3>当前自动签到</h3><p>{status.control.current?"已启用":status.control.enabled?"设置已变化，执行已停止":"未启用"} {status.control.reason}</p>{status.control.id&&<p>原设置编号 <code>{status.control.id}</code></p>}<p>时区：{status.control.location||"尚未核对"}；下次周期签到：{status.control.nextRun||"未安排"}；下次漏签检测：{status.control.nextMissed||"未安排"}{status.control.nextOnce&&<>；运行一次：{status.control.nextOnce}</>}</p>{status.control.enabled&&<button type="button" disabled={busy} onClick={()=>void action(async()=>{await stopSigninControl(session.token,status.control.id);await load();setNotice("已停止自动签到；已发出的请求仍按原记录核对")})}>停止这份自动签到设置</button>}</section>
   <fieldset disabled={busy||!status.goOnly||uncertain}>
    <legend>核对新的执行范围</legend><label><input type="radio" name="signin-mode" checked={mode==="manual"} onChange={()=>{setMode("manual");changed()}}/>本次执行</label><label><input type="radio" name="signin-mode" checked={mode==="activate"} onChange={()=>{setMode("activate");changed()}}/>按已保存设置自动运行</label>
    {mode==="manual"&&<fieldset><legend>本次站点（空选择不执行）</legend><button type="button" onClick={()=>{setSelected(status.settings.sign_sites.filter(id=>status.sites.some(s=>s.id===id)));changed()}}>使用已保存的站点选择</button><div className="signin-site-choices">{status.sites.map(site=><label key={site.id}><input type="checkbox" checked={selected.includes(site.id)} onChange={e=>{setSelected(toggle(selected,site.id,e.target.checked));changed()}}/>{site.name} · {site.host}</label>)}</div></fieldset>}
    {mode==="activate"&&<p>使用已保存的时间、站点、特殊站点、补签和一次运行设置；先保存上方的修改再核对。</p>}
    <fieldset><legend>遇到挑战时使用的原辅助服务</legend>{([ ["ocr","将验证码图片发送到原 OCR 服务"],["douban","用答案文字检索豆瓣并读取公开海报"],["lens","将验证码图片地址发送到 Google Lens"],["ai","将答题题目、选项发送到原 AI 服务"] ] as const).map(([key,label])=><label key={key}><input type="checkbox" checked={options[key]} onChange={e=>{setOptions(v=>({...v,[key]:e.target.checked}));changed()}}/>{label}</label>)}
     {status.savedConfigured&&<label><input type="checkbox" checked={options.savedLogin} onChange={e=>{setOptions(v=>({...v,savedLogin:e.target.checked}));changed()}}/>M-Team 需要登录时使用原保存账户</label>}
     {mode==="activate"&&<label><input type="checkbox" checked={options.remote} onChange={e=>{setOptions(v=>({...v,remote:e.target.checked}));changed()}}/>接收原交互渠道的 /pts 签到命令</label>}
     <p>未选择外部识别时，可在本页处理图片输入；原 52PT／CHDBits 答案缓存缺失时会按原规则随机作答。</p>
    </fieldset>
    <button type="button" disabled={mode==="manual"&&selected.length===0} onClick={()=>void inspect()}>核对签到目标</button>
   </fieldset>
   {review&&<section aria-label="本次签到目标"><h3>{review.mode==="manual"?"本次将访问以下站点":"自动运行将使用以下站点"}</h3><ul>{review.targets.map(t=><li key={t.id}>{t.name} · {t.scheme}://{t.host}{t.path} · {t.adapter}{t.browser?"（浏览器）":""}</li>)}</ul>{review.helpers.length>0&&<><h4>辅助服务和实际发送的数据</h4><ul>{review.helpers.map((h,i)=><li key={i}>{h.kind} · {h.scheme}://{h.host}{h.path} — {h.data}</li>)}</ul></>}{review.remote.length>0&&<><h4>接收 /pts 的渠道</h4><ul>{review.remote.map(c=><li key={c.id}>{c.name} · {c.type} · {c.receiver}；{c.permission}</li>)}</ul><p>接收确认只发给原发送者。其他命令保留输入，等待各自功能迁移。</p></>}{review.settings.notify&&<p>签到汇总按现有渠道的“站点签到”开关发送站点名称和结果，不包含 Cookie 或验证码。</p>}<p>时区 {review.location}；核对有效至 {review.expires}。未知动作不自动重试，浏览器防护需要人工处理。</p><button type="button" disabled={busy} onClick={()=>void begin()}>{review.mode==="manual"?"确认执行本次签到":"确认启用这份设置"}</button></section>}
  </>}
  {uncertain&&<section role="status"><p>上次启动的应答尚未确认。请先刷新原批次或原自动签到设置，不要重复提交。</p><button type="button" disabled={busy} onClick={()=>{void refresh();void load(true)}}>只读核对原记录</button><button type="button" disabled={busy} onClick={()=>{changed();setUncertain(false);setNotice("请核对原站点后再建立新的执行范围；新核对会撤销旧的未启动审核")}}>我已核对，准备新的范围</button></section>}
  <section aria-label="签到批次查询"><h3>批次与历史</h3><label>原批次编号<input value={lookup} onChange={e=>setLookup(e.target.value)} autoComplete="off"/></label><button type="button" disabled={busy||!/^[a-f0-9]{64}$/.test(lookup)} onClick={()=>void refresh(lookup)}>只读查询原批次</button>{batchId&&<p>当前批次 <code>{batchId}</code>{batch&&<button type="button" disabled={busy} onClick={()=>{batchReads.current++;setBatch(undefined);setBatchId("");void load()}}>返回历史列表</button>}</p>}
   <div className="signin-history">{history.map(job=><SigninJobCard key={job.id} job={job} busy={busy} onStop={()=>void action(async()=>{await stopSigninJob(session.token,job.id);if(batchId)await refresh();await load()})} onInput={values=>sendChallenge(job,values)} onRead={()=>readChallenge(job)}/>)}</div>
   {!batch&&<div><button type="button" disabled={page<=1||busy} onClick={()=>setPage(p=>p-1)}>上一页</button><span>第 {page} 页</span><button type="button" disabled={busy||(status?.jobs.length||0)<30} onClick={()=>setPage(p=>p+1)}>下一页</button></div>}
  </section>
  {status?.signals.length? <section><h3>消息与事件记录</h3>{status.signals.map(s=><p key={s.id}>{s.state} · {s.reason} {s.batch&&<button type="button" onClick={()=>void refresh(s.batch)}>查看原批次</button>}</p>)}</section>:null}
  {status?.remote.length? <section><h3>消息接收状态</h3>{status.remote.map(s=><p key={s.id}>{s.name} · {s.mode} · {s.state||"等待启用"} {s.reason}</p>)}</section>:null}
  {status?.dependencies.length? <section><h3>其他功能状态</h3>{status.dependencies.map(d=><p key={d.batch}>{d.plugin} · {d.reason}</p>)}</section>:null}
 </section>
}
function SigninJobCard({job,busy,onStop,onInput,onRead}:{job:SigninJob;busy:boolean;onStop:()=>void;onInput:(v:string[])=>Promise<void>;onRead:()=>Promise<boolean>}){
 return <article aria-label={job.name+"签到记录"}><h4>{job.name} · {labels[job.state]||job.state} {job.outcome.state?" · "+(labels[job.outcome.state]||job.outcome.state):""}</h4><p>{job.outcome.reason}</p><p>{job.day} · 请求 {job.requests} · 动作 {job.actions} · {job.runtimeActive?"执行器在线":"执行器未占用此批次"}</p><p>编号 <code>{job.id}</code></p>{job.outcome.cacheState&&<p>答案缓存：{job.outcome.cacheState==="saved"?"已保存":"保留原内容"}</p>}{job.canStop&&<button type="button" disabled={busy} onClick={onStop}>停止原任务</button>}
  {job.challenge&&<SigninChallengeForm job={job} busy={busy} onInput={onInput} onRead={onRead}/>}
 </article>
}

type InputPhase="idle"|"sending"|"received"|"uncertain"|"checked";
export function SigninChallengeForm({job,busy,onInput,onRead}:{job:SigninJob;busy:boolean;onInput:(values:string[])=>Promise<void>;onRead:()=>Promise<boolean>}){
 const c=job.challenge;
 const [value,setValue]=useState("");const [choices,setChoices]=useState<string[]>([]);
 const [phase,setPhase]=useState<InputPhase>("idle");const [error,setError]=useState("");const [checking,setChecking]=useState(false);const [now,setNow]=useState(Date.now);
 const attempt=useRef(0),sending=useRef(false);
 useEffect(()=>{attempt.current++;sending.current=false;setValue("");setChoices([]);setPhase("idle");setError("");setChecking(false);setNow(Date.now());const timer=setInterval(()=>setNow(Date.now()),1000);return()=>{clearInterval(timer);attempt.current++;};},[c?.nonce]);
 if(!c)return null;
 const values=c.choices.length?choices:[value];const validation=signinInputError(c,values,now);
 const expired=!Number.isFinite(Date.parse(c.deadline))||Date.parse(c.deadline)<=now;
 const label=({captcha:"本次图片验证码",two_step:"本次两步验证码",email:"原站点要求的邮箱",email_code:"本次邮箱验证码"} as Record<string,string>)[c.kind]||"本次输入";
 async function submit(){
  if(!c||busy||sending.current||phase!=="idle")return;
  const reason=signinInputError(c,values);if(reason){setError(reason);return;}
  const version=attempt.current;sending.current=true;setPhase("sending");setError("");const submitted=[...values];setValue("");setChoices([]);
  try{await onInput(submitted);if(version===attempt.current)setPhase("received");}
  catch(e){if(version!==attempt.current)return;if(signinInputRejected(e)){setPhase("idle");setError("输入未被接收，请核对原任务和输入内容");}else{setPhase("uncertain");setError("本次输入应答未确认，不会自动重发。请先只读核对原任务。");}}
  finally{if(version===attempt.current)sending.current=false;}
 }
 async function read(){
  if(checking)return;const version=attempt.current;setChecking(true);
  try{const available=await onRead();if(version===attempt.current&&phase==="uncertain"&&available){setPhase("checked");setError("");}}
  catch{if(version===attempt.current)setError("原任务读取未完成，保留当前输入状态，请稍后只读核对");}
  finally{if(version===attempt.current)setChecking(false);}
 }
 return <form className="signin-challenge" aria-label={job.name+"本次输入"} onSubmit={e=>{e.preventDefault();void submit();}}>
  <p>{c.question||label} · 截止 {c.deadline}</p>
  {c.image&&<img src={c.image} alt="原站点本次验证码"/>}
  {c.choices.length>0?<fieldset disabled={busy||phase!=="idle"||expired}><legend>本次答案</legend>{c.choices.map(choice=><label key={choice.value}><input type={c.multiple?"checkbox":"radio"} name={job.id+":"+c.nonce} checked={choices.includes(choice.value)} onChange={e=>{setError("");setChoices(v=>c.multiple?(e.target.checked?[...v,choice.value]:v.filter(i=>i!==choice.value)):[choice.value]);}}/>{choice.text}</label>)}</fieldset>:<label>{label}<input type={c.kind==="email"?"email":"password"} autoComplete="off" value={value} disabled={busy||phase!=="idle"||expired} maxLength={(c.maxLength??(c.kind==="email"?254:128))*2} onChange={e=>{setValue(e.target.value);setError("");}}/></label>}
  {error&&<p role="alert">{error}</p>}
  {!error&&phase==="idle"&&validation&&(value||choices.length>0||expired)&&<p role="status">{validation}</p>}
  {phase==="received"&&<p role="status">本次输入已接收，请查看原任务结果。</p>}
  {phase==="checked"&&<p role="status">原任务仍在等待这份输入，请确认后重新填写。</p>}
  <p>输入只交给当前任务，不保存到回执；过期或已停止的输入不能再次使用。</p>
  <button type="submit" disabled={busy||phase!=="idle"||!!validation}>{phase==="sending"?"发送中…":"发送本次输入"}</button>
  <button type="button" disabled={busy||checking} onClick={()=>void read()}>只读核对原输入任务</button>
  {phase==="checked"&&!expired&&<button type="button" disabled={busy} onClick={()=>{setValue("");setChoices([]);setError("");setPhase("idle");}}>重新填写尚未接收的输入</button>}
 </form>;
}
