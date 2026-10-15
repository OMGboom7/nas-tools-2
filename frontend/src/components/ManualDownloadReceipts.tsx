import { useEffect, useRef, useState } from "react";
import { ApiError, getManualDownloadReceipts, reconcileManualDownload, type ManualDownloadReceipt } from "../api/client";
export function ManualDownloadReceipts({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [items, setItems] = useState<ManualDownloadReceipt[]>([]); const [page, setPage] = useState(1); const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  const [selected, setSelected] = useState(""); const [confirm, setConfirm] = useState(false); const generation = useRef(0); const inFlight = useRef(false);
  useEffect(() => { generation.current++; inFlight.current = false; setItems([]); setPage(1); setBusy(false); setSelected(""); setConfirm(false); setError(""); return () => { generation.current++; }; }, [token]);
  async function operate(targetPage: number, id = "") {
    if (inFlight.current || id && !confirm) return;
    inFlight.current = true; const current = ++generation.current; setBusy(true); setError("");
    try {
      if (id) await reconcileManualDownload(token, id, confirm);
      const next = await getManualDownloadReceipts(token, targetPage);
      if (current === generation.current) { setItems(next); setPage(targetPage); setSelected(""); setConfirm(false); }
    } catch (err) {
      if (current !== generation.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "请保留原回执"); setConfirm(false);
      if (id) { try { const next = await getManualDownloadReceipts(token, targetPage); if (current === generation.current) setItems(next); } catch { /* no replay or implicit resubmission */ } }
    } finally { if (current === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section aria-label="手动下载提交回执"><h2>手动下载提交回执（管理员）</h2><p>磁力、种子、站点链接和原生搜索按真实 Hash 持久防重。响应不确定时不要重复下种；切换默认下载器或刷新搜索不会解除待核实提交。此处仅支持 qBittorrent / Transmission 正向核实原 Hash，Aria2 / 115 待核实项保留，不自动释放。</p><button type="button" disabled={busy} onClick={() => void operate(page)}>只读刷新原提交</button>{error && <p role="alert">{error}</p>}
    <ul>{items.map((item) => <li key={item.id}><h3>{item.title}</h3><p>{item.id} · {item.source} · {item.downloaderName} ({item.downloaderType}) · {item.state} · {item.reason} · 提交者 {item.actor}</p><p>Hash {item.infoHash} · 任务 {item.taskId || "未核实"} · {item.created}。已提交仅表示当时接收或存在，不证明文件完成、入库或设置已全部生效。</p>{item.state === "pending" && <button type="button" disabled={busy} onClick={() => { setSelected(item.id); setConfirm(false); }}>核对原提交 {item.id}</button>}</li>)}</ul>
    <button type="button" disabled={busy || page === 1} onClick={() => void operate(page - 1)}>上一页</button><span>第 {page} 页，每页最多 100 条；优先待核实，列表变化可能影响分页。</span><button type="button" disabled={busy || items.length < 100 || page >= 10000} onClick={() => void operate(page + 1)}>下一页</button>
    {selected && <fieldset disabled={busy}><p>仅核实回执 {selected} 对应原下载器当前是否存在准确任务，不请求新增、修改标签或删除数据。缺席和错误不解除预留。</p><label><input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />同意只读核实并在有正向证据时补齐本地历史与通知事件</label><button type="button" disabled={!confirm} onClick={() => void operate(page, selected)}>确认只核实原提交</button></fieldset>}
  </section>;
}
