import { useEffect, useRef, useState } from "react";
import { ApiError, reviewDownloadCompletion, type DownloadCompletionReview } from "../api/client";

type Props = { token: string; enabled: boolean; onSessionExpired: () => void };
export function DownloadCompletionReviewForm({ token, enabled, onSessionExpired }: Props) {
  const [downloaderId, setDownloaderID] = useState("");
  const [taskId, setTaskID] = useState("");
  const [review, setReview] = useState<DownloadCompletionReview | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const inFlight = useRef(false);
  useEffect(() => { generation.current++; inFlight.current = false; setDownloaderID(""); setTaskID(""); setReview(null); setError(""); setBusy(false); return () => { generation.current++; }; }, [token]);
  function invalidate() { generation.current++; setReview(null); setError(""); }
  async function inspect() {
    if (!enabled || inFlight.current || !downloaderId || !taskId) return;
    inFlight.current = true; const version = ++generation.current; setBusy(true); setError(""); setReview(null);
    try { const result = await reviewDownloadCompletion(token, { downloaderId, taskId }); if (version === generation.current) setReview(result); }
    catch (err) { if (version !== generation.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "无法核实完成证据"); }
    finally { if (version === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section aria-label="下载完成证据审核"><h2>下载完成证据审核</h2>
    <p>管理员只读入口，支持 qBittorrent、Transmission 和 Aria2。须在下载器配置中启用完成转移。核验任务真实文件清单、标签隔离、目录映射及本地文件，不靠下载中列表或百分比猜测。</p>
    <fieldset disabled={!enabled || busy}><label>下载器配置 ID<input value={downloaderId} inputMode="numeric" maxLength={20} onChange={(e) => { invalidate(); setDownloaderID(e.target.value); }} /></label><label>任务完整 Hash／GID<input value={taskId} maxLength={128} onChange={(e) => { invalidate(); setTaskID(e.target.value); }} /></label><button type="button" disabled={!downloaderId || !taskId} onClick={() => void inspect()}>只读核验完成任务</button></fieldset>
    {error && <p role="alert">{error}</p>}
    {review && <div><p>{review.downloaderName} · {review.downloaderType} · {review.task.name} · {review.task.state} · 原方式 {review.mode}</p><p>{review.eligible ? "本次完成证据及本地静默检查可用，但不是执行授权。" : "尚不满足整理条件。"} 这是单次快照，不证明后续文件不变，也不证明媒体已入库。</p><ul>{review.reasons.map((reason, i) => <li key={i}>{reason}</li>)}</ul><p>任务保存目录 {review.task.directory} → 本地 {review.location.directory}（{review.location.mapped ? "已按配置映射" : "未替换，仍限定在配置内本地根"}）；实际来源根 {review.sourceRoot}。只核对该任务明确选中的 {review.files.length} 个文件，不扫描同目录其他任务。</p><ul>{review.files.map((file) => <li key={file.path}>{file.path} · {file.kind} · {file.size} 字节</li>)}</ul></div>}
    <p>此证据入口只读，不建任务或执行。人工整理和后台完成调度均有独立授权入口，不会因本次审核或刷新自动执行。</p>
  </section>;
}
