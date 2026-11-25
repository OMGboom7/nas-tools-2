import { useEffect, useState } from "react";
import { ApiError, getSystemMessages, type SystemMessage } from "../api/client";

export function SystemMessageCenter({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [messages, setMessages] = useState<SystemMessage[]>();
  const [error, setError] = useState("");

  useEffect(() => {
    setMessages(undefined);
    setError("");
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function refresh() {
      try {
        const items = await getSystemMessages(token, controller.signal);
        if (controller.signal.aborted) return;
        setMessages(items);
        setError("");
      } catch (err) {
        if (controller.signal.aborted) return;
        if (err instanceof ApiError && err.code === 401) {
          onSessionExpired();
          return;
        }
        setError("系统消息暂时不可用");
      }
      timer = setTimeout(refresh, 3000);
    }
    void refresh();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [token, onSessionExpired]);

  return <section className="notification-receipt-panel" aria-label="系统消息">
    <h1>系统消息</h1>
    {error && <p role="alert">{error}</p>}
    {!error && !messages && <p>正在读取系统消息…</p>}
    {messages?.length === 0 && <p>暂无系统消息</p>}
    {messages?.map((item) => <article key={item.id}>
      <h3>{item.title}</h3><time>{item.time}</time><p style={{ whiteSpace: "pre-wrap" }}>{item.content}</p>
    </article>)}
  </section>;
}
