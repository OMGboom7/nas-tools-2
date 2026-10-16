import { useEffect, useRef, useState } from "react";
import { ApiError, applyPTVerification, getPTVerification, reviewPTVerification, type PTVerificationReview, type PTVerificationState } from "../api/client";

export function PTVerification({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [id, setId] = useState(""); const [hash, setHash] = useState("");
  const [note, setNote] = useState(""); const [minutes, setMinutes] = useState(15);
  const [stop, setStop] = useState(false); const [remove, setRemove] = useState(false); const [move, setMove] = useState(false);
  const [status, setStatus] = useState<PTVerificationState | null>(null); const [review, setReview] = useState<PTVerificationReview | null>(null);
  const [confirm, setConfirm] = useState(false); const [account, setAccount] = useState(false); const [automation, setAutomation] = useState(false); const [oldStopped, setOldStopped] = useState(false);
  const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  const generation = useRef(0); const inFlight = useRef(false);
  function reset() { setReview(null); setConfirm(false); setAccount(false); setAutomation(false); setOldStopped(false); }
  useEffect(() => { generation.current++; inFlight.current = false; setBusy(false); setStatus(null); reset(); setError(""); return () => { generation.current++; }; }, [token]);
  async function operate(action: "read" | "review" | "revoke-review" | "apply") {
    if (inFlight.current || action === "apply" && (!review || !confirm || review.mode === "attest" && (!account || !automation || !oldStopped))) return;
    inFlight.current = true; const current = ++generation.current; setBusy(true); setError("");
    const scope = { downloaderId: Number(id), taskId: hash.trim().toLowerCase() };
    try {
      if (action === "review" || action === "revoke-review") {
        const next = await reviewPTVerification(token, { ...scope, mode: action === "review" ? "attest" : "revoke", minutes: action === "review" ? minutes : 0, note: action === "review" ? note.trim() : "", actions: action === "review" ? { stop, remove, move } : { stop: false, remove: false, move: false } });
        if (current === generation.current) { reset(); setReview(next); }
      } else {
        if (action === "apply") await applyPTVerification(token, review!, confirm, account, automation, oldStopped);
        const next = await getPTVerification(token, scope); if (current === generation.current) { setStatus(next); reset(); }
      }
    } catch (err) {
      if (current !== generation.current) return;
      reset(); setStatus(null); if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "请核实准确任务及已知最低要求");
      if (action === "apply") { try { const next = await getPTVerification(token, scope); if (current === generation.current) setStatus(next); } catch { /* no mutation replay */ } }
    } finally { if (current === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section aria-label="PT 限时人工核实"><h2>PT 限时人工核实（管理员）</h2>
    <p>不是站点自动验证。只有您已在站点账户核实此准确任务的 HR 及全部做种义务已满足，才能登记。原约束保留；已知最低条件未达标、数据未知或任务未完整下载时仍拒绝。所选动作可能让已有自动流程立即继续，删除可能涉及文件。</p>
    <fieldset disabled={busy}><label>准确下载器 ID<input inputMode="numeric" value={id} onChange={(e) => { setId(e.target.value); setStatus(null); reset(); }} /></label><label>完整任务 hash<input maxLength={64} value={hash} onChange={(e) => { setHash(e.target.value); setStatus(null); reset(); }} /></label>
      <button type="button" onClick={() => void operate("read")}>只读刷新人工核实记录</button>
      <label>核实说明（10–512 字，不填写链接、Cookie 或口令）<textarea value={note} maxLength={512} onChange={(e) => { setNote(e.target.value); reset(); }} /></label>
      <label>有效期<select value={minutes} onChange={(e) => { setMinutes(Number(e.target.value)); reset(); }}><option value={5}>5 分钟</option><option value={15}>15 分钟</option><option value={30}>30 分钟</option></select></label>
      <label><input type="checkbox" checked={stop} onChange={(e) => { setStop(e.target.checked); reset(); }} />允许原有权限范围内停止</label>
      <label><input type="checkbox" checked={remove} onChange={(e) => { setRemove(e.target.checked); reset(); }} />允许原有权限范围内删除（可能含文件）</label>
      <label><input type="checkbox" checked={move} onChange={(e) => { setMove(e.target.checked); reset(); }} />允许已确认范围的下载任务移动整理</label>
      <button type="button" disabled={!stop && !remove && !move} onClick={() => void operate("review")}>只读审核限时声明范围</button>
      <button type="button" disabled={!status?.record || status.revoked} onClick={() => void operate("revoke-review")}>只读审核撤销当前声明</button>
    </fieldset>
    {error && <p role="alert">{error}</p>}
    {status && <p>{status.record ? `管理员核实：${status.record.actor} · ${status.revoked ? "已撤销" : `有效期截至 ${status.record.expires}（还需实时检查）`} · ${status.record.note}` : "无人工核实记录，不代表没有 PT 义务"}。本地记录状态不等于当前操作授权；请另行只读刷新做种证据。</p>}
    {review && <fieldset disabled={busy}><legend>{review.mode === "attest" ? "登记管理员核实声明" : "撤销声明"}</legend><p>{review.name} · {review.taskId} · 本次审核截至 {review.reviewExpires}。{review.warning}</p>
      {review.evidence && <p>准确任务新增时间标识 {review.evidence.addedAt} · 累计做种 {review.evidence.seconds} 秒 · 分享率 {review.evidence.ratio}。这些计数不是站点账户解除证据。</p>}
      <label><input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />确认当前审核的准确任务、原约束集合及所选范围</label>
      {review.mode === "attest" && <><label><input type="checkbox" checked={account} onChange={(e) => setAccount(e.target.checked)} />已在站点账户逐项核实 HR 及全部义务已满足，自行承担此声明的准确性</label><label><input type="checkbox" checked={automation} onChange={(e) => setAutomation(e.target.checked)} />另行授权有效期内所选动作可由已有权限和确认的自动流程立即继续，知晓删除可能涉及文件</label><label><input type="checkbox" checked={oldStopped} onChange={(e) => setOldStopped(e.target.checked)} />旧 Go / Python 执行器已实际停止，其他客户端不受此锁保护</label></>}
      <button type="button" disabled={!confirm || review.mode === "attest" && (!account || !automation || !oldStopped)} onClick={() => void operate("apply")}>确认仅{review.mode === "attest" ? "登记限时声明" : "撤销声明"}，不直接执行任务</button>
    </fieldset>}
  </section>;
}
