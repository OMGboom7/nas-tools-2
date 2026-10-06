import { useEffect, useMemo, useState } from "react";
import { ApiError, deleteNotification, getNotifications, testNotification, updateNotificationStatus, type AuthSession, type NotificationChannel, type NotificationsData, type NotificationTestResult } from "../api/client";
import { NotificationEditor } from "../components/NotificationEditor";
import { CustomMessageComposer } from "../components/CustomMessageComposer";
import { WorkspaceLayout } from "../components/WorkspaceLayout";

type NotificationsPageProps = {
  session: AuthSession;
  currentPath: string;
  onNavigate: (path: string) => void;
  onLogout: () => Promise<void>;
  onSessionExpired: () => void;
};

type PageState = { kind: "loading" } | { kind: "ready"; data: NotificationsData } | { kind: "error"; message: string };
type TestState = NotificationTestResult | { pending: true };

export function NotificationsPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: NotificationsPageProps) {
  const [state, setState] = useState<PageState>({ kind: "loading" });
  const [query, setQuery] = useState("");
  const [type, setType] = useState("all");
  const [status, setStatus] = useState("all");
  const [notice, setNotice] = useState("");
  const [tests, setTests] = useState<Record<string, TestState>>({});
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [deleting, setDeleting] = useState<NotificationChannel>();
  const [editing, setEditing] = useState<NotificationChannel | null>();
  const [composing, setComposing] = useState(false);
  const [deleteText, setDeleteText] = useState("");

  async function load(quiet = false) {
    if (!quiet) setState({ kind: "loading" });
    try {
      const data = await getNotifications(session.token);
      setState({ kind: "ready", data });
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      const message = error instanceof Error ? error.message : "通知渠道加载失败";
      if (quiet) setNotice(`错误：${message}`); else setState({ kind: "error", message });
    }
  }

  useEffect(() => { void load(); }, [session.token]);

  const items = state.kind === "ready" ? state.data.items : [];
  const types = useMemo(() => Array.from(new Map(items.map((item) => [item.type, item.typeLabel])).entries()), [items]);
  const filtered = useMemo(() => items.filter((item) => {
    const needle = query.trim().toLocaleLowerCase();
    const queryMatch = !needle || `${item.name} ${item.typeLabel}`.toLocaleLowerCase().includes(needle);
    const typeMatch = type === "all" || item.type === type;
    const statusMatch = status === "all" || (status === "enabled" && item.enabled) || (status === "interactive" && item.interactive) || (status === "disabled" && !item.enabled);
    return queryMatch && typeMatch && statusMatch;
  }), [items, query, status, type]);

  async function setChannelStatus(channel: NotificationChannel, field: "enabled" | "interactive", value: boolean) {
    const key = `${channel.id}:${field}`;
    setBusy((current) => ({ ...current, [key]: true }));
    setNotice("");
    try {
      await updateNotificationStatus(session.token, channel.id, field === "enabled" ? { enabled: value } : { interactive: value });
      await load(true);
      setNotice(`已${value ? "开启" : "关闭"}“${channel.name}”的${field === "enabled" ? "通知" : "交互"}能力。`);
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      setNotice(`错误：${error instanceof Error ? error.message : "通知状态更新失败"}`);
    } finally {
      setBusy((current) => ({ ...current, [key]: false }));
    }
  }

  async function runTest(channel: NotificationChannel) {
    setTests((current) => ({ ...current, [channel.id]: { pending: true } }));
    try {
      const result = await testNotification(session.token, channel.id);
      setTests((current) => ({ ...current, [channel.id]: result }));
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      setTests((current) => ({ ...current, [channel.id]: { id: channel.id, ok: false, duration: 0, message: error instanceof Error ? error.message : "测试失败" } }));
    }
  }

  async function confirmDelete() {
    if (!deleting || deleteText !== deleting.name) return;
    setBusy((current) => ({ ...current, [`${deleting.id}:delete`]: true }));
    try {
      await deleteNotification(session.token, deleting.id);
      const name = deleting.name;
      setDeleting(undefined);
      setDeleteText("");
      await load(true);
      setNotice(`已删除通知渠道“${name}”。`);
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      setNotice(`错误：${error instanceof Error ? error.message : "通知渠道删除失败"}`);
    } finally {
      if (deleting) setBusy((current) => ({ ...current, [`${deleting.id}:delete`]: false }));
    }
  }

  async function saved() {
    setEditing(undefined);
    await load(true);
    setNotice("通知渠道已保存。文本凭据仍仅保存在服务器端。");
  }

  function sent(count: number) {
    setComposing(false);
    setNotice(`自定义消息已发送到 ${count} 个通知渠道。`);
  }

  const enabled = items.filter((item) => item.enabled).length;
  const interactive = items.filter((item) => item.interactive).length;

  return <WorkspaceLayout user={session.user} currentPath={currentPath} section="系统设置" page="消息通知"
    onNavigate={onNavigate} onLogout={onLogout}>
    <div className="notifications-page">
      <header className="services-heading"><div><p className="eyebrow">NOTIFICATION CHANNELS</p><h1>消息通知</h1><p className="summary">集中查看通知渠道、接收事件和交互状态。已保存凭据只在服务器端用于连接测试，不会返回浏览器。</p></div>
        <div className="services-heading__actions"><button className="secondary-button" type="button" onClick={() => void load(true)}>刷新</button><button className="secondary-button" type="button" onClick={() => setComposing(true)}>发送自定义消息</button><button className="primary-button" type="button" onClick={() => setEditing(null)}>新增通知渠道</button></div></header>

      {notice && <div className={notice.startsWith("错误：") ? "data-warning" : "data-success"} role="status">{notice}</div>}
      {state.kind === "loading" && <div className="notification-skeleton"><span /><span /><span /></div>}
      {state.kind === "error" && <section className="empty-state"><span className="empty-state__mark">!</span><h2>通知渠道暂时不可用</h2><p>{state.message}</p><button className="secondary-button" type="button" onClick={() => void load()}>重新加载</button></section>}
      {state.kind === "ready" && <>
        <section className="service-stats" aria-label="通知概览"><div><span>通知渠道</span><strong>{items.length}</strong><small>个</small></div><div><span>当前启用</span><strong>{enabled}</strong><small>个</small></div><div><span>交互渠道</span><strong>{interactive}</strong><small>个</small></div></section>
        <div className="notification-toolbar"><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索名称或渠道类型" aria-label="搜索通知渠道" />
          <select value={type} onChange={(event) => setType(event.target.value)} aria-label="按渠道类型筛选"><option value="all">全部类型</option>{types.map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select>
          <select value={status} onChange={(event) => setStatus(event.target.value)} aria-label="按状态筛选"><option value="all">全部状态</option><option value="enabled">已启用</option><option value="interactive">可交互</option><option value="disabled">已停用</option></select><span>显示 {filtered.length} / {items.length}</span></div>
        {filtered.length === 0 ? <div className="inline-empty">没有符合当前筛选条件的通知渠道。</div> : <section className="notification-grid" aria-label="通知渠道列表">{filtered.map((channel) => <NotificationCard key={channel.id} channel={channel} test={tests[channel.id]} busy={busy} onStatus={setChannelStatus} onTest={runTest} onEdit={() => setEditing(channel)} onDelete={() => { setDeleteText(""); setDeleting(channel); }} />)}</section>}
      </>}
      {editing !== undefined && <NotificationEditor session={session} channel={editing || undefined} onClose={() => setEditing(undefined)} onSaved={() => void saved()} onSessionExpired={onSessionExpired} />}
      {composing && <CustomMessageComposer session={session} channels={items} onClose={() => setComposing(false)} onSent={sent} onSessionExpired={onSessionExpired} />}
      {deleting && <DeleteNotificationDialog channel={deleting} value={deleteText} busy={Boolean(busy[`${deleting.id}:delete`])} onChange={setDeleteText} onClose={() => setDeleting(undefined)} onConfirm={() => void confirmDelete()} />}
    </div>
  </WorkspaceLayout>;
}

function NotificationCard({ channel, test, busy, onStatus, onTest, onEdit, onDelete }: { channel: NotificationChannel; test?: TestState; busy: Record<string, boolean>; onStatus: (channel: NotificationChannel, field: "enabled" | "interactive", value: boolean) => Promise<void>; onTest: (channel: NotificationChannel) => Promise<void>; onEdit: () => void; onDelete: () => void }) {
  const pending = Boolean(test && "pending" in test);
  const result = test && !("pending" in test) ? test : undefined;
  return <article className={`notification-card ${channel.enabled ? "is-enabled" : ""}`}>
    <header><span className="notification-card__mark">◉</span><div><small>{channel.typeLabel}</small><h2>{channel.name}</h2></div><span className={`service-state ${channel.enabled ? "is-enabled" : ""}`}>{channel.enabled ? "已启用" : "已停用"}</span></header>
    <div className="notification-events"><span>接收事件</span><div>{channel.switchLabels.length > 0 ? channel.switchLabels.map((label) => <em key={label}>{label}</em>) : <i>尚未选择事件</i>}</div></div>
    <div className="notification-controls"><button type="button" className={channel.enabled ? "is-on" : ""} disabled={busy[`${channel.id}:enabled`]} onClick={() => void onStatus(channel, "enabled", !channel.enabled)}><span />通知 {channel.enabled ? "开" : "关"}</button>
      {channel.canInteract && <button type="button" className={channel.interactive ? "is-on" : ""} disabled={busy[`${channel.id}:interactive`]} onClick={() => void onStatus(channel, "interactive", !channel.interactive)}><span />交互 {channel.interactive ? "开" : "关"}</button>}</div>
    <footer><div className={`service-test-result ${result ? result.ok ? "is-success" : "is-failed" : ""}`} aria-live="polite"><i />{pending ? "正在发送…" : result ? result.ok ? "测试成功" : "测试失败" : channel.configured ? "凭据已配置" : "尚未配置"}{result && result.duration > 0 && <small>{result.duration} ms</small>}{result && <em>{result.message}</em>}</div>
      <div className="service-card__actions"><button type="button" onClick={onEdit}>编辑</button><button type="button" className="is-danger" onClick={onDelete}>删除</button><button type="button" disabled={!channel.configured || pending} onClick={() => void onTest(channel)}>{pending ? "发送中" : "发送测试"}</button></div></footer>
  </article>;
}

function DeleteNotificationDialog({ channel, value, busy, onChange, onClose, onConfirm }: { channel: NotificationChannel; value: string; busy: boolean; onChange: (value: string) => void; onClose: () => void; onConfirm: () => void }) {
  return <div className="editor-backdrop" role="presentation"><form className="delete-site-dialog" aria-label={`删除通知渠道 ${channel.name}`} onSubmit={(event) => { event.preventDefault(); onConfirm(); }}>
    <header><div><p className="eyebrow">DELETE CHANNEL</p><h2>删除通知渠道</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
    <div className="delete-warning"><strong>此操作会删除“{channel.name}”及其保存的凭据。</strong><p>后续消息将不会再通过该渠道发送，已有消息记录不受影响。</p></div>
    <label>请输入渠道名称 <strong>{channel.name}</strong> 以确认<input autoFocus value={value} onChange={(event) => onChange(event.target.value)} autoComplete="off" /></label>
    <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="danger-button" disabled={busy || value !== channel.name}>{busy ? "删除中…" : "确认删除"}</button></footer>
  </form></div>;
}
