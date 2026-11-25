import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, getTorrentTransferReceipt, type TorrentTransferReceipt } from "../api/client";

const stages = { bound: "已关联目标任务", checking: "已观察到校验", verified: "已核实文件完整" };
const actions = { add: "暂停添加", verify: "请求校验", start: "请求启动", "remove-source": "移除源任务" };
const states = { pending: "等待回执", acknowledged: "下载器已应答", unknown: "结果待核实" };

export function TorrentTransferReceipts({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [id, setId] = useState("");
  const [record, setRecord] = useState<TorrentTransferReceipt>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  useEffect(() => () => request.current?.abort(), []);

  async function lookup(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    setRecord(undefined);
    setError("");
    try {
      const result = await getTorrentTransferReceipt(token, id.trim().toLowerCase(), controller.signal);
      if (!controller.signal.aborted) setRecord(result);
    } catch (err) {
      if (controller.signal.aborted) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof ApiError ? err.message : "无法读取转种回执，请重新查询");
    } finally {
      if (!controller.signal.aborted) setBusy(false);
    }
  }

  return <details>
    <summary>自动转种回执</summary>
    <form className="download-magnet-form" onSubmit={lookup}>
      <label>回执编号<input value={id} required pattern="[a-fA-F0-9]{64}" placeholder="64 位回执编号"
        onChange={event => {
          request.current?.abort();
          setBusy(false);
          setId(event.target.value.trim());
          setRecord(undefined);
          setError("");
        }} /></label>
      <button type="submit" disabled={busy}>{busy ? "查询中…" : "查询回执"}</button>
    </form>
    {error && <p role="alert">{error}</p>}
    {record && <div role="status">
      <p>目标任务 · {record.targetStage ? stages[record.targetStage] : "尚未关联"}</p>
      {record.actions.length === 0 ? <p>尚无动作记录</p> : <ul>{record.actions.map(item =>
        <li key={item.action}>{actions[item.action]} · {states[item.state]} · <time dateTime={item.updated}>{new Date(item.updated).toLocaleString()}</time></li>
      )}</ul>}
    </div>}
  </details>;
}
