import { useEffect, useMemo, useState } from "react";
import {
  ApiError,
  controlSubscription,
  controlSubscriptionHistory,
  getSubscriptionOptions,
  getSubscriptions,
  saveSubscription,
  type AuthSession,
  type SubscriptionHistory,
  type SubscriptionItem,
  type SubscriptionsData,
  type SubscriptionOptions,
  type SubscriptionInput,
} from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";
import { SubscriptionEditor } from "../components/SubscriptionEditor";

type SubscriptionsPageProps = {
  session: AuthSession;
  currentPath: string;
  onNavigate: (path: string) => void;
  onLogout: () => Promise<void>;
  onSessionExpired: () => void;
};

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; data: SubscriptionsData }
  | { kind: "error"; message: string };

export function SubscriptionsPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: SubscriptionsPageProps) {
  const [state, setState] = useState<PageState>({ kind: "loading" });
  const [mediaType, setMediaType] = useState<"MOV" | "TV">("MOV");
  const [view, setView] = useState<"active" | "history">("active");
  const [status, setStatus] = useState("");
  const [keyword, setKeyword] = useState("");
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [notice, setNotice] = useState<{ kind: "success" | "error"; message: string } | null>(null);
  const [editorItem, setEditorItem] = useState<SubscriptionItem | null | undefined>(undefined);
  const [options, setOptions] = useState<SubscriptionOptions | null>(null);

  async function load(showLoading = false) {
    if (showLoading) setState({ kind: "loading" });
    try {
      const data = await getSubscriptions(session.token);
      setState({ kind: "ready", data });
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      setState({ kind: "error", message: error instanceof Error ? error.message : "订阅数据加载失败" });
    }
  }

  useEffect(() => { void load(true); }, [session.token]);

  const filtered = useMemo(() => {
    if (state.kind !== "ready") return { items: [] as SubscriptionItem[], history: [] as SubscriptionHistory[] };
    const query = keyword.trim().toLocaleLowerCase("zh-CN");
    return {
      items: state.data.items.filter((item) => item.type === mediaType && (!status || item.state === status)
        && (!query || `${item.name} ${item.year} ${item.season} ${item.keyword}`.toLocaleLowerCase("zh-CN").includes(query))),
      history: state.data.history.filter((item) => item.type === mediaType
        && (!query || `${item.name} ${item.year} ${item.season}`.toLocaleLowerCase("zh-CN").includes(query))),
    };
  }, [state, mediaType, status, keyword]);

  async function handleItemAction(item: SubscriptionItem, action: "refresh" | "remove") {
    if (action === "remove" && !window.confirm(`确定删除“${item.name}”的订阅吗？删除后将停止后续搜索。`)) return;
    await runAction(`item-${item.type}-${item.id}`, async () => {
      await controlSubscription(session.token, item.type, item.id, action);
      setNotice({ kind: "success", message: action === "refresh" ? `已触发“${item.name}”搜索` : `已删除“${item.name}”订阅` });
    });
  }

  async function handleHistoryAction(item: SubscriptionHistory, action: "redo" | "remove") {
    if (action === "remove" && !window.confirm(`确定删除“${item.name}”的订阅历史吗？`)) return;
    await runAction(`history-${item.type}-${item.id}`, async () => {
      await controlSubscriptionHistory(session.token, item.type, item.id, action);
      setNotice({ kind: "success", message: action === "redo" ? `已重新订阅“${item.name}”` : `已删除“${item.name}”历史` });
    });
  }

  async function runAction(key: string, action: () => Promise<void>) {
    setBusy((current) => ({ ...current, [key]: true }));
    setNotice(null);
    try {
      await action();
      await load();
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      setNotice({ kind: "error", message: error instanceof Error ? error.message : "订阅操作失败" });
    } finally {
      setBusy((current) => ({ ...current, [key]: false }));
    }
  }

  async function openEditor(item: SubscriptionItem | null) {
    setNotice(null);
    try { if (!options) setOptions(await getSubscriptionOptions(session.token)); setEditorItem(item); }
    catch (error) { setNotice({ kind: "error", message: error instanceof Error ? error.message : "编辑选项加载失败" }); }
  }

  async function handleSave(input: SubscriptionInput) {
    await saveSubscription(session.token, input); setEditorItem(undefined); setNotice({ kind: "success", message: `已保存“${input.name}”` }); await load();
  }

  return (
    <WorkspaceLayout user={session.user} currentPath={currentPath} section="订阅管理" page={view === "active" ? "订阅" : "历史"}
      onNavigate={onNavigate} onLogout={onLogout}>
      <div className="subscriptions-page">
        <header className="subscriptions-heading">
          <div><p className="eyebrow">SUBSCRIPTIONS</p><h1>订阅管理</h1><p className="summary">跟踪电影和剧集订阅，手动触发搜索，并管理已完成的记录。</p></div>
          <div className="heading-actions"><button className="primary-button" type="button" onClick={() => void openEditor(null)}>新增订阅</button><button className="secondary-button" type="button" onClick={() => void load()} disabled={state.kind === "loading"}>刷新</button></div>
        </header>

        <div className="subscription-toolbar">
          <div className="segmented" aria-label="订阅类型">
            <button type="button" className={mediaType === "MOV" ? "is-active" : ""} aria-pressed={mediaType === "MOV"} onClick={() => setMediaType("MOV")}>电影</button>
            <button type="button" className={mediaType === "TV" ? "is-active" : ""} aria-pressed={mediaType === "TV"} onClick={() => setMediaType("TV")}>电视剧</button>
          </div>
          <div className="segmented" aria-label="订阅视图">
            <button type="button" className={view === "active" ? "is-active" : ""} aria-pressed={view === "active"} onClick={() => setView("active")}>进行中</button>
            <button type="button" className={view === "history" ? "is-active" : ""} aria-pressed={view === "history"} onClick={() => setView("history")}>历史</button>
          </div>
          <label className="subscription-search"><span>⌕</span><input value={keyword} onChange={(event) => setKeyword(event.target.value)} placeholder="筛选名称" aria-label="筛选订阅名称" /></label>
          {view === "active" && <select value={status} onChange={(event) => setStatus(event.target.value)} aria-label="按订阅状态筛选">
            <option value="">全部状态</option><option value="D">队列中</option><option value="S">正在搜索</option><option value="R">正在订阅</option><option value="F">完成</option>
          </select>}
        </div>

        {notice && <div className={`subscription-notice is-${notice.kind}`} role="status">{notice.message}</div>}
        {state.kind === "loading" && <SubscriptionSkeleton />}
        {state.kind === "error" && <section className="empty-state"><span className="empty-state__mark">!</span><h2>订阅数据暂时不可用</h2><p>{state.message}</p><button className="secondary-button" type="button" onClick={() => void load(true)}>重新加载</button></section>}
        {state.kind === "ready" && <>
          {state.data.warnings.length > 0 && <div className="data-warning">部分订阅数据暂时不可用，刷新后会重新获取。</div>}
          {view === "active" ? (
            <section className="subscription-section">
              <div className="section-heading"><div><p className="eyebrow">ACTIVE</p><h2>{mediaType === "MOV" ? "电影订阅" : "电视剧订阅"}</h2></div><span>{filtered.items.length} 项</span></div>
              {filtered.items.length === 0 ? <div className="inline-empty">当前筛选条件下没有订阅。</div> : <div className="subscription-grid">{filtered.items.map((item) => (
                <SubscriptionCard key={`${item.type}-${item.id}`} item={item} busy={busy[`item-${item.type}-${item.id}`]} onEdit={() => void openEditor(item)} onAction={(action) => void handleItemAction(item, action)} />
              ))}</div>}
            </section>
          ) : (
            <section className="subscription-section">
              <div className="section-heading"><div><p className="eyebrow">HISTORY</p><h2>{mediaType === "MOV" ? "电影订阅历史" : "电视剧订阅历史"}</h2></div><span>{filtered.history.length} 条</span></div>
              {filtered.history.length === 0 ? <div className="inline-empty">当前没有已完成的订阅记录。</div> : <div className="subscription-history-list">{filtered.history.map((item) => (
                <HistoryRow key={`${item.type}-${item.id}`} item={item} busy={busy[`history-${item.type}-${item.id}`]} onAction={(action) => void handleHistoryAction(item, action)} />
              ))}</div>}
            </section>
          )}
        </>}
        {editorItem !== undefined && options && <SubscriptionEditor item={editorItem || undefined} options={options} onClose={() => setEditorItem(undefined)} onSave={handleSave} />}
      </div>
    </WorkspaceLayout>
  );
}

