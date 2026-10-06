import { useEffect, useMemo, useState } from "react";
import { ApiError, getPlugins, installPlugin, uninstallPlugin, type AuthSession, type PluginSummary, type PluginsData } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";
import { PluginConfigEditor } from "../components/PluginConfigEditor";
import { PluginPageViewer } from "../components/PluginPageViewer";

type PluginsPageProps = {
  session: AuthSession;
  currentPath: string;
  onNavigate: (path: string) => void;
  onLogout: () => Promise<void>;
  onSessionExpired: () => void;
};

type PageState = { kind: "loading" } | { kind: "ready"; data: PluginsData } | { kind: "error"; message: string };
type Filter = "all" | "installed" | "available";

export function PluginsPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: PluginsPageProps) {
  const [state, setState] = useState<PageState>({ kind: "loading" });
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [removing, setRemoving] = useState<PluginSummary>();
  const [editing, setEditing] = useState<PluginSummary>();
  const [viewing, setViewing] = useState<PluginSummary>();
  const [confirmation, setConfirmation] = useState("");
  const [notice, setNotice] = useState("");

  async function load(quiet = false) {
    if (!quiet) setState({ kind: "loading" });
    try {
      setState({ kind: "ready", data: await getPlugins(session.token) });
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      const message = error instanceof Error ? error.message : "插件列表加载失败";
      if (quiet) setNotice(`错误：${message}`); else setState({ kind: "error", message });
    }
  }

  useEffect(() => { void load(); }, [session.token]);

  const data = state.kind === "ready" ? state.data : undefined;
  const filtered = useMemo(() => (data?.items || []).filter((plugin) => {
    const needle = query.trim().toLocaleLowerCase();
    const matchesText = !needle || `${plugin.name} ${plugin.description} ${plugin.author}`.toLocaleLowerCase().includes(needle);
    const matchesFilter = filter === "all" || (filter === "installed" && plugin.installed) || (filter === "available" && !plugin.installed);
    return matchesText && matchesFilter;
  }), [data, filter, query]);

  async function install(plugin: PluginSummary) {
    setBusy((current) => ({ ...current, [plugin.id]: true }));
    setNotice("");
    try {
      await installPlugin(session.token, plugin.id);
      await load(true);
      setNotice(`已安装插件“${plugin.name}”。`);
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      setNotice(`错误：${error instanceof Error ? error.message : "插件安装失败"}`);
    } finally {
      setBusy((current) => ({ ...current, [plugin.id]: false }));
    }
  }

  async function confirmUninstall() {
    if (!removing || confirmation !== removing.name) return;
    const plugin = removing;
    setBusy((current) => ({ ...current, [plugin.id]: true }));
    setNotice("");
    try {
      await uninstallPlugin(session.token, plugin.id);
      setRemoving(undefined);
      setConfirmation("");
      await load(true);
      setNotice(`已卸载插件“${plugin.name}”。`);
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      setNotice(`错误：${error instanceof Error ? error.message : "插件卸载失败"}`);
    } finally {
      setBusy((current) => ({ ...current, [plugin.id]: false }));
    }
  }

  async function configured(plugin: PluginSummary) {
    setEditing(undefined);
    await load(true);
    setNotice(`已保存“${plugin.name}”的配置并重新加载插件。`);
  }

  return <WorkspaceLayout user={session.user} currentPath={currentPath} section="插件" page="插件管理"
    onNavigate={onNavigate} onLogout={onLogout}>
    <div className="plugins-page">
      <header className="services-heading"><div><p className="eyebrow">PLUGIN CATALOG</p><h1>插件管理</h1><p className="summary">查看插件、运行状态并安全编辑配置。扩展页面以结构化只读内容展示，旧版动态脚本和操作事件不会返回浏览器。</p></div>
        <div className="services-heading__actions"><button className="secondary-button" type="button" onClick={() => void load(true)}>刷新</button></div></header>

      {notice && <div className={notice.startsWith("错误：") ? "data-warning" : "data-success"} role="status">{notice}</div>}
      {state.kind === "loading" && <div className="plugin-skeleton"><span /><span /><span /></div>}
      {state.kind === "error" && <section className="empty-state"><span className="empty-state__mark">!</span><h2>插件列表暂时不可用</h2><p>{state.message}</p><button className="secondary-button" type="button" onClick={() => void load()}>重新加载</button></section>}
      {data && <>
        <section className="service-stats" aria-label="插件概览"><div><span>目录插件</span><strong>{data.items.length}</strong><small>个</small></div><div><span>已安装</span><strong>{data.installedCount}</strong><small>个</small></div><div><span>已确认运行</span><strong>{data.runningCount}</strong><small>{data.unknownStateCount ? `另有 ${data.unknownStateCount} 个状态待迁移` : "个"}</small></div></section>
        <div className="plugin-toolbar"><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索名称、简介或作者" aria-label="搜索插件" />
          <select value={filter} onChange={(event) => setFilter(event.target.value as Filter)} aria-label="按安装状态筛选"><option value="all">全部插件</option><option value="installed">已安装</option><option value="available">可安装</option></select><span>显示 {filtered.length} / {data.items.length}</span></div>
        {filtered.length === 0 ? <div className="inline-empty">没有符合当前筛选条件的插件。</div> : <section className="plugin-grid" aria-label="插件列表">{filtered.map((plugin) => <PluginCard key={plugin.id} plugin={plugin} busy={Boolean(busy[plugin.id])} onView={() => setViewing(plugin)} onConfigure={() => setEditing(plugin)} onInstall={() => void install(plugin)} onUninstall={() => { setConfirmation(""); setRemoving(plugin); }} />)}</section>}
      </>}
      {viewing && <PluginPageViewer session={session} plugin={viewing} onClose={() => setViewing(undefined)} onSessionExpired={onSessionExpired} />}
      {editing && <PluginConfigEditor session={session} plugin={editing} onClose={() => setEditing(undefined)} onSaved={() => void configured(editing)} onSessionExpired={onSessionExpired} />}
      {removing && <UninstallDialog plugin={removing} value={confirmation} busy={Boolean(busy[removing.id])} onChange={setConfirmation} onClose={() => setRemoving(undefined)} onConfirm={() => void confirmUninstall()} />}
    </div>
  </WorkspaceLayout>;
}

