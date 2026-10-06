import { useEffect, useState } from "react";
import { ApiError, addMagnetDownload, addSiteTorrentLink, addTorrentDownload, controlDownload, getDownloads, getSites, type AuthSession, type DownloadsData, type DownloadTask, type SiteSummary } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";

type DownloadsPageProps = {
  session: AuthSession;
  currentPath: string;
  onNavigate: (path: string) => void;
  onLogout: () => Promise<void>;
  onSessionExpired: () => void;
};

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; data: DownloadsData }
  | { kind: "error"; message: string };

export function DownloadsPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: DownloadsPageProps) {
  const [historyPage, setHistoryPage] = useState(1);
  const [state, setState] = useState<PageState>({ kind: "loading" });
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [actionError, setActionError] = useState("");
  const [magnet, setMagnet] = useState("");
  const [addingMagnet, setAddingMagnet] = useState(false);
  const [actionNotice, setActionNotice] = useState("");
  const [torrentFile, setTorrentFile] = useState<File | null>(null);
  const [addingTorrent, setAddingTorrent] = useState(false);
  const [torrentInputKey, setTorrentInputKey] = useState(0);
  const [sites, setSites] = useState<SiteSummary[]>([]);
  const [linkSiteId, setLinkSiteId] = useState("");
  const [torrentLink, setTorrentLink] = useState("");
  const [addingLink, setAddingLink] = useState(false);

  useEffect(() => {
    let active = true;
    async function load(quiet = false) {
      if (!quiet) setState({ kind: "loading" });
      try {
        const data = await getDownloads(session.token, historyPage);
        if (active) setState({ kind: "ready", data });
      } catch (error) {
        if (!active) return;
        if (error instanceof ApiError && error.code === 401) {
          onSessionExpired();
          return;
        }
        if (!quiet) setState({ kind: "error", message: error instanceof Error ? error.message : "下载任务加载失败" });
      }
    }
    void load();
    const timer = window.setInterval(() => void load(true), 10_000);
    return () => { active = false; window.clearInterval(timer); };
  }, [historyPage, session.token, onSessionExpired]);

  useEffect(() => {
    let active = true;
    void getSites(session.token).then((result) => {
      if (!active) return;
      const configured = result.items.filter((site) => site.host !== "");
      setSites(configured);
      setLinkSiteId((current) => current || configured[0]?.id || "");
    }).catch((error) => {
      if (active && error instanceof ApiError && error.code === 401) onSessionExpired();
    });
    return () => { active = false; };
  }, [session.token, onSessionExpired]);

  async function refresh() {
    setActionError("");
    try {
      const data = await getDownloads(session.token, historyPage);
      setState({ kind: "ready", data });
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      setActionError(error instanceof Error ? error.message : "刷新失败");
    }
  }

  async function handleAction(task: DownloadTask, action: "start" | "stop" | "remove") {
    if (action === "remove" && !window.confirm("确定删除这个下载任务？部分下载器可能同时删除已下载文件。")) return;
    setBusy((current) => ({ ...current, [task.id]: true }));
    setActionError("");
    try {
      await controlDownload(session.token, task.id, action);
      await refresh();
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      setActionError(error instanceof Error ? error.message : "任务操作失败");
    } finally {
      setBusy((current) => ({ ...current, [task.id]: false }));
    }
  }

  async function handleMagnetSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setAddingMagnet(true);
    setActionError("");
    setActionNotice("");
    try {
      await addMagnetDownload(session.token, magnet.trim());
      setMagnet("");
      setActionNotice("磁力任务已提交到默认下载器。");
      await refresh();
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) { onSessionExpired(); return; }
      setActionError(error instanceof Error ? error.message : "磁力任务添加失败");
    } finally {
      setAddingMagnet(false);
    }
  }

  async function handleTorrentSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!torrentFile) return;
    setAddingTorrent(true);
    setActionError("");
    setActionNotice("");
    try {
      await addTorrentDownload(session.token, torrentFile);
      setTorrentFile(null);
      setTorrentInputKey((value) => value + 1);
      setActionNotice("种子文件已提交到默认下载器。");
      await refresh();
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) { onSessionExpired(); return; }
      setActionError(error instanceof Error ? error.message : "种子文件添加失败");
    } finally {
      setAddingTorrent(false);
    }
  }

  async function handleLinkSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!linkSiteId) return;
    setAddingLink(true);
    setActionError("");
    setActionNotice("");
    try {
      await addSiteTorrentLink(session.token, linkSiteId, torrentLink.trim());
      setTorrentLink("");
      setActionNotice("站点种子已提交到默认下载器。");
      await refresh();
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) { onSessionExpired(); return; }
      setActionError(error instanceof Error ? error.message : "站点种子链接添加失败");
    } finally {
      setAddingLink(false);
    }
  }

  return (
    <WorkspaceLayout user={session.user} currentPath={currentPath} section="下载管理" page="任务"
      onNavigate={onNavigate} onLogout={onLogout}>
      <div className="downloads-page">
        <header className="downloads-heading">
          <div><p className="eyebrow">DOWNLOAD CENTER</p><h1>下载任务</h1><p className="summary">查看下载进度，控制当前任务，并回顾最近的下载记录。</p></div>
          <button className="secondary-button" type="button" onClick={() => void refresh()} disabled={state.kind === "loading"}>刷新</button>
        </header>

        {actionError && <div className="data-warning" role="alert">{actionError}</div>}
        {actionNotice && <div className="data-notice" role="status">{actionNotice}</div>}
        <form className="download-magnet-form" onSubmit={(event) => void handleMagnetSubmit(event)}>
          <label htmlFor="download-magnet">添加磁力链接</label>
          <div><input id="download-magnet" type="text" value={magnet} onChange={(event) => setMagnet(event.target.value)} placeholder="magnet:?xt=urn:btih:…" maxLength={4096} required />
            <button type="submit" disabled={addingMagnet || magnet.trim() === ""}>{addingMagnet ? "提交中…" : "添加任务"}</button></div>
          <p>使用当前默认下载器；支持 qBittorrent、Transmission 和 Aria2。</p>
        </form>
        <form className="download-magnet-form" onSubmit={(event) => void handleTorrentSubmit(event)}>
          <label htmlFor="download-torrent">添加种子文件</label>
          <div><input key={torrentInputKey} id="download-torrent" type="file" accept=".torrent,application/x-bittorrent" onChange={(event) => setTorrentFile(event.target.files?.[0] || null)} required />
            <button type="submit" disabled={addingTorrent || !torrentFile}>{addingTorrent ? "上传中…" : "上传并添加"}</button></div>
          <p>最多 8 MB；文件直接交给当前默认下载器，不经过旧服务。</p>
        </form>
        <form className="download-magnet-form" onSubmit={(event) => void handleLinkSubmit(event)}>
          <label htmlFor="download-site-link">从已配置站点添加种子链接</label>
          <div><select aria-label="选择站点" value={linkSiteId} onChange={(event) => setLinkSiteId(event.target.value)} disabled={sites.length === 0}>
              {sites.length === 0 ? <option value="">暂无站点</option> : sites.map((site) => <option key={site.id} value={site.id}>{site.name}</option>)}
            </select>
            <input id="download-site-link" type="url" value={torrentLink} onChange={(event) => setTorrentLink(event.target.value)} placeholder="https://站点地址/download.php?id=…" maxLength={4096} required />
            <button type="submit" disabled={addingLink || !linkSiteId || torrentLink.trim() === ""}>{addingLink ? "提交中…" : "获取并添加"}</button></div>
          <p>仅接受所选站点的同协议、同主机链接；需要跳转或首次下载确认的站点暂不支持。</p>
        </form>
        {state.kind === "loading" && <DownloadSkeleton />}
        {state.kind === "error" && (
          <section className="empty-state"><span className="empty-state__mark">!</span><h2>下载数据暂时不可用</h2><p>{state.message}</p><button className="secondary-button" type="button" onClick={() => void refresh()}>重新加载</button></section>
        )}
        {state.kind === "ready" && <>
          {state.data.warnings.length > 0 && <div className="data-warning">部分下载数据暂时不可用，页面会继续自动重试。</div>}
          <section className="download-section">
            <div className="section-heading"><div><p className="eyebrow">ACTIVE TASKS</p><h2>正在下载</h2></div><span>{state.data.active.length} 个任务</span></div>
            {state.data.active.length === 0 ? <div className="inline-empty">当前下载器中没有正在下载的任务。</div> : (
              <div className="download-task-list">{state.data.active.map((task) => <TaskCard key={task.id} task={task} busy={busy[task.id]}
                onAction={(action) => void handleAction(task, action)} />)}</div>
            )}
          </section>

          <section className="download-section">
            <div className="section-heading"><div><p className="eyebrow">RECENT HISTORY</p><h2>近期下载</h2></div><span>第 {historyPage} 页</span></div>
            {state.data.history.length === 0 ? <div className="inline-empty">这一页没有下载记录。</div> : (
              <div className="download-history-grid">{state.data.history.map((item, index) => (
                <article className="download-history-card" key={`${item.id}-${item.date}-${index}`}>
                  <div className="download-history-art"><strong>{item.title.slice(0, 1)}</strong>{item.image && <img src={item.image} alt={`${item.title} 海报`} loading="lazy" />}</div>
                  <div><span>{[item.type, item.year].filter(Boolean).join(" · ") || "下载记录"}</span><h3>{item.title}</h3><p>{item.torrent || "未记录资源名称"}</p><small>{[item.site, item.date].filter(Boolean).join(" · ")}</small></div>
                </article>
              ))}</div>
            )}
            <div className="history-pager"><button type="button" className="secondary-button" disabled={historyPage <= 1} onClick={() => setHistoryPage((page) => page - 1)}>上一页</button><button type="button" className="secondary-button" disabled={state.data.history.length === 0} onClick={() => setHistoryPage((page) => page + 1)}>下一页</button></div>
          </section>
        </>}
      </div>
    </WorkspaceLayout>
  );
}

