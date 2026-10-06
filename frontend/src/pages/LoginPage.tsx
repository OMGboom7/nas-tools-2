import { type FormEvent, useState } from "react";

type Credentials = { username: string; password: string; remember: boolean };

type LoginPageProps = {
  onLogin: (credentials: Credentials) => Promise<void>;
};

export function LoginPage({ onLogin }: LoginPageProps) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [remember, setRemember] = useState(true);
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!username.trim() || !password) {
      setError("请输入用户名和密码");
      return;
    }
    setError("");
    setSubmitting(true);
    try {
      await onLogin({ username: username.trim(), password, remember });
    } catch (loginError) {
      setError(loginError instanceof Error ? loginError.message : "登录失败，请稍后重试");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <main className="login-shell">
      <section className="login-intro">
        <p className="eyebrow">NAS TOOLS · NEXT</p>
        <h1>管理你的媒体，<br />保持简单。</h1>
        <p className="summary">新界面正在渐进迁移。登录、权限与现有 NAS Tools 数据保持兼容。</p>
        <div className="migration-note">
          <span>01</span>
          <p><strong>第一站：账户系统</strong>当前登录请求由 Go 网关安全转发至旧服务。</p>
        </div>
      </section>

      <section className="login-panel" aria-labelledby="login-title">
        <form onSubmit={handleSubmit}>
          <div className="login-panel__header">
            <p className="eyebrow">WELCOME BACK</p>
            <h2 id="login-title">登录 NAS Tools</h2>
          </div>

          <label className="field">
            <span>用户名</span>
            <input name="username" autoComplete="username" autoFocus value={username}
              onChange={(event) => setUsername(event.target.value)} placeholder="admin" disabled={submitting} />
          </label>

          <label className="field">
            <span>密码</span>
            <input type="password" name="password" autoComplete="current-password" value={password}
              onChange={(event) => setPassword(event.target.value)} placeholder="输入密码" disabled={submitting} />
          </label>

          <label className="remember">
            <input type="checkbox" checked={remember}
              onChange={(event) => setRemember(event.target.checked)} disabled={submitting} />
            <span>在这台设备上保持登录</span>
          </label>

          {error && <p className="form-error" role="alert">{error}</p>}
          <button className="primary-button" type="submit" disabled={submitting}>
            {submitting ? "正在登录…" : "登录"}
          </button>
        </form>
      </section>
    </main>
  );
}
