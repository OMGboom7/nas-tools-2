import { useEffect, useRef, useState } from "react";
import { ApiError, getTransmissionProfiles, reviewTransmissionProfile, recoverTransmissionProfile, type TransmissionProfile, type TransmissionProfileReview } from "../api/client";
export function TransmissionProfiles({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [items, setItems] = useState<TransmissionProfile[]>([]); const [review, setReview] = useState<TransmissionProfileReview | null>(null); const [page, setPage] = useState(1); const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  const [confirm, setConfirm] = useState(false); const [settings, setSettings] = useState(false); const [start, setStart] = useState(false); const generation = useRef(0); const inFlight = useRef(false);
  function resetReview() { setReview(null); setConfirm(false); setSettings(false); setStart(false); }
  useEffect(() => { generation.current++; inFlight.current = false; setItems([]); resetReview(); setPage(1); setError(""); setBusy(false); return () => { generation.current++; }; }, [token]);
  async function operate(action: "load" | "review" | "recover", targetPage = page, id = "") {
    if (inFlight.current || action === "recover" && (!review || !confirm || review.next === "configure" && !settings || review.next === "start" && !start)) return;
    inFlight.current = true; const current = ++generation.current; setBusy(true); setError("");
    try {
      if (action === "review") { const next = await reviewTransmissionProfile(token, id); if (current === generation.current) { resetReview(); setReview(next); } }
      else { if (action === "recover") await recoverTransmissionProfile(token, review!, confirm, settings, start); const next = await getTransmissionProfiles(token, targetPage); if (current === generation.current) { setItems(next); setPage(targetPage); resetReview(); } }
    } catch (err) {
      if (current !== generation.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "请核对原设置回执"); resetReview();
      if (action === "recover") { try { const next = await getTransmissionProfiles(token, page); if (current === generation.current) setItems(next); } catch { /* never replay settings or start */ } }
    } finally { if (current === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section aria-label="Transmission 下载设置回执"><h2>Transmission 下载设置回执（管理员）</h2><p>先暂停添加，按原配置应用每任务限速、分享率上限及空闲做种上限，回读核验后才按原选择启动。空闲上限不是 PT 最低做种时间；已知最低要求仍不能忽略。此处不重复添加、不改目录或标签、不删文件。</p><button type="button" disabled={busy} onClick={() => void operate("load")}>只读刷新设置批次</button>{error && <p role="alert">{error}</p>}
    <ul>{items.map((item) => <li key={item.id}><h3>{item.name} · {item.state}</h3><p>{item.id} · Hash {item.infoHash} · {item.reason}。上传 {item.limits.uploadKB} KiB/s，下载 {item.limits.downloadKB} KiB/s，分享率 {item.limits.ratio}，空闲 {item.limits.idleMinutes} 分钟；原选择{item.options.Paused ? "保持暂停" : "配置后启动"}。0 表示禁用该每任务上限，不等于禁用所有全局速度限制。</p><button type="button" disabled={busy || item.state === "rejected"} onClick={() => void operate("review", page, item.id)}>只读核对原任务及下一步</button></li>)}</ul>
    <button type="button" disabled={busy || page === 1} onClick={() => void operate("load", page - 1)}>上一页</button><span>第 {page} 页，最多 100 条，优先待处理</span><button type="button" disabled={busy || items.length < 100 || page >= 10000} onClick={() => void operate("load", page + 1)}>下一页</button>
    {review && <fieldset disabled={busy}><p>操作者 {review.actor} · 原状态 {review.profile.state} · 当前任务状态 {review.current.status} · 当前目录 {review.current.directory} · 下一步：{review.next === "configure" ? "仅应用原冻结设置，不启动" : review.next === "start" ? "仅启动已核验设置的原任务" : "只确认当前证据，不发送任务写入"}。设置恢复后，仍需在原手动／订阅来源明确核实本地提交回执。</p><p>当前标签：{review.current.labels.join("、")}。响应不确定且缺乏正向证据时保持原意图，不通过重发或去掉专用身份标记接管任务。</p><label><input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />确认当前审核的原任务与下一步范围</label>{review.next === "configure" && <label><input type="checkbox" checked={settings} onChange={(e) => setSettings(e.target.checked)} />另行授权按冻结值写入限速和停种上限，保留暂停、目录及全部标签</label>}{review.next === "start" && <label><input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} />另行授权启动该原任务，不重新下种</label>}<button type="button" disabled={!confirm || review.next === "configure" && !settings || review.next === "start" && !start} onClick={() => void operate("recover")}>确认仅执行已审核的下一步</button></fieldset>}
  </section>;
}
