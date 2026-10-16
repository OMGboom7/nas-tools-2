import { useEffect, useRef, useState } from "react";
import { ApiError, getMediaSyncLibraries, getMediaSyncStatus, synchronizeMediaLibraries, type MediaSyncCatalog, type MediaSyncStatus } from "../api/client";

export function MediaSync({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [status, setStatus] = useState<MediaSyncStatus | null>(null);
  const [catalog, setCatalog] = useState<MediaSyncCatalog | null>(null);
  const [selected, setSelected] = useState<string[]>([]);
  const [confirm, setConfirm] = useState(false);
  const [legacyStopped, setLegacyStopped] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const inFlight = useRef(false);
  useEffect(() => {
    generation.current++; inFlight.current = false;
    setStatus(null); setCatalog(null); setSelected([]); setConfirm(false); setLegacyStopped(false); setBusy(false); setError("");
    return () => { generation.current++; };
  }, [token]);
  async function operate(action: "status" | "catalog" | "sync", page = 1) {
    if (inFlight.current || action === "sync" && (!catalog || !confirm || !legacyStopped || !selected.length)) return;
    inFlight.current = true; const current = ++generation.current; setBusy(true); setError("");
    try {
      if (action === "catalog") {
        const next = await getMediaSyncLibraries(token);
        if (current === generation.current) { setCatalog(next); setSelected([]); setConfirm(false); setLegacyStopped(false); }
      } else {
        if (action === "sync") await synchronizeMediaLibraries(token, catalog!, selected, confirm, legacyStopped);
        const next = await getMediaSyncStatus(token, page);
        if (current === generation.current) setStatus(next);
      }
    } catch (err) {
      if (current !== generation.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "快照不可用，请只读刷新核对");
      if (action === "sync") {
        // Only read after a lost response. Never resubmit synchronization.
        try { const next = await getMediaSyncStatus(token); if (current === generation.current) setStatus(next); } catch { /* retain prior visible snapshot */ }
      }
      if (action === "catalog") setCatalog(null);
    } finally {
      if (current === generation.current) { setConfirm(false); inFlight.current = false; setBusy(false); }
    }
  }
  return <section className="content-section" aria-label="媒体库展示快照">
    <h2>媒体库展示快照（管理员）</h2>
    <p>手动同步 Emby、Jellyfin 或 Plex 所选影视库。完整读取后一次性替换该服务器类型的旧登记簿，取消、缺页或失败时保留旧数据。未选择的旧库项目会从此展示快照移除，不删除媒体文件。</p>
    <p>仅供展示，不作为下载查重证据；本批不启动定时同步。请先停止旧 Python 媒体库同步任务。</p>
    <button type="button" className="secondary-button" disabled={busy} onClick={() => void operate("status")}>只读刷新快照</button>{" "}
    <button type="button" className="secondary-button" disabled={busy} onClick={() => void operate("catalog")}>读取可同步媒体库</button>
    {busy && <p role="status">处理中，大型媒体库可能需要数分钟；请勿重复提交。</p>}
    {error && <p role="alert">{error}</p>}
    {catalog && <fieldset disabled={busy}><legend>选择 {catalog.server} 媒体库</legend>
      {catalog.libraries.map((library) => <label key={library.id}><input type="checkbox" checked={selected.includes(library.id)} onChange={(e) => { setSelected(e.target.checked ? [...selected, library.id] : selected.filter((id) => id !== library.id)); setConfirm(false); }} />{library.name} · {library.type === "movie" ? "电影" : "剧集"}</label>)}
      {!catalog.libraries.length && <p>没有可同步的影视库</p>}
      <p><label><input type="checkbox" checked={legacyStopped} onChange={(e) => setLegacyStopped(e.target.checked)} />已停止旧媒体库同步任务，避免两个实现同时写入</label></p>
      <p><label><input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />确认仅用所选库完整替换此服务器类型的展示登记簿</label></p>
      <button type="button" disabled={!confirm || !legacyStopped || !selected.length} onClick={() => void operate("sync")}>开始手动同步</button>
    </fieldset>}
    {status && <div>
      {!status.snapshot.generation ? <p>暂无 Go 同步快照；旧 Python 登记簿尚未替换。</p> : <>
        <p>{status.snapshot.server} · {status.snapshot.updated} · 电影 {status.snapshot.movies} 部 / 剧集 {status.snapshot.series} 部 · 操作人 {status.snapshot.actor}</p>
        <p>{status.current ? "配置匹配，但内容仅代表上次同步时的状态。" : "服务器配置已变化，此快照属于旧配置，不代表当前媒体库。"}</p>
        <ul>{status.snapshot.items.map((item) => <li key={item.id}>{item.title} {item.year} · {item.type === "movie" ? "电影" : `${item.episodes?.length || 0} 集`} · {item.library}</li>)}</ul>
        <button type="button" disabled={busy || status.snapshot.page <= 1} onClick={() => void operate("status", status.snapshot.page - 1)}>上一页</button>{" "}
        <span>第 {status.snapshot.page} 页 · 共 {status.snapshot.total} 项</span>{" "}
        <button type="button" disabled={busy || status.snapshot.page * 100 >= status.snapshot.total} onClick={() => void operate("status", status.snapshot.page + 1)}>下一页</button>
      </>}
    </div>}
  </section>;
}
