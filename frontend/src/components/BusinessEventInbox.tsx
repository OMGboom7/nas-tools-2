import { useEffect, useRef, useState } from "react";
import { ApiError, dismissBusinessDelivery, getBusinessEvents, type BusinessEvent } from "../api/client";

const labels: Record<string, string> = { pending: "待发送", sending: "已记录发送意图：可能正在发送或中断", sent: "渠道已确认接收", uncertain: "响应不确定：不自动重发", blocked: "已阻止：需要核对配置", dismissed: "已明确忽略（不代表送达）" };
export function BusinessEventInbox({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [events, setEvents] = useState<BusinessEvent[]>([]); const [error, setError] = useState(""); const [busy, setBusy] = useState(false);
  const [selected, setSelected] = useState(""); const [reason, setReason] = useState(""); const [confirm, setConfirm] = useState(false);
  const version = useRef(0); const inFlight = useRef(false);
  useEffect(() => { version.current++; inFlight.current = false; setEvents([]); setError(""); setBusy(false); setSelected(""); setReason(""); setConfirm(false); return () => { version.current++; }; }, [token]);
  async function operate(mutate = false) {
    if (inFlight.current || mutate && (!selected || !reason.trim() || !confirm)) return;
    inFlight.current = true; const current = ++version.current; setBusy(true); setError("");
    try {
      if (mutate) await dismissBusinessDelivery(token, selected, reason, confirm);
      const next = await getBusinessEvents(token);
      if (current === version.current) { setEvents(next); setSelected(""); setReason(""); setConfirm(false); }
    } catch (err) {
      if (current !== version.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "请只读刷新回执");
      setConfirm(false);
      // A lost disposition response only permits a readonly receipt refresh.
      if (mutate) { try { const next = await getBusinessEvents(token); if (current === version.current) setEvents(next); } catch { /* retain original error */ } }
    } finally { if (current === version.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section aria-label="业务事件与发送回执"><h2>业务事件与发送回执</h2><p>管理员只读查看最近及优先待核对的最多 50 条事件。后台只发送业务提交时已启用且选择该事件的渠道；不补发历史，不把通知失败变成业务重试。入库完成与下载器回写是不同阶段。</p><button type="button" disabled={busy} onClick={() => void operate()}>只读刷新业务消息</button>{error && <p role="alert">{error}</p>}
    {events.map((event) => <article key={event.id}><h3>{event.title}</h3><p>{event.kind} · {event.created} · {event.reference}</p><p style={{ whiteSpace: "pre-wrap" }}>{event.text}</p>{event.deliveries.length === 0 && <p>仅站内记录；提交时没有匹配且有效的启用渠道事件开关。</p>}<ul>{event.deliveries.map((d) => <li key={d.id}>{d.channelName}（{d.channelType}）· {labels[d.state] || d.state} · {d.reason} {d.actor && `· 处置人：${d.actor}`}{!["sent", "dismissed"].includes(d.state) && <button type="button" disabled={busy} onClick={() => { setSelected(d.id); setReason(""); setConfirm(false); }}>审核忽略此发送回执</button>}</li>)}</ul></article>)}
    {selected && <fieldset disabled={busy}><p>回执 {selected}。忽略不会重发消息、删除业务记录或证明用户已收到；正在发送时需等待后台锁释放。</p><label>处置原因<input value={reason} maxLength={1000} onChange={(e) => { setReason(e.target.value); setConfirm(false); }} /></label><label><input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />仅将此回执标为明确忽略，保留原事件与审计记录</label><button type="button" disabled={!confirm || !reason.trim()} onClick={() => void operate(true)}>确认仅忽略回执</button></fieldset>}
  </section>;
}
