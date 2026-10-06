import { useState, type FormEvent } from "react";
import type { SubscriptionInput, SubscriptionItem, SubscriptionOptions } from "../api/client";

export function SubscriptionEditor({ item, options, onClose, onSave }: { item?: SubscriptionItem; options: SubscriptionOptions; onClose: () => void; onSave: (input: SubscriptionInput) => Promise<void> }) {
  const [form, setForm] = useState<SubscriptionInput>(() => item ? {
    id: item.id, name: item.name, year: item.year, type: item.type, season: item.season, mediaId: item.tmdbId,
    keyword: item.keyword, fuzzyMatch: item.fuzzyMatch, overEdition: item.overEdition, rssSites: item.rssSites,
    searchSites: item.searchSites, quality: item.quality, resolution: item.resolution, releaseGroup: item.releaseGroup,
    filterRule: item.filterRule, include: item.include, exclude: item.exclude, savePath: item.savePath,
    downloadSetting: item.downloadSetting, totalEpisodes: item.totalEpisodes || undefined, currentEpisode: item.currentEpisode || undefined,
  } : { name: "", year: "", type: "MOV", fuzzyMatch: true, overEdition: false, rssSites: [], searchSites: [] });
  const [saving, setSaving] = useState(false); const [error, setError] = useState("");
  function field(key: keyof SubscriptionInput, value: unknown) { setForm((current) => ({ ...current, [key]: value })); }
  function toggle(key: "rssSites" | "searchSites", value: string) { const list=form[key]; field(key, list.includes(value) ? list.filter((v)=>v!==value) : [...list,value]); }
  async function submit(event: FormEvent) { event.preventDefault(); setSaving(true); setError(""); try { await onSave(form); } catch (e) { setError(e instanceof Error ? e.message : "保存失败"); setSaving(false); } }
  return <div className="editor-backdrop" role="presentation"><section className="subscription-editor" role="dialog" aria-modal="true" aria-labelledby="editor-title">
    <header><div><p className="eyebrow">SUBSCRIPTION EDITOR</p><h2 id="editor-title">{item ? "编辑订阅" : "新增订阅"}</h2></div><button type="button" onClick={onClose} aria-label="关闭编辑器">×</button></header>
    <form onSubmit={submit}><div className="editor-grid">
      <label><span>名称</span><input required value={form.name} onChange={(e)=>field("name",e.target.value)} /></label>
      <label><span>类型</span><select value={form.type} onChange={(e)=>field("type",e.target.value as "MOV"|"TV")}><option value="MOV">电影</option><option value="TV">电视剧</option></select></label>
      <label><span>年份</span><input inputMode="numeric" value={form.year} onChange={(e)=>field("year",e.target.value)} /></label>
      {form.type === "TV" && <label><span>季</span><input placeholder="如 S02" value={form.season || ""} onChange={(e)=>field("season",e.target.value)} /></label>}
      <label><span>媒体 ID</span><input value={form.mediaId || ""} onChange={(e)=>field("mediaId",e.target.value)} disabled={form.fuzzyMatch} /></label>
      <label><span>自定义搜索词</span><input value={form.keyword || ""} onChange={(e)=>field("keyword",e.target.value)} /></label>
      <label><span>质量</span><input placeholder="如 WEB-DL" value={form.quality || ""} onChange={(e)=>field("quality",e.target.value)} /></label>
      <label><span>分辨率</span><input placeholder="如 4K" value={form.resolution || ""} onChange={(e)=>field("resolution",e.target.value)} /></label>
      <label><span>制作组</span><input value={form.releaseGroup || ""} onChange={(e)=>field("releaseGroup",e.target.value)} /></label>
      <label><span>过滤规则</span><select value={form.filterRule || ""} onChange={(e)=>field("filterRule",e.target.value)}><option value="">默认</option>{options.filterRules.map(o=><option key={o.value} value={o.value}>{o.label}</option>)}</select></label>
      <label><span>下载设置</span><select value={form.downloadSetting || ""} onChange={(e)=>field("downloadSetting",e.target.value)}><option value="">默认</option>{options.downloadSettings.map(o=><option key={o.value} value={o.value}>{o.label}</option>)}</select></label>
      <label><span>保存目录</span><input list="subscription-save-paths" value={form.savePath || ""} onChange={(e)=>field("savePath",e.target.value)} /><datalist id="subscription-save-paths">{options.savePaths.map(p=><option key={p}>{p}</option>)}</datalist></label>
    </div>
    <div className="editor-switches"><label><input type="checkbox" checked={form.fuzzyMatch} onChange={(e)=>field("fuzzyMatch",e.target.checked)} />模糊匹配</label><label><input type="checkbox" checked={form.overEdition} onChange={(e)=>field("overEdition",e.target.checked)} />洗版</label></div>
    <OptionChecks title="RSS 站点" values={options.rssSites} selected={form.rssSites} onToggle={(v)=>toggle("rssSites",v)} /><OptionChecks title="搜索站点" values={options.searchSites} selected={form.searchSites} onToggle={(v)=>toggle("searchSites",v)} />
    <div className="editor-grid"><label><span>必须包含</span><textarea value={form.include || ""} onChange={(e)=>field("include",e.target.value)} /></label><label><span>排除</span><textarea value={form.exclude || ""} onChange={(e)=>field("exclude",e.target.value)} /></label></div>
    {error && <p className="form-error">{error}</p>}<footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={saving}>{saving ? "保存中…" : "保存订阅"}</button></footer>
    </form></section></div>;
}

function OptionChecks({ title, values, selected, onToggle }: { title: string; values: {value:string;label:string}[]; selected:string[]; onToggle:(value:string)=>void }) {
  return <fieldset className="option-checks"><legend>{title}</legend>{values.length===0 ? <small>使用全部可用站点</small> : values.map(o=><label key={o.value}><input type="checkbox" checked={selected.includes(o.value)} onChange={()=>onToggle(o.value)} />{o.label}</label>)}</fieldset>;
}
