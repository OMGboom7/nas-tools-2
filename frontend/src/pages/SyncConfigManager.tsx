import { useEffect, useRef, useState } from "react";
import { ApiError, getDirectSyncRecords, getSyncConfigReceipts, reviewSyncConfig, applySyncConfig, type OrganizationSyncRecord, type SyncConfigInput, type SyncConfigReview, type SyncConfigReceipt } from "../api/client";
import { buildSyncConfigPatch, type SyncConfigDraft } from "./syncConfigState";

type Props = { token: string; enabled: boolean; onChanged: () => Promise<void>; onSessionExpired: () => void };
const emptyDraft: SyncConfigDraft = { source: "", dest: "", unknown: "", mode: "copy", note: "", enabled: false, rename: false, compatibility: false, locating: false };
export function SyncConfigManager({ token, enabled, onChanged, onSessionExpired }: Props) {
  const [records, setRecords] = useState<OrganizationSyncRecord[]>([]);
  const [receipts, setReceipts] = useState<SyncConfigReceipt[]>([]);
  const [id, setID] = useState("");
  const [restoreID, setRestoreID] = useState("");
  const [draft, setDraft] = useState<SyncConfigDraft>({ ...emptyDraft });
  const [review, setReview] = useState<SyncConfigReview | null>(null);
  const [request, setRequest] = useState<SyncConfigInput | null>(null);
  const [confirmed, setConfirmed] = useState(false);
  const [invalidateJobs, setInvalidateJobs] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const inFlight = useRef(false);
  useEffect(() => { generation.current++; inFlight.current = false; setRecords([]); setReceipts([]); setID(""); setRestoreID(""); setDraft({ ...emptyDraft }); setReview(null); setRequest(null); setConfirmed(false); setInvalidateJobs(false); setBusy(false); setError(""); return () => { generation.current++; }; }, [token]);
  const original = records.find((record) => record.id === id);
  function invalidate() { generation.current++; setReview(null); setRequest(null); setConfirmed(false); setInvalidateJobs(false); setError(""); }
  function select(value: string) {
    invalidate(); setID(value);
    const row = records.find((record) => record.id === value);
    setDraft(row ? { source: row.source, dest: row.dest, unknown: row.unknown, mode: row.mode, note: row.note, enabled: row.enabled !== 0, rename: row.rename !== 0, compatibility: row.compatibility !== 0, locating: row.locating !== 0 } : { ...emptyDraft });
  }
  function edit<K extends keyof SyncConfigDraft>(key: K, value: SyncConfigDraft[K]) { invalidate(); setDraft((current) => ({ ...current, [key]: value })); }
  async function operate(action: "load" | "review" | "delete" | "restore" | "apply") {
    if (!enabled || inFlight.current || action === "apply" && (!review || !request || !confirmed || review.jobs.length > 0 && !invalidateJobs) || action === "delete" && !original || action === "restore" && !restoreID) return;
    inFlight.current = true;
    const version = ++generation.current;
    setBusy(true); setError("");
    try {
      if (action === "load") {
        setReview(null); setConfirmed(false); setInvalidateJobs(false);
        const [rows, saved] = await Promise.all([getDirectSyncRecords(token), getSyncConfigReceipts(token)]);
        if (version === generation.current) { setRecords(rows); setReceipts(saved); setID(""); setDraft({ ...emptyDraft }); }
      } else if (action === "apply") {
        await applySyncConfig(token, request!, review!.fingerprint, true, invalidateJobs);
        if (version !== generation.current) return;
        setReview(null); setRequest(null); setConfirmed(false); setInvalidateJobs(false);
        const [rows, saved] = await Promise.all([getDirectSyncRecords(token), getSyncConfigReceipts(token)]);
        if (version !== generation.current) return;
        setRecords(rows); setReceipts(saved); setID(""); setDraft({ ...emptyDraft }); setRestoreID("");
        await onChanged();
      } else {
        const input: SyncConfigInput = action === "restore" ? { action: "restore", restoreId: restoreID } : action === "delete" ? { action: "delete", id } : { action: original ? "update" : "create", ...(original ? { id } : {}), patch: buildSyncConfigPatch(original, draft) };
        setReview(null); setConfirmed(false); setInvalidateJobs(false);
        const next = await reviewSyncConfig(token, input);
        if (version === generation.current) { setRequest(input); setReview(next); }
      }
    } catch (err) {
      if (version !== generation.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "配置操作失败");
      setConfirmed(false); setInvalidateJobs(false);
      if (action === "apply") {
        setReview(null); setRequest(null);
        // A committed change can outlive a lost response. Read state only.
        try { const [rows, saved] = await Promise.all([getDirectSyncRecords(token), getSyncConfigReceipts(token)]); if (version === generation.current) { setRecords(rows); setReceipts(saved); await onChanged(); } } catch { /* preserve original error */ }
      }
    } finally { if (version === generation.current) { setBusy(false); inFlight.current = false; } }
  }
  const changed = review ? [...review.before.filter((row) => !review.after.some((next) => next.id === row.id && JSON.stringify(next) === JSON.stringify(row))).map((row) => ({ before: row, after: review.after.find((next) => next.id === row.id) })), ...review.after.filter((row) => !review.before.some((old) => old.id === row.id)).map((row) => ({ before: undefined, after: row }))] : [];
  return <section aria-label="同步配置维护">
    <h2>同步配置维护</h2>
    <p>只维护数据库配置，不创建目录、搬文件或启动后台监控。编辑保留原 ID；启用会关闭来源重叠的其他配置，先审核全部变更。Rclone／MinIO 执行须先显式配置各自账户，再到独立入口审核授权；保存不启动。部署前须实际停止旧 Python 同步服务。</p>
    {error && <p role="alert">{error}</p>}
    <button type="button" disabled={!enabled || busy} onClick={() => void operate("load")}>读取／刷新配置和最近回执</button>
    <fieldset disabled={!enabled || busy}>
      <label>配置<select value={id} onChange={(e) => select(e.target.value)}><option value="">新建（默认停用）</option>{records.map((row) => <option key={row.id} value={row.id}>#{row.id} · {row.note || row.source}</option>)}</select></label>
      <label>来源绝对路径<input value={draft.source} maxLength={4096} onChange={(e) => edit("source", e.target.value)} /></label>
      <label>目标路径（识别整理可留空）<input value={draft.dest} maxLength={4096} onChange={(e) => edit("dest", e.target.value)} /></label>
      <label>未识别目录<input value={draft.unknown} maxLength={4096} onChange={(e) => edit("unknown", e.target.value)} /></label>
      <label>转移方式<select value={draft.mode} onChange={(e) => edit("mode", e.target.value)}>{!["copy", "link", "softlink", "move", "rclonecopy", "rclone", "miniocopy", "minio"].includes(draft.mode) && <option value={draft.mode}>原方式 {draft.mode || "未设置"}</option>}<option value="copy">复制</option><option value="link">硬链接</option><option value="softlink">软链接</option><option value="move">移动</option><option value="rclonecopy">Rclone 复制（须独立审核）</option><option value="rclone">Rclone 移动（须独立审核）</option><option value="miniocopy">MinIO 复制（须独立审核）</option><option value="minio">MinIO 移动（须独立审核）</option></select></label>
      <label>备注<input value={draft.note} maxLength={4096} onChange={(e) => edit("note", e.target.value)} /></label>
      {(["enabled", "rename", "compatibility", "locating"] as const).map((key) => <label key={key}><input type="checkbox" checked={draft[key]} onChange={(e) => edit(key, e.target.checked)} />{{ enabled: "启用配置（不代表后台监控已启动）", rename: "识别并重命名", compatibility: "兼容轮询标志", locating: "自动定位标志" }[key]}</label>)}
      <button type="button" onClick={() => void operate("review")}>审核新增／编辑</button>
      <button type="button" disabled={!original} onClick={() => void operate("delete")}>审核仅删除配置</button>
      <label>撤销回执<select value={restoreID} onChange={(e) => { invalidate(); setRestoreID(e.target.value); }}><option value="">请选择</option>{receipts.map((receipt) => <option key={receipt.id} value={receipt.id} disabled={!!receipt.restoreId}>{receipt.created} · {receipt.review.action} · {receipt.review.actor}{receipt.restoreId ? " · 已撤销" : ""}</option>)}</select></label>
      <button type="button" disabled={!restoreID} onClick={() => void operate("restore")}>审核撤销此回执的配置变化</button>
    </fieldset>
    {review && <div>
      <p>审核操作：{review.action}。只更改下面列出的配置；删除不删文件或同步历史。撤销要求当前配置仍与回执后的完整快照一致，不覆盖后来的编辑。</p>
      <ul>{changed.map((item, index) => <li key={index}>#{item.before?.id || item.after?.id || "新建"}：{item.before ? `${item.before.source} → ${item.before.dest || "自动库"} · ${item.before.mode} · 启用 ${item.before.enabled}` : "不存在"} → {item.after ? `${item.after.source} → ${item.after.dest || "自动库"} · ${item.after.mode} · 启用 ${item.after.enabled} · 备注 ${item.after.note || "无"}` : "仅移除配置行，保留回执"}</li>)}</ul>
      {review.jobs.length > 0 && <><p>以下未完成任务会因配置或目录图变化停止后续写入；原凭证、预留和恢复对象保留，不会自动取消、重试或清理。</p><ul>{review.jobs.map((job) => <li key={job.id}>{job.id} · 配置 #{job.configId} · {job.mode}</li>)}</ul><label><input type="checkbox" checked={invalidateJobs} disabled={!enabled || busy} onChange={(e) => setInvalidateJobs(e.target.checked)} />我另外确认使上述任务的旧配置审核失效；这不代替停止进程或恢复文件。</label></>}
      <label><input type="checkbox" checked={confirmed} disabled={!enabled || busy} onChange={(e) => setConfirmed(e.target.checked)} />我已审核全部配置变化及回执，同意仅修改配置，不搬文件、不启动监控。</label>
      <button type="button" disabled={!enabled || busy || !confirmed || review.jobs.length > 0 && !invalidateJobs} onClick={() => void operate("apply")}>确认仅保存配置变化</button>
    </div>}
  </section>;
}
