import { useEffect, useState } from "react";
import { ApiError } from "../api/client";
import { getStatsTraffic, statsDayValid, type StatsTraffic, type TrafficRange } from "../api/siteStatistics";
import { statsSize } from "../utils/statisticsView";

const ranges:{days:TrafficRange;label:string}[]=[
  {days:1,label:"单日"},{days:7,label:"最近一周"},{days:30,label:"最近一月"},{days:365,label:"最近一年"},
];

export function SiteStatisticsTraffic({token,refreshKey,onSessionExpired}:{
  token:string;refreshKey:string;onSessionExpired:()=>void;
}){
  const [days,setDays]=useState<TrafficRange>(1);
  const [day,setDay]=useState("");
  const [today,setToday]=useState("");
  const [result,setResult]=useState<StatsTraffic>();
  const [error,setError]=useState("");
  const [revision,setRevision]=useState(0);

  useEffect(()=>{
    const controller=new AbortController();
    setResult(undefined);
    setError("");
    void getStatsTraffic(token,days,day,controller.signal).then(data=>{
      if(controller.signal.aborted)return;
      setToday(data.today);
      setResult(data);
    }).catch(err=>{
      if(controller.signal.aborted)return;
      if(err instanceof ApiError&&err.code===401)onSessionExpired();
      else setError(err instanceof Error?err.message:"流量统计读取失败");
    });
    return ()=>controller.abort();
  },[token,days,day,revision,refreshKey,onSessionExpired]);

  const endDay=day||today;
  const step=(offset:number)=>{
    const date=new Date(endDay+"T00:00:00Z");
    date.setUTCDate(date.getUTCDate()+offset);
    const next=date.toISOString().slice(0,10);
    if(statsDayValid(next)&&next<=today)setDay(next);
  };
  return <section className="site-stats-feature" aria-label="站点流量图">
    <h3>站点流量</h3>
    <div className="site-statistics__actions" aria-label="流量时间范围">
      {ranges.map(range=><button type="button" key={range.days} aria-pressed={days===range.days} onClick={()=>setDays(range.days)}>{range.label}</button>)}
    </div>
    <div className="site-stats-date">
      <button type="button" disabled={!endDay||endDay==="0001-01-01"} onClick={()=>step(-1)}>前一天</button>
      <label>截至日期 <input type="date" value={endDay} min="0001-01-01" max={today||undefined} onChange={e=>{
        if(!e.target.value||statsDayValid(e.target.value)&&(!today||e.target.value<=today))setDay(e.target.value);
      }}/></label>
      <button type="button" disabled={!endDay||!today||endDay>=today} onClick={()=>step(1)}>后一天</button>
      <button type="button" disabled={!today} onClick={()=>{setDay("");setRevision(v=>v+1);}}>读取今日数据</button>
    </div>
    <p>根据保存的累计快照计算，日期以 NAS Tools 的应用时区为准。</p>
    {error?<p role="alert">{error} <button type="button" onClick={()=>setRevision(v=>v+1)}>重试读取</button></p>:
      result&&result.days===days&&result.endDay===(day||result.today)?
        <TrafficBars data={result}/>:<p role="status">正在读取流量…</p>}
  </section>;
}

function TrafficBars({data}:{data:StatsTraffic}){
  if(!data.items.length)return <p>这个时间范围没有统计快照。</p>;
  let upload=0n,download=0n,largest=0n;
  for(const item of data.items){
    const up=BigInt(item.upload),down=BigInt(item.download);
    upload+=up;
    download+=down;
    if(up>largest)largest=up;
    if(down>largest)largest=down;
  }
  const share=(value:string)=>largest===0n?0:Number(BigInt(value)*10000n/largest)/100;
  return <>
    <p><strong>{data.endDay} · {data.days} 天</strong> · 上传 {statsSize(upload)} · 下载 {statsSize(download)}</p>
    <ul className="site-stats-bars" aria-label="各站点上传和下载流量">{data.items.map(item=><li key={item.siteId}>
      <strong>{item.name}</strong><small>{item.host}</small>
      <div className="site-stats-bar"><span>上传 {statsSize(item.upload)}</span><div aria-hidden="true"><i style={{width:`${share(item.upload)}%`}}/></div></div>
      <div className="site-stats-bar site-stats-bar--download"><span>下载 {statsSize(item.download)}</span><div aria-hidden="true"><i style={{width:`${share(item.download)}%`}}/></div></div>
    </li>)}</ul>
  </>;
}
