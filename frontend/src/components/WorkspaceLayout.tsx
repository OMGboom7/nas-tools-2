import { type ReactNode, useState } from "react";
import type { UserInfo } from "../api/client";
import { AppNavigation } from "./AppNavigation";

type WorkspaceLayoutProps = {
  user: UserInfo;
  currentPath: string;
  section: string;
  page: string;
  onNavigate: (path: string) => void;
  onLogout: () => Promise<void>;
  children: ReactNode;
};

export function WorkspaceLayout({ user, currentPath, section, page, onNavigate, onLogout, children }: WorkspaceLayoutProps) {
  const [loggingOut, setLoggingOut] = useState(false);

  async function handleLogout() {
    setLoggingOut(true);
    try { await onLogout(); } finally { setLoggingOut(false); }
  }

  return (
    <div className="workspace">
      <AppNavigation user={user} currentPath={currentPath} onNavigate={onNavigate} />
      <main className="workspace__main">
        <header className="topbar">
          <div><span className="topbar__section">{section}</span><span className="topbar__separator">/</span><span className="topbar__page">{page}</span></div>
          <div className="account"><span>{user.username}</span><button className="text-button" type="button" onClick={handleLogout} disabled={loggingOut}>{loggingOut ? "退出中…" : "退出"}</button></div>
        </header>
        {children}
      </main>
    </div>
  );
}
