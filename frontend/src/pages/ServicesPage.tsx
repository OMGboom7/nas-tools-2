import { useEffect, useMemo, useState } from "react";
import { ApiError, deleteDownloaderConfig, getServices, setDefaultDownloader, testManagedService, type AuthSession, type ManagedService, type ServicesData, type ServiceTestResult } from "../api/client";
import { ServiceEditor } from "../components/ServiceEditor";
import { WorkspaceLayout } from "../components/WorkspaceLayout";

type ServicesPageProps = {
  session: AuthSession;
  currentPath: string;
  onNavigate: (path: string) => void;
  onLogout: () => Promise<void>;
  onSessionExpired: () => void;
};

type PageState = { kind: "loading" } | { kind: "ready"; data: ServicesData } | { kind: "error"; message: string };
type TestState = ServiceTestResult | { pending: true };

const kindLabels: Record<ManagedService["kind"], string> = { downloader: "下载器", media: "媒体服务器", indexer: "索引器" };

export function ServicesPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: ServicesPageProps) {
  const [state, setState] = useState<PageState>({ kind: "loading" });
  const [kind, setKind] = useState("all");
  const [status, setStatus] = useState("all");
  const [tests, setTests] = useState<Record<string, TestState>>({});
  const [batchTesting, setBatchTesting] = useState(false);
  const [notice, setNotice] = useState("");
  const [editing, setEditing] = useState<{ kind: "downloader" | "media"; service?: ManagedService } | undefined>();
  const [deleting, setDeleting] = useState<ManagedService | undefined>();
  const [deleteText, setDeleteText] = useState("");
  const [deleteBusy, setDeleteBusy] = useState(false);

  async function load(quiet = false) {
    if (!quiet) setState({ kind: "loading" });
    if (!quiet) setNotice("");
    try {
      const data = await getServices(session.token);
      setState({ kind: "ready", data });
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      if (!quiet) setState({ kind: "error", message: error instanceof Error ? error.message : "服务状态加载失败" });
      else setNotice(`错误：${error instanceof Error ? error.message : "刷新失败"}`);
    }
  }

  useEffect(() => { void load(); }, [session.token]);

  const services = useMemo(() => state.kind === "ready"
    ? [...state.data.downloaders, ...state.data.mediaServers, ...state.data.indexers] : [], [state]);
  const filtered = useMemo(() => services.filter((service) => {
    const kindMatch = kind === "all" || service.kind === kind;
    const needsAttention = service.kind === "downloader" ? !service.configured || !service.enabled
      : service.active ? !service.configured : false;
    const statusMatch = status === "all" || (status === "enabled" && service.enabled) || (status === "configured" && service.configured) || (status === "attention" && needsAttention);
    return kindMatch && statusMatch;
  }), [kind, services, status]);
  const testable = filtered.filter((service) => service.canTest);

  function testKey(service: ManagedService) { return `${service.kind}:${service.id}`; }

  async function runTest(service: ManagedService) {
    const key = testKey(service);
    setTests((current) => ({ ...current, [key]: { pending: true } }));
    try {
      const result = await testManagedService(session.token, service);
      setTests((current) => ({ ...current, [key]: result }));
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      setTests((current) => ({ ...current, [key]: { kind: service.kind, id: service.id, ok: false, duration: 0, message: error instanceof Error ? error.message : "测试失败" } }));
    }
  }

  async function testVisible() {
    if (batchTesting || testable.length === 0) return;
    setBatchTesting(true);
    setNotice("");
    let cursor = 0;
    await Promise.all(Array.from({ length: Math.min(3, testable.length) }, async () => {
      while (cursor < testable.length) await runTest(testable[cursor++]);
    }));
    setBatchTesting(false);
    setNotice(`已完成 ${testable.length} 项服务测试。`);
  }

  async function saved() {
    setEditing(undefined);
    await load(true);
    setNotice("已保存服务配置，状态列表已更新。");
  }

  function editService(service: ManagedService) {
    if (service.kind === "downloader" || service.kind === "media") setEditing({ kind: service.kind, service });
  }

  async function makeDefault(service: ManagedService) {
    setNotice("");
    try {
      await setDefaultDownloader(session.token, service.id);
      await load(true);
      setNotice(`已将“${service.name}”设为默认下载器。`);
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      setNotice(`错误：${error instanceof Error ? error.message : "默认下载器设置失败"}`);
    }
  }

  async function confirmDelete() {
    if (!deleting || deleteText !== deleting.name) return;
    setDeleteBusy(true);
    try {
      await deleteDownloaderConfig(session.token, deleting.id);
      setTests((current) => { const next = { ...current }; delete next[testKey(deleting)]; return next; });
      const name = deleting.name;
      setDeleting(undefined);
      await load(true);
      setNotice(`已删除下载器“${name}”。`);
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      setNotice(`错误：${error instanceof Error ? error.message : "下载器删除失败"}`);
    } finally {
      setDeleteBusy(false);
    }
  }

  const configured = services.filter((service) => service.configured).length;
  const enabled = services.filter((service) => service.enabled).length;

  return <WorkspaceLayout user={session.user} currentPath={currentPath} section="服务" page="状态"
    onNavigate={onNavigate} onLogout={onLogout}>
    <div className="services-page">
      <header className="services-heading"><div><p className="eyebrow">SERVICE HEALTH</p><h1>服务状态</h1><p className="summary">查看并配置下载器、媒体服务器和索引器。访问凭据始终留在服务端。</p></div>
        <div className="services-heading__actions"><button className="secondary-button" type="button" onClick={() => void load(true)}>刷新</button><button className="secondary-button" type="button" disabled={batchTesting || testable.length === 0} onClick={() => void testVisible()}>{batchTesting ? "测试中…" : "测试当前列表"}</button><button className="primary-button" type="button" onClick={() => setEditing({ kind: "downloader" })}>新增下载器</button></div></header>

      {notice && <div className={notice.startsWith("错误：") ? "data-warning" : "data-success"} role="status">{notice}</div>}
      {state.kind === "loading" && <div className="service-skeleton"><span /><span /><span /></div>}
      {state.kind === "error" && <section className="empty-state"><span className="empty-state__mark">!</span><h2>服务状态暂时不可用</h2><p>{state.message}</p><button className="secondary-button" type="button" onClick={() => void load()}>重新加载</button></section>}
      {state.kind === "ready" && <>
        {state.data.warnings.length > 0 && <div className="data-warning">部分服务信息暂时不可用，已展示其余可用数据。</div>}
        <section className="service-stats" aria-label="服务概览"><div><span>服务组件</span><strong>{services.length}</strong><small>项</small></div><div><span>已配置</span><strong>{configured}</strong><small>项</small></div><div><span>当前启用</span><strong>{enabled}</strong><small>项</small></div></section>
        <div className="service-toolbar"><select aria-label="按服务类型筛选" value={kind} onChange={(event) => setKind(event.target.value)}><option value="all">全部类型</option><option value="downloader">下载器</option><option value="media">媒体服务器</option><option value="indexer">索引器</option></select>
          <select aria-label="按服务状态筛选" value={status} onChange={(event) => setStatus(event.target.value)}><option value="all">全部状态</option><option value="enabled">当前启用</option><option value="configured">已配置</option><option value="attention">需要处理</option></select><span>显示 {filtered.length} / {services.length}</span></div>
        {filtered.length === 0 ? <div className="inline-empty">没有符合当前筛选条件的服务。</div> : <section className="service-grid" aria-label="服务列表">{filtered.map((service) => <ServiceCard key={testKey(service)} service={service} test={tests[testKey(service)]} batchTesting={batchTesting} onTest={() => void runTest(service)} onEdit={service.kind === "indexer" ? undefined : () => editService(service)} onDelete={service.kind === "downloader" ? () => { setDeleteText(""); setDeleting(service); } : undefined} onDefault={service.kind === "downloader" && !service.default ? () => void makeDefault(service) : undefined} />)}</section>}
      </>}
      {editing && <ServiceEditor session={session} kind={editing.kind} service={editing.service} onClose={() => setEditing(undefined)} onSaved={() => void saved()} onSessionExpired={onSessionExpired} />}
      {deleting && <DeleteDownloaderDialog service={deleting} value={deleteText} busy={deleteBusy} onChange={setDeleteText} onClose={() => setDeleting(undefined)} onConfirm={() => void confirmDelete()} />}
    </div>
  </WorkspaceLayout>;
}

