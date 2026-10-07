import { useEffect, useState, type FormEvent } from "react";
import { ApiError, getOrganizationRoots, previewOrganization, getOrganizationJobs, createOrganizationJob, controlOrganizationJob, type AuthSession, type OrganizationJob, type OrganizationPlan, type OrganizationRoots } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";

type Props = { session: AuthSession; currentPath: string; onNavigate: (path: string) => void; onLogout: () => Promise<void>; onSessionExpired: () => void };
const statusLabels: Record<string, string> = { available: "可规划", conflict: "目标冲突", blocked: "已阻止", unmatched: "附件未匹配", unrecognized: "媒体未识别" };
const jobLabels: Record<string, string> = { ready: "待执行", completed: "已完成", needs_review: "需核对，禁止盲目重试", cancelled: "已取消", planned: "等待执行", running: "执行中或已中断", prepared: "复制凭证已保存，待核实" };

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
  const job = jobs.find((item) => item.id === jobId);
  useEffect(() => {
    let active = true;
    setLoading(true);
    void getOrganizationRoots(session.token).then(async (data) => {
      if (!active) return;
      setRoots(data); setSourceId(data.sources[0]?.id || ""); setTargetId(data.targets[0]?.id || ""); setError("");
      if (data.executionModes?.includes("copy")) {
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
  async function jobOperation(operation: "create" | "refresh" | "execute" | "reconcile" | "cancel") {
    setBusy(true); setError("");
    try {
      if (operation === "refresh") { setJobs(await getOrganizationJobs(session.token)); return; }
      const saved = operation === "create"
        ? await createOrganizationJob(session.token, { sourceId, targetId, path, mode, fingerprint: plan!.fingerprint })
        : await controlOrganizationJob(session.token, jobId, operation);
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
    } finally { setConfirmed(false); setBusy(false); }
  }
  return <WorkspaceLayout user={session.user} currentPath={currentPath} section="媒体整理" page="整理预览" onNavigate={onNavigate} onLogout={onLogout}>
    <section className="organization-preview">
      <h1>整理预览</h1>
      <p>此功能仅限管理员使用。生成预览不会改动文件；本地复制需先保存任务，再单独确认执行，且不会覆盖目标或删除源文件。移动与链接模式目前仅支持预览。</p>
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
        {mode === "copy" && roots.executionModes?.includes("copy") && <><p>仅将“可规划”的文件保存为待确认复制任务；冲突、未识别和已阻止的文件不会执行。</p><button type="button" disabled={busy || !plan.items.some((item) => item.status === "available")} onClick={() => void jobOperation("create")}>保存待确认复制任务</button></>}
      </div>}
      {!loading && !roots.executionModes?.includes("copy") && <p>当前运行模式仅支持预览。实际复制只在旧后端已禁用的 Go 模式开放；请勿直接切换未经完整验收的生产环境。</p>}
      {roots.executionModes?.includes("copy") && <section aria-label="持久复制任务">
        <h2>已保存的复制任务</h2>
        <button type="button" disabled={busy} onClick={() => void jobOperation("refresh")}>刷新任务状态</button>
        <label>最近 30 个任务<select value={jobId} disabled={busy} onChange={(event) => { setJobId(event.target.value); setConfirmed(false); }}><option value="">请选择</option>{jobs.map((item) => <option key={item.id} value={item.id}>{item.created} · {item.id.slice(0, 8)} · {jobLabels[item.state] || item.state}</option>)}</select></label>
        {job && <><p>{jobLabels[job.state] || job.state}。中断记录不会自动重跑；“核对已发布文件”只根据保存的凭证确认结果，不会重新复制。</p>
          <div className="organization-table"><table><thead><tr><th>源文件</th><th>目标相对路径</th><th>任务状态</th></tr></thead><tbody>{job.items.map((item) => <tr key={item.index}><td>{item.source}</td><td>{item.target}</td><td>{jobLabels[item.state] || item.state}{item.reason && <small>{item.reason}</small>}</td></tr>)}</tbody></table></div>
          {job.state === "ready" && <><label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />我已确认源文件下载完成、至少 30 秒未修改，并同意复制以上待执行文件。</label><button type="button" disabled={busy || !confirmed} onClick={() => void jobOperation("execute")}>确认执行复制</button></>}
          {job.state === "needs_review" && <button type="button" disabled={busy} onClick={() => void jobOperation("reconcile")}>核对已发布文件，不重试复制</button>}
          {job.items.every((item) => item.state === "planned") && <button type="button" disabled={busy} onClick={() => void jobOperation("cancel")}>取消未执行任务</button>}
        </>}
      </section>}
    </section>
  </WorkspaceLayout>;
}
