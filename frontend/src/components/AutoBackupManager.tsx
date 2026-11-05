import { useEffect, useRef, useState } from "react";
import { ApiError } from "../api/client";

type Run = {
  id: string;
  kind: string;
  state: string;
  created: string;
  message: string;
  archive: { name: string; size: number };
  retired: boolean;
  pruned: number;
};

type Status = {
  goOnly: boolean;
  fingerprint: string;
  target: string;
  settings: { enabled: boolean; full: boolean; onlyonce: boolean; cron: string; cnt: string };
  epoch: string;
  next: string;
  once: string;
  authorized: boolean;
  runs: Run[];
};

const kinds: Record<string, string> = { manual: "手动", once: "一次运行", cron: "周期", legacy: "配置导出" };
const states: Record<string, string> = { running: "执行中", staged: "正在发布", complete: "已完成", failed: "失败", stopped: "已停止", unknown: "待核对" };
const base = "/api/v1/backup";

async function request<T>(token: string, path: string, signal: AbortSignal, body?: unknown): Promise<T> {
  const response = await fetch(base + path, {
    method: body === undefined ? "GET" : "POST",
    signal,
    headers: { Authorization: token, ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const reply = await response.json().catch(() => null);
  if (!response.ok || reply?.code !== 0 || reply?.success !== true || !reply?.data) {
    throw new ApiError(reply?.message || "无法读取备份结果", response.status);
  }
  return reply.data as T;
}

async function downloadBackup(token: string, path: string, method = "GET") {
  const response = await fetch(path, { method, headers: { Authorization: token } });
  const name = response.headers.get("Content-Disposition")?.match(/filename="(bk_[a-z0-9_]+\.zip)"/)?.[1];
  if (!response.ok || !name || response.headers.get("Content-Type") !== "application/zip") {
    const reply = await response.json().catch(() => null);
    throw new ApiError(reply?.message || "无法核验备份文件", response.status);
  }
  const url = URL.createObjectURL(await response.blob());
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

export function BackupExport({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [confirmed, setConfirmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function exportBackup() {
    if (!confirmed || busy) return;
    setBusy(true);
    setConfirmed(false);
    setError("");
    try {
      await downloadBackup(token, "/backup", "POST");
    } catch (e) {
      if (e instanceof ApiError && e.code === 401) onSessionExpired();
      else setError(e instanceof Error ? e.message : "导出失败");
    } finally {
      setBusy(false);
    }
  }
  return <section className="signin-manager" aria-label="配置备份">
    <header><div><h2>配置备份</h2><p>将精简备份保存到配置目录下的 backup_file 并下载。归档含账户和服务配置，请妥善保管。</p></div></header>
    <label><input type="checkbox" checked={confirmed} disabled={busy} onChange={e => setConfirmed(e.target.checked)}/>确认创建并下载配置备份</label>
    <button type="button" disabled={!confirmed || busy} onClick={() => void exportBackup()}>{busy ? "正在创建备份" : "下载配置备份"}</button>
    {error && <p role="alert">{error}</p>}
  </section>;
}

export function AutoBackupManager({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [status, setStatus] = useState<Status>();
  const [confirmed, setConfirmed] = useState("");
  const [retention, setRetention] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const nonce = useRef("");
  const inFlight = useRef(false);
  const revision = useRef(0);

  function report(e: unknown) {
    if (e instanceof ApiError && e.code === 401) onSessionExpired();
    else setError(e instanceof Error ? e.message : "无法读取自动备份");
  }

  async function load(signal: AbortSignal) {
    const version = ++revision.current;
    try {
      const result = await request<Status>(token, "", signal);
      if (!signal.aborted && version === revision.current) {
        setStatus(result);
        setConfirmed(value => value === result.fingerprint ? value : "");
        setRetention(value => value === result.fingerprint ? value : "");
      }
    } catch (e) {
      if (!signal.aborted && version === revision.current) report(e);
    }
  }

  useEffect(() => {
    const session = new AbortController();
    controller.current = session;
    void load(session.signal);
    const timer = window.setInterval(() => void load(session.signal), 4000);
    return () => { session.abort(); window.clearInterval(timer); };
  }, [token]);

  const active = status?.runs.find(run => run.state === "running" || run.state === "staged");
  const hasRetention = Number(status?.settings.cnt) > 0;
  const approved = !!status && confirmed === status.fingerprint && (!hasRetention || retention === status.fingerprint);
  const maySchedule = status?.settings.onlyonce || status?.settings.enabled && !!status.settings.cron;

  async function perform(action: "run" | "activate" | "stop") {
    const signal = controller.current?.signal;
    if (!status?.goOnly || !signal || inFlight.current || action !== "stop" && !approved) return;
    inFlight.current = true;
    setBusy(true);
    setError("");
    try {
      if (action === "run" && !nonce.current) {
        nonce.current = Array.from(crypto.getRandomValues(new Uint8Array(32)), byte => byte.toString(16).padStart(2, "0")).join("");
      }
      await request(token, "/" + action, signal, {
        confirm: true, confirmRetention: hasRetention && retention === status.fingerprint,
        fingerprint: status.fingerprint, epoch: status.epoch, activeId: active?.id || "",
        ...(action === "run" ? { requestId: nonce.current } : {}),
      });
      if (action === "run") nonce.current = "";
      setConfirmed("");
      setRetention("");
    } catch (e) {
      if (!signal.aborted) report(e);
    } finally {
      await load(signal);
      inFlight.current = false;
      if (!signal.aborted) setBusy(false);
    }
  }

  return <section className="signin-manager" aria-label="自动备份管理">
    <header><div><h2>自动备份管理</h2><p>在插件配置中保存周期、目录和保留数量，再核对备份范围。</p></div>
      <button type="button" disabled={busy} onClick={() => { const signal = controller.current?.signal; if (signal) void load(signal); }}>重新读取备份</button></header>
    {error && <p role="alert">{error}</p>}
    {status && <>
      <p>目标目录 <code>{status.target}</code></p>
      <p>{status.settings.full ? "完整备份" : "精简备份"} · 包含配置、分类规则和数据库</p>
      <p>{status.authorized ? "已授权" : "未启用"} · 周期 {status.settings.cron || "未设置"}</p>
      {status.authorized && status.next && <p>下次周期 {new Date(status.next).toLocaleString()}</p>}
      {status.authorized && status.once && <p>一次运行 {new Date(status.once).toLocaleString()}</p>}
      <label><input type="checkbox" checked={confirmed === status.fingerprint} disabled={busy || !status.goOnly}
        onChange={e => setConfirmed(e.target.checked ? status.fingerprint : "")}/>确认向上述目录写入备份</label>
      {hasRetention && <label><input type="checkbox" checked={retention === status.fingerprint} disabled={busy || !status.goOnly}
        onChange={e => setRetention(e.target.checked ? status.fingerprint : "")}/>确认清理该目录中有记录并通过校验的较早 Go 自动备份，保留 {status.settings.cnt} 份</label>}
      <div className="site-statistics__actions">
        <button type="button" disabled={!status.goOnly || busy || !!active || !approved} onClick={() => void perform("run")}>备份一次</button>
        <button type="button" disabled={!status.goOnly || busy || !!active || !approved || !maySchedule} onClick={() => void perform("activate")}>启用保存的周期或一次运行</button>
        <button type="button" disabled={!status.goOnly || busy || !active && !status.next && !status.once} onClick={() => void perform("stop")}>停止备份和定时</button>
      </div>
      <h3>备份记录</h3>
      {status.runs.length === 0 ? <p>暂无备份记录。</p> : <ul>{status.runs.map(run => <li key={run.id}>
        <strong>{kinds[run.kind]} · {states[run.state]}</strong>
        <p>{run.message} · {new Date(run.created).toLocaleString()}</p>
        <small>编号 {run.id}</small>
        {run.pruned > 0 && <p>已清理 {run.pruned} 份较早归档</p>}
        {run.retired && <p>归档已按保留设置清理</p>}
        {run.state === "complete" && !run.retired && <p><button type="button" onClick={() => void downloadBackup(token, base + "/runs/" + encodeURIComponent(run.id) + "/file").catch(report)}>下载 {run.archive.name}</button></p>}
      </li>)}</ul>}
    </>}
  </section>;
}
