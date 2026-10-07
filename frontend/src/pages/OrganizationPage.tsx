import { useEffect, useState, type FormEvent } from "react";
import { ApiError, getOrganizationRoots, previewOrganization, type AuthSession, type OrganizationPlan, type OrganizationRoots } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";

type Props = { session: AuthSession; currentPath: string; onNavigate: (path: string) => void; onLogout: () => Promise<void>; onSessionExpired: () => void };
const statusLabels: Record<string, string> = { available: "可规划", conflict: "目标冲突", blocked: "已阻止", unmatched: "附件未匹配", unrecognized: "媒体未识别" };

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
  useEffect(() => {
    let active = true;
    setLoading(true);
    void getOrganizationRoots(session.token).then((data) => {
      if (!active) return;
      setRoots(data); setSourceId(data.sources[0]?.id || ""); setTargetId(data.targets[0]?.id || ""); setError("");
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
  return <WorkspaceLayout user={session.user} currentPath={currentPath} section="媒体整理" page="整理预览" onNavigate={onNavigate} onLogout={onLogout}>
    <section className="organization-preview">
      <h1>整理预览</h1>
      <p>核对媒体身份、文件命名和目标冲突。此功能仅限管理员使用；这里只生成计划，不会创建目录、移动、覆盖或删除文件，实际转移尚未接入。</p>
      {error && <p role="alert">{error}</p>}
      {loading ? <p>正在读取配置目录…</p> : <form onSubmit={(event) => void preview(event)}>
        <label>来源目录<select value={sourceId} disabled={busy} required onChange={(e) => { setSourceId(e.target.value); invalidate(); }}><option value="">请选择</option>{roots.sources.map((root) => <option key={root.id} value={root.id}>{root.label} · {root.path}</option>)}</select></label>
        <label>源目录内的相对路径<input value={path} disabled={busy} required placeholder=". 或 Movies/某个目录" onChange={(e) => { setPath(e.target.value); invalidate(); }} /></label>
        <label>目标目录<select value={targetId} disabled={busy} required onChange={(e) => { setTargetId(e.target.value); invalidate(); }}><option value="">请选择</option>{roots.targets.map((root) => <option key={root.id} value={root.id}>{root.label} · {root.path}</option>)}</select></label>
        <label>计划转移方式<select value={mode} disabled={busy} onChange={(e) => { setMode(e.target.value); invalidate(); }}><option value="copy">复制</option><option value="move">移动</option><option value="link">硬链接</option><option value="softlink">软链接</option></select></label>
        <button disabled={busy || !sourceId || !targetId} type="submit">{busy ? "正在核验…" : "生成只读预览"}</button>
        {(!roots.sources.length || !roots.targets.length) && <p>请先在现有配置中设置下载/同步源目录和媒体库目标目录。</p>}
      </form>}
      {plan && <div aria-live="polite"><p>共 {plan.items.length} 个媒体或附件；跳过 {plan.skipped} 个隐藏、非媒体或常见未完成后缀项。此计划不能直接执行，也不证明文件已下载完成。</p>
        <div className="organization-table"><table><thead><tr><th>源文件</th><th>目标相对路径</th><th>核验状态</th></tr></thead><tbody>{plan.items.map((item) => <tr key={item.source}><td>{item.source}<small>{item.size} 字节 · {item.kind}</small></td><td>{item.target || "未生成"}{item.tmdbId && <small>TMDB {item.tmdbId}</small>}</td><td>{statusLabels[item.status] || item.status}{item.reason && <small>{item.reason}</small>}</td></tr>)}</tbody></table></div>
      </div>}
    </section>
  </WorkspaceLayout>;
}
