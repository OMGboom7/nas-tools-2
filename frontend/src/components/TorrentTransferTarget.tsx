import { useEffect, useRef, useState } from "react";
import { ApiError } from "../api/client";

type Target = { receipt: string; proof: string; target: string; wireId: string; directory: string; name: string; addedAt: number; state: string; targetStage: string; canStart: boolean; sourceName: string; sourceId: string; sourceDirectory: string; sourceTask: string; canRemove: boolean; canConfirmAbsence: boolean };
type Action = "inspect" | "bind" | "verify" | "start" | "remove-source" | "confirm-absence";
const labels = { bind: "确认归属并绑定", verify: "推进校验", start: "启动做种", "remove-source": "移除源任务并保留文件", "confirm-absence": "核实源任务移除结果" };

export function TorrentTransferTarget({ token, receipt, refresh, expired }: {
  token: string; receipt: string; refresh: () => void; expired: () => void;
}) {
  const [target, setTarget] = useState<Target>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const request = useRef<AbortController | null>(null);
  useEffect(() => () => request.current?.abort(), []);
  async function run(action: Action) {
    if (request.current) return;
    if (action !== "inspect") {
      if (!target) return;
      const message = action === "bind"
        ? "请核对该任务由本次暂停添加创建，并确认期间没有其他程序或用户替换此任务。"
        : labels[action] + "？";
      if (action === "remove-source") {
        if (!window.confirm(`从 ${target.sourceName} 移除任务「${target.sourceTask}」并保留文件？\n任务编号：${target.sourceId}\n源目录：${target.sourceDirectory}\n目标下载器：${target.target}`)) return;
      } else if (action !== "confirm-absence" && !window.confirm(target.target + " · " + target.name + "\n" + target.wireId + "\n" + message)) return;
    }
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const response = await fetch("/api/v1/torrent-transfer/records/" + receipt + "/" + action, {
        method: "POST", headers: { Authorization: token, "Content-Type": "application/json" },
        body: JSON.stringify(action === "inspect" ? {} : {proof: target?.proof, confirmAction: true, confirmOwner: action === "bind", keepFiles: action === "remove-source"}),
        signal: controller.signal,
      });
      const body = await response.json().catch(() => null);
      if (controller.signal.aborted) return;
      if (!response.ok) throw new ApiError(body?.message || "目标任务核对失败", response.status);
      const data = body?.result;
      if (body?.code !== 0 || data?.receipt !== receipt) throw new Error();
      if (action === "inspect") {
        if (typeof data.proof !== "string" || !/^[a-f0-9]{64}$/.test(data.proof) ||
            typeof data.wireId !== "string" || !/^[a-f0-9]{40}$/.test(data.wireId) ||
            ![data.target, data.name, data.directory, data.state].every(v => typeof v === "string") ||
            ![data.sourceName, data.sourceDirectory, data.sourceTask].every(v => typeof v === "string") ||
            typeof data.sourceId !== "string" || !/^[a-f0-9]{40}$/.test(data.sourceId) ||
            ![data.canStart, data.canRemove, data.canConfirmAbsence].every(v => typeof v === "boolean") ||
            !Number.isSafeInteger(data.addedAt) || !["", "bound", "checking", "verified"].includes(data.targetStage)) throw new Error();
        setTarget(data);
      } else if (action === "confirm-absence" || action === "remove-source") {
        if (data.state !== "source-absent") throw new Error();
        setTarget(undefined);
        setNotice("已核实源任务不存在，转种历史已更新。");
      } else {
        setTarget(undefined);
        refresh();
      }
    } catch (err) {
      if (controller.signal.aborted) return;
      setTarget(undefined);
      if (err instanceof ApiError && err.code === 401) expired();
      else setError(err instanceof ApiError ? err.message : "无法确认操作结果，请刷新回执");
    } finally {
      if (!controller.signal.aborted) setBusy(false);
      request.current = null;
    }
  }
  const action = target?.targetStage === "" ? "bind" : target?.targetStage === "verified" ? (target.canStart ? "start" : undefined) : "verify";
  return <section aria-label="目标任务核对">
    <button type="button" disabled={busy} onClick={() => void run("inspect")}>查看目标任务</button>
    {target && <div>
      <p>{target.target} · {target.name} · {target.state}</p>
      <p>任务编号 <code>{target.wireId}</code> · 创建于 {new Date(target.addedAt * 1000).toLocaleString()}</p>
      <p>下载目录 {target.directory}</p>
      {action && <button type="button" disabled={busy} onClick={() => void run(action)}>{labels[action]}</button>}
      {(target.canRemove || target.canConfirmAbsence) && <div>
        <p>源任务 {target.sourceName} · {target.sourceTask} · <code>{target.sourceId}</code></p>
        <p>源目录 {target.sourceDirectory}</p>
        {target.canRemove && <button type="button" disabled={busy} onClick={() => void run("remove-source")}>{labels["remove-source"]}</button>}
        {target.canConfirmAbsence && <button type="button" disabled={busy} onClick={() => void run("confirm-absence")}>{labels["confirm-absence"]}</button>}
      </div>}
    </div>}
    {error && <p role="alert">{error}</p>}
    {notice && <p role="status">{notice} <button type="button" onClick={refresh}>刷新回执</button></p>}
  </section>;
}
