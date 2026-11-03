import { useEffect, useState } from "react";
import { ApiError } from "../api/client";
import { activateCF, closeCF, getCF, getCFJob, reconcileCF, reviewCF, runCF, stopCF, type CFStatus } from "../api/cloudflare";

type Props={token:string;onSessionExpired:()=>void};
const labels:Record<string,string>={queued:"已排队",running:"测速中",applying:"核对 Hosts 更新",complete:"已确认完成",failed:"未完成",blocked:"原设置已变化",unknown:"结果待核对",closed:"本地占用已关闭"};
export function CloudflareManager({token,onSessionExpired}:Props){
 const [status,setStatus]=useState<CFStatus>();const [review,setReview]=useState<CFStatus>();const [mode,setMode]=useState<"activate"|"run">("activate");const [nonce,setNonce]=useState("");const [pending,setPending]=useState("");const [allowSignin,setAllowSignin]=useState(false);const [confirm,setConfirm]=useState(false);const [busy,setBusy]=useState(false);const [message,setMessage]=useState("");
 const error=(e:unknown)=>{if(e instanceof ApiError&&(e.code===401||e.code===403))onSessionExpired();setMessage(e instanceof Error?e.message:"优选应答未确认");};
 const load=async()=>{try{setStatus(await getCF(token));}catch(e){error(e);}};
 useEffect(()=>{let active=true;getCF(token).then(v=>{if(active)setStatus(v);}).catch(e=>{if(active)error(e);});return()=>{active=false;};},[token]);
 const action=async(fn:()=>Promise<unknown>)=>{setBusy(true);setMessage("");try{await fn();await load();}catch(e){error(e);}finally{setBusy(false);}};
 const prepare=(kind:"activate"|"run")=>action(async()=>{const id=crypto.randomUUID();const value=await reviewCF(token,id);setReview(value);setMode(kind);setNonce(id);setAllowSignin(value.fromSignin);setConfirm(false);});
 const execute=()=>action(async()=>{if(!review||!confirm)return;if(mode==="activate"){await activateCF(token,review.fingerprint,allowSignin);setMessage("已接管此配置，请查看下次运行时间。");}else{if(!review.requestID)throw new Error("原请求编号缺失，请重新核对");setPending(review.requestID);const job=await runCF(token,review.fingerprint,nonce);if(job.id!==review.requestID)throw new Error("应答不是原优选任务");setMessage("原优选任务已排队。");}setReview(undefined);setConfirm(false);});
 return <section className="settings-panel" aria-label="Cloudflare IP 优选管理">
  <header className="services-heading"><div><h2>Cloudflare IP 优选</h2><p>按当前配置测速，核对结果后更新自定义 Hosts。</p></div><button type="button" className="secondary-button" disabled={busy} onClick={()=>void load()}>刷新回执</button></header>
  {message&&<p role="status">{message}</p>}
  {!status?<p>正在读取优选配置…</p>:<>
   {!status.ready&&<p>请先保存优选配置并启用自定义 Hosts。服务器需配置可写的 Hosts 目标文件。</p>}
   <p>当前 IP：{status.values.cf_ip||"尚未配置"} · 版本：{status.values.version||"尚未核对"} · {status.current?"已接管":status.enabled?"原设置已变化，需重新核对":"未接管"}</p>
   <p>下次周期：{status.nextRun||"未安排"} · 单次运行：{status.nextOnce||"未安排"}</p>
   <div className="services-heading__actions"><button type="button" disabled={busy||!status.ready} onClick={()=>void prepare("activate")}>核对并接管设置</button><button type="button" disabled={busy||!status.current} onClick={()=>void prepare("run")}>核对手动测速</button><button type="button" className="secondary-button" disabled={busy||!status.enabled} onClick={()=>void action(()=>stopCF(token))}>停止优选</button></div>
   {pending&&<p>本次请求：<code>{pending}</code> <button type="button" disabled={busy} onClick={()=>void action(async()=>{const j=await getCFJob(token,pending);setMessage(`原回执：${labels[j.state]||j.state}，${j.reason||"请稍后刷新"}`);})}>查询这次回执</button></p>}
   {review&&<div className="delete-warning" aria-label="优选执行核对"><h3>{mode==="activate"?"核对未来优选范围":"核对本次测速"}</h3><p>当前目标：{review.values.cf_ip}；地址族：{[review.values.ipv4?"IPv4":"",review.values.ipv6?"IPv6":""].filter(Boolean).join("、")}</p><p>周期：{review.values.cron||"无"}；单次开关：{review.values.onlyonce?"接管后三秒运行一次":"关闭"}</p><p>高级参数：{review.values.additional_args||"默认"}；{review.values.re_install?"运行时重新下载安装包":"保留可用安装并检查更新"}</p><p>会从官方发布地址读取或下载原生测速程序，运行后更新对应的自定义 Hosts 映射。中断后的未知结果保留原回执。</p>
    {mode==="activate"&&<label><input type="checkbox" checked={allowSignin} onChange={e=>setAllowSignin(e.target.checked)}/>允许签到失败达到原阈值时触发一次优选</label>}
    <label><input type="checkbox" checked={confirm} onChange={e=>setConfirm(e.target.checked)}/>我已核对以上配置，允许{mode==="activate"?"未来的周期、单次及所选触发":"本次手动测速和 Hosts 更新"}</label>
    <button type="button" disabled={busy||!confirm} onClick={()=>void execute()}>确认{mode==="activate"?"接管":"本次执行"}</button><button type="button" className="secondary-button" disabled={busy} onClick={()=>setReview(undefined)}>取消</button>
   </div>}
   <h3>优选历史</h3>{status.jobs.length===0?<p>暂无原生优选回执。</p>:<div className="table-scroll"><table><thead><tr><th>时间</th><th>结果</th><th>IP</th><th>回执</th></tr></thead><tbody>{status.jobs.map(j=><tr key={j.id}><td>{j.updated}</td><td>{labels[j.state]||j.state}<p>{j.reason}</p></td><td>{j.beforeIP&&`${j.beforeIP} → `}{j.afterIP||j.bestIP||"待核对"}</td><td><code>{j.id}</code>{j.canReconcile&&<button type="button" disabled={busy} onClick={()=>void action(()=>reconcileCF(token,j.id))}>只读核对文件结果</button>}{j.canClose&&<button type="button" disabled={busy} onClick={()=>{if(window.confirm("仅关闭这条本地未决占用，保留历史和文件；不会重新测速或写入。是否继续？"))void action(()=>closeCF(token,j.id));}}>关闭本地未决占用</button>}</td></tr>)}</tbody></table></div>}
  </>}
 </section>;
}
