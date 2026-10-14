import { useEffect, useRef, useState } from "react";
import { ApiError, OrganizationRenameError, previewRecordRename, executeRecordRename, type OrganizationRenameInput, type OrganizationRenameReview } from "../api/client";

type Props = { token: string; enabled: boolean; onChanged: (jobId: string) => Promise<void>; onSessionExpired: () => void };

// This is an explicit preview/execute workflow. Loading, edits and errors never
// execute, expand, retry or reconcile a filesystem job in the background.
export function RecordRenameForm({ token, enabled, onChanged, onSessionExpired }: Props) {
  const [kind, setKind] = useState("unknown");
  const [recordID, setRecordID] = useState("");
  const [mode, setMode] = useState("copy");
  const [mediaType, setMediaType] = useState<"" | "MOV" | "TV" | "ANIME">("");
  const [tmdb, setTMDB] = useState("");
  const [season, setSeason] = useState("");
  const [format, setFormat] = useState("");
  const [details, setDetails] = useState("");
  const [offset, setOffset] = useState("");
  const [minimum, setMinimum] = useState("");
  const [partMode, setPartMode] = useState("auto");
  const [part, setPart] = useState("");
  const [parentConsent, setParentConsent] = useState(false);
  const [confirmed, setConfirmed] = useState(false);
  const [sourceRemoval, setSourceRemoval] = useState(false);
  const [review, setReview] = useState<OrganizationRenameReview | null>(null);
  const [savedID, setSavedID] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  const inFlight = useRef(false);
  useEffect(() => { generation.current++; inFlight.current = false; setReview(null); setRecordID(""); setTMDB(""); setMediaType(""); setSeason(""); setFormat(""); setDetails(""); setOffset(""); setMinimum(""); setPartMode("auto"); setPart(""); setParentConsent(false); setConfirmed(false); setSourceRemoval(false); setSavedID(""); setError(""); setBusy(false); return () => { generation.current++; }; }, [token]);
  function invalidate() { generation.current++; setReview(null); setConfirmed(false); setSourceRemoval(false); setParentConsent(false); setError(""); }
  function input(): OrganizationRenameInput {
    if (!/^[1-9][0-9]*$/.test(recordID) || BigInt(recordID) > 9223372036854775807n) throw new Error("请输入完整、精确的正整数记录 ID");
    return { ...(kind === "unknown" ? { unknown_id: recordID } : { logid: recordID }), syncmod: mode, ...(mediaType ? { type: mediaType } : {}), ...(tmdb ? { tmdb } : {}), ...(season ? { season } : {}), ...(format ? { episode_format: format } : {}), ...(details ? { episode_details: details } : {}), ...(offset ? { episode_offset: offset } : {}), ...(minimum ? { min_filesize: minimum } : {}), ...(partMode === "auto" ? {} : { episode_part: partMode === "clear" ? "" : part }), confirmParentScope: parentConsent };
  }
  async function operate(execute: boolean) {
    if (!enabled || inFlight.current || execute && (!review || !confirmed || mode === "move" && !sourceRemoval)) return;
    inFlight.current = true;
    const version = ++generation.current;
    setBusy(true); setError(""); setSavedID("");
    try {
      const request = input();
      if (!execute) {
        setReview(null); setConfirmed(false); setSourceRemoval(false);
        const next = await previewRecordRename(token, request);
        if (version === generation.current) setReview(next);
      } else {
        const result = await executeRecordRename(token, request, review!.plan.fingerprint, true, sourceRemoval);
        if (version !== generation.current) return;
        setSavedID(result.jobId); setReview(null); setConfirmed(false); setSourceRemoval(false); setParentConsent(false);
        await onChanged(result.jobId);
      }
    } catch (err) {
      if (version !== generation.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "改名请求未完成");
      setConfirmed(false); setSourceRemoval(false);
      if (execute) {
        setReview(null); setParentConsent(false);
        if (err instanceof OrganizationRenameError && err.jobId) setSavedID(err.jobId);
        // A request can commit before its response is lost. Only read saved
        // jobs on failure, never send the mutation again automatically.
        try { await onChanged(err instanceof OrganizationRenameError ? err.jobId || "" : ""); } catch { /* retain original failure */ }
      }
    } finally { if (version === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section aria-label="关联记录手动识别与改名">
    <h2>关联记录手动识别与改名</h2>
    <p>使用上方列表中的记录 ID。保留旧历史和已发布媒体，仅从原来源按本次方式整理，不覆盖目标；模板作用于单文件记录时必须另行确认扩展到父目录。预览和刷新不执行。</p>
    {error && <p role="alert">{error}</p>}
    {savedID && <p>已保存任务：<code>{savedID}</code>。请在任务列表查看持久回执，错误不代表可以重跑。</p>}
    <fieldset disabled={!enabled || busy}>
      <label>记录类型<select value={kind} onChange={(e) => { setKind(e.target.value); invalidate(); }}><option value="unknown">未识别记录</option><option value="history">原历史</option></select></label>
      <label>精确记录 ID<input value={recordID} inputMode="numeric" maxLength={19} onChange={(e) => { setRecordID(e.target.value); invalidate(); }} /></label>
      <label>本次转移方式<select value={mode} onChange={(e) => { setMode(e.target.value); invalidate(); }}><option value="copy">复制</option><option value="link">硬链接</option><option value="softlink">软链接</option><option value="move">移动</option></select></label>
      <label>媒体查询类型<select value={mediaType} onChange={(e) => { setMediaType(e.target.value as typeof mediaType); invalidate(); }}><option value="">自动识别</option><option value="MOV">电影</option><option value="TV">电视剧</option><option value="ANIME">动漫（TV 元数据查询）</option></select></label>
      <p>动漫身份由 TMDB 动画分类确定，不将非动画作品强改为动漫。未指定目的的旧记录按配置类型选库；原记录明确指定目的时仍使用该目录。</p>
      <label>TMDB ID（留空为自动）<input value={tmdb} maxLength={19} onChange={(e) => { setTMDB(e.target.value); invalidate(); }} /></label>
      <label>季号（留空沿用识别；TV 无值时为 1）<input type="number" value={season} min="0" max="99" onChange={(e) => { setSeason(e.target.value); invalidate(); }} /></label>
      <label>完整文件名集号模板<input value={format} maxLength={512} placeholder="Show.{ep}.mkv" onChange={(e) => { setFormat(e.target.value); invalidate(); }} /></label>
      <label>集号规则<input value={details} placeholder="n 或 n-m 指定；n,m 筛选原始集号" maxLength={20} onChange={(e) => { setDetails(e.target.value); invalidate(); }} /></label>
      <label>集号偏移<input type="number" min="-99999" max="99999" value={offset} onChange={(e) => { setOffset(e.target.value); invalidate(); }} /></label>
      <label>本次最小 MiB（留空沿用配置）<input type="number" min="0" max="1000000" value={minimum} onChange={(e) => { setMinimum(e.target.value); invalidate(); }} /></label>
      <label>分段<select value={partMode} onChange={(e) => { setPartMode(e.target.value); invalidate(); }}><option value="auto">自动</option><option value="set">明确指定</option><option value="clear">明确清空</option></select></label>
      {partMode === "set" && <label>分段值<input value={part} maxLength={32} onChange={(e) => { setPart(e.target.value); invalidate(); }} /></label>}
      {format && <label><input type="checkbox" checked={parentConsent} onChange={(e) => { generation.current++; setReview(null); setConfirmed(false); setSourceRemoval(false); setParentConsent(e.target.checked); }} />若此记录是单个媒体文件，我明确同意将模板范围扩展到它的父目录；若是目录记录请勿勾选。</label>}
      <button type="button" onClick={() => void operate(false)}>生成关联记录只读预览</button>
    </fieldset>
    {review && <div>
      <p>审核记录 #{review.origin.recordId}：{review.unknown?.path || `${review.history?.sourcePath}/${review.history?.sourceFilename}`} · 原方式 {review.unknown?.mode || review.history?.mode || "未指定"} → 本次 {review.plan.mode}</p>
      <p>来源根：<code>{review.sourceRoot}</code><br />目标根：<code>{review.targetRoot}</code>。{review.options.expandedScope ? "本次已扩展到原文件的父目录。" : "未扩展来源范围。"}{review.options.preserveUnknown ? "仅选中范围执行；旧未识别记录保持原状，不标记整条成功。" : review.history ? "原历史和已发布媒体不变；新的转移成功后另记历史。" : "关联未识别记录仅在全范围成功和完整回执后更新。"}</p>
      <p>{review.explicitTarget ? "本次使用原记录明确指定的配置目标根，不按媒体类型另选。" : "本次按已核验媒体类型选择配置媒体库。"}</p>
      <ul>{review.plan.items.map((item) => <li key={item.source}>{item.source} → {item.target || "不执行"} · {item.mediaType || "未核验类型"} · {item.category || "无分类"} · {item.status}</li>)}</ul>
      <label><input type="checkbox" checked={confirmed} disabled={!enabled || busy} onChange={(e) => setConfirmed(e.target.checked)} />我已审核本次范围、身份规则和路径，确认来源已下载完成且至少 30 秒未修改，同意执行以上可规划项。</label>
      {mode === "move" && <label><input type="checkbox" checked={sourceRemoval} disabled={!enabled || busy} onChange={(e) => setSourceRemoval(e.target.checked)} />我另外同意移除上述来源中的原文件名，并在目标核验及历史提交后清理本任务源恢复备份；不删除源目录或其他文件。</label>}
      <button type="button" disabled={!enabled || busy || !confirmed || mode === "move" && !sourceRemoval} onClick={() => void operate(true)}>按已审核范围确认执行</button>
    </div>}
  </section>;
}
