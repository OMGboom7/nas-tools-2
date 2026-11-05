import { useEffect, useRef, useState } from "react";
import { RemoteTransferConsent } from "../components/RemoteTransferConsent";
import { ApiError, activateSyncMonitor, dismissSyncMonitorEvent, getDirectSyncRecords, getSyncMonitor, reviewSyncMonitor, stopSyncMonitor, type OrganizationSyncRecord, type SyncMonitorReview, type SyncMonitorStatus } from "../api/client";

type Props = { token: string; enabled: boolean; onSessionExpired: () => void; onChanged: (jobID: string) => Promise<void> };
export function SyncMonitorManager({ token, enabled, onSessionExpired, onChanged }: Props) {
  const [records, setRecords] = useState<OrganizationSyncRecord[]>([]);
  const [status, setStatus] = useState<SyncMonitorStatus | null>(null);
  const [ids, setIDs] = useState<string[]>([]);
  const [includeExisting, setIncludeExisting] = useState(false);
  const [timer, setTimer] = useState(false);
  const [review, setReview] = useState<SyncMonitorReview | null>(null);
  const [confirmed, setConfirmed] = useState(false);
  const [oldStopped, setOldStopped] = useState(false);
  const [sourceRemoval, setSourceRemoval] = useState(false);
  const [remoteExclusive,setRemoteExclusive]=useState(false),[remoteRecovery,setRemoteRecovery]=useState(false);
  const remoteRequired=!!review?.remoteTargets?.length;
  const [stopConfirmed, setStopConfirmed] = useState(false);
  const [eventKey, setEventKey] = useState("");
  const [reason, setReason] = useState("");
  const [dismissConfirmed, setDismissConfirmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const inFlight = useRef(false);
  useEffect(() => { generation.current++; inFlight.current = false; setRecords([]); setStatus(null); setIDs([]); setIncludeExisting(false); setTimer(false); setReview(null); setConfirmed(false); setOldStopped(false); setSourceRemoval(false);setRemoteExclusive(false);setRemoteRecovery(false); setStopConfirmed(false); setEventKey(""); setReason(""); setDismissConfirmed(false); setBusy(false); setError(""); return () => { generation.current++; }; }, [token]);
  function invalidate() { generation.current++; setReview(null); setConfirmed(false); setOldStopped(false); setSourceRemoval(false);setRemoteExclusive(false);setRemoteRecovery(false); }
  async function viewJob(id: string) {
    if (!enabled || inFlight.current) return;
    inFlight.current = true; const version = ++generation.current; setBusy(true); setError("");
    try { await onChanged(id); } catch (err) { if (version === generation.current) { if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "任务读取失败"); } }
    finally { if (version === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  async function operate(action: "load" | "review" | "activate" | "stop" | "dismiss") {
    if (!enabled || inFlight.current || action === "activate" && (!review || !confirmed || !oldStopped || remoteRequired && (!remoteExclusive || !remoteRecovery) || review.needsSourceRemoval && !sourceRemoval) || action === "stop" && !stopConfirmed || action === "dismiss" && (!eventKey || !reason.trim() || !dismissConfirmed)) return;
    inFlight.current = true; const version = ++generation.current; setBusy(true); setError("");
    try {
      if (action === "review") {
        setReview(null); setConfirmed(false); setOldStopped(false); setSourceRemoval(false);setRemoteExclusive(false);setRemoteRecovery(false);
        const next = await reviewSyncMonitor(token, { ids, includeExisting: timer || includeExisting, ...(timer ? { timer: true } : {}) });
        if (version === generation.current) setReview(next);
      } else {
        if (action === "activate") await activateSyncMonitor(token, { ids, includeExisting: timer || includeExisting, ...(timer ? { timer: true } : {}) }, review!.fingerprint, confirmed, oldStopped, sourceRemoval, remoteRequired?{confirmRemoteExclusive:remoteExclusive,confirmRemoteRecoveryCopy:remoteRecovery,confirmOldExecutorsStopped:oldStopped}:undefined);
        if (action === "stop") await stopSyncMonitor(token, stopConfirmed);
        if (action === "dismiss") await dismissSyncMonitorEvent(token, eventKey, reason, dismissConfirmed);
        if (version !== generation.current) return;
        setReview(null); setConfirmed(false); setOldStopped(false); setSourceRemoval(false);setRemoteExclusive(false);setRemoteRecovery(false); setStopConfirmed(false); setDismissConfirmed(false);
        const [rows, next] = await Promise.all([getDirectSyncRecords(token), getSyncMonitor(token)]);
        if (version === generation.current) {
          setRecords(rows);
          setStatus(next);
          if (!next.timer?.installed || !next.timer.cron) setTimer(false);
        }
      }
    } catch (err) {
      if (version !== generation.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "监控操作失败，请核对状态和任务账本");
      setReview(null); setConfirmed(false); setOldStopped(false); setSourceRemoval(false);setRemoteExclusive(false);setRemoteRecovery(false); setStopConfirmed(false); setDismissConfirmed(false);
      // A lost response is not evidence that a mutation failed. Never replay it.
      try {
        const next = await getSyncMonitor(token);
        if (version === generation.current) {
          setStatus(next);
          if (!next.timer?.installed || !next.timer.cron) setTimer(false);
        }
      } catch { /* retain original error */ }
    } finally { if (version === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section aria-label="自动同步监控">
    <h2>自动同步监控（明确授权后运行）</h2>
    <p>默认关闭。先实际停止旧同步执行器，再审核来源并授权。可选择十秒轮询或使用插件配置的定时周期。两种方式共用同一份授权和任务账本，文件至少稳定 30 秒后才可执行。</p>
    {error && <p role="alert">{error}</p>}
    <button type="button" disabled={!enabled || busy} onClick={() => void operate("load")}>只读刷新配置、授权状态和事件</button>
    {status && <p>授权状态：{status.control.state} · 批次 {status.control.lease.generation || "无"} · 操作者 {status.control.lease.actor || "无"}。{status.control.reason} 配置修改或根身份变化会暂停，撤销配置不恢复授权。</p>}
    {status?.timer?.installed && <div>
      <p>定时目录同步 {status.timer.cron || "未设置周期"} · {status.timer.active ? "已启用" : "未授权"}
        {status.timer.active && status.timer.next && (" · 下次扫描 " + new Date(status.timer.next).toLocaleString())}</p>
      {!!status.timer.runs.length && <details><summary>最近定时扫描记录</summary><ul>{status.timer.runs.map(run =>
        <li key={run.id}>{new Date(run.slot).toLocaleString()} · {({ observing: "等待稳定复核", scanned: "已扫描", needs_review: "需核对" } as Record<string, string>)[run.state] || run.state} · 候选 {run.candidates} · {run.id}</li>
      )}</ul></details>}
    </div>}
    <fieldset disabled={!enabled || busy}>
      <legend>选择本轮授权范围（最多 16 项）</legend>
      {status?.timer?.installed && <label><input type="checkbox" checked={timer} disabled={!status.timer.cron} onChange={event => {
        invalidate();
        const useTimer = event.target.checked;
        setTimer(useTimer);
        const activeIDs = records.filter(row => row.enabled === 1).map(row => row.id);
        if (useTimer && activeIDs.length <= 16) setIDs(activeIDs);
      }} />按已保存的定时目录同步周期执行（包含当前已有文件）</label>}
      {records.map((row) => <label key={row.id}><input type="checkbox" disabled={row.enabled !== 1 || !ids.includes(row.id) && ids.length >= 16} checked={ids.includes(row.id)} onChange={(e) => { invalidate(); setIDs(e.target.checked ? [...ids, row.id] : ids.filter((id) => id !== row.id)); }} />#{row.id} · {row.source} → {row.dest || "按媒体类型选库"} · {row.rename === 1 ? "识别整理" : "原结构同步"} · {row.mode}</label>)}
      <label><input type="checkbox" checked={timer || includeExisting} disabled={timer} onChange={(e) => { invalidate(); setIncludeExisting(e.target.checked); }} />将当前已有文件也纳入自动处理（不选则只建立基线）</label>
      <button type="button" disabled={!ids.length} onClick={() => void operate("review")}>只读审核来源快照</button>
    </fieldset>
    {review && <div><p>本次触发 {review.lease.timer ? "定时 " + review.lease.timer.cron + "（" + review.lease.timer.timezone + "）" : "十秒轮询"} · 范围 {review.lease.ids.join("、")} · 快照 {review.snapshots.length} 组、{review.snapshots.reduce((n, item) => n + item.files.length, 0)} 个文件；{review.lease.includeExisting ? "已有文件会进入待稳定队列" : "已有文件仅作为基线"}。未识别、冲突或不确定状态留待审核；蓝光按整盘处理，远程须独立授权，迟到附件不自动并入已完成任务。</p><ul>{review.snapshots.map((item) => <li key={`${item.configId}:${item.scope}`}>#{item.configId} · {item.scope} · {item.files.length} 个文件</li>)}</ul>
      <label><input type="checkbox" disabled={!enabled || busy} checked={oldStopped} onChange={(e) => setOldStopped(e.target.checked)} />我已实际停止旧同步执行器及会写本轮来源／远程范围的所有外部程序（不是仅切换开关）</label>
      {remoteRequired && <RemoteTransferConsent automatic targets={review.remoteTargets} busy={!enabled || busy} exclusive={remoteExclusive} recovery={remoteRecovery} onExclusive={setRemoteExclusive} onRecovery={setRemoteRecovery}/>}
      <label><input type="checkbox" disabled={!enabled || busy} checked={confirmed} onChange={(e) => setConfirmed(e.target.checked)} />同意后台按上述范围自动创建并执行任务</label>
      {review.needsSourceRemoval && <label><input type="checkbox" disabled={!enabled || busy} checked={sourceRemoval} onChange={(e) => setSourceRemoval(e.target.checked)} />另行授权移动模式核验目标后移除源名和已核验备份</label>}
      <button type="button" disabled={!enabled || busy || !confirmed || !oldStopped || remoteRequired && (!remoteExclusive || !remoteRecovery) || review.needsSourceRemoval && !sourceRemoval} onClick={() => void operate("activate")}>{review.lease.timer ? "确认启用定时目录同步" : "确认启用本轮自动同步"}</button>
    </div>}
    <label><input type="checkbox" disabled={!enabled || busy} checked={stopConfirmed} onChange={(e) => setStopConfirmed(e.target.checked)} />请求暂停后续自动写入；正在进行的步骤仍须核对账本</label>
    <button type="button" disabled={!enabled || busy || !stopConfirmed} onClick={() => void operate("stop")}>确认请求暂停</button>
    <p>最近最多 100 个事件（优先待审核，含旧批次）。刷新只读，不触发执行。忽略事件不取消任务、不清文件、不记成功。</p>
    <ul>{status?.events.map((event) => <li key={event.key}>#{event.snapshot.configId} · {event.snapshot.scope} · {event.state} · 批次 {event.generation} · {event.reason}{event.jobs.map((id) => <button type="button" key={id} disabled={!enabled || busy} onClick={() => void viewJob(id)}>查看任务 {id}</button>)}</li>)}</ul>
    <fieldset disabled={!enabled || busy}><legend>人工审核后忽略事件</legend>
      <select value={eventKey} onChange={(e) => { setEventKey(e.target.value); setDismissConfirmed(false); }}><option value="">选择事件</option>{status?.events.map((event) => <option key={event.key} value={event.key}>{event.snapshot.scope} · {event.state} · {event.key}</option>)}</select>
      <label>原因<input value={reason} maxLength={512} onChange={(e) => { setReason(e.target.value); setDismissConfirmed(false); }} /></label>
      <label><input type="checkbox" checked={dismissConfirmed} onChange={(e) => setDismissConfirmed(e.target.checked)} />已核对关联任务均已完成／取消／放弃，仅忽略此事件</label>
      <button type="button" disabled={!eventKey || !reason.trim() || !dismissConfirmed} onClick={() => void operate("dismiss")}>确认仅忽略事件</button>
    </fieldset>
  </section>;
}
