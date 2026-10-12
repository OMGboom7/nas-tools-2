import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, getOrganizationRoots, previewOrganization, getOrganizationJobs, getOrganizationJob, getOrganizationUnknown, getOrganizationHistory, reviewOrganizationUnknown, detachOrganizationUnknown, createOrganizationJob, controlOrganizationJob, abandonOrganizationJob, type AuthSession, type OrganizationJob, type OrganizationPlan, type OrganizationRoots, type OrganizationMediaAssignment, type OrganizationUnknownItem, type OrganizationUnknownData, type OrganizationUnknownReview, type OrganizationHistoryItem, type OrganizationHistoryData } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";
import { canAbandonOrganizationJob, buildOrganizationAssignment } from "./organizationState";

type Props = { session: AuthSession; currentPath: string; onNavigate: (path: string) => void; onLogout: () => Promise<void>; onSessionExpired: () => void };
const statusLabels: Record<string, string> = { available: "可规划", conflict: "目标冲突", blocked: "已阻止", unmatched: "附件未匹配", unrecognized: "媒体未识别" };
const jobLabels: Record<string, string> = { ready: "待执行", completed: "已完成", needs_review: "需核对，禁止盲目重试", cancelled: "已取消", planned: "等待执行", running: "执行中或已中断", prepared: "转移凭证已保存，待核实", moving: "源移除意图已保存，需核验恢复对象", quarantined: "源文件已转入恢复目录，待提交" };
const modeLabels: Record<string,string> = {copy:"复制",link:"硬链接",softlink:"软链接",move:"移动"};
jobLabels.abandoning = "放弃意图已保存，暂存清理待核实";
jobLabels.discarding = "暂存清理待回执";
jobLabels.abandoned = "已明确放弃，未记录整理成功";
jobLabels.staging = "写入前凭证已保存，尚未完整准备";
const jobReasonLabels: Record<string, string> = {
  "Move source recovery cleanup pending; explicit source-removal confirmation required": "历史已提交，源恢复备份清理尚未确认完成；请检查后明确授权续办。",
  "Move target recovery cleanup pending": "源恢复备份已清理，目标暂存清理尚未确认完成；请核验续办。",
  "Execution interrupted or failed; verify the saved proof before any retry": "操作已中断或失败；必须核验保存凭证，不能盲目重试。",
  "Explicit unpublished staging disposal requested; no successful transfer recorded": "管理员已明确放弃未发布暂存；不是整理成功记录。",
};

