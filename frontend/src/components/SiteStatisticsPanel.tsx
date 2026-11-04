import { useEffect, useMemo, useRef, useState } from "react";
import { ApiError, type AuthSession } from "../api/client";
import { activateStatistics, getStatistics, getStatsDetail, getStatsOverview, refreshStatistics, stopStatistics, type StatsOverview, type StatsStatus, type StatsDetail } from "../api/siteStatistics";
import { SiteRankingPanel } from "./SiteRankingDetail";
import { SiteStatisticsHistory } from "./SiteStatisticsHistory";
import { SiteStatisticsShare } from "./SiteStatisticsShare";
import { SiteStatisticsTraffic } from "./SiteStatisticsTraffic";
import { statsRatio, statsRows, statsSize, statsTotals } from "../utils/statisticsView";
import "./SiteStatisticsPanel.css";

const states:Record<string,string>={queued:"等待执行",running:"刷新中",complete:"已完成",partial:"部分完成",failed:"未完成",stopped:"已停止",unknown:"执行中断，请查看原数据"};
const nonce=()=>Array.from(crypto.getRandomValues(new Uint8Array(32)),n=>n.toString(16).padStart(2,"0")).join("");
export function SiteStatisticsPanel({session,onSessionExpired,onClose,readOnly=false}:{session:AuthSession;onSessionExpired:()=>void;onClose?:()=>void;readOnly?:boolean}){
 const [data,setData]=useState<StatsOverview|StatsStatus>();const [error,setError]=useState("");const [busy,setBusy]=useState(false);const [readMessages,setReadMessages]=useState(false);const [remote,setRemote]=useState(false);const [selected,setSelected]=useState<string[]>([]);const [detail,setDetail]=useState<{name:string;data:StatsDetail}>();const [message,setMessage]=useState("");const request=useRef<string>("");const live=useRef(true);
 const [rankTarget,setRankTarget]=useState<StatsStatus["targets"][number]>();
 const [showShare,setShowShare]=useState(false);
 const [trafficRevision,setTrafficRevision]=useState(0);
 function fail(e:unknown){if(e instanceof ApiError&&e.code===401){onSessionExpired();return;}if(live.current)setError(e instanceof Error?e.message:"统计读取失败");}
 async function load(){try{const v=await (readOnly?getStatsOverview(session.token):getStatistics(session.token));if(live.current)setData(v);}catch(e){fail(e);}}
 function reload(){setTrafficRevision(v=>v+1);void load();}
 useEffect(()=>{live.current=true;void load();const timer=readOnly?undefined:window.setInterval(()=>void load(),4000);return()=>{live.current=false;window.clearInterval(timer);};},[session.token,readOnly]);
 async function action(run:()=>Promise<unknown>,text:string){if(busy)return;setBusy(true);setError("");setMessage("");try{await run();setMessage(text);await load();}catch(e){fail(e);}finally{if(live.current)setBusy(false);}}
 const admin=!readOnly&&data&&"jobs" in data?data:undefined;
 const running=!!admin?.jobs.some(j=>j.state==="running"||j.state==="queued");
 const finishedRefreshes=admin?.jobs.filter(j=>j.state!=="queued"&&j.state!=="running").map(j=>j.id+":"+j.updated).join("|");
 const trafficKey=`${trafficRevision}:${finishedRefreshes||""}`;
 const rows=useMemo(()=>data?statsRows(data):[],[data]);
 const totals=useMemo(()=>statsTotals(rows),[rows]);
 return <section className="site-statistics" aria-label="站点用户统计"><header><div><h2>站点用户统计</h2><p>查看上传下载量、等级进度、做种明细和每日历史。</p></div>{onClose&&<button type="button" onClick={onClose}>关闭统计</button>}</header>
 {error&&<p role="alert" className="data-warning">{error}</p>}{message&&<p role="status">{message}</p>}
 {!data?<p>正在读取统计…</p>:<>
 {admin?<div className="site-statistics__controls"><label><input type="checkbox" checked={readMessages} onChange={e=>setReadMessages(e.target.checked)}/>读取未读消息内容（可能将站内消息标记为已读）</label><label><input type="checkbox" checked={remote} onChange={e=>setRemote(e.target.checked)}/>允许原交互渠道的管理员发送 /sta 刷新全部站点</label>
 <p>定时配置：{admin.cron||"未配置"} · {admin.current?"后台刷新已启用":admin.control.enabled?"设置已变化，需要重新启用":"后台刷新未启用"}{admin.current&&admin.control.nextRun?` · 下次执行 ${new Date(admin.control.nextRun).toLocaleString()}`:""}</p>
 {admin.current&&<p>当前后台范围：{admin.control.readMessages?"会读取消息内容":"只读取未读数量"}；{admin.control.remote?"允许 /sta":"未启用 /sta"}。</p>}
 <div className="site-statistics__actions"><button type="button" disabled={busy||running||!admin.goOnly||!admin.targets.length} onClick={()=>void action(async()=>{request.current ||= nonce();await refreshStatistics(session.token,admin.fingerprint,request.current,selected,readMessages);request.current="";},"刷新已登记，请查看下方回执")}>{busy?"处理中…":selected.length?`刷新已选 ${selected.length} 个站点`:"刷新全部统计站点"}</button><button type="button" disabled={busy||!admin.goOnly||!admin.targets.length} onClick={()=>void action(()=>activateStatistics(session.token,admin.fingerprint,admin.control.epoch,readMessages,remote),"后台刷新设置已启用")}>启用以上后台设置</button><button type="button" disabled={busy||!admin.goOnly||(!admin.control.enabled&&!running)} onClick={()=>void action(()=>stopStatistics(session.token,admin.control.epoch),"已停止当前刷新与后台接管")}>停止刷新与后台接管</button><button type="button" disabled={busy} onClick={reload}>重新读取</button></div>
 {!admin.goOnly&&<p>请在 Go 独立运行模式中执行刷新。</p>}
 </div>:<p><button type="button" disabled={busy} onClick={reload}>重新读取</button></p>}
 {rows.some(row=>row.item)&&<dl className="site-stats-totals" aria-label="站点统计总量">
   <div><dt>总上传量</dt><dd>{statsSize(totals.upload)}</dd></div>
   <div><dt>总下载量</dt><dd>{statsSize(totals.download)}</dd></div>
   <div><dt>总做种数</dt><dd>{totals.seeding.toLocaleString("zh-CN")}</dd></div>
   <div><dt>总做种体积</dt><dd>{statsSize(totals.seedingSize)}</dd></div>
 </dl>}
 {rows.length>0&&<SiteStatisticsTraffic token={session.token} refreshKey={trafficKey} onSessionExpired={onSessionExpired}/>}
 <header className="site-stats-list-heading"><h3>站点数据</h3>
   <button type="button" disabled={!rows.some(row=>row.item)} aria-expanded={showShare} onClick={()=>setShowShare(v=>!v)}>导出统计 PNG</button>
 </header>
 {showShare&&<SiteStatisticsShare rows={rows} storageKey={`nastool.stats-share.v1:${session.user.userid}`} onClose={()=>setShowShare(false)}/>}
 <div className="site-statistics__scroll"><table><thead><tr>{admin&&<th>选择</th>}<th>站点 / 用户</th><th>上传 / 下载</th><th>分享率 / 积分</th><th>做种 / 体积</th><th>未读 / 更新时间</th><th>明细</th></tr></thead>
  <tbody>{rows.map(t=>{
    const item=t.item;
    return <tr key={t.id}>
      {admin&&<td><input type="checkbox" aria-label={`选择 ${t.name}`} checked={selected.includes(t.id)} onChange={e=>setSelected(s=>e.target.checked?[...s,t.id]:s.filter(v=>v!==t.id))}/></td>}
      <td>{t.name}{t.host&&<small>{t.host}</small>}<small>{item?.username||"尚无统计"}{item?.userLevel?` · ${item.userLevel}`:""}</small></td>
      <td>{item?<>{statsSize(item.upload)}<small>{statsSize(item.download)}</small></>:"—"}</td>
      <td>{item?<>{statsRatio(item.ratio,3)}<small>{item.bonus.toLocaleString()}</small></>:"—"}</td>
      <td>{item?<>{item.seeding} 个<small>{statsSize(item.seedingSize)}</small></>:"—"}</td>
      <td>{item?<>{item.messageUnread} 条<small>{item.updateAt}</small></>:"—"}</td>
      <td><div className="site-statistics__actions">
        <button type="button" disabled={!item} aria-label={`${t.name} ${t.host} 等级进度`} onClick={()=>setRankTarget(t)}>等级进度</button>
        <button type="button" disabled={busy} onClick={()=>void action(async()=>{const d=await getStatsDetail(session.token,t.id);if(live.current)setDetail({name:t.host?`${t.name} · ${t.host}`:t.name,data:d});},"")}>历史与做种</button>
      </div></td>
    </tr>;
  })}</tbody></table></div>
 {!data.targets.length&&<p>暂无参与统计的站点，请先在站点设置中开启“数据统计”。</p>}
 {rankTarget&&<SiteRankingPanel key={rankTarget.id} token={session.token} siteId={rankTarget.id} name={rankTarget.host?`${rankTarget.name} · ${rankTarget.host}`:rankTarget.name} onClose={()=>setRankTarget(undefined)} onSessionExpired={onSessionExpired}/>}
 {detail&&<SiteStatisticsHistory key={detail.data.siteId} name={detail.name} detail={detail.data} onClose={()=>setDetail(undefined)}/>}
 {admin&&<section aria-label="统计刷新回执"><h3>刷新回执</h3>{admin.jobs.slice(0,20).map(j=><details key={j.id}><summary>{new Date(j.created).toLocaleString()} · {states[j.state]||j.state} · {j.requests} 次请求</summary><p className="site-statistics__receipt">编号 {j.id}</p><p>读取消息内容：{j.readMessages?"已允许":"未允许"}；消息内容请求 {j.messageRequests} 次。</p><ul>{j.results.map(r=><li key={r.siteId}>{r.name}：{r.reason}</li>)}</ul></details>)}{!admin.jobs.length&&<p>暂无刷新记录。</p>}</section>}
 </>}
 </section>;
}
