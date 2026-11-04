import {useEffect,useState} from "react";
import {ApiError} from "../api/client";
import {getStatsRanking,type SiteRanking} from "../api/siteStatistics";

const requirementLabels:Record<string,string>={weeks:"注册时间",download_gb:"下载量",ratio:"分享率",bonus:"积分"};
const stepLabels={locked:"未达成",done:"已超过",current:"当前等级",target:"下一等级"};

export function SiteRankingPanel({token,siteId,name,onClose,onSessionExpired}:{
  token:string;siteId:string;name:string;onClose:()=>void;onSessionExpired:()=>void;
}){
  const [ranking,setRanking]=useState<SiteRanking>();
  const [error,setError]=useState("");
  useEffect(()=>{
    const controller=new AbortController();
    void getStatsRanking(token,siteId,controller.signal).then(result=>{
      if(!controller.signal.aborted)setRanking(result.ranking);
    }).catch(reason=>{
      if(controller.signal.aborted)return;
      if(reason instanceof ApiError&&reason.code===401)onSessionExpired();
      else setError(reason instanceof Error?reason.message:"等级规则读取失败");
    });
    return ()=>controller.abort();
  },[token,siteId,onSessionExpired]);
  return <section className="site-ranking" aria-label={`${name} 等级进度`}>
    <header><h3>{name} · 等级进度</h3><button type="button" onClick={onClose}>收起等级进度</button></header>
    {error?<p role="alert">{error}</p>:ranking?<SiteRankingDetail ranking={ranking}/>:<p role="status">正在读取等级进度…</p>}
  </section>;
}

export function SiteRankingDetail({ranking}:{ranking:SiteRanking}){
  const r=ranking;
  return <>
    <p>当前等级 {r.current_level||"未知"}{r.next_level&&<> · 下一等级 {r.next_level}</>}</p>
    {r.keep_account_level&&<p>保号等级 {r.keep_account_level} · {r.current_is_keep?"已达到":"未达到"}</p>}
    {r.message&&<p>{r.message}</p>}
    {r.special_level_description&&<p>{r.special_level_description}</p>}
    {r.status_type==="normal"&&<p><label>总体进度 <progress max={100} value={r.overall_progress}/></label> {r.overall_progress}% · 已达到 {r.requirements.filter(c=>c.met).length}/{r.requirements.length} 项条件</p>}
    {r.level_steps.length>0&&<ol className="site-ranking__steps" aria-label="站点等级顺序">{r.level_steps.map((step,i)=>
      <li key={i} aria-current={step.state==="current"?"step":undefined}>
        <strong>{step.name}</strong><small>{stepLabels[step.state]}{step.is_keep&&" · 保号"}</small>
      </li>)}</ol>}
    {r.requirements.length>0&&<div className="site-statistics__scroll"><table aria-label="晋升条件">
      <thead><tr><th>条件</th><th>当前</th><th>要求</th><th>仍需</th><th>进度</th></tr></thead>
      <tbody>{r.requirements.map((c,i)=>{
        const unit=c.unit==="weeks"?"周":c.unit;
        return <tr key={i}><th scope="row">{requirementLabels[c.key]||c.label}</th><td>{c.current} {unit}</td>
          <td>{c.required} {unit}</td><td>{c.need} {unit}</td><td>{c.met?"已达到":`${c.progress}%`}</td></tr>;
      })}</tbody>
    </table></div>}
    {r.rule_url&&<p><a href={r.rule_url} target="_blank" rel="noreferrer">查看站点等级规则</a></p>}
  </>;
}
