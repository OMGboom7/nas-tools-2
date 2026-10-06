import { useEffect, useMemo, useState, type FormEvent } from "react";
import { ApiError, getNotification, getNotificationOptions, saveNotification, type AuthSession, type NotificationChannel, type NotificationChannelOption, type NotificationDetail, type NotificationFieldOption, type NotificationInput, type NotificationOptions } from "../api/client";

type NotificationEditorProps = {
  session: AuthSession;
  channel?: NotificationChannel;
  onClose: () => void;
  onSaved: () => void;
  onSessionExpired: () => void;
};

type ConfigValues = Record<string, string | boolean>;
type EditorState = { name: string; type: string; enabled: boolean; interactive: boolean; events: string[]; config: ConfigValues; clearConfig: string[] };

function defaultValue(field: NotificationFieldOption): string | boolean {
  if (field.type === "switch") return field.default === true || field.default === 1 || field.default === "1";
  return field.default == null ? "" : String(field.default);
}

function createState(options: NotificationOptions, detail?: NotificationDetail): EditorState {
  const type = detail?.type || options.channels[0]?.type || "";
  const schema = options.channels.find((channel) => channel.type === type);
  const config: ConfigValues = {};
  for (const field of schema?.fields || []) {
    if (field.writeOnly) config[field.key] = "";
    else if (detail && Object.hasOwn(detail.config, field.key)) {
      const value = detail.config[field.key];
      config[field.key] = field.type === "switch" ? value === true || value === 1 || value === "1" : String(value ?? "");
    } else config[field.key] = defaultValue(field);
  }
  return {
    name: detail?.name || "", type, enabled: detail?.enabled ?? true,
    interactive: detail?.interactive ?? false,
    events: detail?.events || options.events.map((event) => event.id), config, clearConfig: [],
  };
}

