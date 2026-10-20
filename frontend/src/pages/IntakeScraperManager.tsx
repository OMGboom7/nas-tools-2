import { useEffect, useRef, useState } from "react";
import { ApiError, getIntakeScraper, saveIntakeScraperOptions, setIntakeScraperEnabled, type ScraperOptions, type ScraperStatus } from "../api/client";

const labels: Record<string,string> = { basic:"基础信息", credits:"演职人员", credits_chinese:"中文演职员（豆瓣唯一匹配／TMDB 别名，繁体转简体）", season_basic:"季信息", episode_basic:"集信息", episode_credits:"集演职人员", poster:"海报", backdrop:"背景图 fanart", background:"背景图 background", logo:"标志", disc:"光盘图", clearart:"透明图", banner:"横幅", thumb:"缩略图", season_poster:"季海报", season_banner:"季横幅", season_thumb:"季缩略图", episode_thumb:"集缩略图", episode_thumb_ffmpeg:"集图缺失时用 FFmpeg 只读截帧（第 181 秒）" };
const states: Record<string,string> = { pending:"等待处理", running:"执行中或中断，禁止自动重跑", completed:"所选项目处理完成", partial:"部分完成／有缺口，需核查", blocked:"已阻止", writing:"写入意图已保存，结果需核查", created:"已生成", preserved:"原文件保留（未验证原内容）", uncertain:"结果不确定，禁止重发" };

export function IntakeScraperManager({ token, enabled, onSessionExpired, onChanged }: { token:string; enabled:boolean; onSessionExpired:()=>void; onChanged:()=>void }) {
  const [status,setStatus]=useState<ScraperStatus|null>(null);
  const [options,setOptions]=useState<ScraperOptions>({scraper_nfo:{},scraper_pic:{}});
  const [saveConsent,setSaveConsent]=useState(false);
  const [switchConsent,setSwitchConsent]=useState(false);
  const [busy,setBusy]=useState(false);
  const [error,setError]=useState("");
  const generation=useRef(0), inFlight=useRef(false);
  useEffect(()=>{generation.current++;inFlight.current=false;setStatus(null);setOptions({scraper_nfo:{},scraper_pic:{}});setSaveConsent(false);setSwitchConsent(false);setError("");setBusy(false);return()=>{generation.current++;};},[token]);
  async function operate(action:"load"|"save"|"switch") {
    if (!enabled || inFlight.current || action==="save" && (!status || !saveConsent) || action==="switch" && (!status || !switchConsent)) return;
    const version=++generation.current;inFlight.current=true;setBusy(true);setError("");
    try {
      const next=action==="load" ? await getIntakeScraper(token) : action==="save" ? await saveIntakeScraperOptions(token,status!.fingerprint,options,saveConsent) : await setIntakeScraperEnabled(token,!status!.enabled,switchConsent);
      if(version!==generation.current)return;
      setStatus(next);setOptions(next.options);setSaveConsent(false);setSwitchConsent(false);
      if(action!=="load")onChanged();
    } catch(err) {
      if(version!==generation.current)return;
      if(err instanceof ApiError && err.code===401)onSessionExpired();else setError(err instanceof Error ? err.message : "刮削操作失败，请只读刷新核查，不要重发操作");
      setSaveConsent(false);setSwitchConsent(false);
      // Only a separate readonly refresh can observe an ambiguous save. No
      // implicit replay, enable switch, file operation or media reorganization.
    } finally {if(version===generation.current){inFlight.current=false;setBusy(false);}}
  }
  function toggle(family:keyof ScraperOptions,kind:string,name:string,value:boolean){setOptions({...options,[family]:{...options[family],[kind]:{...options[family][kind],[name]:value}}});setSaveConsent(false);}
  return <section aria-label="入库刮削与回执"><h2>入库 NFO 与图片</h2>
    <p>读取原有刮削配置。仅处理新审核并完整完成的原生入库任务；保存选项不会启用后台写入。已有文件和媒体文件不覆盖，普通文件同步不刮削，中断回执不自动重跑；此处不扫描整个媒体库、不执行强制覆盖刮削。</p>
    <p>中文名无法可靠匹配时保留原名并提示。仅在集图来源缺失且选中回退时读取已核验视频截帧；不调用命令行模板、不读取网络视频。缺少 FFmpeg、视频过短或生成失败会留下部分完成回执，不改视频、不自动重试。</p>
    {error && <p role="alert">{error}</p>}
    <button type="button" disabled={!enabled || busy} onClick={()=>void operate("load")}>只读刷新刮削配置与回执</button>
    {status && <><p>后续新任务入库刮削：{status.enabled ? "已启用" : "已停用"}。配置变更会使旧待处理范围失效，请重新审核尚未执行的整理计划。</p>
      {status.warnings.map((warning,i)=><p role="status" key={i}>{warning}</p>)}
      <fieldset disabled={!enabled || busy}><legend>刮削项目（保持未修改的旧配置）</legend>
        {(["scraper_nfo","scraper_pic"] as const).map((family)=><div key={family}><h3>{family==="scraper_nfo"?"NFO 元数据":"图片"}</h3>{Object.entries(status.fields[family]).map(([kind,flags])=><fieldset key={kind}><legend>{kind==="movie"?"电影":"剧集／动漫"}</legend>{Object.keys(flags).map((name)=><label key={name}><input type="checkbox" checked={options[family]?.[kind]?.[name]===true} onChange={(e)=>toggle(family,kind,name,e.target.checked)}/>{labels[name]||name}</label>)}</fieldset>)}</div>)}
        <label><input type="checkbox" checked={saveConsent} onChange={(e)=>setSaveConsent(e.target.checked)}/>确认只保存上述选项，不开启写入、不重放旧任务</label>
        <button type="button" disabled={!saveConsent} onClick={()=>void operate("save")}>确认保存刮削选项</button>
      </fieldset>
      <fieldset disabled={!enabled || busy}><legend>独立开关</legend><label><input type="checkbox" checked={switchConsent} onChange={(e)=>setSwitchConsent(e.target.checked)}/>{status.enabled?"确认停用后续新任务刮削；已开始的写入需核查回执":"同意对后续新审核、完整完成的原生入库任务补齐所选缺失 NFO 和图片"}</label><button type="button" disabled={!switchConsent} onClick={()=>void operate("switch")}>{status.enabled?"确认停用入库刮削":"确认启用入库刮削"}</button></fieldset>
      <p>最多展示 100 条，未完成／待核查记录优先。字幕提交等待本轮刮削处理；刮削失败不回滚入库，不重发下载或整理操作。</p>
      <ul>{status.receipts.map((row)=><li key={row.id}>{row.name} · 入库任务 {row.jobId} · {states[row.state]||row.state} · {row.updated}{row.reason && <p>{row.reason}</p>}<ul>{row.results.map((result,i)=><li key={i}>{result.name} · {states[result.state]||result.state}{result.reason && <small> · {result.reason}</small>}</li>)}</ul></li>)}</ul>
    </>}
  </section>;
}
