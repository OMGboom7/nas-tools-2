import { useCallback, useEffect, useState } from "react";
import { ApiError, getDashboard, type AuthSession, type DashboardData, type ResumeItem } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";

type DashboardPageProps = {
  session: AuthSession;
  currentPath: string;
  onNavigate: (path: string) => void;
  onLogout: () => Promise<void>;
  onSessionExpired: () => void;
};

type DashboardState =
  | { kind: "loading" }
  | { kind: "ready"; data: DashboardData }
  | { kind: "error"; message: string };

export function DashboardPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: DashboardPageProps) {
  const [state, setState] = useState<DashboardState>({ kind: "loading" });

  const load = useCallback(() => {
    setState({ kind: "loading" });
    getDashboard(session.token)
      .then((data) => setState({ kind: "ready", data }))
      .catch((error: unknown) => {
        if (error instanceof ApiError && error.code === 401) {
          onSessionExpired();
          return;
        }
        setState({
          kind: "error",
          message: error instanceof Error ? error.message : "媒体库数据加载失败",
        });
      });
  }, [onSessionExpired, session.token]);

  useEffect(() => load(), [load]);

  return (
    <WorkspaceLayout user={session.user} currentPath={currentPath} section="媒体库" page="概览"
      onNavigate={onNavigate} onLogout={onLogout}>
      <div className="dashboard">
          <section className="dashboard__heading">
            <div>
              <p className="eyebrow">MEDIA OVERVIEW</p>
              <h1>你的媒体库</h1>
              <p className="summary">集中查看影视数量、存储空间和未看完的内容。</p>
            </div>
            {state.kind === "ready" && (
              <div className="status status--ready"><span className="status__dot" />数据已连接</div>
            )}
          </section>

          {state.kind === "loading" && <DashboardSkeleton />}
          {state.kind === "error" && (
            <section className="empty-state">
              <span className="empty-state__mark">!</span>
              <h2>媒体库暂时不可用</h2>
              <p>{state.message}。请确认旧 Flask 服务和媒体服务器正在运行。</p>
              <button className="secondary-button" type="button" onClick={load}>重新加载</button>
            </section>
          )}
          {state.kind === "ready" && <DashboardContent data={state.data} />}
      </div>
    </WorkspaceLayout>
  );
}

function DashboardContent({ data }: { data: DashboardData }) {
  const statistics = [
    { label: "电影", value: data.statistics.movies || "—", detail: "部" },
    { label: "剧集", value: data.statistics.series || "—", detail: `${data.statistics.episodes || "0"} 集` },
    { label: "音乐", value: data.statistics.music || "—", detail: "首" },
    { label: "用户", value: data.statistics.users || "—", detail: "位" },
  ];

  return (
    <>
      {data.warnings.length > 0 && (
        <p className="data-warning">部分数据暂时不可用，页面已展示其余可用内容。</p>
      )}

      <section className="stats-grid" aria-label="媒体库统计">
        {statistics.map((item) => (
          <article className="stat-card" key={item.label}>
            <p>{item.label}</p>
            <strong>{item.value}</strong>
            <span>{item.detail}</span>
          </article>
        ))}
        <article className="storage-card">
          <div className="storage-card__header">
            <div><p>存储空间</p><strong>{data.storage.total || "—"}</strong></div>
            <span>{formatPercent(data.storage.usedPercent)} 已使用</span>
          </div>
          <div className="storage-bar" aria-label={`存储空间已使用 ${formatPercent(data.storage.usedPercent)}`}>
            <span style={{ width: `${clampPercent(data.storage.usedPercent)}%` }} />
          </div>
          <div className="storage-card__legend">
            <span>已使用 {data.storage.used || "—"}</span><span>可用 {data.storage.free || "—"}</span>
          </div>
        </article>
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div><p className="eyebrow">CONTINUE WATCHING</p><h2>继续观看</h2></div>
          <span>{data.resume.length} 个项目</span>
        </div>

        {data.resume.length === 0 ? (
          <div className="inline-empty">当前没有未看完的内容</div>
        ) : (
          <div className="resume-grid">
            {data.resume.map((item) => (
              <a className="resume-card" key={item.id} href={item.link || undefined}
                target={item.link ? "_blank" : undefined} rel="noreferrer">
                <MediaArtwork item={item} layout="backdrop" />
                <div className="resume-card__body">
                  <span className="media-type">{item.type || "视频"}</span>
                  <h3>{item.name}</h3>
                  <div className="resume-progress"><span style={{ width: `${clampPercent(item.percent || 0)}%` }} /></div>
                  <small>已观看 {formatPercent(item.percent || 0)}</small>
                </div>
              </a>
            ))}
          </div>
        )}
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div><p className="eyebrow">RECENTLY ADDED</p><h2>最新入库</h2></div>
          <span>{data.latest.length} 个项目</span>
        </div>

        {data.latest.length === 0 ? (
          <div className="inline-empty">媒体服务器暂未返回最新入库内容</div>
        ) : (
          <div className="latest-grid">
            {data.latest.map((item) => (
              <a className="latest-card" key={item.id} href={item.link || undefined}
                target={item.link ? "_blank" : undefined} rel="noreferrer">
                <MediaArtwork item={item} layout="poster" />
                <div className="latest-card__body">
                  <span className="media-type">{item.type || "视频"}</span>
                  <h3>{item.name}</h3>
                </div>
              </a>
            ))}
          </div>
        )}
      </section>
    </>
  );
}

function MediaArtwork({ item, layout }: { item: ResumeItem; layout: "backdrop" | "poster" }) {
  const title = item.name || "未命名";
  return (
    <div className={`media-artwork media-artwork--${layout}`}>
      <span>{item.type === "电影" ? "MOVIE" : "SERIES"}</span>
      <strong>{title.slice(0, 1)}</strong>
      {item.image && (
        <img src={item.image} alt={`${title} 封面`} loading="lazy"
          onError={(event) => { event.currentTarget.hidden = true; }} />
      )}
    </div>
  );
}

function DashboardSkeleton() {
  return (
    <div className="dashboard-skeleton" aria-label="正在加载媒体库">
      <div className="skeleton-row">{[1, 2, 3, 4].map((item) => <span key={item} />)}</div>
      <div className="skeleton-wide" />
    </div>
  );
}

function clampPercent(value: number): number {
  return Math.min(100, Math.max(0, Number.isFinite(value) ? value : 0));
}

function formatPercent(value: number): string {
  return `${clampPercent(value).toFixed(1).replace(".0", "")}%`;
}