function ServiceCard({ service, test, batchTesting, onTest, onEdit, onDelete, onDefault }: { service: ManagedService; test?: TestState; batchTesting: boolean; onTest: () => void; onEdit?: () => void; onDelete?: () => void; onDefault?: () => void }) {
  const pending = Boolean(test && "pending" in test);
  const result = test && !("pending" in test) ? test : undefined;
  return <article className={`service-card service-card--${service.kind}`}>
    <header><span className="service-card__mark">{service.kind === "downloader" ? "↓" : service.kind === "media" ? "▣" : "⌕"}</span><div><small>{kindLabels[service.kind]}</small><h2>{service.name}</h2></div><span className={`service-state ${service.enabled ? "is-enabled" : ""}`}>{service.enabled ? "已启用" : service.configured ? "已配置" : "未配置"}</span></header>
    <p className="service-host">{service.host || service.summary || "尚未提供连接信息"}</p>
    {service.host && service.summary && <p className="service-summary">{service.summary}</p>}
    <div className="service-tags">{service.default && <span>默认</span>}{service.active && <span>当前使用</span>}{service.monitoring && <span>监控转移</span>}{service.sourceCount > 0 && <span>{service.sourceCount} 个来源</span>}</div>
    <footer><div className={`service-test-result ${result ? result.ok ? "is-success" : "is-failed" : ""}`} aria-live="polite"><i />{pending ? "正在测试…" : result ? result.ok ? "连接正常" : "连接失败" : "尚未测试"}{result && result.duration > 0 && <small>{result.duration} ms</small>}{result && <em>{result.message}</em>}</div>
      <div className="service-card__actions">{onDefault && <button type="button" onClick={onDefault}>设为默认</button>}{onEdit && <button type="button" onClick={onEdit}>编辑</button>}{onDelete && <button type="button" className="is-danger" onClick={onDelete}>删除</button>}<button type="button" onClick={onTest} disabled={!service.canTest || pending || batchTesting}>{pending ? "测试中" : "测试"}</button></div></footer>
  </article>;
}

function DeleteDownloaderDialog({ service, value, busy, onChange, onClose, onConfirm }: { service: ManagedService; value: string; busy: boolean; onChange: (value: string) => void; onClose: () => void; onConfirm: () => void }) {
  return <div className="editor-backdrop" role="presentation"><form className="delete-site-dialog" aria-label={`删除下载器 ${service.name}`} onSubmit={(event) => { event.preventDefault(); onConfirm(); }}>
    <header><div><p className="eyebrow">DELETE DOWNLOADER</p><h2>删除下载器</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
    <div className="delete-warning"><strong>此操作会删除“{service.name}”的连接与目录配置。</strong><p>{service.default ? "它当前是默认下载器，删除后请立即设置新的默认下载器。" : "已有下载任务不会从下载软件中删除，但 NAStool 将不再管理它。"}</p></div>
    <label>请输入下载器名称 <strong>{service.name}</strong> 以确认<input autoFocus value={value} onChange={(event) => onChange(event.target.value)} autoComplete="off" /></label>
    <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="danger-button" disabled={busy || value !== service.name}>{busy ? "删除中…" : "确认删除"}</button></footer>
  </form></div>;
}
