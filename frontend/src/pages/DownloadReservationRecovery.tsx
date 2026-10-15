import { useEffect, useRef, useState } from "react";
import { ApiError, findDownloadOrganization, reviewDownloadReservation, recoverDownloadReservation, type DownloadOrganizationBatch, type DownloadReservationReview } from "../api/client";

type Props = { token: string; enabled: boolean; batch: DownloadOrganizationBatch; onRecovered: (batch: DownloadOrganizationBatch) => void; onSessionExpired: () => void };
export function DownloadReservationRecovery({ token, enabled, batch, onRecovered, onSessionExpired }: Props) {
  const [reason, setReason] = useState(""); const [review, setReview] = useState<DownloadReservationReview | null>(null);
  const [confirm, setConfirm] = useState(false); const [oldStopped, setOldStopped] = useState(false); const [omitted, setOmitted] = useState(false);
  const [busy, setBusy] = useState(false); const [error, setError] = useState(""); const generation = useRef(0); const inFlight = useRef(false);
  function clearReview() { setReview(null); setConfirm(false); setOldStopped(false); setOmitted(false); }
  useEffect(() => { generation.current++; inFlight.current = false; setReason(""); clearReview(); setBusy(false); setError(""); return () => { generation.current++; }; }, [token, batch.id, batch.state]);
  async function operate(mutate: boolean) {
    if (!enabled || inFlight.current || !reason.trim() || mutate && (!review || !confirm || !oldStopped || review.omitted.length > 0 && !omitted)) return;
    const current = ++generation.current; inFlight.current = true; setBusy(true); setError("");
    try {
      if (mutate) { const next = await recoverDownloadReservation(token, batch.id, review!, confirm, oldStopped, omitted); if (current === generation.current) { clearReview(); onRecovered(next); } }
      else { const next = await reviewDownloadReservation(token, batch.id, reason); if (current === generation.current) { clearReview(); setReview(next); } }
    } catch (err) {
      if (current !== generation.current) return;
      clearReview();
      if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "请只读核对原批次");
      // Only fetch receipts after an ambiguous mutation; never replay it.
      if (mutate) { try { const next = await findDownloadOrganization(token, { downloaderId: batch.origin.downloaderId, taskId: batch.origin.task.id }); if (current === generation.current) onRecovered(next); } catch { /* retain original error */ } }
    } finally { if (current === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  if (batch.state !== "reserving" && batch.state !== "needs_review") return null;
  return <section aria-label="补齐中断预留"><h3>补齐中断预留（不执行文件）</h3><p>仅限原任务全部仍未开始的批次。重新核对来源、配置、命名和目标；只补齐原计划的缺失任务，不重新规划范围、不恢复后台授权。任何已开始、取消或有恢复凭证的任务都不能使用此入口。</p>{error && <p role="alert">{error}</p>}
    <fieldset disabled={!enabled || busy}><label>补齐原因<input value={reason} maxLength={1000} onChange={(e) => { generation.current++; clearReview(); setReason(e.target.value); setError(""); }} /></label><button type="button" disabled={!reason.trim()} onClick={() => void operate(false)}>只读审核原预留</button></fieldset>
    {review && <div><p>批次 {review.batchId} · 当前操作者 {review.actor} · 原状态 {review.state}。保留 {review.jobs.length} 个原任务，补齐 {review.missing.length} 个缺失任务。{review.missing.length === 0 && "任务已齐全，只修复预留提交状态。"}</p>{review.plans.map((plan, i) => <div key={i}><p>原计划 {i + 1}{review.missing.includes(i) ? "（待补齐）" : "（保留原任务）"}</p><ul>{plan.items.map((item) => <li key={item.source}>{item.source} → {item.target || "不转移"} · {item.status}</li>)}</ul></div>)}
      {review.omitted.length > 0 && <><ul>{review.omitted.map((file) => <li key={file.path}>保留／筛除：{file.path}</li>)}</ul><label><input type="checkbox" disabled={!enabled || busy} checked={omitted} onChange={(e) => setOmitted(e.target.checked)} />另行确认原保留／筛除范围</label></>}
      <label><input type="checkbox" disabled={!enabled || busy} checked={oldStopped} onChange={(e) => setOldStopped(e.target.checked)} />旧 Python 和其他整理执行器已实际停止</label><label><input type="checkbox" disabled={!enabled || busy} checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />仅补齐原任务及预留回执，不移动文件、不暂停种子、不回写下载器</label><button type="button" disabled={!enabled || busy || !confirm || !oldStopped || review.omitted.length > 0 && !omitted} onClick={() => void operate(true)}>确认仅补齐预留</button>
    </div>}
  </section>;
}
