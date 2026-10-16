import { useEffect, useRef, useState } from "react";
import { ApiError, applyPTAdmission, getPTAdmission, reviewPTAdmission, type PTAdmissionReview } from "../api/client";

export function PTAdmission({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [id, setId] = useState("");
  const [enabled, setEnabled] = useState(true);
  const [status, setStatus] = useState<PTAdmissionReview | null>(null);
  const [review, setReview] = useState<PTAdmissionReview | null>(null);
  const [confirm, setConfirm] = useState(false);
  const [oldStopped, setOldStopped] = useState(false);
  const [hold, setHold] = useState(false);
  const [unlimited, setUnlimited] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const inFlight = useRef(false);
  function reset() { setReview(null); setConfirm(false); setOldStopped(false); setHold(false); setUnlimited(false); }
  useEffect(() => { generation.current++; inFlight.current = false; setBusy(false); setStatus(null); setError(""); reset(); return () => { generation.current++; }; }, [token]);
  async function operate(action: "read" | "review" | "apply") {
    if (inFlight.current || action === "apply" && (!review || !confirm || review.enabled && (!oldStopped || !hold || !unlimited))) return;
    inFlight.current = true; const current = ++generation.current; setBusy(true); setError("");
    const target = Number(id);
    try {
      if (action === "review") { const next = await reviewPTAdmission(token, target, enabled); if (current === generation.current) { reset(); setReview(next); } }
      else {
        if (action === "apply") await applyPTAdmission(token, review!, confirm, oldStopped, hold, unlimited);
        const next = await getPTAdmission(token, target); if (current === generation.current) { setStatus(next); reset(); }
      }
    } catch (err) {
      if (current !== generation.current) return;
      reset(); setStatus(null);
      if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "请只读核对原接管授权");
      if (action === "apply") { try { const next = await getPTAdmission(token, target); if (current === generation.current) setStatus(next); } catch { /* never repeat consent or downloader actions */ } }
    } finally { if (current === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section aria-label="PT 自动接管授权"><h2>qBittorrent / Transmission PT 自动接管授权（管理员）</h2>
    <p>默认关闭，按准确下载器配置启用。覆盖搜索下种、订阅搜索与订阅 RSS 中带正值最低要求的新任务。不携带这些元数据的其他来源不会因此获得此类接管能力；此操作本身不向下载器发送写入。</p>
    <fieldset disabled={busy}><label>准确下载器 ID<input inputMode="numeric" value={id} onChange={(e) => { setId(e.target.value); setStatus(null); reset(); }} /></label>
      <label><input type="checkbox" checked={enabled} onChange={(e) => { setEnabled(e.target.checked); reset(); }} />本次目标：启用接管（取消勾选表示撤销后续接管）</label>
      <button type="button" onClick={() => void operate("read")}>只读刷新接管授权</button><button type="button" onClick={() => void operate("review")}>只读审核本次变更</button>
    </fieldset>
    {error && <p role="alert">{error}</p>}
    {status && <p>{status.name} · {status.type} · 当前配置{status.active ? "已启用" : "未启用或配置已变化"}；原授权人 {status.grant?.actor ?? "无"}，时间 {status.grant?.created ?? "无"}。</p>}
    {review && <fieldset disabled={busy}><legend>{review.enabled ? "启用当前配置的新任务接管" : "撤销后续接管"}</legend><p>{review.name} · {review.type}。{review.warning}</p>
      <label><input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />确认本次审核的准确下载器配置与变更范围</label>
      {review.enabled && <><label><input type="checkbox" checked={oldStopped} onChange={(e) => setOldStopped(e.target.checked)} />已实际停止旧 Go / Python 执行器</label>
        <label><input type="checkbox" checked={hold} onChange={(e) => setHold(e.target.checked)} />另行确认未知最低要求或 HR 会持续拦截停止、删除、移动；限时人工核实须另行审核，不会删改原约束</label>
        <label><input type="checkbox" checked={unlimited} onChange={(e) => setUnlimited(e.target.checked)} />另行授权这类新任务禁用自动做种上限（qB：分享率、累计与空闲时间；TR：分享率与空闲时间）；所选上限必须为 0，否则拒绝，不会静默覆盖</label></>}
      <button type="button" disabled={!confirm || review.enabled && (review.type !== "transmission" && review.type !== "qbittorrent" || !oldStopped || !hold || !unlimited)} onClick={() => void operate("apply")}>确认仅更新接管授权</button>
    </fieldset>}
  </section>;
}