export function OrganizationPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: Props) {
  const [roots, setRoots] = useState<OrganizationRoots>({ sources: [], targets: [] });
  const [sourceId, setSourceId] = useState("");
  const [targetId, setTargetId] = useState("");
  const [path, setPath] = useState(".");
  const [mode, setMode] = useState("copy");
  const [plan, setPlan] = useState<OrganizationPlan | null>(null);
  const [assignments, setAssignments] = useState<OrganizationMediaAssignment[]>([]);
  const [unknown, setUnknown] = useState<OrganizationUnknownData>({ items: [], total: 0, page: 1, pageSize: 30, totalPages: 1 });
  const [unknownKeyword, setUnknownKeyword] = useState("");
  const [selectedUnknown, setSelectedUnknown] = useState<OrganizationUnknownItem | null>(null);
  const [history, setHistory] = useState<OrganizationHistoryData>({ items: [], total: 0, page: 1, totalPages: 1 });
  const [historyKeyword, setHistoryKeyword] = useState("");
  const [selectedHistory, setSelectedHistory] = useState<OrganizationHistoryItem | null>(null);
  const [detachReview, setDetachReview] = useState<OrganizationUnknownReview | null>(null);
  const [detachReason, setDetachReason] = useState("");
  const [detachConfirmed, setDetachConfirmed] = useState(false);
  const [manualSource, setManualSource] = useState("");
  const [manualKind, setManualKind] = useState<"movie" | "tv">("movie");
  const [manualId, setManualId] = useState("");
  const [manualSeason, setManualSeason] = useState("");
  const [manualEpisode, setManualEpisode] = useState("");
  const [manualEndEpisode, setManualEndEpisode] = useState("");
  const [manualEpisodeFormat, setManualEpisodeFormat] = useState("");
  const [manualEpisodeOffset, setManualEpisodeOffset] = useState("");
  useEffect(() => { setManualEpisodeFormat(""); setManualEpisodeOffset(""); }, [sourceId, targetId, path, mode, session.token]);
  const previewGeneration = useRef(0);
  const unknownGeneration = useRef(0);
  const detachGeneration = useRef(0);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [jobs, setJobs] = useState<OrganizationJob[]>([]);
  const [jobId, setJobId] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [sourceRemoval, setSourceRemoval] = useState(false);
  const [discardStaging, setDiscardStaging] = useState(false);
  const [oldExecutorsStopped, setOldExecutorsStopped] = useState(false);
  const job = jobs.find((item) => item.id === jobId);
  const selectedDest = selectedUnknown?.dest || selectedHistory?.dest;
  useEffect(() => { setConfirmed(false); setSourceRemoval(false); }, [jobId, session.token]);
  useEffect(() => { detachGeneration.current++; setDetachReview(null); setDetachReason(""); setDetachConfirmed(false); }, [jobId, session.token]);
  useEffect(() => { setDiscardStaging(false); setOldExecutorsStopped(false); }, [jobId, session.token]);
  useEffect(() => { previewGeneration.current++; unknownGeneration.current++; setUnknown({ items: [], total: 0, page: 1, pageSize: 30, totalPages: 1 }); setHistory({ items: [], total: 0, page: 1, totalPages: 1 }); setAssignments([]); setSelectedUnknown(null); setSelectedHistory(null); setPlan(null); setManualSource(""); setManualId(""); setBusy(false); }, [session.token]);
  useEffect(() => {
    let active = true;
    setLoading(true);
    void getOrganizationRoots(session.token).then(async (data) => {
      if (!active) return;
      setRoots(data); setSourceId(data.sources[0]?.id || ""); setTargetId(data.targets[0]?.id || ""); setError("");
      const records = await getOrganizationUnknown(session.token);
      if (active) setUnknown(records);
      const logs = await getOrganizationHistory(session.token);
      if (active) setHistory(logs);
      if (data.executionModes?.length) {
        const saved = await getOrganizationJobs(session.token);
        if (active) { setJobs(saved); setJobId(saved[0]?.id || ""); }
      }
    }).catch((err: unknown) => {
      if (!active) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "整理目录加载失败");
    }).finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [session.token, onSessionExpired]);

  async function preview(event: FormEvent) {
    event.preventDefault(); await generatePreview(assignments);
  }
  async function generatePreview(selected: OrganizationMediaAssignment[]) {
    const generation = ++previewGeneration.current;
    setBusy(true); setError(""); setPlan(null); setManualSource("");
    try {
      const result = await previewOrganization(session.token, { sourceId, targetId, path, mode, ...(selected.length ? { assignments: selected } : {}), ...(selectedUnknown ? { unknownRecord: { id: selectedUnknown.id, fingerprint: selectedUnknown.fingerprint } } : {}), ...(selectedHistory ? { historyRecord: { id: selectedHistory.id, fingerprint: selectedHistory.fingerprint } } : {}) });
      if (generation === previewGeneration.current) setPlan(result);
    }
    catch (err) {
      if (generation !== previewGeneration.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "整理预览失败");
    } finally { if (generation === previewGeneration.current) setBusy(false); }
  }
  function invalidate() { previewGeneration.current++; setPlan(null); setAssignments([]); setManualSource(""); setManualId(""); setError(""); }
  function selectUnknown(item: OrganizationUnknownItem) {
    if (item.reason || !item.sourceId || !item.targetId || item.relative === undefined) return;
    invalidate(); setSelectedUnknown(item); setSelectedHistory(null); setSourceId(item.sourceId); setTargetId(item.targetId); setPath(item.relative);
    if (item.nativeMode) setMode(item.nativeMode);
  }
  function selectHistory(item: OrganizationHistoryItem) {
    if (item.reason || !item.sourceId || !item.targetId || item.relative === undefined) return;
    invalidate(); setSelectedHistory(item); setSelectedUnknown(null); setSourceId(item.sourceId); setTargetId(item.targetId); setPath(item.relative);
    if (item.nativeMode) setMode(item.nativeMode);
  }
  async function loadHistory(page: number) {
    const generation = ++unknownGeneration.current;
    setBusy(true); setError("");
    try { const data = await getOrganizationHistory(session.token, page, historyKeyword); if (generation === unknownGeneration.current) setHistory(data); }
    catch (err) { if (generation !== unknownGeneration.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "历史读取失败"); }
    finally { if (generation === unknownGeneration.current) setBusy(false); }
  }
  async function loadUnknown(page: number, job?: string) {
    const generation = ++unknownGeneration.current;
    setBusy(true); setError("");
    try {
      if (job) { const saved = await getOrganizationJob(session.token, job); if (generation === unknownGeneration.current) { setJobs((current) => [saved, ...current.filter((item) => item.id !== saved.id)]); setJobId(saved.id); } }
      else { const saved = await getOrganizationUnknown(session.token, page, unknownKeyword); if (generation === unknownGeneration.current) setUnknown(saved); }
    } catch (err) { if (generation !== unknownGeneration.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "未识别记录读取失败"); }
    finally { if (generation === unknownGeneration.current) setBusy(false); }
  }
  async function assignIdentity(event: FormEvent) {
    event.preventDefault();
    const file = plan?.items.find((item) => item.source === manualSource && item.kind === "media");
    if (!file) { setError("请从当前预览选择要纠正身份的媒体文件。"); return; }
    try {
      const selection = buildOrganizationAssignment(file, manualKind, manualId, manualSeason, manualEpisode, manualEndEpisode, manualEpisodeFormat, manualEpisodeOffset);
      const updated = [...assignments.filter((item) => item.source !== selection.source), selection];
      setAssignments(updated); await generatePreview(updated);
    } catch (err) { setError(err instanceof Error ? err.message : "人工身份选择无效"); }
  }
  async function unknownDetachOperation(apply: boolean) {
    if (apply && (!detachConfirmed || !detachReview?.eligible || !detachReason.trim() || detachReview.jobId !== jobId)) return;
    const generation = ++detachGeneration.current;
    setBusy(true); setError(""); setDetachConfirmed(false);
    try {
      if (!apply) {
        const review = await reviewOrganizationUnknown(session.token, jobId);
        if (generation === detachGeneration.current) setDetachReview(review);
      } else {
        const saved = await detachOrganizationUnknown(session.token, jobId, detachReview!.fingerprint, detachReason.trim(), true);
        if (generation !== detachGeneration.current) return;
        setJobs((current) => [saved, ...current.filter((item) => item.id !== saved.id)]); setDetachReview(null); setDetachReason("");
        const records = await getOrganizationUnknown(session.token, unknown.page, unknownKeyword);
        if (generation === detachGeneration.current) setUnknown(records);
      }
    } catch (err) {
      if (generation !== detachGeneration.current) return;
      setDetachReview(null);
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else { setError(err instanceof Error ? err.message : "关联处置失败"); try { const saved = await getOrganizationJobs(session.token); if (generation === detachGeneration.current) setJobs(saved); } catch { /* never retry a mutation */ } }
    } finally { if (generation === detachGeneration.current) setBusy(false); }
  }
  async function jobOperation(operation: "create" | "refresh" | "execute" | "reconcile" | "cancel" | "resume-move" | "resume-publication" | "abandon-unpublished" | "reconcile-unknown") {
    if (operation === "reconcile-unknown" && !confirmed) return;
    if (operation === "abandon-unpublished" && (!discardStaging || !oldExecutorsStopped || !job?.sourceRoot || !job.targetRoot)) return;
    if (operation === "resume-publication" && (!confirmed || !job?.sourceRoot || !job.targetRoot)) return;
    if ((operation === "execute" || operation === "resume-move" || operation === "resume-publication") && job?.mode === "move" && (!confirmed || !sourceRemoval || !job.sourceRoot || !job.targetRoot)) return;
    setBusy(true); setError("");
    try {
      if (operation === "refresh") { setJobs(await getOrganizationJobs(session.token)); return; }
      const saved = operation === "create"
        ? await createOrganizationJob(session.token, { sourceId, targetId, path, mode, fingerprint: plan!.fingerprint, ...(assignments.length ? { assignments } : {}), ...(selectedUnknown ? { unknownRecord: { id: selectedUnknown.id, fingerprint: selectedUnknown.fingerprint } } : {}), ...(selectedHistory ? { historyRecord: { id: selectedHistory.id, fingerprint: selectedHistory.fingerprint } } : {}) })
        : operation === "abandon-unpublished" ? await abandonOrganizationJob(session.token, jobId, discardStaging, oldExecutorsStopped)
        : await controlOrganizationJob(session.token, jobId, operation, job?.mode === "move" && (operation === "execute" || operation === "resume-move" || operation === "resume-publication") && sourceRemoval);
      setJobs((current) => [saved, ...current.filter((item) => item.id !== saved.id)]);
      setJobId(saved.id); setPlan(null); setAssignments([]); setSelectedUnknown(null); setSelectedHistory(null); setManualSource("");
      setUnknown(await getOrganizationUnknown(session.token, unknown.page, unknownKeyword));
    } catch (err) {
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else {
        setError(err instanceof Error ? err.message : "整理任务操作失败");
        // A failed response may follow a successful file write: refresh state,
        // but never automatically retry a mutation or reconciliation.
        try { setJobs(await getOrganizationJobs(session.token)); } catch { /* retain the original error */ }
      }
    } finally { setConfirmed(false); setSourceRemoval(false); setDiscardStaging(false); setOldExecutorsStopped(false); setBusy(false); }
  }
  return <WorkspaceLayout user={session.user} currentPath={currentPath} section="媒体整理" page="整理预览" onNavigate={onNavigate} onLogout={onLogout}>
    <section className="organization-preview">
      <h1>整理预览</h1>
      <p>此功能仅限管理员使用。各方式均需先保存任务，再单独确认执行，且不覆盖目标。复制和链接保留源名；移动会移除源名及核验后的恢复备份，需额外授权。硬链接共享文件内容，软链接依赖源文件长期存在。</p>
      {error && <p role="alert">{error}</p>}
      {!loading && <section aria-label="存量未识别记录"><h2>存量未识别记录</h2><p>旧记录只读展示，不删除或自动重跑。选择后须重新预览、纠正身份并另行确认执行；不能只处理目录的一部分就标记成功。</p><label>路径关键词<input value={unknownKeyword} disabled={busy} onChange={(event) => setUnknownKeyword(event.target.value)} /></label><button type="button" disabled={busy} onClick={() => void loadUnknown(1)}>查询／刷新记录</button><p>共 {unknown.total} 条，第 {unknown.page}/{unknown.totalPages} 页</p><div className="organization-table"><table><thead><tr><th>原路径／目标</th><th>原方式／核验</th><th>操作</th></tr></thead><tbody>{unknown.items.map((item) => <tr key={item.id}><td>#{item.id} · {item.path}<small>{item.dest || "未指定目标，需明确选择配置目录"}</small></td><td>{item.mode || "未记录方式，请自行确认"}{item.reason && <small>{item.reason}</small>}</td><td>{item.jobId ? <button type="button" disabled={busy || !roots.executionModes?.length} onClick={() => void loadUnknown(unknown.page, item.jobId)}>查看关联任务</button> : <button type="button" disabled={busy || !!item.reason || !roots.executionModes?.length} onClick={() => selectUnknown(item)}>选择并核验记录</button>}</td></tr>)}</tbody></table></div><button type="button" disabled={busy || unknown.page <= 1} onClick={() => void loadUnknown(unknown.page - 1)}>上一页</button><button type="button" disabled={busy || unknown.page >= unknown.totalPages} onClick={() => void loadUnknown(unknown.page + 1)}>下一页</button></section>}
      {selectedUnknown && <p>关联旧记录 #{selectedUnknown.id}：{selectedUnknown.path}。只有全范围成功才更新此条记录；原路径、方式和其他记录保留。<button type="button" disabled={busy} onClick={() => { invalidate(); setSelectedUnknown(null); }}>取消关联，返回普通规划</button></p>}
      {!loading && <section aria-label="历史记录重新规划"><h2>从历史重新规划</h2><p>保留原历史及原媒体；只从记录中的原来源重新扫描、核验身份并创建新任务。若移动后原来源已不存在，不会改用已发布目标，不直接改名或覆盖。</p><label>标题／源文件名<input value={historyKeyword} disabled={busy} onChange={(event) => setHistoryKeyword(event.target.value)} /></label><button type="button" disabled={busy} onClick={() => void loadHistory(1)}>查询／刷新历史</button><p>共 {history.total} 条，第 {history.page}/{history.totalPages} 页</p><div className="organization-table"><table><thead><tr><th>原身份／来源</th><th>原目标／方式</th><th>操作</th></tr></thead><tbody>{history.items.map((item) => <tr key={item.id}><td>#{item.id} · {item.title} {item.year} · TMDB {item.tmdbId}<small>{item.sourcePath}/{item.sourceFilename}</small></td><td>{item.destPath}/{item.destFilename}<small>{item.mode} · {item.date}</small>{item.reason && <small>{item.reason}</small>}</td><td><button type="button" disabled={busy || !!item.reason || !roots.executionModes?.length} onClick={() => selectHistory(item)}>定位原来源并重新核验</button></td></tr>)}</tbody></table></div><button type="button" disabled={busy || history.page <= 1} onClick={() => void loadHistory(history.page - 1)}>上一页</button><button type="button" disabled={busy || history.page >= history.totalPages} onClick={() => void loadHistory(history.page + 1)}>下一页</button></section>}
      {selectedHistory && <p>原历史 #{selectedHistory.id}：{selectedHistory.title} · TMDB {selectedHistory.tmdbId}。先重新预览，再逐文件纠正身份；原历史不会修改。<button type="button" disabled={busy} onClick={() => { invalidate(); setSelectedHistory(null); }}>取消历史关联</button></p>}
      {loading ? <p>正在读取配置目录…</p> : <form onSubmit={(event) => void preview(event)}>
        <label>来源目录<select value={sourceId} disabled={busy || !!selectedUnknown || !!selectedHistory} required onChange={(e) => { setSourceId(e.target.value); invalidate(); }}><option value="">请选择</option>{roots.sources.map((root) => <option key={root.id} value={root.id}>{root.label} · {root.path}</option>)}</select></label>
        <label>源目录内的相对路径<input value={path} disabled={busy || !!selectedUnknown || !!selectedHistory} required placeholder=". 或 Movies/某个目录" onChange={(e) => { setPath(e.target.value); invalidate(); }} /></label>
        <label>目标目录<select value={targetId} disabled={busy} required onChange={(e) => { setTargetId(e.target.value); invalidate(); }}><option value="">请选择</option>{roots.targets.map((root) => <option key={root.id} value={root.id} disabled={!!selectedDest && selectedDest !== root.path}>{root.label} · {root.path}</option>)}</select></label>
        <label>计划转移方式<select value={mode} disabled={busy || !!selectedUnknown?.nativeMode || !!selectedHistory?.nativeMode} onChange={(e) => { setMode(e.target.value); invalidate(); }}><option value="copy">复制</option><option value="move">移动</option><option value="link">硬链接</option><option value="softlink">软链接</option></select></label>
        <button disabled={busy || !sourceId || !targetId} type="submit">{busy ? "正在核验…" : "生成只读预览"}</button>
        {(!roots.sources.length || !roots.targets.length) && <p>请先在现有配置中设置下载/同步源目录和媒体库目标目录。</p>}
      </form>}
      {plan && <div aria-live="polite"><p>共 {plan.items.length} 个媒体或附件；跳过 {plan.skipped} 个隐藏、非媒体或常见未完成后缀项。此预览不能直接执行，也不证明文件已下载完成。</p>
        <div className="organization-table"><table><thead><tr><th>源文件</th><th>目标相对路径</th><th>核验状态</th></tr></thead><tbody>{plan.items.map((item) => <tr key={item.source}><td>{item.source}<small>{item.size} 字节 · {item.kind}{item.part ? ` · 分段 ${item.part}` : ""}</small>{(item.episodeFormat || !!item.episodeOffset) && <small>集号规则：{item.episodeFormat || "明确编号"} · 偏移 {item.episodeOffset || 0}</small>}</td><td>{item.target || "未生成"}{item.tmdbId && <small>{item.title} {item.year} · TMDB {item.tmdbId}{item.assigned ? " · 人工指定" : ""}</small>}</td><td>{statusLabels[item.status] || item.status}{item.reason && <small>{item.reason}</small>}</td></tr>)}</tbody></table></div>
        <form onSubmit={(event) => void assignIdentity(event)} aria-label="逐文件指定媒体身份">
          <h2>纠正媒体身份</h2><p>只影响所选媒体文件，同名附件仍须唯一匹配；其他文件继续自动识别。TMDB 标题、年份、分类和偏移后的季集上限由服务器核验，不允许自填目标路径，也不能绕过大小、蓝光或冲突限制。应用只重新生成只读预览，不执行整理。</p>
          <label>当前预览中的媒体文件<select value={manualSource} disabled={busy} required onChange={(event) => setManualSource(event.target.value)}><option value="">请选择</option>{plan.items.filter((item) => item.kind === "media").map((item) => <option key={item.source} value={item.source}>{item.source}</option>)}</select></label>
          <label>媒体类型<select value={manualKind} disabled={busy} onChange={(event) => { setManualKind(event.target.value as "movie" | "tv"); setManualSeason(""); setManualEpisode(""); setManualEndEpisode(""); setManualEpisodeFormat(""); setManualEpisodeOffset(""); }}><option value="movie">电影</option><option value="tv">电视剧／动漫</option></select></label>
          <label>TMDB ID<input value={manualId} disabled={busy} required inputMode="numeric" pattern="[1-9][0-9]*" onChange={(event) => setManualId(event.target.value)} /></label>
          {manualKind === "tv" && <>
            <label>季号（0 为特别篇）<input type="number" min="0" max="99" value={manualSeason} disabled={busy} required onChange={(event) => setManualSeason(event.target.value)} /></label>
            <label>集号定位模板（留空则使用下面的明确集号）<input value={manualEpisodeFormat} maxLength={512} disabled={busy} placeholder="Show.第{ep}集.{a}.mkv" onChange={(event) => setManualEpisodeFormat(event.target.value)} /></label>
            <p>模板完整匹配所选文件名（含扩展名）。用 {'{ep}'} 标定数字或 EP11-EP13 范围，其他命名占位符如 {'{a}'} 表示非空可变内容；不支持类型格式、重复或相邻占位符。不会应用到目录中的其他文件。</p>
            <label>起始集号<input type="number" min="1" max="9999" value={manualEpisode} disabled={busy || !!manualEpisodeFormat} required={!manualEpisodeFormat} onChange={(event) => setManualEpisode(event.target.value)} /></label>
            <label>结束集号（单集留空）<input type="number" min="1" max="9999" value={manualEndEpisode} disabled={busy || !!manualEpisodeFormat} onChange={(event) => setManualEndEpisode(event.target.value)} /></label>
            <label>集号偏移（留空为 0，如 -10）<input type="number" min="-99999" max="99999" step="1" value={manualEpisodeOffset} disabled={busy} onChange={(event) => setManualEpisodeOffset(event.target.value)} /></label>
          </>}
          <button type="submit" disabled={busy || !manualSource}>应用所选身份并重新预览</button>
        </form>
        {roots.executionModes?.includes(mode) && <><p>仅将“可规划”的文件保存为待确认{modeLabels[mode] || mode}任务；冲突、未识别和已阻止的文件不会执行。正在编辑的人工身份必须先应用并重新预览。</p><button type="button" disabled={busy || !!manualSource || !plan.items.some((item) => item.status === "available")} onClick={() => void jobOperation("create")}>保存待确认{modeLabels[mode] || mode}任务</button></>}
      </div>}
      {!!assignments.length && <button type="button" disabled={busy} onClick={() => { setAssignments([]); void generatePreview([]); }}>清除所有人工指定并重新预览</button>}
      {!loading && !roots.executionModes?.length && <p>当前运行模式仅支持预览。实际转移只在旧后端已禁用的 Go 模式开放；请勿直接切换未经完整验收的生产环境。</p>}
      {!!roots.executionModes?.length && <section aria-label="持久整理任务">
        <h2>已保存的整理任务</h2>
        <button type="button" disabled={busy} onClick={() => void jobOperation("refresh")}>刷新任务状态</button>
        <label>最近 30 个任务<select value={jobId} disabled={busy} onChange={(event) => { setJobId(event.target.value); setConfirmed(false); setSourceRemoval(false); }}><option value="">请选择</option>{jobs.map((item) => <option key={item.id} value={item.id}>{item.created} · {modeLabels[item.mode] || item.mode} · {item.id.slice(0, 8)} · {jobLabels[item.state] || item.state}</option>)}</select></label>
        {job && <><p>转移方式：{modeLabels[job.mode] || job.mode}。{jobLabels[job.state] || job.state}。中断记录不会自动重跑；“核对已发布文件”只根据保存的凭证确认结果，不会重新转移。</p>
          <p>任务保存的来源：<code>{job.sourceRoot || "未提供，请刷新后确认"}</code><br />任务保存的目标：<code>{job.targetRoot || "未提供，请刷新后确认"}</code></p>
          {job.historyRecordId && <p>由原历史 #{job.historyRecordId} 重新规划。原历史保留，只有新的核验转移成功才追加新历史；不是覆盖原媒体的授权。</p>}
          {job.unknownRecordId && <p>关联旧记录 #{job.unknownRecordId}：{job.unknownState === "resolved" ? "已根据完整成功回执更新为已处理" : job.unknownState === "cancelled" ? "已取消关联，旧记录仍保留" : job.unknownState === "detached" ? "管理员已明确解除关联，未标记旧记录成功" : "仍待处理；全部项目与清理回执完成后才更新"}。</p>}
          {job.unknownDetach && <p>解除关联回执：{job.unknownDetach.actor} · {job.unknownDetach.created} · {job.unknownDetach.reason}。不代表旧记录已处理。</p>}
          {job.unknownState === "pending" && job.items.every((item) => item.state === "completed" && !item.reason) && <section aria-label="明确解除旧记录关联"><p>记录变更、删除或新文件到来导致兼容回执不能完成时，可在所有原项目已成功且清理完成后另行解除关联。此动作不改变旧行、不标识已处理、不动文件或历史，也不释放媒体目标预留。</p><button type="button" disabled={busy} onClick={() => void unknownDetachOperation(false)}>读取原始及当前记录快照</button>{detachReview && <><p>原记录：{detachReview.original.path} → {detachReview.original.dest || "未指定"} · {detachReview.original.mode} · {detachReview.original.state}<br />当前记录：{detachReview.current ? `${detachReview.current.path} → ${detachReview.current.dest || "未指定"} · ${detachReview.current.mode} · ${detachReview.current.state}` : "此 ID 的记录已不存在"}</p><label>处置原因<input value={detachReason} disabled={busy} maxLength={128} onChange={(event) => { setDetachReason(event.target.value); setDetachConfirmed(false); }} /></label><label className="organization-confirm"><input type="checkbox" checked={detachConfirmed} disabled={busy || !detachReview.eligible || !detachReason.trim()} onChange={(event) => setDetachConfirmed(event.target.checked)} />我已核对这份快照和原任务完整成功回执，同意仅解除记录关联；旧记录保持原状并留待单独处理。</label><button type="button" disabled={busy || !detachConfirmed || !detachReview.eligible || !detachReason.trim()} onClick={() => void unknownDetachOperation(true)}>确认仅解除关联并保存审核回执</button></>}</section>}
          {job.unknownState === "pending" && job.items.every((item) => item.state === "completed" && !item.reason) && <><label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />我同意仅核验完整成功账本并补交原未识别记录状态，不重新操作文件。</label><button type="button" disabled={busy || !confirmed} onClick={() => void jobOperation("reconcile-unknown")}>确认核对未识别记录回执</button></>}
          <div className="organization-table"><table><thead><tr><th>源文件</th><th>目标相对路径</th><th>任务状态</th></tr></thead><tbody>{job.items.map((item) => <tr key={item.index}><td>{item.source}{item.part && <small>分段 {item.part}</small>}{(item.episodeFormat || !!item.episodeOffset) && <small>集号规则：{item.episodeFormat || "明确编号"} · 偏移 {item.episodeOffset || 0}</small>}</td><td>{item.target}<small>TMDB {item.tmdbId}{item.assigned ? " · 人工指定" : ""}</small></td><td>{jobLabels[item.state] || item.state}{item.reason && <small>{jobReasonLabels[item.reason] || item.reason}</small>}</td></tr>)}</tbody></table></div>
          {job.mode === "move" && (job.state === "ready" || job.state === "needs_review") && <label className="organization-confirm"><input type="checkbox" checked={sourceRemoval} disabled={busy || !job.sourceRoot || !job.targetRoot} onChange={(event) => setSourceRemoval(event.target.checked)} />我同意从上述任务保存的来源路径移除原源文件名，并在目标核验及历史提交后清理本任务的源恢复备份；不删除新到的同名文件、其他文件或源目录。</label>}
          {job.state === "ready" && <><label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />我已确认源文件下载完成、至少 30 秒未修改，并同意按{modeLabels[job.mode] || job.mode}方式整理以上待执行文件。</label><button type="button" disabled={busy || !confirmed || (job.mode === "move" && (!sourceRemoval || !job.sourceRoot || !job.targetRoot))} onClick={() => void jobOperation("execute")}>确认执行{modeLabels[job.mode] || job.mode}</button></>}
          {job.state === "needs_review" && (job.mode === "move" ? <><p>普通核对不会继续移动。只有保存凭证能证明原文件身份时才可继续；未知状态仍保留，未开始的文件不会由恢复操作执行。</p><label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />我已检查上述任务路径和恢复记录，同意继续核验移动及清理。</label><button type="button" disabled={busy || !confirmed || !sourceRemoval || !job.sourceRoot || !job.targetRoot} onClick={() => void jobOperation("resume-move")}>确认继续移动及恢复清理</button></> : <button type="button" disabled={busy} onClick={() => void jobOperation("reconcile")}>核对已发布文件，不重试转移</button>)}
          {job.state === "needs_review" && job.items.some((item) => item.state === "prepared") && <><p>“继续发布”只使用账本中已完整准备的原暂存对象，重新核验源、暂存和目标，不重新复制、不覆盖现有目标；证据不足时仍保留待核对。移动会在发布核验后按上述授权继续源移除。</p>{job.mode !== "move" && <label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />我已检查上述任务保存的路径，确认源文件已完成，同意继续发布完整暂存对象并提交整理记录。</label>}<button type="button" disabled={busy || !confirmed || !job.sourceRoot || !job.targetRoot || (job.mode === "move" && !sourceRemoval)} onClick={() => void jobOperation("resume-publication")}>确认继续发布已完整暂存文件{job.mode === "move" ? "并完成移动" : ""}</button></>}
          {canAbandonOrganizationJob(job) && <section aria-label="放弃未发布暂存"><p>这是停止整个任务的独立操作，不是继续发布或移动。只清理账本能够证明归属的未发布暂存；包括有写入前凭证、与完整原源核验相符的半写文件。必须重新证明原源文件完整且目标不存在。已发布、已移动、无凭证的执行中状态及未知半成品不会清理，也不会接续复制。所有清理回执保存后才释放目标预留，不写入整理成功历史。</p><label className="organization-confirm"><input type="checkbox" checked={oldExecutorsStopped} disabled={busy} onChange={(event) => setOldExecutorsStopped(event.target.checked)} />我已实际停止旧 Python、未升级的 Go 整理执行器及其他会操作这些路径的任务；勾选此项不会替我停止服务。</label><label className="organization-confirm"><input type="checkbox" checked={discardStaging} disabled={busy || !job.sourceRoot || !job.targetRoot} onChange={(event) => setDiscardStaging(event.target.checked)} />我同意放弃上述整个任务，并删除其已核验的未发布暂存；保留原源文件，不删除媒体库文件或未知对象。</label><button type="button" disabled={busy || !discardStaging || !oldExecutorsStopped || !job.sourceRoot || !job.targetRoot} onClick={() => void jobOperation("abandon-unpublished")}>{job.state === "abandoning" ? "确认续办暂存处置回执" : "确认放弃未发布暂存及整个任务"}</button></section>}
          {job.items.every((item) => item.state === "planned") && <button type="button" disabled={busy} onClick={() => void jobOperation("cancel")}>取消未执行任务</button>}
        </>}
      </section>}
    </section>
  </WorkspaceLayout>;
}
