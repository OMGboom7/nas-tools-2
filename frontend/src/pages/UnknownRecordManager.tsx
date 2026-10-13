import { useEffect, useRef, useState } from "react";
import { ApiError, deleteUnknownRecord, getUnknownDeletions, restoreUnknownRecord, reviewUnknownDelete, type UnknownDeleteReceipt, type UnknownDeleteReview } from "../api/client";

type Props = { token: string; records: Array<{ id: number; idText?: string; path: string }>; enabled: boolean; onChanged: () => Promise<void>; onSessionExpired: () => void };
const exactID = (record: Props["records"][number]) => record.idText || (Number.isSafeInteger(record.id) ? String(record.id) : "");

export function UnknownRecordManager({ token, records, enabled, onChanged, onSessionExpired }: Props) {
  const [review, setReview] = useState<UnknownDeleteReview | null>(null);
  const [receipts, setReceipts] = useState<UnknownDeleteReceipt[]>([]);
  const [confirmed, setConfirmed] = useState(false);
  const [restoreID, setRestoreID] = useState("");
  const [restoreConfirmed, setRestoreConfirmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  useEffect(() => { generation.current++; setReview(null); setReceipts([]); setConfirmed(false); setRestoreID(""); setRestoreConfirmed(false); setBusy(false); setError(""); return () => { generation.current++; }; }, [token]);
  const restoreReview = receipts.find((receipt) => receipt.id === restoreID);
  async function operate(action: "review" | "delete" | "list" | "restore", id = "") {
    if (!enabled || busy || action === "delete" && (!confirmed || !review?.eligible) || action === "restore" && (!restoreConfirmed || !restoreReview || !!restoreReview.restore)) return;
    const version = ++generation.current;
    setBusy(true); setError("");
    try {
      if (action === "review") { const next = await reviewUnknownDelete(token, id); if (version === generation.current) { setReview(next); setConfirmed(false); } }
      else if (action === "list") { const saved = await getUnknownDeletions(token); if (version === generation.current) setReceipts(saved); }
      else {
        if (action === "delete") await deleteUnknownRecord(token, review!.record.id, review!.fingerprint, true);
        else await restoreUnknownRecord(token, restoreID, true);
        if (version !== generation.current) return;
        setReview(null); setConfirmed(false); setRestoreID(""); setRestoreConfirmed(false);
        const saved = await getUnknownDeletions(token);
        if (version !== generation.current) return;
        setReceipts(saved); await onChanged();
      }
    } catch (err) {
      if (version !== generation.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "记录操作失败");
      setConfirmed(false); setRestoreConfirmed(false);
      // A lost response may follow a committed transaction. Read receipts only;
      // never repeat deletion, restore, transfer or reconciliation automatically.
      if (action === "delete" || action === "restore") {
        try { const saved = await getUnknownDeletions(token); if (version === generation.current) setReceipts(saved); } catch { /* keep the original error */ }
      }
    } finally { if (version === generation.current) setBusy(false); }
  }
  return <section aria-label="未识别记录管理">
    <h2>仅删除／恢复未识别记录</h2>
    <p>只处理数据库记录，绝不删除、恢复或重跑媒体文件，不清理历史或释放任务目标。待处理关联不能删除。删除保留原行及审核回执，可单独恢复；恢复遇到相同 ID 或待处理关联时拒绝，不覆盖当前数据。</p>
    {error && <p role="alert">{error}</p>}
    {records.map((record) => <p key={exactID(record) || record.path}>{record.path} <button type="button" disabled={!enabled || busy || !exactID(record)} onClick={() => void operate("review", exactID(record))}>审核 #{exactID(record) || "无法精确表示 ID"} 的记录删除</button></p>)}
    {review && <div><p>原行 #{review.record.id}：{review.record.path ?? "NULL"} → {review.record.dest ?? "NULL"} · {review.record.mode ?? "NULL"} · 状态 {review.record.state ?? "NULL"}</p>
      {review.pendingJobId && <p>仍关联任务 {review.pendingJobId}，不能删除或强制释放。</p>}
      <label><input type="checkbox" checked={confirmed} disabled={!enabled || busy || !review.eligible} onChange={(event) => setConfirmed(event.target.checked)} />我已审核此快照，同意仅删除这条数据库记录，并保留原行回执；不删除文件，也不标记整理成功。</label>
      <button type="button" disabled={!enabled || busy || !confirmed || !review.eligible} onClick={() => void operate("delete")}>确认仅删除记录</button>
    </div>}
    <button type="button" disabled={!enabled || busy} onClick={() => void operate("list")}>读取最近 30 条删除／恢复回执</button>
    {receipts.map((receipt) => <p key={receipt.id}>#{receipt.review.record.id} · {receipt.review.record.path ?? "NULL"} · 删除回执 {receipt.actor} · {receipt.created}{receipt.restore ? <> · 曾恢复：{receipt.restore.actor} · {receipt.restore.created}（回执不证明当前行仍存在）</> : <button type="button" disabled={!enabled || busy} onClick={() => { setRestoreID(receipt.id); setRestoreConfirmed(false); }}>审核恢复原行</button>}</p>)}
    {restoreReview && <div><p>将恢复原行 #{restoreReview.review.record.id}：{restoreReview.review.record.path ?? "NULL"} → {restoreReview.review.record.dest ?? "NULL"} · {restoreReview.review.record.mode ?? "NULL"} · 状态 {restoreReview.review.record.state ?? "NULL"}</p><label><input type="checkbox" checked={restoreConfirmed} disabled={!enabled || busy || !!restoreReview.restore} onChange={(event) => setRestoreConfirmed(event.target.checked)} />我同意仅恢复上述原 ID、字段、NULL 和状态；不恢复文件或启动整理。</label><button type="button" disabled={!enabled || busy || !restoreConfirmed || !!restoreReview.restore} onClick={() => void operate("restore")}>确认仅恢复原记录</button></div>}
  </section>;
}
