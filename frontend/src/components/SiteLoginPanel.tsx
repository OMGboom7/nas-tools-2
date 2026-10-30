import {useEffect,useRef,useState} from "react";
import {ApiError,type AuthSession,type SiteSummary} from "../api/client";
import {getSiteLoginBatch,getSiteLoginHistory,getSiteLoginStatus,reviewSiteLogin,siteLoginActive,startSiteLogin,stopSiteLogin,submitSiteLoginInput,type SiteLoginBatch,type SiteLoginJob,type SiteLoginReview,type SiteLoginStatus} from "../api/siteLogin";
import "./SiteLoginPanel.css";

type Props={session:AuthSession;sites:SiteSummary[];onClose:()=>void;onSaved:()=>void;onSessionExpired:()=>void};
const stateNames:Record<string,string>={queued:"排队",running:"处理中",waiting_input:"等待本次验证信息",completed:"已核验并保存",failed:"登录未确认",blocked:"来源或权限不再匹配",uncertain:"结果待核实",stopped:"本地已停止"};
export function SiteLoginPanel({session,sites,onClose,onSaved,onSessionExpired}:Props){
 const [status,setStatus]=useState<SiteLoginStatus>();
 const [selected,setSelected]=useState(()=>sites.map(s=>s.id));
 const [username,setUsername]=useState(""),[password,setPassword]=useState(""),[twoStep,setTwoStep]=useState("");
 const [useSaved,setUseSaved]=useState(false),[remember,setRemember]=useState(false),[ocr,setOCR]=useState(false);
 const [review,setReview]=useState<SiteLoginReview>();
 const [batch,setBatch]=useState<SiteLoginBatch>();
 const [batchId,setBatchId]=useState("");
 const [history,setHistory]=useState<SiteLoginJob[]>([]),[page,setPage]=useState(1);
 const [busy,setBusy]=useState(false),[error,setError]=useState(""),[uncertain,setUncertain]=useState(false);
 const savedBatch=useRef("");
 function failure(e:unknown){
  if(e instanceof ApiError&&e.code===401){onSessionExpired();return;}
  setError(e instanceof Error?e.message:"站点登录请求未完成");
 }
 async function loadHistory(next=page){
  try{const data=await getSiteLoginHistory(session.token,next);setHistory(data.items);setPage(next);}catch(e){failure(e);}
 }
 useEffect(()=>{void getSiteLoginStatus(session.token).then(setStatus).catch(failure);void loadHistory(1);},[session.token]);
 async function refresh(){
  if(!batchId)return;
  try{const value=await getSiteLoginBatch(session.token,batchId);setBatch(value);setError("");setUncertain(false);}catch(e){failure(e);}
 }
 useEffect(()=>{
  if(!batchId)return;
  let active=true;let timer:ReturnType<typeof setTimeout>|undefined;
  const poll=async()=>{
   try{
    const value=await getSiteLoginBatch(session.token,batchId);if(!active)return;setBatch(value);
    if(value.items.some(i=>siteLoginActive(i.state)&&i.runtimeActive)||value.rememberState==="pending"&&value.items.some(i=>i.runtimeActive))timer=setTimeout(()=>void poll(),1500);
    if(value.items.every(i=>!siteLoginActive(i.state))&&savedBatch.current!==value.id){savedBatch.current=value.id;onSaved();void loadHistory();}
   }catch(e){if(active)failure(e);}
  };
  void poll();return()=>{active=false;if(timer)clearTimeout(timer);};
 },[session.token,batchId]);
 function changed(){setReview(undefined);}
 async function inspect(){
  setBusy(true);setError("");
  try{setReview(await reviewSiteLogin(session.token,{siteIds:selected,useSaved,remember,ocr}));}catch(e){failure(e);}finally{setBusy(false);}
 }
 async function begin(){
  if(!review||busy)return;setBusy(true);setError("");setUncertain(false);const originalId=review.id;
  try{
   const value=await startSiteLogin(session.token,{scopeToken:review.scopeToken,username:useSaved?"":username,password:useSaved?"":password,twoStepCode:twoStep,confirm:true});
   if(value.id!==originalId)throw new ApiError("应答不是原确认批次，请只读查询",502);
   setBatch(value);setBatchId(value.id);setPassword("");setTwoStep("");setReview(undefined);
  }catch(e){
   setReview(undefined);setPassword("");setTwoStep("");
   try{const original=await getSiteLoginBatch(session.token,originalId);setBatch(original);setBatchId(originalId);setUncertain(false);}
   catch{const known=e instanceof ApiError&&[400,403,409,501].includes(e.code??0);setUncertain(!known);if(!known)setBatchId(originalId);}
   setError("请核对原批次 "+originalId+"；刷新不会重发登录。"+(e instanceof Error?e.message:"提交应答未确认"));
   if(e instanceof ApiError&&e.code===401)onSessionExpired();
  }finally{setBusy(false);}
 }
 const running=batch?.items.some(i=>siteLoginActive(i.state)&&i.runtimeActive);
 return <div className="editor-backdrop" role="presentation"><section className="site-login-panel" aria-label="站点登录与 Cookie 更新">
  <header><div><p className="eyebrow">SITE LOGIN</p><h2>登录并更新站点会话</h2></div><button type="button" aria-label="关闭站点登录" onClick={onClose}>×</button></header>
  <p>只向下方核对的原站点发送账户信息。确认登录成功后更新该站点 Cookie 和浏览器标识；原请求中断或结果未知时不会自动重发。</p>
  {error&&<p role="alert" className="form-error">{error}</p>}
  {!status&&<p>正在读取登录配置…</p>}
  {status&&(!status.goOnly||!status.browserConfigured)&&<p role="alert">当前需要 Go 独立运行模式和可执行的 Chromium／Chrome。历史回执仍可只读查看。</p>}
  {status&&!running&&!uncertain&&<fieldset disabled={busy||!status.goOnly||!status.browserConfigured}>
   <legend>本次登录</legend>
   <fieldset><legend>选择站点（最多 {status.maxSites} 个）</legend>{sites.map(site=><label key={site.id}><input type="checkbox" checked={selected.includes(site.id)} onChange={e=>{setSelected(current=>e.target.checked?[...current,site.id]:current.filter(id=>id!==site.id));changed();}}/>{site.name} · {site.host}</label>)}</fieldset>
   {status.savedConfigured&&<label><input type="checkbox" checked={useSaved} onChange={e=>{setUseSaved(e.target.checked);changed();}}/>使用原来保存的站点账户（不回显密码）</label>}
   {!useSaved&&<><label>站点用户名<input autoComplete="off" value={username} onChange={e=>{setUsername(e.target.value);changed();}}/></label><label>站点密码<input type="password" autoComplete="new-password" value={password} onChange={e=>{setPassword(e.target.value);changed();}}/></label></>}
   <label>本次两步验证码（可选）<input type="password" autoComplete="off" value={twoStep} onChange={e=>{setTwoStep(e.target.value);changed();}} placeholder="留空时按原站点请求输入"/></label>
   <label><input type="checkbox" checked={remember} onChange={e=>{setRemember(e.target.checked);changed();}}/>全部登录成功后记住本次用户名和密码；本次验证码不写入保存账户</label>
   <label><input type="checkbox" checked={ocr} onChange={e=>{setOCR(e.target.checked);changed();}}/>遇图片验证码时，发送图片到原配置的 OCR 服务识别</label>
   <button type="button" className="secondary-button" disabled={selected.length===0||selected.length>status.maxSites||!useSaved&&(!username||!password)} onClick={()=>void inspect()}>核对登录目标</button>
   {review&&<section aria-label="本次登录目标"><h3>将登录以下原站点</h3><ul>{review.targets.map(t=><li key={t.id}>{t.name} · {t.scheme}://{t.host}{t.path}</li>)}</ul>
    {review.ocrReceivers.length>0&&<><h4>验证码图片接收方</h4><ul>{review.ocrReceivers.map((t,i)=><li key={i}>{t.kind} · {t.scheme}://{t.host}{t.path}</li>)}</ul><p>百度未返回文字时可使用列出的原服务端；不明应答会停止。服务可能消耗配额。</p></>}
    <p>核对有效至 {review.expires}。需要人工浏览器验证的页面会停止，不自动点击验证控件。</p>
    <button type="button" className="primary-button" onClick={()=>void begin()} disabled={busy}>登录并更新所选站点 Cookie</button>
   </section>}
  </fieldset>}
  {batchId&&<section aria-label="原登录批次"><h3>原批次</h3><code>{batchId}</code><button type="button" onClick={()=>void refresh()}>只读刷新原批次</button>
   {batch&&batch.rememberState!=="not_requested"&&<p>保存账户：{batch.rememberState} {batch.rememberReason}</p>}
   {batch?.items.map(item=><SiteLoginReceipt key={item.id} token={session.token} item={item} refresh={refresh} failure={failure}/>)}
  </section>}
  <section aria-label="站点登录历史"><h3>原站点登录回执</h3><p>刷新与打开回执不会重新登录。</p><button type="button" onClick={()=>void loadHistory()}>只读刷新登录回执</button>
   {history.map(item=><article key={item.id}><strong>{item.name} · {stateNames[item.state]||item.state}</strong><p><code>{item.id}</code> · {item.updated}</p><button type="button" onClick={()=>{setBatchId(item.batch);setUncertain(false);}}>打开原批次</button></article>)}
   <button type="button" disabled={page<=1} onClick={()=>void loadHistory(page-1)}>上一页</button><span>第 {page} 页</span><button type="button" disabled={history.length<30} onClick={()=>void loadHistory(page+1)}>下一页</button>
  </section>
  <footer><p>关闭面板不会撤回已发送的站点请求；可通过原回执停止本地后续处理。</p><button type="button" onClick={onClose}>返回站点列表</button></footer>
 </section></div>;
}
function SiteLoginReceipt({token,item,refresh,failure}:{token:string;item:SiteLoginJob;refresh:()=>Promise<void>;failure:(e:unknown)=>void}){
 const [value,setValue]=useState(""),[reason,setReason]=useState(""),[busy,setBusy]=useState(false);
 const [sent,setSent]=useState("");
 const c=item.challenge;
 useEffect(()=>setValue(""),[c?.nonce]);
 async function input(){
  if(!c||busy||sent===c.nonce)return;setBusy(true);setSent(c.nonce);const code=value;setValue("");
  try{await submitSiteLoginInput(token,item.id,c.nonce,code);await refresh();}catch(e){if(e instanceof ApiError&&e.code===400)setSent("");failure(e);}finally{setBusy(false);}
 }
 async function stop(){setBusy(true);try{await stopSiteLogin(token,item.id,reason);await refresh();}catch(e){failure(e);}finally{setBusy(false);}}
 const labels:Record<string,string>={captcha:"图片验证码",two_step:"本次两步验证码",email:"站点要求的邮箱地址",email_code:"本次邮箱验证码"};
 return <article className="site-login-receipt"><h4>{item.name} · {stateNames[item.state]||item.state}</h4><p>{item.host}</p><code>{item.id}</code><p>{item.reason} · {item.updated}</p><p>页面与服务请求 {item.requests} 次，浏览器登录／发邮件动作 {item.actions} 次。{item.loginState==="existing"?"复用了原站点的有效登录态。":""}</p>
  {siteLoginActive(item.state)&&!item.runtimeActive&&<p role="status">原浏览器不在本进程。可核对回执并停止本地记录；不会恢复或重发登录。</p>}
  {c&&<div aria-label={labels[c.kind]}>{c.image&&<img src={c.image} alt="原站点本次验证码" style={{maxWidth:320,maxHeight:180}}/>}<label>{labels[c.kind]}<input type={c.kind==="email"?"email":"text"} autoComplete="off" value={value} onChange={e=>setValue(e.target.value)} disabled={busy||sent===c.nonce}/></label><p>有效至 {c.deadline}。输入仅交给原请求，不写入回执。</p><button type="button" onClick={()=>void input()} disabled={busy||!value||sent===c.nonce}>{sent===c.nonce?"输入已发送，请只读刷新":"提交本次验证信息"}</button></div>}
  {item.canStop&&<div><label>停止本地后续处理的说明<input value={reason} onChange={e=>setReason(e.target.value)} maxLength={512}/></label><button type="button" disabled={busy||reason.length<10} onClick={()=>void stop()}>停止本地处理</button><p>已发送的登录或邮件不能撤回，原回执保留。</p></div>}
 </article>;
}
