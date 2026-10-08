import { useEffect, useState, type FormEvent } from "react";
import { ApiError, getOrganizationRoots, previewOrganization, getOrganizationJobs, createOrganizationJob, controlOrganizationJob, type AuthSession, type OrganizationJob, type OrganizationPlan, type OrganizationRoots } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";

type Props = { session: AuthSession; currentPath: string; onNavigate: (path: string) => void; onLogout: () => Promise<void>; onSessionExpired: () => void };
const statusLabels: Record<string, string> = { available: "可规划", conflict: "目标冲突", blocked: "已阻止", unmatched: "附件未匹配", unrecognized: "媒体未识别" };
const jobLabels: Record<string, string> = { ready: "待执行", completed: "已完成", needs_review: "需核对，禁止盲目重试", cancelled: "已取消", planned: "等待执行", running: "执行中或已中断", prepared: "转移凭证已保存，待核实", moving: "源移除意图已保存，需核验恢复对象", quarantined: "源文件已转入恢复目录，待提交" };
const modeLabels: Record<string,string> = {copy:"复制",link:"硬链接",softlink:"软链接",move:"移动"};
const jobReasonLabels: Record<string, string> = {
  "Move source recovery cleanup pending; explicit source-removal confirmation required": "历史已提交，源恢复备份清理尚未确认完成；请检查后明确授权续办。",
  "Move target recovery cleanup pending": "源恢复备份已清理，目标暂存清理尚未确认完成；请核验续办。",
  "Execution interrupted or failed; verify the saved proof before any retry": "操作已中断或失败；必须核验保存凭证，不能盲目重试。",
};

