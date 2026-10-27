import {useEffect,useRef,useState} from "react";
import {ApiError} from "../api/client";
import {getFileSelection,getFileSelections,reviewFileSelection,recoverFileSelection,type FileSelection,type FileReview} from "../api/subscriptionFiles";
const nextLabels:Record<FileReview["next"],string>={none:"没有可执行的后续步骤","adopt-add":"仅核实原暂停添加","adopt-unwanted":"仅核实原取消选择","adopt-selected":"仅核实原文件选择","adopt-settings":"仅核实原下载设置",settings:"仅执行尚未发出的下载设置",select:"仅执行尚未发出的文件选择",start:"仅启动原已选文件任务",acknowledge:"仅核实原任务就绪",record:"仅登记原订阅接收回执"};
export function SubscriptionFileSelectionManager({token,lastId,onChanged,onSessionExpired}:{token:string;lastId:string;onChanged:()=>void;onSessionExpired:()=>void}){
 const [lookup,setLookup]=useState(""),[receipt,setReceipt]=useState<FileSelection|null>(null),[review,setReview]=useState<FileReview|null>(null),[recent,setRecent]=useState<FileSelection[]>([]),[page,setPage]=useState(1),[busy,setBusy]=useState(false),[error,setError]=useState(""),[notice,setNotice]=useState("");
 const [confirm,setConfirm]=useState(false),[files,setFiles]=useState(false),[settings,setSettings]=useState(false),[start,setStart]=useState(false),[record,setRecord]=useState(false);const epoch=useRef(0),flight=useRef(false);
 function clearConsent(){setReview(null);setConfirm(false);setFiles(false);setSettings(false);setStart(false);setRecord(false)}
 function showReceipt(value:FileSelection){setReceipt(value);setRecent(rows=>rows.map(entry=>entry.id===value.id?value:entry))}
 useEffect(()=>{epoch.current++;flight.current=false;setBusy(false);clearConsent();setLookup("");setReceipt(null);setRecent([]);setError("");setNotice("")},[token]);
 useEffect(()=>{if(/^[a-f0-9]{32}$/.test(lastId)){setLookup(lastId);setReceipt(null);clearConsent()}},[lastId]);
 async function run(action:()=>Promise<void>,mutation=false){if(flight.current)return;const v=++epoch.current;flight.current=true;setBusy(true);setError("");setNotice("");try{await action()}catch(e){if(v!==epoch.current)return;clearConsent();if(e instanceof ApiError&&e.code===401)onSessionExpired();else setError((e instanceof Error?e.message:"原回执未核实")+(mutation?"。保留原编号，只读查回，不要重发。":""))}finally{if(v===epoch.current){setBusy(false);flight.current=false}}}
 async function find(id=lookup){const v=epoch.current;clearConsent();const out=await getFileSelection(token,id);if(v===epoch.current){setLookup(out.id);showReceipt(out)}}
 async function list(){const v=epoch.current;clearConsent();const out=await getFileSelections(token,page);if(v===epoch.current)setRecent(out)}
 async function inspect(){const v=epoch.current;clearConsent();const out=await reviewFileSelection(token,lookup);if(v===epoch.current){setReview(out);showReceipt(out.receipt)}}
 async function recover(){if(!review)return;const v=epoch.current,saved=review;setLookup(saved.receipt.id);clearConsent();const out=await recoverFileSelection(token,saved,confirm,{files,settings,start,record});if(v===epoch.current){showReceipt(out);setNotice("本阶段回执已保存。没有重新添加种子；继续前请重新审核。文件已选中或任务已启动不代表下载完成。");onChanged()}}
 return <section className="fuzzy-subscription-panel" aria-label="订阅种子文件选择与恢复"><h2>订阅种子文件选择与恢复</h2>
  <p>缺集交集不是整包下载授权。整季包与需要拆选的资源都先核验完整种子文件清单，暂停添加，只选择能完整对应目标集的媒体与同名字幕，再核验后启动；缺少的集数不会按标题补齐。其他文件不选择，跨季订阅协调仍待迁移。</p>
  <p>添加、下载设置、取消选择、选择和启动分别保存意图；失败、重启及未知回执不自动重发。只按原下载器、任务标记、完整文件清单和原规则核实，不能借此接管其他任务或清除未知预留。</p>
  <label>回执页码<input type="number" min={1} max={10000} value={page} disabled={busy} onChange={e=>setPage(Number(e.target.value))}/></label><button type="button" disabled={busy} onClick={()=>void run(list)}>只读查看本页 30 条</button>
  <label>原文件选择稳定编号<input disabled={busy} value={lookup} onChange={e=>{setLookup(e.target.value);setReceipt(null);clearConsent()}}/></label>
  <button type="button" disabled={busy||!/^[a-f0-9]{32}$/.test(lookup)} onClick={()=>void run(()=>find())}>按编号只读查回</button><button type="button" disabled={busy||!/^[a-f0-9]{32}$/.test(lookup)} onClick={()=>void run(inspect)}>只读核对原任务与下一阶段</button>
  {receipt&&<section aria-label="原文件选择回执"><h3>{receipt.title}</h3><p>稳定编号：{receipt.id} · 原订阅 TV/{receipt.subscriptionId} · 原下载器 {receipt.downloaderType}/{receipt.downloaderId}</p><p>阶段：{receipt.state} · 订阅回执：{receipt.sourceRecorded?"已登记":"尚未登记"} · 目标 S{receipt.season} 集 {receipt.episodes.join(", ")}</p><p>只有实际选择已核验、所需启动已有正向回执，才允许登记订阅缺集进度；不代表文件已下载或入库。</p><details><summary>完整原文件清单（{receipt.files.length}）</summary><ol>{receipt.files.map(f=><li key={f.index}>{f.path} · {f.size} 字节 · {f.selected?"本次选择":"不选择"}{f.padding?"（填充）":""}</li>)}</ol></details></section>}
  {review&&<section aria-label="原文件选择审核"><p>下一阶段：{nextLabels[review.next]}</p>{review.next!=="none"&&<>
   <label><input type="checkbox" disabled={busy} checked={confirm} onChange={e=>setConfirm(e.target.checked)}/>确认只处理原编号及完整原范围，不重新添加、不删除文件、不释放预留。</label>
   {review.next==="select"&&<label><input type="checkbox" checked={files} disabled={busy} onChange={e=>setFiles(e.target.checked)}/>独立确认仅执行尚未发出的选文件操作；未选择文件保持不下载。</label>}
   {review.next==="settings"&&<label><input type="checkbox" checked={settings} disabled={busy} onChange={e=>setSettings(e.target.checked)}/>独立确认原下载设置，不修改全局或放宽 PT 义务。</label>}
   {review.next==="start"&&<label><input type="checkbox" checked={start} disabled={busy} onChange={e=>setStart(e.target.checked)}/>独立确认启动原已选文件任务，会产生下载流量。</label>}
   {review.next==="record"&&<label><input type="checkbox" checked={record} disabled={busy} onChange={e=>setRecord(e.target.checked)}/>独立确认核实原订阅接收记账，不发起下载。</label>}
   <button type="button" disabled={busy||!confirm||review.next==="select"&&!files||review.next==="settings"&&!settings||review.next==="start"&&!start||review.next==="record"&&!record} onClick={()=>void run(recover,true)}>{nextLabels[review.next]}</button>
  </>}</section>}
  {recent.length>0&&<details><summary>本页文件选择回执</summary><ul>{recent.map(p=><li key={p.id}>{p.title} · {p.state} · {p.id} <button type="button" disabled={busy} onClick={()=>void run(()=>find(p.id))}>只读查回</button></li>)}</ul></details>}
  {notice&&<p role="status">{notice}</p>}{error&&<p role="alert">{error}</p>}
 </section>;
}
