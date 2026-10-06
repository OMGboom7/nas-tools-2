import { useMemo, useState, type FormEvent } from "react";
import { ApiError, sendCustomMessage, type AuthSession, type NotificationChannel } from "../api/client";

type CustomMessageComposerProps = {
  session: AuthSession;
  channels: NotificationChannel[];
  onClose: () => void;
  onSent: (count: number) => void;
  onSessionExpired: () => void;
};

export function CustomMessageComposer({ session, channels, onClose, onSent, onSessionExpired }: CustomMessageComposerProps) {
  const available = useMemo(() => channels.filter((channel) => channel.enabled && channel.configured), [channels]);
  const [title, setTitle] = useState("");
  const [text, setText] = useState("");
  const [image, setImage] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [sending, setSending] = useState(false);
  const [error, setError] = useState("");

  function toggleChannel(id: string) {
    setSelected((current) => current.includes(id) ? current.filter((item) => item !== id) : [...current, id]);
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError("");
    if (!title.trim()) return setError("请输入消息标题");
    if (selected.length === 0) return setError("请至少选择一个通知渠道");
    if (image.trim()) {
      try {
        const parsed = new URL(image.trim());
        if (parsed.protocol !== "http:" && parsed.protocol !== "https:") throw new Error();
      } catch {
        return setError("图片地址必须是有效的 HTTP 或 HTTPS URL");
      }
    }
    setSending(true);
    try {
      const result = await sendCustomMessage(session.token, { title: title.trim(), text, image: image.trim(), channelIds: selected });
      onSent(result.channelCount);
    } catch (reason) {
      if (reason instanceof ApiError && reason.code === 401) return onSessionExpired();
      setError(reason instanceof Error ? reason.message : "自定义消息发送失败");
    } finally {
      setSending(false);
    }
  }

  const allSelected = available.length > 0 && selected.length === available.length;
  return <div className="editor-backdrop" role="presentation"><form className="service-editor custom-message-composer" aria-label="发送自定义消息" onSubmit={(event) => void submit(event)}>
    <header><div><p className="eyebrow">CUSTOM MESSAGE</p><h2>发送自定义消息</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
    {error && <div className="form-error" role="alert">{error}</div>}
    <div className="custom-message-fields"><label>标题 <strong>*</strong><input autoFocus required maxLength={200} value={title} onChange={(event) => setTitle(event.target.value)} placeholder="例如 今晚入库完成" /></label>
      <label>图片地址<input type="url" maxLength={2048} value={image} onChange={(event) => setImage(event.target.value)} placeholder="https://example.com/poster.jpg" /></label>
      <label>内容<textarea rows={6} maxLength={10000} value={text} onChange={(event) => setText(event.target.value)} placeholder="输入要发送的消息内容" /></label></div>
    <fieldset className="custom-message-channels"><legend><span>发送渠道</span>{available.length > 0 && <button type="button" onClick={() => setSelected(allSelected ? [] : available.map((channel) => channel.id))}>{allSelected ? "取消全选" : "全选"}</button>}</legend>
      {available.length === 0 ? <p>没有已启用且已配置的通知渠道，请先完成渠道配置。</p> : <div>{available.map((channel) => <label key={channel.id}><input type="checkbox" checked={selected.includes(channel.id)} onChange={() => toggleChannel(channel.id)} /><span><strong>{channel.name}</strong><small>{channel.typeLabel}</small></span></label>)}</div>}</fieldset>
    <div className="editor-note">发送后会写入系统消息记录，并立即投递到所选渠道。停用或未完成配置的渠道不会出现在列表中。</div>
    <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={sending || available.length === 0}>{sending ? "发送中…" : `发送到 ${selected.length} 个渠道`}</button></footer>
  </form></div>;
}
