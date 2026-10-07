import type { MouseEvent } from "react";
import type { UserInfo } from "../api/client";

type NavigationItem = {
  permission: string;
  label: string;
  mark: string;
  path?: string;
};

const navigation: NavigationItem[] = [
  { permission: "我的媒体库", label: "媒体库", mark: "⌂", path: "/" },
  { permission: "资源搜索", label: "搜索", mark: "⌕", path: "/search" },
  { permission: "探索", label: "探索", mark: "◇", path: "/discover" },
  { permission: "订阅管理", label: "订阅", mark: "◎", path: "/subscriptions" },
  { permission: "下载管理", label: "下载", mark: "↓", path: "/downloads" },
  { permission: "媒体整理", label: "整理预览", mark: "⇥", path: "/organization" },
  { permission: "站点管理", label: "站点", mark: "▦", path: "/sites" },
  { permission: "服务", label: "服务", mark: "⌘", path: "/services" },
  { permission: "插件", label: "插件", mark: "⬡", path: "/plugins" },
  { permission: "系统设置", label: "通知", mark: "◉", path: "/notifications" },
];

type AppNavigationProps = {
  user: UserInfo;
  currentPath: string;
  onNavigate: (path: string) => void;
};

export function AppNavigation({ user, currentPath, onNavigate }: AppNavigationProps) {
  const permissions = new Set(user.userpris);
  const visibleItems = navigation.filter((item) => permissions.has(item.permission));

  function navigate(event: MouseEvent<HTMLAnchorElement>, path: string) {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    onNavigate(path);
  }

  return (
    <aside className="sidebar">
      <a className="brand sidebar__brand" href="/" onClick={(event) => navigate(event, "/")} aria-label="NAS Tools 首页">
        <span className="brand__mark">N</span>
        <span>NAS Tools</span>
      </a>

      <nav className="sidebar__nav" aria-label="主导航">
        <p className="sidebar__label">工作区</p>
        {visibleItems.map((item) => {
          const active = item.path === currentPath;
          if (item.path) {
            return (
              <a key={item.permission} href={item.path} onClick={(event) => navigate(event, item.path!)}
                className={`nav-item ${active ? "is-active" : ""}`} aria-label={item.label}
                aria-current={active ? "page" : undefined}>
                <span className="nav-item__mark">{item.mark}</span><span>{item.label}</span>
              </a>
            );
          }
          return (
            <button key={item.permission} type="button" className="nav-item" disabled title={`${item.label}将在后续批次迁移`}>
              <span className="nav-item__mark">{item.mark}</span><span>{item.label}</span><span className="nav-item__soon">待迁移</span>
            </button>
          );
        })}
      </nav>

      <div className="sidebar__account">
        <span className="account__avatar">{user.username.slice(0, 1).toUpperCase()}</span>
        <span><strong>{user.username}</strong><small>{user.userpris.length} 项权限</small></span>
      </div>
    </aside>
  );
}