export function OrganizationPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: Props) {
  const [roots, setRoots] = useState<OrganizationRoots>({ sources: [], targets: [] });
  const [sourceId, setSourceId] = useState("");
  const [targetId, setTargetId] = useState("");
  const [path, setPath] = useState(".");
  const [mode, setMode] = useState("copy");
  const [plan, setPlan] = useState<OrganizationPlan | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [jobs, setJobs] = useState<OrganizationJob[]>([]);
  const [jobId, setJobId] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [sourceRemoval, setSourceRemoval] = useState(false);
  const job = jobs.find((item) => item.id === jobId);
  useEffect(() => { setConfirmed(false); setSourceRemoval(false); }, [jobId, session.token]);
  useEffect(() => {
    let active = true;
    setLoading(true);
    void getOrganizationRoots(session.token).then(async (data) => {
      if (!active) return;
      setRoots(data); setSourceId(data.sources[0]?.id || ""); setTargetId(data.targets[0]?.id || ""); setError("");
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
    event.preventDefault(); setBusy(true); setError(""); setPlan(null);
    try { setPlan(await previewOrganization(session.token, { sourceId, targetId, path, mode })); }
    catch (err) {
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "整理预览失败");
    } finally { setBusy(false); }
  }
  function invalidate() { setPlan(null); setError(""); }
  async function jobOperation(operation: "create" | "refresh" | "execute" | "reconcile" | "cancel" | "resume-move") {
    if ((operation === "execute" || operation === "resume-move") && job?.mode === "move" && (!confirmed || !sourceRemoval || !job.sourceRoot || !job.targetRoot)) return;
    setBusy(true); setError("");
    try {
      if (operation === "refresh") { setJobs(await getOrganizationJobs(session.token)); return; }
      const saved = operation === "create"
        ? await createOrganizationJob(session.token, { sourceId, targetId, path, mode, fingerprint: plan!.fingerprint })
        : await controlOrganizationJob(session.token, jobId, operation, job?.mode === "move" && (operation === "execute" || operation === "resume-move") && sourceRemoval);
      setJobs((current) => [saved, ...current.filter((item) => item.id !== saved.id)]);
      setJobId(saved.id); setPlan(null);
    } catch (err) {
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else {
        setError(err instanceof Error ? err.message : "整理任务操作失败");
        // A failed response may follow a successful file write: refresh state,
        // but never automatically retry a mutation or reconciliation.
        try { setJobs(await getOrganizationJobs(session.token)); } catch { /* retain the original error */ }
      }
    } finally { setConfirmed(false); setSourceRemoval(false); setBusy(false); }
  }
  return <WorkspaceLayout user={session.user} currentPath={currentPath} section="媒体整理" page="整理预览" onNavigate={onNavigate} onLogout={onLogout}>
    <section className="organization-preview">
      <h1>整理预览</h1>
      <p>此功能仅限管理员使用。各方式均需先保存任务，再单独确认执行，且不覆盖目标。复制和链接保留源名；移动会移除源名及核验后的恢复备份，需额外授权。硬链接共享文件内容，软链接依赖源文件长期存在。</p>
      {error && <p role="alert">{error}</p>}
      {loading ? <p>正在读取配置目录…</p> : <form onSubmit={(event) => void preview(event)}>
        <label>来源目录<select value={sourceId} disabled={busy} required onChange={(e) => { setSourceId(e.target.value); invalidate(); }}><option value="">请选择</option>{roots.sources.map((root) => <option key={root.id} value={root.id}>{root.label} · {root.path}</option>)}</select></label>
        <label>源目录内的相对路径<input value={path} disabled={busy} required placeholder=". 或 Movies/某个目录" onChange={(e) => { setPath(e.target.value); invalidate(); }} /></label>
        <label>目标目录<select value={targetId} disabled={busy} required onChange={(e) => { setTargetId(e.target.value); invalidate(); }}><option value="">请选择</option>{roots.targets.map((root) => <option key={root.id} value={root.id}>{root.label} · {root.path}</option>)}</select></label>
        <label>计划转移方式<select value={mode} disabled={busy} onChange={(e) => { setMode(e.target.value); invalidate(); }}><option value="copy">复制</option><option value="move">移动</option><option value="link">硬链接</option><option value="softlink">软链接</option></select></label>
        <button disabled={busy || !sourceId || !targetId} type="submit">{busy ? "正在核验…" : "生成只读预览"}</button>
        {(!roots.sources.length || !roots.targets.length) && <p>请先在现有配置中设置下载/同步源目录和媒体库目标目录。</p>}
      </form>}
      {plan && <div aria-live="polite"><p>共 {plan.items.length} 个媒体或附件；跳过 {plan.skipped} 个隐藏、非媒体或常见未完成后缀项。此预览不能直接执行，也不证明文件已下载完成。</p>
        <div className="organization-table"><table><thead><tr><th>源文件</th><th>目标相对路径</th><th>核验状态</th></tr></thead><tbody>{plan.items.map((item) => <tr key={item.source}><td>{item.source}<small>{item.size} 字节 · {item.kind}</small></td><td>{item.target || "未生成"}{item.tmdbId && <small>TMDB {item.tmdbId}</small>}</td><td>{statusLabels[item.status] || item.status}{item.reason && <small>{item.reason}</small>}</td></tr>)}</tbody></table></div>
        {roots.executionModes?.includes(mode) && <><p>仅将“可规划”的文件保存为待确认{modeLabels[mode] || mode}任务；冲突、未识别和已阻止的文件不会执行。</p><button type="button" disabled={busy || !plan.items.some((item) => item.status === "available")} onClick={() => void jobOperation("create")}>保存待确认{modeLabels[mode] || mode}任务</button></>}
      </div>}
      {!loading && !roots.executionModes?.length && <p>当前运行模式仅支持预览。实际转移只在旧后端已禁用的 Go 模式开放；请勿直接切换未经完整验收的生产环境。</p>}
      {!!roots.executionModes?.length && <section aria-label="持久整理任务">
        <h2>已保存的整理任务</h2>
        <button type="button" disabled={busy} onClick={() => void jobOperation("refresh")}>刷新任务状态</button>
        <label>最近 30 个任务<select value={jobId} disabled={busy} onChange={(event) => { setJobId(event.target.value); setConfirmed(false); setSourceRemoval(false); }}><option value="">请选择</option>{jobs.map((item) => <option key={item.id} value={item.id}>{item.created} · {modeLabels[item.mode] || item.mode} · {item.id.slice(0, 8)} · {jobLabels[item.state] || item.state}</option>)}</select></label>
        {job && <><p>转移方式：{modeLabels[job.mode] || job.mode}。{jobLabels[job.state] || job.state}。中断记录不会自动重跑；“核对已发布文件”只根据保存的凭证确认结果，不会重新转移。</p>
          <p>任务保存的来源：<code>{job.sourceRoot || "未提供，请刷新后确认"}</code><br />任务保存的目标：<code>{job.targetRoot || "未提供，请刷新后确认"}</code></p>
          <div className="organization-table"><table><thead><tr><th>源文件</th><th>目标相对路径</th><th>任务状态</th></tr></thead><tbody>{job.items.map((item) => <tr key={item.index}><td>{item.source}</td><td>{item.target}</td><td>{jobLabels[item.state] || item.state}{item.reason && <small>{jobReasonLabels[item.reason] || item.reason}</small>}</td></tr>)}</tbody></table></div>
          {job.mode === "move" && (job.state === "ready" || job.state === "needs_review") && <label className="organization-confirm"><input type="checkbox" checked={sourceRemoval} disabled={busy || !job.sourceRoot || !job.targetRoot} onChange={(event) => setSourceRemoval(event.target.checked)} />我同意从上述任务保存的来源路径移除原源文件名，并在目标核验及历史提交后清理本任务的源恢复备份；不删除新到的同名文件、其他文件或源目录。</label>}
          {job.state === "ready" && <><label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />我已确认源文件下载完成、至少 30 秒未修改，并同意按{modeLabels[job.mode] || job.mode}方式整理以上待执行文件。</label><button type="button" disabled={busy || !confirmed || (job.mode === "move" && (!sourceRemoval || !job.sourceRoot || !job.targetRoot))} onClick={() => void jobOperation("execute")}>确认执行{modeLabels[job.mode] || job.mode}</button></>}
          {job.state === "needs_review" && (job.mode === "move" ? <><p>普通核对不会继续移动。只有保存凭证能证明原文件身份时才可继续；未知状态仍保留，未开始的文件不会由恢复操作执行。</p><label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />我已检查上述任务路径和恢复记录，同意继续核验移动及清理。</label><button type="button" disabled={busy || !confirmed || !sourceRemoval || !job.sourceRoot || !job.targetRoot} onClick={() => void jobOperation("resume-move")}>确认继续移动及恢复清理</button></> : <button type="button" disabled={busy} onClick={() => void jobOperation("reconcile")}>核对已发布文件，不重试转移</button>)}
          {job.items.every((item) => item.state === "planned") && <button type="button" disabled={busy} onClick={() => void jobOperation("cancel")}>取消未执行任务</button>}
        </>}
      </section>}
    </section>
  </WorkspaceLayout>;
}
