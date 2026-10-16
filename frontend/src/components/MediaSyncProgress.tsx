import { useEffect, useRef, useState } from "react";
import { ApiError, getMediaSyncProgress, type MediaSyncActivity } from "../api/client";

export function MediaSyncProgress({ token, follow = false, onSessionExpired }: { token: string; follow?: boolean; onSessionExpired: () => void }) {
  const [items, setItems] = useState<MediaSyncActivity[]>([]);
  const [watch, setWatch] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const wasFollowing = useRef(false);
  useEffect(() => { generation.current++; wasFollowing.current = false; setItems([]); setWatch(false); setError(""); setRefresh(0); return () => { generation.current++; }; }, [token]);
  useEffect(() => {
    const justFinished = wasFollowing.current && !follow; wasFollowing.current = follow;
    if (!follow && !watch && !refresh && !justFinished) return;
    const current = ++generation.current; const controller = new AbortController(); let busy = false;
    async function load() {
      if (busy || controller.signal.aborted) return; busy = true;
      try { const next = await getMediaSyncProgress(token, controller.signal); if (current === generation.current) { setItems(next); setError(""); } }
      catch (err) { if (controller.signal.aborted || current !== generation.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "进度不可用"); }
      finally { busy = false; }
    }
    void load(); const timer = follow || watch ? window.setInterval(() => void load(), 1500) : undefined;
    return () => { controller.abort(); if (timer !== undefined) window.clearInterval(timer); generation.current++; };
  }, [token, follow, watch, refresh, onSessionExpired]);
  return <section aria-label="媒体库同步处理进度"><h3>媒体库同步处理进度</h3>
    <p>手动与定时共用持久回执。显示真实处理计数，不估算未知总量的百分比；采集/待提交不等于成功。collecting 也可能是进程中断后尚待核实的记录。</p>
    <button type="button" onClick={() => setRefresh((value) => value + 1)}>只读刷新进度</button>{" "}
    <label><input type="checkbox" checked={watch} onChange={(e) => setWatch(e.target.checked)} />只读观察进度，不触发或重试同步</label>
    {error && <p role="alert">{error}</p>}
    <ul>{items.map((item) => <li key={item.id}><strong>{item.kind === "manual" ? "手动" : "定时"} · {item.server} · {item.state}</strong><p>{item.progress.stage} · 媒体库 {item.progress.librariesDone}/{item.progress.librariesTotal} · 已处理影视 {item.progress.itemsDone} · 集数 {item.progress.episodesDone} · 内容页 {item.progress.pagesRead}</p><p>{item.reason} · {item.updated} {item.state === "completed" && item.snapshotGeneration ? `· 快照已提交 ${item.snapshotGeneration}` : "· 不作为同步成功证明"}</p></li>)}</ul>
  </section>;
}
