import { useMemo, useState } from "react";
import { statsDayValid, type StatsDetail, type StatsHistory } from "../api/siteStatistics";
import { scatterSample, statsSize } from "../utils/statisticsView";

const trendFields={
  upload:"总上传量",download:"总下载量",seedingSize:"做种体积",bonus:"积分",
} as const;
type TrendField=keyof typeof trendFields;
const chart={width:580,height:148,left:16,right:564,top:10,bottom:128};

export function SiteStatisticsHistory({name,detail,onClose}:{name:string;detail:StatsDetail;onClose:()=>void}){
  const [metric,setMetric]=useState<TrendField>("upload");
  const [period,setPeriod]=useState(30);
  const history=detail.history.slice(-period);
  return <section className="site-stats-feature" aria-label={`${name} 统计明细`}>
    <header><h3>{name} · 历史与做种</h3><button type="button" onClick={onClose}>收起明细</button></header>
    <div className="site-stats-date">
      <label>趋势指标 <select value={metric} onChange={e=>setMetric(e.target.value as TrendField)}>
        {Object.entries(trendFields).map(([value,label])=><option key={value} value={value}>{label}</option>)}
      </select></label>
      <label>观察记录 <select value={period} onChange={e=>setPeriod(Number(e.target.value))}>
        <option value={30}>最近 30 条</option><option value={90}>最近 90 条</option><option value={730}>全部（最多 730 条）</option>
      </select></label>
    </div>
    <HistoryTrend metric={metric} rows={history.filter(row=>statsDayValid(row.date))}/>
    <details><summary>每日统计表</summary><div className="site-statistics__scroll site-stats-history-table">
      <table><thead><tr><th>日期</th><th>上传量</th><th>下载量</th><th>积分</th><th>做种</th></tr></thead><tbody>
        {history.map(h=><tr key={h.date}><td>{h.date}</td><td>{statsSize(h.upload)}</td><td>{statsSize(h.download)}</td>
          <td>{h.bonus.toLocaleString()}</td><td>{h.seeding} 个 · {statsSize(h.seedingSize)}</td></tr>)}
      </tbody></table>
    </div></details>
    <SeedScatter rows={detail.seedingInfo}/>
    {detail.seedingInfo.length>0&&<details><summary>做种明细（做种人数 / 文件大小）</summary>
      <ol>{detail.seedingInfo.slice(0,500).map((s,i)=><li key={i}>{s[0]} 人 · {statsSize(s[1])}</li>)}</ol>
      {detail.seedingInfo.length>500&&<p>表格展示前 500 条明细。</p>}
    </details>}
  </section>;
}

function fraction(value:bigint,max:bigint){
  return max>0n?Number(value*10000n/max)/10000:0;
}

function HistoryTrend({rows,metric}:{rows:StatsHistory[];metric:TrendField}){
  if(!rows.length)return <p>暂无历史快照。</p>;
  const first=rows[0],last=rows[rows.length-1];
  const date=(row:StatsHistory)=>Date.parse(row.date+"T00:00:00Z");
  const span=Math.max(1,date(last)-date(first));
  const maxBonus=metric==="bonus"?rows.reduce((max,row)=>Math.max(max,row.bonus),0):0;
  const maxBytes=metric==="bonus"?0n:rows.reduce((max,row)=>BigInt(row[metric])>max?BigInt(row[metric]):max,0n);
  const points=rows.map(row=>{
    const x=chart.left+(date(row)-date(first))/span*(chart.right-chart.left);
    const progress=metric==="bonus"?(maxBonus?row.bonus/maxBonus:0):fraction(BigInt(row[metric]),maxBytes);
    return {x,y:chart.bottom-progress*(chart.bottom-chart.top)};
  });
  const display=(row:StatsHistory)=>metric==="bonus"?row.bonus.toLocaleString():statsSize(row[metric]);
  return <figure className="site-stats-figure">
    <figcaption>{trendFields[metric]} · {rows.length} 条快照 · 最近值 {display(last)}</figcaption>
    <p>纵轴 0 至 {metric==="bonus"?maxBonus.toLocaleString():statsSize(maxBytes)} · 横轴 {first.date} 至 {last.date}</p>
    <svg role="img" aria-label={`${trendFields[metric]}历史趋势图`} viewBox={`0 0 ${chart.width} ${chart.height}`}>
      <path d={`M${chart.left} ${chart.top} V${chart.bottom} H${chart.right}`} className="site-stats-axis"/>
      <polyline className="site-stats-trend-line" points={points.map(p=>`${p.x},${p.y}`).join(" ")}/>
      {points.map((p,i)=><circle key={rows[i].date} cx={p.x} cy={p.y} r={rows.length>90?1.6:3} className="site-stats-point">
        <title>{rows[i].date} · {display(rows[i])}</title>
      </circle>)}
    </svg>
  </figure>;
}

function SeedScatter({rows}:{rows:StatsDetail["seedingInfo"]}){
  const {maxCount,maxBytes,points}=useMemo(()=>scatterSample(rows),[rows]);
  if(!rows.length)return <p>暂无做种明细。</p>;
  return <figure className="site-stats-figure">
    <figcaption>做种分布 · {rows.length} 条{points.length<rows.length?` · 显示 ${points.length} 个抽样点，含极值`:""}</figcaption>
    <p>横轴 0 至 {maxCount.toString()} 人 · 纵轴 0 至 {statsSize(maxBytes)}</p>
    <svg role="img" aria-label="做种人数与文件大小散点图" viewBox={`0 0 ${chart.width} ${chart.height}`}>
      <path d={`M${chart.left} ${chart.top} V${chart.bottom} H${chart.right}`} className="site-stats-axis"/>
      {points.map(([count,bytes],i)=>{
        const cx=chart.left+fraction(BigInt(count),maxCount)*(chart.right-chart.left);
        const cy=chart.bottom-fraction(BigInt(bytes),maxBytes)*(chart.bottom-chart.top);
        return <circle key={i} className="site-stats-point site-stats-point--scatter" cx={cx} cy={cy} r="3">
          <title>{count} 人 · {statsSize(bytes)}</title>
        </circle>;
      })}
    </svg>
  </figure>;
}
