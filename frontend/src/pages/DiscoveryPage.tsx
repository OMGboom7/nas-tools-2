import { useEffect, useState } from "react";
import { addDefaultSubscription, ApiError, getDiscovery, type AuthSession, type DiscoveryData, type DiscoveryMedia } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";

type DiscoveryPageProps = {
  session: AuthSession;
  currentPath: string;
  onNavigate: (path: string) => void;
  onLogout: () => Promise<void>;
  onSessionExpired: () => void;
};

const categories = [
  ["trending", "本周趋势"], ["popular-movies", "热门电影"], ["popular-series", "热门剧集"],
  ["new-movies", "最新电影"], ["new-series", "最新剧集"],
] as const;

type PageState = { kind: "loading" } | { kind: "ready"; data: DiscoveryData } | { kind: "error"; message: string };

export function DiscoveryPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: DiscoveryPageProps) {
  const [category, setCategory] = useState("trending");
  const [page, setPage] = useState(1);
  const [state, setState] = useState<PageState>({ kind: "loading" });
  const [subscribing, setSubscribing] = useState<Record<string, boolean>>({});
  const [notice, setNotice] = useState("");

  useEffect(() => {
    let active = true;
    setState({ kind: "loading" });
    setNotice("");
    getDiscovery(session.token, category, page).then((data) => {
      if (active) setState({ kind: "ready", data });
    }).catch((error) => {
      if (!active) return;
      if (error instanceof ApiError && error.code === 401) onSessionExpired();
      else setState({ kind: "error", message: error instanceof Error ? error.message : "探索内容加载失败" });
    });
    return () => { active = false; };
  }, [category, page, session.token]);

  function chooseCategory(next: string) {
    setCategory(next);
    setPage(1);
  }

  async function subscribe(media: DiscoveryMedia) {
    const key = `${media.type}-${media.id}`;
    if (media.type === "TV" && !window.confirm(`“${media.title}”将按默认规则订阅最新季，确定继续吗？`)) return;
    setSubscribing((current) => ({ ...current, [key]: true }));
    setNotice("");
    try {
      await addDefaultSubscription(session.token, media);
      setState((current) => current.kind === "ready" ? {
        kind: "ready", data: { ...current.data, items: current.data.items.map((item) => item.id === media.id && item.type === media.type ? { ...item, subscribed: true } : item) },
      } : current);
      setNotice(`已订阅“${media.title}”`);
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) onSessionExpired();
      else setNotice(error instanceof Error ? error.message : "添加订阅失败");
    } finally {
      setSubscribing((current) => ({ ...current, [key]: false }));
    }
  }

  const canSubscribe = session.user.userpris.includes("订阅管理");
  return <WorkspaceLayout user={session.user} currentPath={currentPath} section="探索" page="发现"
    onNavigate={onNavigate} onLogout={onLogout}>
    <div className="discovery-page">
      <header className="discovery-heading"><div><p className="eyebrow">DISCOVERY</p><h1>发现下一部</h1><p className="summary">浏览趋势、热门和最新影视，并使用已有默认规则直接加入订阅。</p></div></header>
      <div className="discovery-categories" aria-label="探索分类">{categories.map(([value, label]) => <button key={value} type="button"
        className={category === value ? "is-active" : ""} aria-pressed={category === value} onClick={() => chooseCategory(value)}>{label}</button>)}</div>
      {notice && <div className="subscription-notice is-success" role="status">{notice}</div>}
      {state.kind === "loading" && <DiscoverySkeleton />}
      {state.kind === "error" && <section className="empty-state"><span className="empty-state__mark">!</span><h2>探索内容暂时不可用</h2><p>{state.message}</p></section>}
      {state.kind === "ready" && <>
        {state.data.items.length === 0 ? <div className="inline-empty discovery-empty">这一页没有更多内容。</div> : <div className="discovery-grid">{state.data.items.map((media) => {
          const key = `${media.type}-${media.id}`;
          return <article className="discovery-card" key={key}>
            <div className="discovery-poster"><strong>{media.title.slice(0, 1)}</strong>{media.image && <img src={media.image} alt={`${media.title} 海报`} loading="lazy" />}</div>
            <div className="discovery-card__body"><div className="discovery-meta"><span>{media.type === "MOV" ? "电影" : "剧集"}</span>{media.vote && <strong>{media.vote} 分</strong>}</div>
              <h2>{media.link ? <a href={media.link} target="_blank" rel="noreferrer">{media.title}</a> : media.title}</h2><p>{media.year || "年份未知"}</p><small>{media.overview || "暂无简介"}</small>
              {canSubscribe && <button type="button" className={media.subscribed ? "is-subscribed" : ""} disabled={media.subscribed || subscribing[key]} onClick={() => void subscribe(media)}>
                {media.subscribed ? "已订阅" : subscribing[key] ? "订阅中…" : "加入订阅"}
              </button>}
            </div>
          </article>;
        })}</div>}
        <div className="discovery-pager"><button className="secondary-button" type="button" disabled={page <= 1} onClick={() => setPage((value) => value - 1)}>上一页</button><span>第 {page} 页</span><button className="secondary-button" type="button" disabled={state.data.items.length === 0} onClick={() => setPage((value) => value + 1)}>下一页</button></div>
      </>}
    </div>
  </WorkspaceLayout>;
}

function DiscoverySkeleton() {
  return <div className="discovery-skeleton">{Array.from({ length: 10 }, (_, index) => <span key={index} />)}</div>;
}
