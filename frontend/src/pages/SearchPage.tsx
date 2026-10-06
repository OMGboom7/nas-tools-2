import { type FormEvent, useMemo, useState } from "react";
import { addSearchResource, ApiError, searchResources, type AuthSession, type SearchData, type SearchResource } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";

type SearchPageProps = {
  session: AuthSession;
  currentPath: string;
  onNavigate: (path: string) => void;
  onLogout: () => Promise<void>;
  onSessionExpired: () => void;
};

type SearchState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "ready"; data: SearchData }
  | { kind: "error"; message: string };

type DownloadStatus = { kind: "loading" | "success" | "error"; message?: string };

export function SearchPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: SearchPageProps) {
  const [keyword, setKeyword] = useState("");
  const [quick, setQuick] = useState(false);
  const [state, setState] = useState<SearchState>({ kind: "idle" });
  const [site, setSite] = useState("");
  const [resolution, setResolution] = useState("");
  const [downloads, setDownloads] = useState<Record<string, DownloadStatus>>({});

  const filters = useMemo(() => {
    if (state.kind !== "ready") return { sites: [] as string[], resolutions: [] as string[] };
    const resources = state.data.items.flatMap((item) => item.resources);
    return {
      sites: unique(resources.map((item) => item.site)),
      resolutions: unique(resources.map((item) => item.resolution)),
    };
  }, [state]);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const value = keyword.trim();
    if (!value) return;
    setSite("");
    setResolution("");
    setDownloads({});
    setState({ kind: "loading" });
    try {
      const data = await searchResources(session.token, value, quick);
      setState({ kind: "ready", data });
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      setState({ kind: "error", message: error instanceof Error ? error.message : "资源搜索失败" });
    }
  }

  async function handleDownload(resource: SearchResource) {
    setDownloads((current) => ({ ...current, [resource.id]: { kind: "loading" } }));
    try {
      const message = await addSearchResource(session.token, resource.id);
      setDownloads((current) => ({ ...current, [resource.id]: { kind: "success", message } }));
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) {
        onSessionExpired();
        return;
      }
      setDownloads((current) => ({
        ...current,
        [resource.id]: { kind: "error", message: error instanceof Error ? error.message : "添加下载失败" },
      }));
    }
  }

  return (
    <WorkspaceLayout user={session.user} currentPath={currentPath} section="资源搜索" page="搜索"
      onNavigate={onNavigate} onLogout={onLogout}>
      <div className="search-page">
        <section className="search-hero">
          <p className="eyebrow">RESOURCE SEARCH</p>
          <h1>找到想看的内容</h1>
          <p className="summary">同时查询已配置的索引器，并按媒体、清晰度和站点整理结果。</p>

          <form className="search-form" onSubmit={handleSubmit}>
            <label className="search-box">
              <span aria-hidden="true">⌕</span>
              <input value={keyword} onChange={(event) => setKeyword(event.target.value)}
                placeholder="输入电影、剧集或资源名称" aria-label="搜索关键词" maxLength={120} disabled={state.kind === "loading"} />
            </label>
            <button className="search-submit" type="submit" disabled={!keyword.trim() || state.kind === "loading"}>
              {state.kind === "loading" ? "搜索中…" : "搜索"}
            </button>
          </form>
          <label className="quick-search">
            <input type="checkbox" checked={quick} onChange={(event) => setQuick(event.target.checked)} disabled={state.kind === "loading"} />
            <span>快速模式：跳过媒体识别，直接按关键词搜索</span>
          </label>
        </section>

        {state.kind === "idle" && (
          <section className="search-idle"><span>⌕</span><h2>开始一次资源搜索</h2><p>结果会在这里按媒体条目归类显示。</p></section>
        )}
        {state.kind === "loading" && <SearchSkeleton />}
        {state.kind === "error" && (
          <section className="empty-state"><span className="empty-state__mark">!</span><h2>搜索没有完成</h2><p>{state.message}</p></section>
        )}
        {state.kind === "ready" && (
          <section className="search-results">
            <div className="search-results__header">
              <div><p className="eyebrow">SEARCH RESULTS</p><h2>“{state.data.keyword}”</h2><span>共找到 {state.data.total} 个资源</span></div>
              <div className="search-filters">
                <select value={site} onChange={(event) => setSite(event.target.value)} aria-label="按站点筛选">
                  <option value="">全部站点</option>{filters.sites.map((value) => <option key={value}>{value}</option>)}
                </select>
                <select value={resolution} onChange={(event) => setResolution(event.target.value)} aria-label="按分辨率筛选">
                  <option value="">全部分辨率</option>{filters.resolutions.map((value) => <option key={value}>{value}</option>)}
                </select>
              </div>
            </div>

            {state.data.warnings?.map((warning) => <p className="inline-empty" role="status" key={warning}>{warning}</p>)}
            {state.data.items.length === 0 ? (
              <div className="inline-empty">没有找到相关资源，可以尝试缩短关键词或启用快速模式。</div>
            ) : (
              <div className="media-results">
                {state.data.items.map((media) => {
                  const resources = media.resources.filter((item) => (!site || item.site === site) && (!resolution || item.resolution === resolution));
                  return (
                    <article className="media-result" key={media.key || `${media.title}-${media.year}`}>
                      <div className="media-result__summary">
                        <div className="search-poster">
                          <strong>{media.title.slice(0, 1)}</strong>
                          {media.poster && <img src={media.poster} alt={`${media.title} 海报`} loading="lazy" onError={(event) => { event.currentTarget.hidden = true; }} />}
                        </div>
                        <div>
                          <div className="media-result__title"><h3>{media.title}</h3>{media.exists && <span>已入库</span>}{media.existsKnown === false && <span>库状态未知</span>}</div>
                          <p>{[media.type, media.year, media.vote ? `${media.vote} 分` : ""].filter(Boolean).join(" · ")}</p>
                          {media.overview && <small>{media.overview}</small>}
                        </div>
                      </div>

                      <div className="resource-list">
                        {resources.length === 0 ? <div className="resource-empty">当前筛选条件下没有资源</div> : resources.map((resource) => (
                          <ResourceRow resource={resource} key={resource.id}
                            canDownload={session.user.userpris.includes("下载管理")}
                            status={downloads[resource.id]} onDownload={() => handleDownload(resource)} />
                        ))}
                      </div>
                    </article>
                  );
                })}
              </div>
            )}
          </section>
        )}
      </div>
    </WorkspaceLayout>
  );
}

