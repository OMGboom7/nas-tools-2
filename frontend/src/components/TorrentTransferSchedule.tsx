import { useEffect, useRef, useState } from "react";
import { ApiError } from "../api/client";

type Schedule = {
  fingerprint: string; ready: boolean; source: string; target: string;
  cron: string; onlyOnce: boolean; autoStart: boolean; deleteSource: boolean;
  control: { epoch: string; actor: string; next?: string; once?: string; pending?: {id: string; kind: string; due: string}; latest?: {id: string; submitted: number; existing: number; unknown: number; failed: number; completed?: number; review?: number} };
};
const base = "/api/v1/torrent-transfer/schedule";

export function TorrentTransferSchedule({token, expired, select}: {token: string; expired: () => void; select: (receipt: string) => void}) {
  const [status, setStatus] = useState<Schedule>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [exclusive, setExclusive] = useState(false);
  const [batchId, setBatchId] = useState("");
  const [receipts, setReceipts] = useState<string[]>();
  const request = useRef<AbortController | null>(null);
  useEffect(() => () => request.current?.abort(), []);
  async function lookupBatch(id: string) {
    if (request.current || !/^[a-f0-9]{64}$/.test(id)) return;
    const c = new AbortController();
    request.current = c;
    setBatchId(id);
    setBusy(true);
    setError("");
    setReceipts(undefined);
    try {
      const response = await fetch("/api/v1/torrent-transfer/batches/" + id, {headers:{Authorization:token}, cache:"no-store", signal:c.signal});
      const body = await response.json();
      if (!response.ok) throw new ApiError(body?.message || "读取批次失败", response.status);
      const batch = body?.result;
      if (body?.code !== 0 || batch?.id !== id || !Array.isArray(batch.receipts) ||
          !batch.receipts.every((entry: unknown) => typeof entry === "string" && /^[a-f0-9]{64}$/.test(entry))) throw new Error();
      if (!c.signal.aborted) setReceipts(batch.receipts);
    } catch (e) {
      if (c.signal.aborted) return;
      if (e instanceof ApiError && e.code === 401) expired();
      else setError(e instanceof ApiError ? e.message : "无法读取批次回执");
    } finally { request.current = null; if (!c.signal.aborted) setBusy(false); }
  }
  async function operate(action?: "activate" | "stop") {
    if (request.current || (action && !status)) return;
    if (action && !window.confirm(action === "stop" ? "停止自动转种调度？" :
      `启用从 ${status!.source} 到 ${status!.target} 的自动转种？\n自动启动：${status!.autoStart ? "开启" : "关闭"}\n移除源任务并保留文件：${status!.deleteSource ? "开启" : "关闭"}`)) return;
    const c = new AbortController();
    request.current = c;
    setBusy(true);
    setError("");
    try {
      const options = {headers: {Authorization: token, "Content-Type": "application/json"}, signal: c.signal};
      if (action) {
        const response = await fetch(base + "/" + action, {...options, method: "POST", body: JSON.stringify({epoch: status!.control.epoch, fingerprint: status!.fingerprint, confirm: true, exclusive, allowStart: exclusive && status!.autoStart, allowRemoval: exclusive && status!.deleteSource})});
        const body = await response.json();
        if (!response.ok || body?.code !== 0) throw new ApiError(body?.message || "调度操作失败", response.status);
      }
      const response = await fetch(base, {...options, cache: "no-store"});
      const body = await response.json();
      if (!response.ok || body?.code !== 0) throw new ApiError(body?.message || "读取调度失败", response.status);
      const s = body.result;
      if (!s?.control || typeof s.control.epoch !== "string" || typeof s.control.actor !== "string" ||
          ![s.source,s.target,s.cron,s.fingerprint].every(v => typeof v === "string") ||
          ![s.ready,s.onlyOnce,s.autoStart,s.deleteSource].every(v => typeof v === "boolean") ||
          (s.control.pending && typeof s.control.pending.id !== "string") ||
          (s.control.latest && (typeof s.control.latest.id !== "string" || ![s.control.latest.submitted, s.control.latest.existing, s.control.latest.unknown, s.control.latest.failed].every(n => Number.isSafeInteger(n) && n >= 0)))) throw new Error();
      if (!c.signal.aborted) { setStatus(s); setExclusive(false); }
    } catch (e) {
      if (c.signal.aborted) return;
      setStatus(undefined);
      if (e instanceof ApiError && e.code === 401) expired();
      else setError(e instanceof ApiError ? e.message : "请刷新并核对调度状态");
    } finally { request.current = null; if (!c.signal.aborted) setBusy(false); }
  }
  return <section aria-label="自动转种调度">
    <button type="button" disabled={busy} onClick={() => void operate()}>查看转种调度</button>
    {status && <div>
      <p>{status.source} → {status.target} · {status.control.actor ? "已启用调度" : "调度未启用"}</p>
      {status.cron && <p>执行周期 <code>{status.cron}</code></p>}
      {status.control.next && <p>下次周期触发 {new Date(status.control.next).toLocaleString()}</p>}
      {(status.onlyOnce || status.control.once) && <p>已配置单次运行</p>}
      {status.control.pending && <p>等待执行的批次 <code>{status.control.pending.id}</code></p>}
      {status.control.latest && <p>最近批次 <code>{status.control.latest.id}</code> · 暂停添加已应答 {status.control.latest.submitted} · 目标已有 {status.control.latest.existing} · 待核实 {status.control.latest.unknown} · 失败 {status.control.latest.failed} · 后续操作完成 {status.control.latest.completed ?? 0} · 需人工核对 {status.control.latest.review ?? 0}</p>}
      {status.control.latest && <button type="button" disabled={busy} onClick={() => void lookupBatch(status.control.latest!.id)}>查看最近批次回执</button>}
      {!status.control.actor && status.ready && <label><input type="checkbox" checked={exclusive} disabled={busy} onChange={e => setExclusive(e.target.checked)} />确认旧转种执行器已停止，源和目标任务仅由此调度修改。授权后台绑定并校验目标{status.autoStart ? "、启动做种" : ""}{status.deleteSource ? "、移除源任务并保留文件" : ""}。</label>}
      {status.control.actor ? <button disabled={busy} onClick={() => void operate("stop")}>停止调度</button> :
        <button disabled={busy || !status.ready} onClick={() => void operate("activate")}>启用调度</button>}
    </div>}
    <form onSubmit={e => { e.preventDefault(); void lookupBatch(batchId); }}>
      <label>批次编号<input value={batchId} disabled={busy} required pattern="[a-f0-9]{64}" onChange={e => { setBatchId(e.target.value.trim().toLowerCase()); setReceipts(undefined); }} /></label>
      <button type="submit" disabled={busy}>查询批次回执</button>
    </form>
    {receipts && (receipts.length ? <ul>{receipts.map(id => <li key={id}><button type="button" onClick={() => select(id)}>{id}</button></li>)}</ul> : <p>此批次没有候选任务回执。</p>)}
    {error && <p role="alert">{error}</p>}
  </section>;
}
