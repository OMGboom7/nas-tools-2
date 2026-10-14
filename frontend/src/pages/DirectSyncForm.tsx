import { useEffect, useRef, useState } from "react";
import { ApiError, getDirectSyncRecords, previewDirectSync, createDirectSync, type OrganizationSyncRecord, type OrganizationPlan } from "../api/client";

type Props = { token: string; enabled: boolean; onChanged: (jobId: string) => Promise<void>; onSessionExpired: () => void };
const labels: Record<string, string> = { available: "可保存", conflict: "目标冲突，不覆盖", recorded: "已有旧同步历史，不重做（非当前目标存在证明）", filtered: "忽略，不执行", blocked: "已阻止" };

export function DirectSyncForm({ token, enabled, onChanged, onSessionExpired }: Props) {
  const [records, setRecords] = useState<OrganizationSyncRecord[]>([]);
  const [id, setID] = useState("");
  const [path, setPath] = useState(".");
  const [plan, setPlan] = useState<OrganizationPlan | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  const inFlight = useRef(false);
  useEffect(() => { generation.current++; inFlight.current = false; setRecords([]); setID(""); setPath("."); setPlan(null); setError(""); setBusy(false); return () => { generation.current++; }; }, [token]);
  const record = records.find((item) => item.id === id);
  async function operate(action: "list" | "preview" | "save") {
    if (!enabled || inFlight.current || action !== "list" && !record || action === "save" && !plan?.items.some((item) => item.status === "available")) return;
    inFlight.current = true;
    const version = ++generation.current;
    setBusy(true); setError("");
    try {
      if (action === "list") {
        setPlan(null);
        const data = await getDirectSyncRecords(token);
        if (version === generation.current) { setRecords(data); setID(""); }
      } else if (action === "preview") {
        setPlan(null);
        const data = await previewDirectSync(token, id, path);
        if (version === generation.current) setPlan(data);
      } else {
        const job = await createDirectSync(token, id, path, plan!.fingerprint);
        if (version !== generation.current) return;
        setPlan(null);
        await onChanged(job.id);
      }
    } catch (err) {
      if (version !== generation.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "同步请求失败");
      if (action === "save") {
        setPlan(null);
        // Reservation may have committed before response loss: read jobs only.
        try { await onChanged(""); } catch { /* retain original failure */ }
      }
    } finally { if (version === generation.current) { setBusy(false); inFlight.current = false; } }
  }
  function invalidate() { generation.current++; setPlan(null); setError(""); }
  return <section aria-label="保留目录结构的直接同步">
    <h2>不识别、保留目录结构的同步</h2>
    <p>读取已有 CONFIG_SYNC_PATHS 配置，仅处理启用且未勾选识别重命名的记录。普通文件、媒体、字幕均保持相对路径，不查询 TMDB，不写媒体识别历史。目标必须已存在；这里不启动自动监控，也不创建或修改同步配置。</p>
    <p>部署执行前必须实际停止旧 Python 同步服务及其他会修改这些路径的执行器；Go-only 设置不会替你停止旧进程。</p>
    {error && <p role="alert">{error}</p>}
    <button type="button" disabled={!enabled || busy} onClick={() => void operate("list")}>读取／刷新同步配置</button>
    <fieldset disabled={!enabled || busy}>
      <label>同步配置<select value={id} onChange={(event) => { setID(event.target.value); invalidate(); }}><option value="">请选择</option>{records.map((item) => <option key={item.id} value={item.id} disabled={item.enabled !== 1 || item.rename !== 0}>#{item.id} · {item.note || item.source} · {item.mode}{item.enabled !== 1 ? " · 未启用" : item.rename !== 0 ? " · 识别整理，非直接同步" : ""}</option>)}</select></label>
      {record && <p>来源：<code>{record.source}</code><br />目标：<code>{record.dest || "未指定，不能直接同步"}</code><br />方式：{record.mode} · 配置备注：{record.note || "无"}</p>}
      <label>来源内相对路径<input value={path} maxLength={4096} onChange={(event) => { setPath(event.target.value); invalidate(); }} /></label>
      <button type="button" disabled={!record} onClick={() => void operate("preview")}>生成只读同步预览</button>
    </fieldset>
    {plan && <div>
      <p>方式 {plan.mode}；跳过 {plan.skipped} 个隐藏／系统／非普通文件对象。忽略词按目的路径及文件名匹配。旧同步历史只抑制重做，不代表当前目标仍存在。此任务只处理保存的可用条目，不代表整个目录已同步。</p>
      <ul>{plan.items.map((item) => <li key={item.source}>{item.source} → {item.target} · {labels[item.status] || item.status}{item.reason ? ` · ${item.reason}` : ""}</li>)}</ul>
      <button type="button" disabled={!enabled || busy || !plan.items.some((item) => item.status === "available")} onClick={() => void operate("save")}>保存待确认同步任务（不执行）</button>
      <p>保存后在下方任务区核对路径，再另行确认执行；移动必须单独授权源移除。出错只刷新账本，不自动重试或改为其他方式。</p>
    </div>}
  </section>;
}
