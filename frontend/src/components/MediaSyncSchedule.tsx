import { useEffect, useRef, useState } from "react";
import { ApiError, getMediaSchedule, getMediaSyncLibraries, reviewMediaSchedule, activateMediaSchedule, stopMediaSchedule, type MediaScheduleStatus, type MediaScheduleReview, type MediaSyncCatalog } from "../api/client";

export function MediaSyncSchedule({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [status, setStatus] = useState<MediaScheduleStatus | null>(null);
  const [catalog, setCatalog] = useState<MediaSyncCatalog | null>(null);
  const [selected, setSelected] = useState<string[]>([]);
  const [hours, setHours] = useState("24");
  const [review, setReview] = useState<MediaScheduleReview | null>(null);
  const [confirm, setConfirm] = useState(false);
  const [legacyStopped, setLegacyStopped] = useState(false);
  const [confirmStop, setConfirmStop] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0); const inFlight = useRef(false);
  function resetReview() { setReview(null); setConfirm(false); setLegacyStopped(false); }
  useEffect(() => {
    generation.current++; inFlight.current = false; setStatus(null); setCatalog(null); setSelected([]); setHours("24"); resetReview(); setConfirmStop(false); setBusy(false); setError("");
    return () => { generation.current++; };
  }, [token]);
  async function operate(action: "status" | "catalog" | "review" | "activate" | "stop") {
    if (inFlight.current || action === "activate" && (!review || !confirm || !legacyStopped) || action === "stop" && (!status?.schedule.revision || !confirmStop)) return;
    inFlight.current = true; const current = ++generation.current; setBusy(true); setError("");
    try {
      if (action === "catalog") {
        const next = await getMediaSyncLibraries(token); if (current === generation.current) { setCatalog(next); setSelected([]); resetReview(); }
      } else if (action === "review") {
        if (!catalog) throw new Error("请先读取并选择媒体库");
        const next = await reviewMediaSchedule(token, catalog, selected, Number(hours)); if (current === generation.current) { resetReview(); setReview(next); }
      } else {
        if (action === "activate") await activateMediaSchedule(token, review!, confirm, legacyStopped);
        if (action === "stop") await stopMediaSchedule(token, status!.schedule.revision, confirmStop);
        const next = await getMediaSchedule(token); if (current === generation.current) { setStatus(next); resetReview(); setConfirmStop(false); }
      }
    } catch (err) {
      if (current !== generation.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "请只读刷新定时同步状态");
      resetReview(); setConfirmStop(false);
      if (action === "catalog") setCatalog(null);
      if (action === "activate" || action === "stop") {
        // Unknown responses grant no authority to repeat a state change.
        try { const next = await getMediaSchedule(token); if (current === generation.current) setStatus(next); } catch { /* keep previous receipts visible */ }
      }
    } finally { if (current === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section className="content-section" aria-label="定时媒体库同步">
    <h2>定时媒体库同步（管理员）</h2>
    <p>默认关闭。明确授权后按周期完整替换所选库的展示快照，不删除媒体文件、不影响实时查重。重启保留计划，错过多次只合并一次完整读取；中断不认定成功。配置、旧周期设置或管理员授权变化会暂停，恢复原配置也不会自动恢复授权。</p>
    <button type="button" className="secondary-button" disabled={busy} onClick={() => void operate("status")}>只读刷新计划与回执</button>{" "}
    <button type="button" className="secondary-button" disabled={busy} onClick={() => void operate("catalog")}>选择定时同步媒体库</button>
    {error && <p role="alert">{error}</p>}
    {busy && <p role="status">正在核对，请勿重复提交。</p>}
    {status && <div>
      <p>计划：{status.schedule.enabled ? "已授权" : "未启用 / 已停止"} · {status.schedule.lastState} · {status.schedule.reason}</p>
      <p>{status.workerRunning ? "当前服务包含定时执行器" : "当前服务未运行定时执行器，保存计划不代表已执行"}。旧配置周期建议：{status.suggestedHours || "未设置"} 小时；不会据此自动启用。</p>
      {status.warning && <p role="alert">{status.warning}</p>}
      {status.schedule.revision && <>
        <p>范围：{status.schedule.definition.server} / {status.schedule.definition.libraries.join("、")} · 周期 {status.schedule.definition.intervalSeconds / 3600} 小时 · 授权人 {status.schedule.definition.actor}</p>
        {status.schedule.enabled && <p>下一次：{status.schedule.nextRun} · {status.current ? "配置匹配" : "配置或权限已变化，执行器将暂停此授权"}</p>}
        <label><input type="checkbox" disabled={busy} checked={confirmStop} onChange={(e) => setConfirmStop(e.target.checked)} />确认停止此代次，不清除已有快照</label>{" "}
        <button type="button" disabled={busy || !confirmStop || !status.schedule.enabled} onClick={() => void operate("stop")}>停止定时同步</button>
      </>}
      <h3>最近 50 次定时运行</h3><ul>{status.runs.map((run) => <li key={run.id}>{run.started} · {run.state} · {run.reason} {run.snapshotGeneration && `· 快照 ${run.snapshotGeneration}`} · {run.definition.libraries.join("、")}</li>)}</ul>
    </div>}
    {catalog && <fieldset disabled={busy}><legend>明确选择 {catalog.server} 的周期范围</legend>
      {catalog.libraries.map((library) => <label key={library.id}><input type="checkbox" checked={selected.includes(library.id)} onChange={(e) => { setSelected(e.target.checked ? [...selected, library.id] : selected.filter((id) => id !== library.id)); resetReview(); }} />{library.name}</label>)}
      <p><label>周期（小时，最短 1 分钟，最长 1 年）<input type="number" min={1 / 60} max="8760" step="any" value={hours} onChange={(e) => { setHours(e.target.value); resetReview(); }} /></label></p>
      <button type="button" disabled={!selected.length || !hours} onClick={() => void operate("review")}>只读审核周期授权</button>
    </fieldset>}
    {review && <fieldset disabled={busy}><legend>审核后的授权范围</legend>
      <p>{review.definition.server} · {review.definition.libraries.join("、")} · 每 {review.definition.intervalSeconds} 秒；首次执行在启用一个周期后。{review.replacesEnabled && "将替换现有计划，旧采集结果不能再提交。"}</p>
      <p><label><input type="checkbox" checked={legacyStopped} onChange={(e) => setLegacyStopped(e.target.checked)} />已停止旧 Python 媒体库同步任务</label></p>
      <p><label><input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />授权持续更新所选范围，并在服务重启后保留此计划</label></p>
      <button type="button" disabled={!confirm || !legacyStopped} onClick={() => void operate("activate")}>启用此定时计划</button>
    </fieldset>}
  </section>;
}
