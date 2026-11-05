import { useRef, useState } from "react";
import { ApiError } from "../api/client";

type RestoreFile = { name: string; size: number; sha256: string };
type RestoreReceipt = { id: string; created: string; files: RestoreFile[] };
const names = ["config.yaml", "default-category.yaml", "user.db"];
const maxArchive = 2 * 1024 ** 3;

function isReceipt(value: unknown): value is RestoreReceipt {
  const receipt = value as RestoreReceipt | null;
  return !!receipt && typeof receipt.id === "string" && /^[a-f0-9]{32}$/.test(receipt.id) &&
    typeof receipt.created === "string" && Array.isArray(receipt.files) && receipt.files.length === names.length &&
    names.every(name => receipt.files.some(file => file?.name === name && Number.isSafeInteger(file.size) &&
      file.size > 0 && typeof file.sha256 === "string" && /^[a-f0-9]{64}$/.test(file.sha256)));
}

export function BackupRestorePanel({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [file, setFile] = useState<File>();
  const [receipt, setReceipt] = useState<RestoreReceipt>();
  const [confirmed, setConfirmed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const inFlight = useRef(false);

  async function stage() {
    if (!file || !confirmed || inFlight.current) return;
    if (file.size < 1 || file.size > maxArchive) {
      setError("请选择不超过 2 GiB 的 ZIP 备份");
      return;
    }
    inFlight.current = true;
    setBusy(true);
    setConfirmed(false);
    setError("");
    setReceipt(undefined);
    try {
      const response = await fetch("/api/v1/backup/restore", {
        method: "POST",
        headers: { Authorization: token, "Content-Type": "application/zip" },
        body: file,
      });
      const reply = await response.json().catch(() => null);
      if (!response.ok || reply?.success !== true || reply?.code !== 0 || !isReceipt(reply?.data)) {
        throw new ApiError(reply?.message || "暂存应答未确认", response.status);
      }
      setReceipt(reply.data);
    } catch (e) {
      if (e instanceof ApiError && e.code === 401) onSessionExpired();
      else setError(e instanceof Error ? e.message : "无法暂存备份");
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  }

  return <section className="signin-manager" aria-label="从备份恢复">
    <header><div><h2>从备份恢复</h2><p>服务器会校验 ZIP，并把其中的配置、分类规则和数据库暂存在私有恢复目录。</p></div></header>
    <label>备份 ZIP <input type="file" accept=".zip,application/zip" disabled={busy} onChange={event => {
      setFile(event.target.files?.[0]);
      setConfirmed(false);
      setReceipt(undefined);
      setError("");
    }} /></label>
    <label><input type="checkbox" checked={confirmed} disabled={!file || busy} onChange={event => setConfirmed(event.target.checked)} />确认将选中的备份上传至当前 NAS Tools 服务器</label>
    <button type="button" disabled={!file || !confirmed || busy} onClick={() => void stage()}>{busy ? "校验中…" : "校验并暂存"}</button>
    {error && <p role="alert">{error}</p>}
    {receipt && <div role="status">
      <p>校验通过。恢复编号 <code>{receipt.id}</code></p>
      <ul>{receipt.files.map(file => <li key={file.name}>{file.name} · {file.size.toLocaleString()} 字节</li>)}</ul>
      <p>停止使用此配置目录的所有服务后，用同一挂载和服务账户执行离线命令。</p>
      <pre><code>{`nas-tools-go restore apply ${receipt.id} --offline`}</code></pre>
      <p>启动前可以回滚原文件。核对选定版本后，显式放行启动。已保存的调度可能在启动后继续执行。</p>
      <pre><code>{`nas-tools-go restore rollback ${receipt.id} --offline\nnas-tools-go restore resume ${receipt.id} --confirm-resume`}</code></pre>
    </div>}
  </section>;
}
