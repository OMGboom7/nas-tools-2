import { useEffect, useRef, useState } from "react";
import { ApiError, getMediaDetail, getMediaSeason, getRelatedMedia, getMediaPlayback, subscribeMediaDetail, type AuthSession, type MediaDetailKind, type MediaDisplayDetail, type MediaSeasonDetail, type DiscoveryData, type MediaPerson } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";
import { mediaCacheLabel } from "../utils/mediaCache";
import { isMediaSeasonSubscribed, mediaDetailPath } from "../utils/mediaDetail";

type Props = { session: AuthSession; kind: MediaDetailKind; id: string; onNavigate: (path: string) => void; onLogout: () => Promise<void>; onSessionExpired: () => void };
export function MediaDetailPage({ session, kind, id, onNavigate, onLogout, onSessionExpired }: Props) {
  const [detail, setDetail] = useState<MediaDisplayDetail | null>(null); const [loading, setLoading] = useState(true); const [error, setError] = useState(""); const [reload, setReload] = useState(0);
  const [season, setSeason] = useState<number | null>(null); const [episodePage, setEpisodePage] = useState(1); const [episodes, setEpisodes] = useState<MediaSeasonDetail | null>(null); const [episodeError, setEpisodeError] = useState(""); const [episodeLoading, setEpisodeLoading] = useState(false);
  const [relation, setRelation] = useState<"similar" | "recommendations" | null>(null); const [relatedPage, setRelatedPage] = useState(1); const [related, setRelated] = useState<DiscoveryData | null>(null); const [relatedError, setRelatedError] = useState("");
  const [notice, setNotice] = useState(""); const [submitting, setSubmitting] = useState(false); const [playback, setPlayback] = useState(""); const [playLoading, setPlayLoading] = useState(false); const generation = useRef(0); const submittingRef = useRef(false); const playingRef = useRef(false);
  useEffect(() => {
    const current = ++generation.current; const controller = new AbortController(); setDetail(null); setLoading(true); setError(""); setSeason(null); setEpisodePage(1); setEpisodes(null); setRelation(null); setRelated(null); setNotice(""); setPlayback(""); setSubmitting(false); setPlayLoading(false); submittingRef.current = false; playingRef.current = false;
    getMediaDetail(session.token, kind, id, controller.signal).then((d) => { if (current !== generation.current) return; setDetail(d); const chosen = d.seasons.find((s) => s.number > 0) || d.seasons[0]; if (chosen) setSeason(chosen.number); }).catch((err) => { if (controller.signal.aborted || current !== generation.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "详情不可用"); }).finally(() => { if (current === generation.current) setLoading(false); });
    return () => { controller.abort(); generation.current++; };
  }, [session.token, kind, id, reload]);
  const resolvedID = detail?.id;
  const returnPath = session.user.userpris.includes("探索") ? "/discover" : session.user.userpris.includes("资源搜索") ? "/search" : session.user.userpris.includes("订阅管理") ? "/subscriptions" : "/";
  useEffect(() => {
    if (!resolvedID || kind !== "tv" || season === null) return;
    const controller = new AbortController(); const current = generation.current; setEpisodeLoading(true); setEpisodes(null); setEpisodeError("");
    getMediaSeason(session.token, resolvedID, season, episodePage, controller.signal).then((d) => { if (!controller.signal.aborted && current === generation.current) setEpisodes(d); }).catch((err) => { if (controller.signal.aborted || current !== generation.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setEpisodeError(err instanceof Error ? err.message : "季集不可用"); }).finally(() => { if (!controller.signal.aborted && current === generation.current) setEpisodeLoading(false); });
    return () => controller.abort();
  }, [resolvedID, season, episodePage, kind, session.token]);
  useEffect(() => {
    if (!resolvedID || !relation) return;
    const controller = new AbortController(); const current = generation.current; setRelated(null); setRelatedError("");
    getRelatedMedia(session.token, kind, resolvedID, relation, relatedPage, controller.signal).then((d) => { if (!controller.signal.aborted && current === generation.current) setRelated(d); }).catch((err) => { if (controller.signal.aborted || current !== generation.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setRelatedError(err instanceof Error ? err.message : "相关媒体不可用"); });
    return () => controller.abort();
  }, [resolvedID, relation, relatedPage, kind, session.token]);
  async function subscribe() {
    if (!detail || submittingRef.current || kind === "tv" && season === null) return;
    const chosen = kind === "tv" ? season! : undefined;
    if (!window.confirm(`按默认规则订阅“${detail.title}”${chosen === undefined ? "" : ` 第 ${chosen} 季`}？缓存标记不代表当前缺失，提交后由既有订阅流程处理。`)) return;
    const current = generation.current; submittingRef.current = true; setSubmitting(true); setNotice("");
    try { await subscribeMediaDetail(session.token, detail, chosen, true); if (current !== generation.current) return; setNotice("订阅已提交，正在只读核实记录。"); const fresh = await getMediaDetail(session.token, kind, id); if (current === generation.current) { setDetail(fresh); setNotice("订阅已提交，请以当前订阅记录为准。"); } }
    catch (err) { if (current !== generation.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setNotice(`${err instanceof Error ? err.message : "订阅响应未核实"}。请先只读刷新或查看订阅管理，不自动重试。`); }
    finally { if (current === generation.current) { submittingRef.current = false; setSubmitting(false); } }
  }
  async function lookupPlayback() {
    if (!detail || playingRef.current) return; const current = generation.current; playingRef.current = true; setPlayLoading(true); setPlayback(""); setNotice("");
    try { const link = await getMediaPlayback(session.token, kind, detail.id); if (current === generation.current) setPlayback(link); }
    catch (err) { if (current !== generation.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setNotice(err instanceof Error ? err.message : "播放入口不可用"); }
    finally { if (current === generation.current) { playingRef.current = false; setPlayLoading(false); } }
  }
  return <WorkspaceLayout user={session.user} currentPath={returnPath} section="媒体" page="详情" onNavigate={onNavigate} onLogout={onLogout}>
    <div className="media-detail-page"><button className="text-button" type="button" onClick={() => onNavigate(returnPath)}>返回媒体列表</button>{" "}<button className="text-button" type="button" disabled={loading || submitting || playLoading} onClick={() => setReload((n) => n + 1)}>只读刷新详情</button>
      {loading && <p role="status">正在读取媒体详情…</p>}{error && <p role="alert">{error}</p>}{notice && <p role="status" className="data-warning">{notice}</p>}
      {detail && <><header className="media-detail-hero" style={detail.backdrop ? { backgroundImage: `linear-gradient(90deg, #09111ff2, #09111fbc), url("${detail.backdrop.replace(/"/g, "%22")}")` } : undefined}>
        {detail.poster && <img className="media-detail-poster" src={detail.poster} alt={`${detail.title} 海报`} />}
        <div><p className="eyebrow">{kind === "movie" ? "MOVIE" : "TV SERIES"} · TMDB {detail.id}</p><h1>{detail.title}</h1><p>{detail.originalTitle} · {detail.year || "年份未知"} · {detail.vote} 分 · {detail.runtime === null ? "时长未知" : `${detail.runtime} 分钟`}</p><p>{detail.genres.join(" · ")}</p>{detail.tagline && <p>{detail.tagline}</p>}<p className="summary">{detail.overview || "暂无简介"}</p>
          <p>{mediaCacheLabel(detail.cache, detail.cachedPresence, kind === "tv")} {detail.cache.updated && `· ${detail.cache.updated}`}</p><p>展示快照非实时，不作为下种依据；不影响下面的明确订阅操作。</p>
          <a href={detail.link} target="_blank" rel="noreferrer">在 TMDB 查看</a>{detail.doubanLink && <> · <a href={detail.doubanLink} target="_blank" rel="noreferrer">豆瓣来源</a></>}
          <p><button type="button" disabled={playLoading} onClick={() => void lookupPlayback()}>{playLoading ? "正在实时核实…" : "查找当前播放入口"}</button>{playback && <> · <a href={playback} target="_blank" rel="noreferrer">在媒体服务器打开</a></>}</p>
        </div></header>
        {detail.warnings.map((warning) => <p key={warning} className="data-warning">{warning}</p>)}
        <section className="content-section"><h2>制作与发行</h2><dl className="media-detail-facts">{detail.facts.map((fact) => <div key={fact.label}><dt>{fact.label}</dt><dd>{fact.value}</dd></div>)}</dl></section>
        <section className="content-section"><h2>{kind === "tv" ? "季与集" : "订阅"}</h2>
          {!detail.subscriptionsKnown && <p className="data-warning">当前订阅状态无法核实，请先查看订阅管理。不会由缓存自动创建或取消订阅。</p>}
          {kind === "tv" && <div className="media-detail-seasons" aria-label="选择季">{detail.seasons.map((s) => <button key={s.number} type="button" aria-pressed={season === s.number} onClick={() => { setSeason(s.number); setEpisodePage(1); }}><strong>{s.name || `第 ${s.number} 季`}</strong><span>{s.episodeCount === null ? "集数未知" : `${s.episodeCount} 集`} · {s.airDate || "日期未知"}</span><small>{mediaCacheLabel(detail.cache, s.cachedPresence, true)}</small></button>)}</div>}
          {session.user.userpris.includes("订阅管理") && <p><button type="button" disabled={submitting || kind === "tv" && (season === null || season > 999) || isMediaSeasonSubscribed(detail, kind === "tv" ? season ?? undefined : undefined)} onClick={() => void subscribe()}>{submitting ? "提交中…" : isMediaSeasonSubscribed(detail, kind === "tv" ? season ?? undefined : undefined) ? "此范围已订阅" : kind === "tv" ? `明确订阅第 ${season ?? "—"} 季` : "加入订阅"}</button></p>}
          {detail.subscriptions.length > 0 && <p>现有订阅：{detail.subscriptions.map((ref) => `${ref.id}${ref.season ? ` / ${ref.season}` : ""}`).join("、")}</p>}
          {kind === "tv" && <>{episodeLoading && <p role="status">正在读取本季集列表…</p>}{episodeError && <p role="alert">{episodeError}</p>}{episodes && <><p>{episodes.name} · 总计 {episodes.total} 集；每页最多 100 集。未播/缺失日期保留为未知，不自动下种。</p><ul className="media-detail-episodes">{episodes.episodes.map((ep) => <li key={ep.id}>{ep.image && <img src={ep.image} alt="" loading="lazy" />}<div><h3>第 {ep.number} 集 · {ep.name || "标题未知"}</h3><p>{ep.airDate || "播出日期未知"} · {ep.runtime === null ? "时长未知" : `${ep.runtime} 分钟`} · {ep.vote} 分</p><p>{ep.overview || "暂无简介"}</p><small>{mediaCacheLabel(episodes.cache, ep.cachedPresence, false)}</small></div></li>)}</ul><button type="button" disabled={episodePage <= 1} onClick={() => setEpisodePage((n) => n - 1)}>上一页</button>{" "}<span>第 {episodePage} 页</span>{" "}<button type="button" disabled={episodePage * 100 >= episodes.total} onClick={() => setEpisodePage((n) => n + 1)}>下一页</button></>}{!detail.seasons.length && <p>上游尚未返回季信息。</p>}</>}
        </section>
        <People title={kind === "tv" ? "演职人员（TMDB 最近一季名单）" : "演职人员"} people={detail.cast} />
        <People title="制作人员" people={detail.crew} />
        <section className="content-section"><h2>继续发现</h2><button type="button" aria-pressed={relation === "similar"} onClick={() => { setRelation("similar"); setRelatedPage(1); }}>相似媒体</button>{" "}<button type="button" aria-pressed={relation === "recommendations"} onClick={() => { setRelation("recommendations"); setRelatedPage(1); }}>同类推荐</button>{relatedError && <p role="alert">{relatedError}</p>}{relation && !related && !relatedError && <p role="status">正在读取相关媒体…</p>}
          {related && <><div className="media-detail-related">{related.items.map((item) => { const path = mediaDetailPath(item.type, item.id); return <article key={`${item.type}-${item.id}`}>{item.image && <img src={item.image} alt="" loading="lazy" />}<h3>{path ? <button className="text-button" type="button" onClick={() => onNavigate(path)}>{item.title}</button> : item.title}</h3><p>{item.year} · {item.vote} 分</p><small>{mediaCacheLabel(related.cache, item.cachedPresence, item.type === "TV")}</small></article>; })}</div>{!related.items.length && <p>本页暂无相关媒体。</p>}<button type="button" disabled={relatedPage <= 1} onClick={() => setRelatedPage((n) => n - 1)}>上一页</button>{" "}<span>第 {relatedPage} 页</span>{" "}<button type="button" disabled={!related.items.length || relatedPage >= 100} onClick={() => setRelatedPage((n) => n + 1)}>下一页</button></>}
        </section>
      </>}
    </div>
  </WorkspaceLayout>;
}
function People({ title, people }: { title: string; people: MediaPerson[] }) { return <section className="content-section"><h2>{title}</h2>{!people.length ? <p>暂无可用名单</p> : <div className="media-detail-people">{people.map((person, i) => <article key={`${person.id}-${i}`}><a href={person.link} target="_blank" rel="noreferrer">{person.image && <img src={person.image} alt="" loading="lazy" />}<strong>{person.name}</strong></a><small>{person.role || person.job || person.department || person.originalName}</small></article>)}</div>}</section>; }
