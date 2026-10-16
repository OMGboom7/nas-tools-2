import { useEffect, useRef, useState } from "react";
import { ApiError, getQbProfiles, reviewQbProfile, recoverQbProfile, type QbProfile, type QbProfileReview } from "../api/client";

export function QbProfiles({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [items, setItems] = useState<QbProfile[]>([]);
  const [review, setReview] = useState<QbProfileReview | null>(null);
  const [page, setPage] = useState(1);
  const [confirm, setConfirm] = useState(false);
  const [settings, setSettings] = useState(false);
  const [start, setStart] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const inFlight = useRef(false);
  function reset() { setReview(null); setConfirm(false); setSettings(false); setStart(false); }
  useEffect(() => { generation.current++; inFlight.current = false; setItems([]); setPage(1); reset(); setBusy(false); setError(""); return () => { generation.current++; }; }, [token]);
  async function operate(action: "read" | "review" | "recover", targetPage = page, id = "") {
    if (inFlight.current || action === "recover" && (!review || !confirm || review.next === "configure" && !settings || review.next === "start" && !start)) return;
    inFlight.current = true; const current = ++generation.current; setBusy(true); setError("");
    try {
      if (action === "review") { const next = await reviewQbProfile(token, id); if (current === generation.current) { reset(); setReview(next); } }
      else {
        if (action === "recover") await recoverQbProfile(token, review!, confirm, settings, start);
        const next = await getQbProfiles(token, targetPage); if (current === generation.current) { setItems(next); setPage(targetPage); reset(); }
      }
    } catch (err) {
      if (current !== generation.current) return;
      reset(); if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "请核实原 qB 设置回执");
      if (action === "recover") { try { const next = await getQbProfiles(token, page); if (current === generation.current) setItems(next); } catch { /* read only, never replay */ } }
    } finally { if (current === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section aria-label="qBittorrent PT 设置回执"><h2>qBittorrent PT 设置回执（管理员）</h2>
    <p>仅处理独立授权后创建、已绑定来源约束的原任务。不重新下种，不接管没有本流程标记的任务，不改目录、分类、标签或限速。设置恢复只禁用分享率、累计和空闲时间上限；未知 HR 仍拦截停止、删除和移动。</p>
    <button type="button" disabled={busy} onClick={() => void operate("read")}>只读刷新 qB 设置回执</button>
    {error && <p role="alert">{error}</p>}
    <ul>{items.map((item) => <li key={item.id}><h3>{item.name} · {item.state}</h3><p>Hash {item.infoHash} · 来源策略 {item.seedingPolicyId} · 上传 {item.options.UploadLimitKB} KiB/s，下载 {item.options.DownloadLimitKB} KiB/s；原选择{item.options.Paused ? "保持暂停" : "核验后启动"}。</p><button type="button" disabled={busy || item.state === "rejected"} onClick={() => void operate("review", page, item.id)}>只读核对原 qB 任务及下一步</button></li>)}</ul>
    <button type="button" disabled={busy || page === 1} onClick={() => void operate("read", page - 1)}>上一页</button><span>第 {page} 页，最多 100 条，优先待处理</span><button type="button" disabled={busy || items.length < 100 || page >= 10000} onClick={() => void operate("read", page + 1)}>下一页</button>
    {review && <fieldset disabled={busy}><legend>原任务 {review.profile.infoHash}</legend><p>qB {review.current.version} · {review.current.state} · 目录 {review.current.directory} · 分类 {review.current.category || "无"} · 下一步 {review.next === "configure" ? "仅禁用三项上限，保持暂停" : review.next === "start" ? "仅启动原任务" : "仅核实当前证据，不写下载器"}。恢复后仍需在原手动/订阅来源核实本地提交回执。</p>
      <label><input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />确认当前审核的准确原任务及下一步范围</label>
      {review.next === "configure" && <label><input type="checkbox" checked={settings} onChange={(e) => setSettings(e.target.checked)} />另行授权禁用该原任务的三项自动做种上限，保持暂停及其余设置</label>}
      {review.next === "start" && <label><input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} />另行授权启动已核验设置的原任务，不重新下种</label>}
      <button type="button" disabled={!confirm || review.next === "configure" && !settings || review.next === "start" && !start} onClick={() => void operate("recover")}>确认仅执行已审核的 qB 下一步</button>
    </fieldset>}
  </section>;
}