function PluginCard({ plugin, busy, onView, onConfigure, onInstall, onUninstall }: { plugin: PluginSummary; busy: boolean; onView: () => void; onConfigure: () => void; onInstall: () => void; onUninstall: () => void }) {
  const status = plugin.stateKnown === false ? "状态待迁移" : plugin.running ? "运行中" : plugin.installed ? "未运行" : "未安装";
  const unavailable = busy || plugin.actionsAvailable === false;
  return <article className={`plugin-card ${plugin.running ? "is-running" : ""}`}>
    <header><span className="plugin-card__mark">{plugin.name.slice(0, 1).toUpperCase()}</span><div><small>{plugin.version ? `VERSION ${plugin.version}` : "PLUGIN"}</small><h2 title={plugin.name}>{plugin.name}</h2></div><span className={`service-state ${plugin.running ? "is-enabled" : ""}`}>{status}</span></header>
    <p className="plugin-description">{plugin.description || "暂无插件简介。"}</p>
    <div className="plugin-meta"><span>{plugin.configurable ? "可配置" : "无需配置"}</span>{plugin.hasPage && <span>含只读扩展页</span>}{plugin.actionsAvailable === false && <span>功能尚未迁移</span>}</div>
    <footer><div className="plugin-author"><small>作者</small>{plugin.authorUrl ? <a href={plugin.authorUrl} target="_blank" rel="noreferrer">{plugin.author || "查看主页"}</a> : <strong>{plugin.author || "未知"}</strong>}</div>
      <div className="plugin-card__actions">{plugin.installed && plugin.hasPage && <button className="plugin-action" type="button" disabled={unavailable} onClick={onView}>查看</button>}{plugin.installed && plugin.configurable && <button className="plugin-action" type="button" disabled={unavailable} onClick={onConfigure}>配置</button>}{plugin.installed ? <button className="plugin-action is-danger" type="button" disabled={unavailable} onClick={onUninstall}>{busy ? "处理中…" : "卸载"}</button> : <button className="plugin-action" type="button" disabled={unavailable} onClick={onInstall}>{busy ? "安装中…" : "安装"}</button>}</div></footer>
  </article>;
}

function UninstallDialog({ plugin, value, busy, onChange, onClose, onConfirm }: { plugin: PluginSummary; value: string; busy: boolean; onChange: (value: string) => void; onClose: () => void; onConfirm: () => void }) {
  return <div className="editor-backdrop" role="presentation"><form className="delete-site-dialog" aria-label={`卸载插件 ${plugin.name}`} onSubmit={(event) => { event.preventDefault(); onConfirm(); }}>
    <header><div><p className="eyebrow">UNINSTALL PLUGIN</p><h2>卸载插件</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
    <div className="delete-warning"><strong>此操作会停止“{plugin.name}”并从已安装列表移除。</strong><p>已保存的插件配置不会由本页面展示。重新安装后，旧系统可能继续使用原配置。</p></div>
    <label>请输入插件名称 <strong>{plugin.name}</strong> 以确认<input autoFocus value={value} onChange={(event) => onChange(event.target.value)} autoComplete="off" /></label>
    <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="danger-button" disabled={busy || value !== plugin.name}>{busy ? "卸载中…" : "确认卸载"}</button></footer>
  </form></div>;
}
