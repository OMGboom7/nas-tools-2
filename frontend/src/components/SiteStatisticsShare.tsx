import { useEffect, useMemo, useRef, useState } from "react";
import { drawStatisticsPng, maxStatisticsPngRows } from "../utils/statisticsPng";
import { shareCandidates, shareDefaults, shareFields, shareProjection, type ShareOptions, type ShareOrder, type StatRow } from "../utils/statisticsView";

type Preferences={options:ShareOptions;siteIds:string[]};

function sharePreferences(key:string):Preferences|undefined{
  try{
    const saved=JSON.parse(localStorage.getItem(key)||"null");
    if(!saved?.options||!Array.isArray(saved.siteIds))return;
    const options=saved.options;
    if(!Object.hasOwn(shareFields,options.order)||!["asc","desc"].includes(options.direction)||
      typeof options.hideSite!=="boolean"||typeof options.hideUser!=="boolean")return;
    return {
      options:{order:options.order,direction:options.direction,hideSite:options.hideSite,hideUser:options.hideUser},
      siteIds:saved.siteIds.filter((id:unknown)=>typeof id==="string"&&/^[1-9]\d{0,18}$/.test(id)),
    };
  }catch{return undefined;}
}

export function SiteStatisticsShare({rows,storageKey,onClose}:{rows:StatRow[];storageKey:string;onClose:()=>void}){
  const available=useMemo(()=>shareCandidates(rows),[rows]);
  const [saved]=useState(()=>sharePreferences(storageKey));
  const [options,setOptions]=useState(saved?.options||shareDefaults);
  const [selected,setSelected]=useState(()=>new Set(saved?.siteIds??available.map(r=>r.id)));
  const [error,setError]=useState("");
  const [notice,setNotice]=useState("");
  const [busy,setBusy]=useState(false);
  const canvas=useRef<HTMLCanvasElement>(null);
  const pending=useRef(false);
  const mounted=useRef(true);
  const report=useMemo(()=>shareProjection(available,selected,options),[available,selected,options]);
  const canDraw=report.rows.length>0&&report.rows.length<=maxStatisticsPngRows;
  useEffect(()=>{mounted.current=true;return ()=>{mounted.current=false;};},[]);
  useEffect(()=>{
    if(!canvas.current||!canDraw)return;
    try{drawStatisticsPng(canvas.current,report);}
    catch(err){setError(err instanceof Error?err.message:"统计图片绘制失败");}
  },[report,canDraw]);

  function edit(next:ShareOptions){
    setOptions(next);
    setError("");
    setNotice("");
  }
  function select(ids:Set<string>){
    setSelected(ids);
    setError("");
    setNotice("");
  }
  async function download(){
    if(!canvas.current||!canDraw||pending.current)return;
    pending.current=true;
    setBusy(true);
    setError("");
    setNotice("");
    try{
      const preview=canvas.current;
      drawStatisticsPng(preview,report);
      const blob=await new Promise<Blob>((resolve,reject)=>preview.toBlob(
        value=>value?.type==="image/png"?resolve(value):reject(new Error("无法生成 PNG 图片")),"image/png",
      ));
      if(!mounted.current)return;
      const url=URL.createObjectURL(blob);
      const link=document.createElement("a");
      try{
        link.href=url;
        link.download="nastool_statistics.png";
        document.body.append(link);
        link.click();
        try{localStorage.setItem(storageKey,JSON.stringify({options,siteIds:available.filter(row=>selected.has(row.id)).map(row=>row.id)}));}
        catch{/* Preferences are optional. */}
        setNotice("已生成 PNG，请查看浏览器下载。");
      }finally{
        link.remove();
        window.setTimeout(()=>URL.revokeObjectURL(url),10000);
      }
    }catch(err){
      if(mounted.current)setError(err instanceof Error?err.message:"统计图片导出失败");
    }finally{
      pending.current=false;
      if(mounted.current)setBusy(false);
    }
  }
  return <section className="site-stats-feature" aria-label="统计 PNG 导出">
    <header><h3>统计 PNG 导出</h3><button type="button" disabled={busy} onClick={onClose}>关闭导出设置</button></header>
    <p>图片由当前浏览器生成。请核对下方预览后再下载。</p>
    <fieldset className="site-stats-share-options" disabled={busy}>
      <legend>图片内容</legend>
      <div className="site-statistics__actions">
        <button type="button" onClick={()=>select(new Set(available.map(row=>row.id)))}>全选有数据站点</button>
        <button type="button" onClick={()=>select(new Set())}>清空选择</button>
      </div>
      <div className="site-stats-share-sites">
        {available.map(row=><label key={row.id}><input type="checkbox" checked={selected.has(row.id)} onChange={e=>{
          const next=new Set(selected);
          if(e.target.checked)next.add(row.id);else next.delete(row.id);
          select(next);
        }}/> {row.name} · {row.host}</label>)}
      </div>
      <div className="site-stats-share-fields">
        <label>排序依据 <select value={options.order} onChange={e=>edit({...options,order:e.target.value as ShareOrder})}>
          {Object.entries(shareFields).map(([value,label])=><option key={value} value={value}>{label}</option>)}
        </select></label>
        <label>排序方式 <select value={options.direction} onChange={e=>edit({...options,direction:e.target.value==="asc"?"asc":"desc"})}>
          <option value="desc">降序</option><option value="asc">升序</option>
        </select></label>
        <label><input type="checkbox" checked={options.hideSite} onChange={e=>edit({...options,hideSite:e.target.checked})}/> 隐藏站点名及地址</label>
        <label><input type="checkbox" checked={options.hideUser} onChange={e=>edit({...options,hideUser:e.target.checked})}/> 隐藏用户名</label>
      </div>
    </fieldset>
    <p>已选 {report.rows.length} 个站点{report.rows.length>maxStatisticsPngRows?"，每张图片最多支持 200 个。":"。"}</p>
    {canDraw&&<div className="site-stats-preview"><canvas ref={canvas} aria-label="统计图片预览" role="img"/></div>}
    {error&&<p role="alert">{error}</p>}{notice&&<p role="status">{notice}</p>}
    <button type="button" disabled={!canDraw||busy} onClick={()=>void download()}>{busy?"正在生成图片…":"下载 PNG"}</button>
  </section>;
}