export function NotificationEditor({ session, channel, onClose, onSaved, onSessionExpired }: NotificationEditorProps) {
  const [options, setOptions] = useState<NotificationOptions>();
  const [detail, setDetail] = useState<NotificationDetail>();
  const [input, setInput] = useState<EditorState>();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    const request = channel
      ? Promise.all([getNotificationOptions(session.token), getNotification(session.token, channel.id)])
      : getNotificationOptions(session.token).then((value) => [value, undefined] as const);
    request.then(([nextOptions, nextDetail]) => {
      if (!active) return;
      setOptions(nextOptions);
      setDetail(nextDetail);
      setInput(createState(nextOptions, nextDetail));
      setLoading(false);
    }).catch((reason) => {
      if (!active) return;
      if (reason instanceof ApiError && reason.code === 401) return onSessionExpired();
      setError(reason instanceof Error ? reason.message : "通知渠道设置加载失败");
      setLoading(false);
    });
    return () => { active = false; };
  }, [channel?.id, session.token]);

  const schema = useMemo(() => options?.channels.find((item) => item.type === input?.type), [input?.type, options]);
  const configured = useMemo(() => new Set(detail && detail.type === input?.type ? detail.configuredFields : []), [detail, input?.type]);
  const cleared = useMemo(() => new Set(input?.clearConfig || []), [input?.clearConfig]);

  function patch<K extends keyof EditorState>(key: K, value: EditorState[K]) {
    setInput((current) => current ? { ...current, [key]: value } : current);
  }

  function changeType(type: string) {
    if (!options || channel) return;
    const nextSchema = options.channels.find((item) => item.type === type);
    const config: ConfigValues = {};
    for (const field of nextSchema?.fields || []) config[field.key] = defaultValue(field);
    setInput((current) => current ? { ...current, type, interactive: nextSchema?.canInteract ? current.interactive : false, config, clearConfig: [] } : current);
  }

  function patchConfig(key: string, value: string | boolean) {
    setInput((current) => current ? { ...current, config: { ...current.config, [key]: value }, clearConfig: current.clearConfig.filter((item) => item !== key) } : current);
  }

  function toggleClear(key: string, value: boolean) {
    setInput((current) => current ? { ...current, clearConfig: value ? [...current.clearConfig, key] : current.clearConfig.filter((item) => item !== key), config: value ? { ...current.config, [key]: "" } : current.config } : current);
  }

  function toggleEvent(id: string) {
    if (!input) return;
    patch("events", input.events.includes(id) ? input.events.filter((item) => item !== id) : [...input.events, id]);
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!input || !schema || !options) return;
    setError("");
    if (!input.name.trim()) return setError("请输入通知渠道名称");
    for (const field of schema.fields) {
      if (!field.required) continue;
      if (field.writeOnly) {
        if (cleared.has(field.key) || (!String(input.config[field.key] || "").trim() && !configured.has(field.key))) return setError(`请填写${field.title}`);
      } else if (field.type !== "switch" && !String(input.config[field.key] || "").trim()) return setError(`请选择或填写${field.title}`);
    }
    const config: Record<string, string | boolean> = {};
    for (const field of schema.fields) {
      const value = input.config[field.key];
      if (field.writeOnly) {
        if (typeof value === "string" && value !== "") config[field.key] = field.type === "textarea" ? value : value.trim();
      } else config[field.key] = field.type === "switch" ? Boolean(value) : String(value ?? "");
    }
    const payload: NotificationInput = {
      name: input.name.trim(), type: input.type, enabled: input.enabled,
      interactive: schema.canInteract && input.interactive, events: input.events,
      config, clearConfig: input.clearConfig,
    };
    setSaving(true);
    try {
      await saveNotification(session.token, payload, channel?.id);
      onSaved();
    } catch (reason) {
      if (reason instanceof ApiError && reason.code === 401) return onSessionExpired();
      setError(reason instanceof Error ? reason.message : "通知渠道保存失败");
    } finally {
      setSaving(false);
    }
  }

  return <div className="editor-backdrop" role="presentation"><form className="service-editor notification-editor" aria-label={channel ? `编辑通知渠道 ${channel.name}` : "新增通知渠道"} onSubmit={(event) => void submit(event)}>
    <header><div><p className="eyebrow">CHANNEL SETTINGS</p><h2>{channel ? "编辑通知渠道" : "新增通知渠道"}</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
    {loading ? <div className="site-editor-loading">正在读取通知渠道设置…</div> : !input || !options || !schema ? <div className="form-error" role="alert">{error || "没有可用的通知渠道类型"}</div> : <>
      {error && <div className="form-error" role="alert">{error}</div>}
      <div className="editor-grid"><label>名称<input required maxLength={80} value={input.name} onChange={(event) => patch("name", event.target.value)} placeholder="例如 家庭通知" /></label>
        <label>渠道类型<select value={input.type} disabled={Boolean(channel)} onChange={(event) => changeType(event.target.value)}>{options.channels.map((item) => <option key={item.type} value={item.type}>{item.name}</option>)}</select></label></div>

      <section className="notification-config-fields" aria-labelledby="notification-config-title"><div className="secret-fields__heading"><div><h3 id="notification-config-title">渠道配置</h3><p>文本和模板均为只写字段；留空表示保留已保存内容。</p></div><span>安全配置</span></div>
        <div className="notification-field-grid">{schema.fields.map((field) => <NotificationField key={field.key} field={field} value={input.config[field.key]} configured={configured.has(field.key)} clear={cleared.has(field.key)} onValue={(value) => patchConfig(field.key, value)} onClear={(value) => toggleClear(field.key, value)} />)}</div></section>

      <fieldset className="option-checks service-option-checks"><legend>运行状态</legend><label><input type="checkbox" checked={input.enabled} onChange={(event) => patch("enabled", event.target.checked)} />启用通知</label>
        {schema.canInteract && <label><input type="checkbox" checked={input.interactive} onChange={(event) => patch("interactive", event.target.checked)} />启用交互</label>}</fieldset>

      <fieldset className="notification-event-options"><legend>接收事件 <button type="button" onClick={() => patch("events", input.events.length === options.events.length ? [] : options.events.map((item) => item.id))}>{input.events.length === options.events.length ? "取消全选" : "全选"}</button></legend>
        <div>{options.events.map((item) => <label key={item.id}><input type="checkbox" checked={input.events.includes(item.id)} onChange={() => toggleEvent(item.id)} />{item.label}</label>)}</div></fieldset>
    </>}
    <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={loading || saving || !input}>{saving ? "保存中…" : "保存通知渠道"}</button></footer>
  </form></div>;
}

function NotificationField({ field, value, configured, clear, onValue, onClear }: { field: NotificationFieldOption; value?: string | boolean; configured: boolean; clear: boolean; onValue: (value: string | boolean) => void; onClear: (value: boolean) => void }) {
  if (field.type === "switch") return <label className="notification-switch-field"><span>{field.title}{field.required && <b>*</b>}</span><input type="checkbox" checked={Boolean(value)} onChange={(event) => onValue(event.target.checked)} />{field.tooltip && <small>{field.tooltip}</small>}</label>;
  if (field.type === "select") return <label><span>{field.title}{field.required && <b>*</b>}</span><select value={String(value ?? "")} onChange={(event) => onValue(event.target.value)}>{!field.required && <option value="">未设置</option>}{field.options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select>{field.tooltip && <small>{field.tooltip}</small>}</label>;
  return <div className={`notification-config-field ${field.type === "textarea" ? "notification-field-wide" : ""}`}><label><span>{field.title}{field.required && <b>*</b>}{configured && !clear && <em>已配置</em>}</span>
    {field.type === "textarea" ? <textarea rows={7} disabled={clear} value={String(value ?? "")} onChange={(event) => onValue(event.target.value)} placeholder={configured ? "留空以保留现有内容" : field.placeholder} /> : <input type="password" disabled={clear} value={String(value ?? "")} onChange={(event) => onValue(event.target.value)} placeholder={configured ? "留空以保留现有内容" : field.placeholder} autoComplete="new-password" />}</label>
    {field.tooltip && <small>{field.tooltip}</small>}{configured && <label className="notification-clear-field"><input type="checkbox" checked={clear} onChange={(event) => onClear(event.target.checked)} />清除已保存内容</label>}</div>;
}
