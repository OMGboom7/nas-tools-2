import { useEffect, useRef, useState } from "react";
import { ApiError, reviewTorrentTransfers, submitPausedTransfer, type TorrentTransferReview as Review } from "../api/client";

export function TorrentTransferReview({ token, onSessionExpired, select }: {
  token: string; onSessionExpired: () => void; select: (id: string) => void;
}) {
  const [result, setResult] = useState<Review>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [submission, setSubmission] = useState("");
  const [attempted, setAttempted] = useState<string[]>([]);
  const request = useRef<AbortController | null>(null);
  useEffect(() => () => request.current?.abort(), []);
  async function review() {
    if (request.current) return;
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    setError("");
    setResult(undefined);
    setAttempted([]);
    setSubmission("");
    try {
      const next = await reviewTorrentTransfers(token, controller.signal);
      if (!controller.signal.aborted) setResult(next);
    } catch (err) {
      if (controller.signal.aborted) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof ApiError ? err.message : "无法审核转种计划");
    } finally {
      if (!controller.signal.aborted) setBusy(false);
      request.current = null;
    }
  }
  async function submit(item: Review["items"][number]) {
    if (!result || request.current || attempted.includes(item.receipt)) return;
    if (!window.confirm(`确认将「${item.name || item.sourceId}」以暂停状态添加到 ${result.target}？\n目标目录：${item.directory}`)) return;
    const controller = new AbortController();
    request.current = controller;
    setAttempted(previous => [...previous, item.receipt]);
    setBusy(true);
    setSubmission("");
    setError("");
    try {
      const reply = await submitPausedTransfer(token, item.receipt, controller.signal);
      if (controller.signal.aborted) return;
      const messages: Record<string, string> = {
        acknowledged: "目标下载器已应答", duplicate: "目标已有该任务", pending: "原提交等待应答", unknown: "原提交结果待核实",
      };
      setSubmission(messages[reply.state] + " · " + reply.receipt);
      select(reply.receipt);
    } catch (err) {
      if (controller.signal.aborted) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof ApiError ? err.message : "无法确认添加结果，请查看原回执");
    } finally {
      request.current = null;
      if (!controller.signal.aborted) setBusy(false);
    }
  }
  return <section aria-label="转种计划审核">
    <p>从配置的下载器和种子目录读取已完成任务，核对目标目录并保存计划回执。</p>
    <button type="button" disabled={busy} onClick={() => void review()}>读取任务并生成转种计划</button>
    {submission && <p role="status">{submission}</p>}
    {error && <p role="alert">{error}</p>}
    {result && <div>
      <p role="status">{result.source} → {result.target} · 待转种 {result.items.length} · 目标已有 {result.existing} · 核对失败 {result.failed}</p>
      <ul>{result.items.map(item => <li key={item.receipt}>
        <strong>{item.name || item.sourceId}</strong> · 目标目录 {item.directory}
        <button type="button" disabled={busy || attempted.includes(item.receipt)} onClick={() => void submit(item)}>暂停添加到目标下载器</button>
        <p>回执 <code>{item.receipt}</code> <button type="button" onClick={() => select(item.receipt)}>查看回执</button></p>
      </li>)}</ul>
    </div>}
  </section>;
}
