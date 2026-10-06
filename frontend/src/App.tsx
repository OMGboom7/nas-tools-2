import { useEffect, useState } from "react";
import { login, logout, type AuthSession } from "./api/client";
import { clearSession, loadSession, saveSession } from "./auth/storage";
import { DashboardPage } from "./pages/DashboardPage";
import { DownloadsPage } from "./pages/DownloadsPage";
import { DiscoveryPage } from "./pages/DiscoveryPage";
import { LoginPage } from "./pages/LoginPage";
import { NotificationsPage } from "./pages/NotificationsPage";
import { PluginsPage } from "./pages/PluginsPage";
import { SearchPage } from "./pages/SearchPage";
import { SitesPage } from "./pages/SitesPage";
import { ServicesPage } from "./pages/ServicesPage";
import { SubscriptionsPage } from "./pages/SubscriptionsPage";

export function App() {
  const [session, setSession] = useState<AuthSession | null>(() => loadSession());
  const [path, setPath] = useState(() => window.location.pathname);

  useEffect(() => {
    const handlePopState = () => setPath(window.location.pathname);
    window.addEventListener("popstate", handlePopState);
    return () => window.removeEventListener("popstate", handlePopState);
  }, []);

  async function handleLogin(credentials: { username: string; password: string; remember: boolean }) {
    const nextSession = await login(credentials.username, credentials.password);
    saveSession(nextSession, credentials.remember);
    setSession(nextSession);
  }

  async function handleLogout() {
    try {
      if (session) await logout(session.token);
    } catch {
      // Local sign-out must still succeed if the legacy service is unavailable.
    } finally {
      clearSession();
      setSession(null);
    }
  }

  function handleSessionExpired() {
    clearSession();
    setSession(null);
  }

  function handleNavigate(nextPath: string) {
    if (window.location.pathname !== nextPath) {
      window.history.pushState({}, "", nextPath);
    }
    setPath(nextPath);
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  if (!session) return <LoginPage onLogin={handleLogin} />;
  if (path === "/search" && session.user.userpris.includes("资源搜索")) {
    return <SearchPage session={session} currentPath="/search" onNavigate={handleNavigate}
      onLogout={handleLogout} onSessionExpired={handleSessionExpired} />;
  }
  if (path === "/downloads" && session.user.userpris.includes("下载管理")) {
    return <DownloadsPage session={session} currentPath="/downloads" onNavigate={handleNavigate}
      onLogout={handleLogout} onSessionExpired={handleSessionExpired} />;
  }
  if (path === "/discover" && session.user.userpris.includes("探索")) {
    return <DiscoveryPage session={session} currentPath="/discover" onNavigate={handleNavigate}
      onLogout={handleLogout} onSessionExpired={handleSessionExpired} />;
  }
  if (path === "/subscriptions" && session.user.userpris.includes("订阅管理")) {
    return <SubscriptionsPage session={session} currentPath="/subscriptions" onNavigate={handleNavigate}
      onLogout={handleLogout} onSessionExpired={handleSessionExpired} />;
  }
  if (path === "/sites" && session.user.userpris.includes("站点管理")) {
    return <SitesPage session={session} currentPath="/sites" onNavigate={handleNavigate}
      onLogout={handleLogout} onSessionExpired={handleSessionExpired} />;
  }
  if (path === "/services" && session.user.userpris.includes("服务")) {
    return <ServicesPage session={session} currentPath="/services" onNavigate={handleNavigate}
      onLogout={handleLogout} onSessionExpired={handleSessionExpired} />;
  }
  if (path === "/notifications" && session.user.userpris.includes("系统设置")) {
    return <NotificationsPage session={session} currentPath="/notifications" onNavigate={handleNavigate}
      onLogout={handleLogout} onSessionExpired={handleSessionExpired} />;
  }
  if (path === "/plugins" && session.user.userpris.includes("插件")) {
    return <PluginsPage session={session} currentPath="/plugins" onNavigate={handleNavigate}
      onLogout={handleLogout} onSessionExpired={handleSessionExpired} />;
  }
  return <DashboardPage session={session} currentPath="/" onNavigate={handleNavigate}
    onLogout={handleLogout} onSessionExpired={handleSessionExpired} />;
}