function TaskCard({ task, busy, onAction }: { task: DownloadTask; busy?: boolean; onAction: (action: "start" | "stop" | "remove") => void }) {
  const stopped = ["stoped", "stopped", "paused"].includes(task.state.toLowerCase());
  return (
    <article className="download-task">
      <div className="download-task__art"><strong>{task.title.slice(0, 1)}</strong>{task.image && <img src={task.image} alt="" loading="lazy" />}</div>
      <div className="download-task__body">
        <div className="download-task__title"><div>{task.siteUrl ? <a href={task.siteUrl} target="_blank" rel="noreferrer">{task.title}</a> : <h3>{task.title}</h3>}<span>{task.speed || task.state || "等待中"}</span></div><strong>{Math.round(task.progress)}%</strong></div>
        {task.showProgress && <div className="download-progress" role="progressbar" aria-label={`${task.title} 下载进度`} aria-valuenow={task.progress} aria-valuemin={0} aria-valuemax={100}><span style={{ width: `${task.progress}%` }} /></div>}
      </div>
      {(task.canControl || task.canRemove) && <div className="download-task__actions">
        {task.canControl && <button type="button" disabled={busy} onClick={() => onAction(stopped ? "start" : "stop")}>{busy ? "处理中…" : stopped ? "继续" : "暂停"}</button>}
        {task.canRemove && <button type="button" className="is-danger" disabled={busy} onClick={() => onAction("remove")}>删除</button>}
      </div>}
    </article>
  );
}

function DownloadSkeleton() {
  return <div className="downloads-skeleton"><span /><span /><span /></div>;
}