function ResourceRow({ resource, canDownload, status, onDownload }: {
  resource: SearchResource;
  canDownload: boolean;
  status?: DownloadStatus;
  onDownload: () => void;
}) {
  const promotion = resource.promotionKnown === false ? "促销未知" : resource.downloadFactor === 0 ? "FREE" : resource.downloadFactor < 1 ? `${resource.downloadFactor * 100}% DL` : "";
  return (
    <div className="resource-row">
      <div className="resource-row__main">
        {resource.pageUrl ? <a href={resource.pageUrl} target="_blank" rel="noreferrer">{resource.name}</a> : <strong>{resource.name}</strong>}
        {resource.description && <p>{resource.description}</p>}
        <div className="resource-tags">
          <span>{resource.site || "未知站点"}</span>
          {resource.existsKnown && resource.exists && <span>对应内容已入库</span>}
          {resource.season && resource.season !== "MOV" && <span>{resource.season}</span>}
          {resource.resolution && <span>{resource.resolution}</span>}
          {resource.medium && <span>{resource.medium}</span>}
          {resource.videoCodec && <span>{resource.videoCodec}</span>}
          {resource.effect && <span>{resource.effect}</span>}
          {resource.labels.map((label) => <span key={label}>{label}</span>)}
          {promotion && <span className={resource.promotionKnown === false ? "" : "is-free"}>{promotion}</span>}
        </div>
      </div>
      <div className="resource-row__meta">
        <strong>{resource.size || "—"}</strong><span>{resource.seedersKnown === false ? "未知" : resource.seeders} 做种</span><small>{resource.releaseGroup || "未知发布组"}</small>
        {canDownload && <button type="button" className={`download-resource ${status?.kind === "success" ? "is-success" : ""}`}
          onClick={onDownload} disabled={status?.kind === "loading" || status?.kind === "success"}
          aria-label={`下载 ${resource.name}`} title={status?.message}>
          {status?.kind === "loading" ? "添加中…" : status?.kind === "success" ? "已添加" : status?.kind === "error" ? "重试" : "下载"}
        </button>}
        {status?.kind === "error" && <span className="download-action-error" title={status.message}>添加失败</span>}
      </div>
    </div>
  );
}

function SearchSkeleton() {
  return <section className="search-loading"><div /><div /><div /><p>正在连接索引器，搜索可能需要一些时间…</p></section>;
}

function unique(values: string[]): string[] {
  return [...new Set(values.filter(Boolean))].sort((left, right) => left.localeCompare(right, "zh-CN"));
}
