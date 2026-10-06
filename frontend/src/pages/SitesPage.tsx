import { useEffect, useMemo, useState } from "react";
import { ApiError, deleteSite, getSites, testSiteConnection, type AuthSession, type SiteSummary, type SiteTestResult, type SitesData } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";
import { SiteEditor } from "../components/SiteEditor";

type SitesPageProps = {
  session: AuthSession;
  currentPath: string;
  onNavigate: (path: string) => void;
  onLogout: () => Promise<void>;
  onSessionExpired: () => void;
};

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; data: SitesData }
  | { kind: "error"; message: string };

type TestState = SiteTestResult | { id: string; pending: true };

export function SitesPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: SitesPageProps) {
  const [state, setState] = useState<PageState>({ kind: "loading" });
  const [query, setQuery] = useState("");
  const [capability, setCapability] = useState("all");
  const [tests, setTests] = useState<Record<string, TestState>>({});
  const [actionError, setActionError] = useState("");
  const [actionMessage, setActionMessage] = useState("");
  const [editing, setEditing] = useState<SiteSummary | null | undefined>(undefined);
  const [batchTesting, setBatchTesting] = useState(false);
  const [deleting, setDeleting] = useState<SiteSummary | undefined>();
  const [deleteText, setDeleteText] = useState("");
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteError, setDeleteError] = useState("");

  async function load(quiet = false) {
    if (!quiet) setState({ kind: "loading" });
    try {
      const data = await getSites(session.token);
      setState({ kind: "ready", data });
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      if (!quiet) setState({ kind: "error", message: error instanceof Error ? error.message : "站点列表加载失败" });
      else setActionError(error instanceof Error ? error.message : "刷新失败");
    }
  }

  useEffect(() => { void load(); }, [session.token]);

  const filtered = useMemo(() => {
    if (state.kind !== "ready") return [];
    const needle = query.trim().toLocaleLowerCase();
    return state.data.items.filter((site) => {
      const matchesQuery = !needle || site.name.toLocaleLowerCase().includes(needle) || site.host.toLocaleLowerCase().includes(needle);
      const matchesCapability = capability === "all"
        || (capability === "rss" && site.rssEnabled)
        || (capability === "brush" && site.brushEnabled)
        || (capability === "statistic" && site.statisticEnabled);
      return matchesQuery && matchesCapability;
    });
  }, [capability, query, state]);

  async function testConnection(site: SiteSummary) {
    setActionError("");
    setActionMessage("");
    setTests((current) => ({ ...current, [site.id]: { id: site.id, pending: true } }));
    try {
      const result = await testSiteConnection(session.token, site.id);
      setTests((current) => ({ ...current, [site.id]: result }));
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      const message = error instanceof Error ? error.message : "连接测试失败";
      setTests((current) => ({ ...current, [site.id]: { id: site.id, ok: false, duration: 0, message } }));
    }
  }

  async function testVisibleSites() {
    if (batchTesting || filtered.length === 0) return;
    setBatchTesting(true);
    setActionError("");
    setActionMessage("");
    let cursor = 0;
    const workers = Array.from({ length: Math.min(3, filtered.length) }, async () => {
      while (cursor < filtered.length) {
        const site = filtered[cursor++];
        await testConnection(site);
      }
    });
    await Promise.all(workers);
    setBatchTesting(false);
    setActionMessage(`已完成 ${filtered.length} 个站点的连接测试。`);
  }

  const total = state.kind === "ready" ? state.data.items.length : 0;
  const rssCount = state.kind === "ready" ? state.data.items.filter((site) => site.rssEnabled).length : 0;
  const statisticCount = state.kind === "ready" ? state.data.items.filter((site) => site.statisticEnabled).length : 0;

  async function saved() {
    setEditing(undefined);
    setActionMessage("站点已保存，列表已更新。");
    await load(true);
  }

  function requestDelete(site: SiteSummary) {
    setDeleteText("");
    setDeleteError("");
    setDeleting(site);
  }

  async function confirmDelete() {
    if (!deleting || deleteText !== deleting.name) return;
    setDeleteBusy(true);
    setDeleteError("");
    try {
      await deleteSite(session.token, deleting.id);
      setTests((current) => { const next = { ...current }; delete next[deleting.id]; return next; });
      setActionMessage(`站点“${deleting.name}”已删除。`);
      setDeleting(undefined);
      await load(true);
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      setDeleteError(error instanceof Error ? error.message : "站点删除失败");
    } finally {
      setDeleteBusy(false);
    }
  }

  return (
    <WorkspaceLayout user={session.user} currentPath={currentPath} section="站点管理" page="站点"
      onNavigate={onNavigate} onLogout={onLogout}>
      <div className="sites-page">
        <header className="sites-heading">
          <div><p className="eyebrow">SITE DIRECTORY</p><h1>站点管理</h1><p className="summary">集中查看站点用途并按需测试连接。敏感凭据只保留在服务端。</p></div>
          <div className="sites-heading__actions"><button className="secondary-button" type="button" onClick={() => void load(true)} disabled={state.kind === "loading"}>刷新</button><button className="secondary-button" type="button" onClick={() => void testVisibleSites()} disabled={batchTesting || filtered.length === 0}>{batchTesting ? "测试中…" : "测试当前列表"}</button><button className="primary-button" type="button" onClick={() => setEditing(null)}>新增站点</button></div>
        </header>

        {actionError && <div className="data-warning" role="alert">{actionError}</div>}
        {actionMessage && <div className="data-success" role="status">{actionMessage}</div>}
        {state.kind === "loading" && <div className="site-skeleton"><span /><span /><span /></div>}
        {state.kind === "error" && <section className="empty-state"><span className="empty-state__mark">!</span><h2>站点数据暂时不可用</h2><p>{state.message}</p><button className="secondary-button" type="button" onClick={() => void load()}>重新加载</button></section>}
        {state.kind === "ready" && <>
          <section className="site-stats" aria-label="站点概览">
            <div><span>已配置</span><strong>{total}</strong><small>个站点</small></div>
            <div><span>参与订阅</span><strong>{rssCount}</strong><small>个站点</small></div>
            <div><span>数据统计</span><strong>{statisticCount}</strong><small>个站点</small></div>
          </section>

          <div className="site-toolbar">
            <label className="site-search"><span>⌕</span><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索站点名称或域名" /></label>
            <select aria-label="按用途筛选" value={capability} onChange={(event) => setCapability(event.target.value)}>
              <option value="all">全部用途</option><option value="rss">订阅</option><option value="brush">刷流</option><option value="statistic">数据统计</option>
            </select>
            <span className="site-toolbar__count">显示 {filtered.length} / {total}</span>
          </div>

          {filtered.length === 0 ? <div className="inline-empty">没有符合当前筛选条件的站点。</div> : (
            <section className="site-grid" aria-label="站点列表">
              {filtered.map((site) => <SiteCard key={site.id} site={site} test={tests[site.id]} batchTesting={batchTesting} onDelete={() => requestDelete(site)} onEdit={() => setEditing(site)} onTest={() => void testConnection(site)} />)}
            </section>
          )}
        </>}
        {editing !== undefined && <SiteEditor session={session} site={editing || undefined} onClose={() => setEditing(undefined)} onSaved={() => void saved()} onSessionExpired={onSessionExpired} />}
        {deleting && <DeleteSiteDialog site={deleting} value={deleteText} busy={deleteBusy} error={deleteError} onChange={setDeleteText} onClose={() => setDeleting(undefined)} onConfirm={() => void confirmDelete()} />}
      </div>
    </WorkspaceLayout>
  );
}

