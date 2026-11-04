import { useEffect, useRef, useState } from "react";
import { ApiError, type AuthSession } from "../api/client";
import { activateStatistics, getStatistics, getStatsDetail, getStatsOverview, refreshStatistics, stopStatistics, type StatsOverview, type StatsStatus, type StatsDetail } from "../api/siteStatistics";
import { SiteRankingPanel } from "./SiteRankingDetail";
import "./SiteStatisticsPanel.css";

const size=(v:string)=>{const value=BigInt(v);if(value<1024n)return `${v} B`;let n=Number(value),i=0;const units=["B","KiB","MiB","GiB","TiB","PiB","EiB"];while(n>=1024&&i<units.length-1){n/=1024;i++;}return `${n.toFixed(2)} ${units[i]}`;};
const states:Record<string,string>={queued:"等待执行",running:"刷新中",complete:"已完成",partial:"部分完成",failed:"未完成",stopped:"已停止",unknown:"执行中断，请查看原数据"};
const nonce=()=>Array.from(crypto.getRandomValues(new Uint8Array(32)),n=>n.toString(16).padStart(2,"0")).join("");
export function SiteStatisticsPanel({session,onSessionExpired,onClose,readOnly=false}:{session:AuthSession;onSessionExpired:()=>void;onClose?:()=>void;readOnly?:boolean}){
 const [data,setData]=useState<StatsOverview|StatsStatus>();const [error,setError]=useState("");const [busy,setBusy]=useState(false);const [readMessages,setReadMessages]=useState(false);const [remote,setRemote]=useState(false);const [selected,setSelected]=useState<string[]>([]);const [detail,setDetail]=useState<{name:string;data:StatsDetail}>();const [message,setMessage]=useState("");const request=useRef<string>("");const live=useRef(true);
 const [rankTarget,setRankTarget]=useState<StatsStatus["targets"][number]>();
 function fail(e:unknown){if(e instanceof ApiError&&e.code===401){onSessionExpired();return;}if(live.current)setError(e instanceof Error?e.message:"统计读取失败");}
 async function load(){try{const v=await (readOnly?getStatsOverview(session.token):getStatistics(session.token));if(live.current)setData(v);}catch(e){fail(e);}}
 useEffect(()=>{live.current=true;void load();const timer=readOnly?undefined:window.setInterval(()=>void load(),4000);return()=>{live.current=false;window.clearInterval(timer);};},[session.token,readOnly]);
 async function action(run:()=>Promise<unknown>,text:string){if(busy)return;setBusy(true);setError("");setMessage("");try{await run();setMessage(text);await load();}catch(e){fail(e);}finally{if(live.current)setBusy(false);}}
 const admin=!readOnly&&data&&"jobs" in data?data:undefined;
 const running=!!admin?.jobs.some(j=>j.state==="running"||j.state==="queued");
 return <section className="site-statistics" aria-label="站点用户统计"><header><div><h2>站点用户统计</h2><p>查看上传下载量、等级进度、做种明细和每日历史。</p></div>{onClose&&<button type="button" onClick={onClose}>关闭统计</button>}</header>
 {error&&<p role="alert" className="data-warning">{error}</p>}{message&&<p role="status">{message}</p>}
 {!data?<p>正在读取统计…</p>:<>
 {admin?<div className="site-statistics__controls"><label><input type="checkbox" checked={readMessages} onChange={e=>setReadMessages(e.target.checked)}/>读取未读消息内容（可能将站内消息标记为已读）</label><label><input type="checkbox" checked={remote} onChange={e=>setRemote(e.target.checked)}/>允许原交互渠道的管理员发送 /sta 刷新全部站点</label>
 <p>定时配置：{admin.cron||"未配置"} · {admin.current?"后台刷新已启用":admin.control.enabled?"设置已变化，需要重新启用":"后台刷新未启用"}{admin.current&&admin.control.nextRun?` · 下次执行 ${new Date(admin.control.nextRun).toLocaleString()}`:""}</p>
 {admin.current&&<p>当前后台范围：{admin.control.readMessages?"会读取消息内容":"只读取未读数量"}；{admin.control.remote?"允许 /sta":"未启用 /sta"}。</p>}
 <div className="site-statistics__actions"><button type="button" disabled={busy||running||!admin.goOnly||!admin.targets.length} onClick={()=>void action(async()=>{request.current ||= nonce();await refreshStatistics(session.token,admin.fingerprint,request.current,selected,readMessages);request.current="";},"刷新已登记，请查看下方回执")}>{busy?"处理中…":selected.length?`刷新已选 ${selected.length} 个站点`:"刷新全部统计站点"}</button><button type="button" disabled={busy||!admin.goOnly||!admin.targets.length} onClick={()=>void action(()=>activateStatistics(session.token,admin.fingerprint,admin.control.epoch,readMessages,remote),"后台刷新设置已启用")}>启用以上后台设置</button><button type="button" disabled={busy||!admin.goOnly||(!admin.control.enabled&&!running)} onClick={()=>void action(()=>stopStatistics(session.token,admin.control.epoch),"已停止当前刷新与后台接管")}>停止刷新与后台接管</button><button type="button" disabled={busy} onClick={()=>void load()}>重新读取</button></div>
 {!admin.goOnly&&<p>请在 Go 独立运行模式中执行刷新。</p>}
 </div>:<p><button type="button" disabled={busy} onClick={()=>void load()}>重新读取</button></p>}
 <div className="site-statistics__scroll"><table><thead><tr>{admin&&<th>选择</th>}<th>站点 / 用户</th><th>上传 / 下载</th><th>分享率 / 积分</th><th>做种 / 体积</th><th>未读 / 更新时间</th><th>明细</th></tr></thead>
  <tbody>{data.targets.map(t=>{
    const item=data.items.find(i=>t.url?i.url===t.url:i.site===t.name);
    return <tr key={t.id}>
      {admin&&<td><input type="checkbox" aria-label={`选择 ${t.name}`} checked={selected.includes(t.id)} onChange={e=>setSelected(s=>e.target.checked?[...s,t.id]:s.filter(v=>v!==t.id))}/></td>}
      <td>{t.name}<small>{item?.username||"尚无统计"}{item?.userLevel?` · ${item.userLevel}`:""}</small></td>
      <td>{item?<>{size(item.upload)}<small>{size(item.download)}</small></>:"—"}</td>
      <td>{item?<>{item.ratio.toFixed(3)}<small>{item.bonus.toLocaleString()}</small></>:"—"}</td>
      <td>{item?<>{item.seeding} 个<small>{size(item.seedingSize)}</small></>:"—"}</td>
      <td>{item?<>{item.messageUnread} 条<small>{item.updateAt}</small></>:"—"}</td>
      <td><div className="site-statistics__actions">
        <button type="button" disabled={!item} aria-label={`${t.name} ${t.host} 等级进度`} onClick={()=>setRankTarget(t)}>等级进度</button>
        <button type="button" disabled={busy} onClick={()=>void action(async()=>{const d=await getStatsDetail(session.token,t.id);if(live.current)setDetail({name:t.name,data:d});},"")}>历史与做种</button>
      </div></td>
    </tr>;
  })}</tbody></table></div>
 {!data.targets.length&&<p>暂无参与统计的站点，请先在站点设置中开启“数据统计”。</p>}
 {rankTarget&&<SiteRankingPanel key={rankTarget.id} token={session.token} siteId={rankTarget.id} name={rankTarget.name} onClose={()=>setRankTarget(undefined)} onSessionExpired={onSessionExpired}/>}
 {detail&&<section aria-label={`${detail.name} 统计明细`}><h3>{detail.name} · 历史与做种</h3><button type="button" onClick={()=>setDetail(undefined)}>收起明细</button><p>当前做种明细 {detail.data.seedingInfo.length} 条 · 最近 {Math.min(30,detail.data.history.length)} 个统计日</p><div className="site-statistics__scroll"><table><thead><tr><th>日期</th><th>上传量</th><th>下载量</th><th>积分</th><th>做种</th></tr></thead><tbody>{detail.data.history.slice(-30).map(h=><tr key={h.date}><td>{h.date}</td><td>{size(h.upload)}</td><td>{size(h.download)}</td><td>{h.bonus.toLocaleString()}</td><td>{h.seeding} 个 · {size(h.seedingSize)}</td></tr>)}</tbody></table></div><details><summary>做种分布（做种人数 / 文件大小）</summary><ol>{detail.data.seedingInfo.slice(0,500).map((s,i)=><li key={i}>{s[0]} 人 · {size(s[1])}</li>)}</ol>{detail.data.seedingInfo.length>500&&<p>页面展示前 500 条明细。</p>}</details></section>}
 {admin&&<section aria-label="统计刷新回执"><h3>刷新回执</h3>{admin.jobs.slice(0,20).map(j=><details key={j.id}><summary>{new Date(j.created).toLocaleString()} · {states[j.state]||j.state} · {j.requests} 次请求</summary><p className="site-statistics__receipt">编号 {j.id}</p><p>读取消息内容：{j.readMessages?"已允许":"未允许"}；消息内容请求 {j.messageRequests} 次。</p><ul>{j.results.map(r=><li key={r.siteId}>{r.name}：{r.reason}</li>)}</ul></details>)}{!admin.jobs.length&&<p>暂无刷新记录。</p>}</section>}
 </>}
 </section>;
}
