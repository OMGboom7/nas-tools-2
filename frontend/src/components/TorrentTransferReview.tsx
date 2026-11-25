import { useEffect, useRef, useState } from "react";
import { ApiError, reviewTorrentTransfers, type TorrentTransferReview as Review } from "../api/client";

export function TorrentTransferReview({ token, onSessionExpired, select }: {
  token: string; onSessionExpired: () => void; select: (id: string) => void;
}) {
  const [result, setResult] = useState<Review>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  useEffect(() => () => request.current?.abort(), []);
  async function review() {
    if (request.current) return;
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    setError("");
    setResult(undefined);
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
  return <section aria-label="转种计划审核">
    <p>从配置的下载器和种子目录读取已完成任务，核对目标目录并保存计划回执。</p>
    <button type="button" disabled={busy} onClick={() => void review()}>{busy ? "正在审核…" : "读取任务并生成转种计划"}</button>
    {error && <p role="alert">{error}</p>}
    {result && <div>
      <p role="status">{result.source} → {result.target} · 待转种 {result.items.length} · 目标已有 {result.existing} · 核对失败 {result.failed}</p>
      <ul>{result.items.map(item => <li key={item.receipt}>
        <strong>{item.name || item.sourceId}</strong> · 目标目录 {item.directory}
        <p>回执 <code>{item.receipt}</code> <button type="button" onClick={() => select(item.receipt)}>查看回执</button></p>
      </li>)}</ul>
    </div>}
  </section>;
}