function SiteCard({ site, test, batchTesting, onTest, onEdit, onDelete }: { site: SiteSummary; test?: TestState; batchTesting: boolean; onTest: () => void; onEdit: () => void; onDelete: () => void }) {
  const pending = test && "pending" in test;
  const result = test && !("pending" in test) ? test : undefined;
  return (
    <article className="site-card">
      <header><div className="site-priority" title={`优先级 ${site.priority}`}>{site.priority}</div><div><h2>{site.name}</h2><p>{site.host || "未配置公开域名"}</p></div></header>
      <div className="site-capabilities">
        {site.capabilities.length > 0 ? site.capabilities.map((item) => <span key={item}>{item}</span>) : <span className="is-muted">未启用用途</span>}
      </div>
      <footer>
        <div className={`site-test-result ${result ? result.ok ? "is-success" : "is-failed" : ""}`} aria-live="polite">
          {pending ? <><i />正在测试…</> : result ? <><i />{result.ok ? "连接正常" : "连接失败"}{result.duration > 0 && <small>{result.duration} ms</small>}<em>{result.message}</em></> : <><i />尚未测试</>}
        </div>
        <div className="site-card__actions"><button type="button" onClick={onEdit}>编辑</button><button type="button" className="is-danger" onClick={onDelete}>删除</button><button type="button" onClick={onTest} disabled={pending || batchTesting}>{pending ? "测试中" : "测试连接"}</button></div>
      </footer>
    </article>
  );
}

function DeleteSiteDialog({ site, value, busy, error, onChange, onClose, onConfirm }: { site: SiteSummary; value: string; busy: boolean; error: string; onChange: (value: string) => void; onClose: () => void; onConfirm: () => void }) {
  return <div className="editor-backdrop" role="presentation"><form className="delete-site-dialog" aria-label={`删除站点 ${site.name}`} onSubmit={(event) => { event.preventDefault(); onConfirm(); }}>
    <header><div><p className="eyebrow">DELETE SITE</p><h2>删除站点</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
    <div className="delete-warning"><strong>此操作会删除“{site.name}”的本地站点配置。</strong><p>现有下载记录不会删除，但订阅、刷流和统计将不再使用该站点。</p></div>
    <label>请输入站点名称 <strong>{site.name}</strong> 以确认<input autoFocus value={value} onChange={(event) => onChange(event.target.value)} autoComplete="off" /></label>
    {error && <div className="form-error" role="alert">{error}</div>}
    <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="danger-button" disabled={busy || value !== site.name}>{busy ? "删除中…" : "确认删除"}</button></footer>
  </form></div>;
}