function SubscriptionCard({ item, busy, onEdit, onAction }: { item: SubscriptionItem; busy?: boolean; onEdit: () => void; onAction: (action: "refresh" | "remove") => void }) {
  const tags = [item.overEdition ? "洗版" : "", item.quality, item.resolution, item.releaseGroup, ...item.rssSites, ...item.searchSites].filter(Boolean);
  return <article className="subscription-card">
    <div className="subscription-art"><strong>{item.name.slice(0, 1)}</strong>{item.image && <img src={item.image} alt={`${item.name} 海报`} loading="lazy" />}</div>
    <div className="subscription-card__body">
      <div className="subscription-title"><div><h3>{item.name}</h3><p>{[item.year, item.season !== "S00" ? item.season : ""].filter(Boolean).join(" · ")}</p></div><span className={`subscription-state state-${item.state.toLowerCase()}`}>{item.stateLabel}</span></div>
      {item.type === "TV" && item.total > 0 && <><div className="subscription-progress"><span style={{ width: `${item.progress}%` }} /></div><small>已获取 {item.total - item.remaining}/{item.total} 集</small></>}
      {tags.length > 0 && <div className="subscription-tags">{tags.map((tag, index) => <span key={`${tag}-${index}`}>{tag}</span>)}</div>}
      <div className="subscription-actions"><button type="button" disabled={busy} onClick={onEdit}>编辑</button><button type="button" disabled={busy} onClick={() => onAction("refresh")}>{busy ? "处理中…" : "立即搜索"}</button><button type="button" className="is-danger" disabled={busy} onClick={() => onAction("remove")}>删除</button></div>
    </div>
  </article>;
}

function HistoryRow({ item, busy, onAction }: { item: SubscriptionHistory; busy?: boolean; onAction: (action: "redo" | "remove") => void }) {
  return <article className="subscription-history-row">
    <div className="history-art"><strong>{item.name.slice(0, 1)}</strong>{item.image && <img src={item.image} alt="" loading="lazy" />}</div>
    <div><h3>{item.name} {item.year && `(${item.year})`} {item.season}</h3><p>{item.overview || "没有简介"}</p><small>{item.finishTime || "完成时间未知"}</small></div>
    <div className="subscription-actions"><button type="button" disabled={busy} onClick={() => onAction("redo")}>{busy ? "处理中…" : "重新订阅"}</button><button type="button" className="is-danger" disabled={busy} onClick={() => onAction("remove")}>删除历史</button></div>
  </article>;
}

function SubscriptionSkeleton() {
  return <div className="subscription-skeleton"><span /><span /><span /><span /></div>;
}
