import { useEffect, useState, type FormEvent } from "react";
import {
  ApiError, getDownloaderConfig, getDownloaderOptions, getMediaConfig, saveDownloaderConfig, saveMediaConfig,
  type AuthSession, type DownloadDirectory, type DownloaderConfigDetail, type DownloaderConfigInput, type DownloaderOptions,
  type ManagedService, type MediaConfigDetail, type MediaConfigInput,
} from "../api/client";

type ServiceEditorProps = {
  session: AuthSession;
  service?: ManagedService;
  kind: "downloader" | "media";
  onClose: () => void;
  onSaved: () => void;
  onSessionExpired: () => void;
};

const emptyDownloader: DownloaderConfigDetail = {
  id: "", name: "", type: "qbittorrent", host: "", port: "8080", proxy: "",
  torrentManagement: "manual", rmtMode: "link", enabled: true, transfer: false,
  onlyNastool: false, matchPath: false, default: false, usernameConfigured: false,
  passwordConfigured: false, secretConfigured: false, cookieConfigured: false,
  directoriesConfigured: false, directories: [],
};

const emptyDownloaderOptions: DownloaderOptions = { categories: { "电影": [], "电视剧": [], "动漫": [] }, warnings: [] };

function downloaderInput(detail: DownloaderConfigDetail): DownloaderConfigInput {
  return {
    name: detail.name, type: detail.type, host: detail.host, port: detail.port, proxy: detail.proxy,
    torrentManagement: detail.torrentManagement || "manual", rmtMode: detail.rmtMode || "link",
    enabled: detail.enabled, transfer: detail.transfer, onlyNastool: detail.onlyNastool, matchPath: detail.matchPath,
    directories: detail.directories.map((item) => ({ ...item })),
    username: "", password: "", secret: "", cookie: "",
    clearUsername: false, clearPassword: false, clearSecret: false, clearCookie: false,
  };
}

function mediaInput(detail: MediaConfigDetail): MediaConfigInput {
  return {
    host: detail.host, playHost: detail.playHost, serverName: detail.serverName,
    apiKey: "", token: "", username: "", password: "", activate: detail.active,
    clearApiKey: false, clearToken: false, clearUsername: false, clearPassword: false,
  };
}

export function ServiceEditor(props: ServiceEditorProps) {
  if (props.kind === "media" && props.service) return <MediaEditor {...props} service={props.service} />;
  return <DownloaderEditor {...props} />;
}

function DownloaderEditor({ session, service, onClose, onSaved, onSessionExpired }: ServiceEditorProps) {
  const [detail, setDetail] = useState(emptyDownloader);
  const [input, setInput] = useState<DownloaderConfigInput>(() => downloaderInput(emptyDownloader));
  const [loading, setLoading] = useState(Boolean(service));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [options, setOptions] = useState<DownloaderOptions>(emptyDownloaderOptions);

  useEffect(() => {
    let active = true;
    if (!service) {
      setDetail(emptyDownloader);
      setInput(downloaderInput(emptyDownloader));
      setLoading(true);
      getDownloaderOptions(session.token).then((value) => {
        if (!active) return;
        setOptions(value);
        setLoading(false);
      }).catch((reason) => {
        if (!active) return;
        if (reason instanceof ApiError && reason.code === 401) return onSessionExpired();
        setError(reason instanceof Error ? reason.message : "下载目录选项加载失败");
        setLoading(false);
      });
      return () => { active = false; };
    }
    setLoading(true);
    Promise.all([getDownloaderConfig(session.token, service.id), getDownloaderOptions(session.token)]).then(([value, nextOptions]) => {
      if (!active) return;
      setDetail(value);
      setInput(downloaderInput(value));
      setOptions(nextOptions);
      setLoading(false);
    }).catch((reason) => {
      if (!active) return;
      if (reason instanceof ApiError && reason.code === 401) return onSessionExpired();
      setError(reason instanceof Error ? reason.message : "下载器配置加载失败");
      setLoading(false);
    });
    return () => { active = false; };
  }, [service?.id, session.token]);

  function patch<K extends keyof DownloaderConfigInput>(key: K, value: DownloaderConfigInput[K]) {
    setInput((current) => ({ ...current, [key]: value }));
  }

  function changeType(type: string) {
    const ports: Record<string, string> = { qbittorrent: "8080", transmission: "9091", aria2: "6800" };
    setInput((current) => ({ ...current, type, port: ports[type] || "", host: ["pan115", "pikpak"].includes(type) ? "" : current.host,
      transfer: ["pan115", "pikpak"].includes(type) ? false : current.transfer,
      onlyNastool: ["pan115", "pikpak"].includes(type) ? false : current.onlyNastool,
      matchPath: ["pan115", "pikpak"].includes(type) ? false : current.matchPath }));
  }

  function addDirectory() {
    const next: DownloadDirectory = { type: "", category: "", savePath: "", containerPath: "", label: "" };
    patch("directories", [...input.directories, next]);
  }

  function patchDirectory<K extends keyof DownloadDirectory>(index: number, key: K, value: DownloadDirectory[K]) {
    patch("directories", input.directories.map((item, position) => position === index
      ? { ...item, [key]: value, ...(key === "type" ? { category: "" } : {}) } : item));
  }

  function removeDirectory(index: number) {
    patch("directories", input.directories.filter((_, position) => position !== index));
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError("");
    if (!input.name.trim()) return setError("请输入下载器名称");
    const sameType = Boolean(service && detail.type === input.type);
    if (["qbittorrent", "transmission", "aria2"].includes(input.type) && (!input.host.trim() || !input.port.trim())) return setError("请输入下载器地址和端口");
    if (["qbittorrent", "transmission", "pikpak"].includes(input.type) && !input.username.trim() && !(sameType && detail.usernameConfigured && !input.clearUsername)) return setError("请输入账户名称");
    if (input.type === "aria2" && !input.secret && !(sameType && detail.secretConfigured && !input.clearSecret)) return setError("请输入 Aria2 令牌");
    if (input.type === "pan115" && !input.cookie && !(sameType && detail.cookieConfigured && !input.clearCookie)) return setError("请输入 115 Cookie");
    if (input.type === "pikpak" && !input.password && !(sameType && detail.passwordConfigured && !input.clearPassword)) return setError("请输入 PikPak 密码");
    const directoryRules = new Set<string>();
    const directoryLabels = new Set<string>();
    for (const directory of input.directories) {
      if (!directory.type && directory.category) return setError("选择二级分类前需要先选择媒体类型");
      if (!directory.type && !directory.category && !directory.savePath.trim() && !directory.containerPath.trim() && !directory.label.trim()) continue;
      const key = `${directory.type}\u0000${directory.category}`;
      if (directoryRules.has(key)) return setError("相同媒体类型和二级分类只能配置一条目录规则");
      directoryRules.add(key);
      const label = directory.label.trim();
      if (input.type === "qbittorrent" && label && directoryLabels.has(label)) return setError("qBittorrent 标签不能重复");
      if (label) directoryLabels.add(label);
    }
    setSaving(true);
    try {
      await saveDownloaderConfig(session.token, { ...input, name: input.name.trim(), host: input.host.trim(), port: input.port.trim(), proxy: input.proxy.trim(), username: input.username.trim() }, service?.id);
      onSaved();
    } catch (reason) {
      if (reason instanceof ApiError && reason.code === 401) return onSessionExpired();
      setError(reason instanceof Error ? reason.message : "下载器保存失败");
    } finally {
      setSaving(false);
    }
  }

  const localCredentials = !service || detail.type !== input.type ? { username: false, password: false, secret: false, cookie: false } : {
    username: detail.usernameConfigured, password: detail.passwordConfigured, secret: detail.secretConfigured, cookie: detail.cookieConfigured,
  };
  const supportsMonitor = !["pan115", "pikpak"].includes(input.type);

  return <div className="editor-backdrop" role="presentation"><form className="service-editor" aria-label={service ? `编辑下载器 ${service.name}` : "新增下载器"} onSubmit={(event) => void submit(event)}>
    <header><div><p className="eyebrow">DOWNLOADER SETTINGS</p><h2>{service ? "编辑下载器" : "新增下载器"}</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
    {loading ? <div className="site-editor-loading">正在读取下载器设置…</div> : <>
      {error && <div className="form-error" role="alert">{error}</div>}
      <div className="editor-grid">
        <label>名称<input required maxLength={80} value={input.name} onChange={(event) => patch("name", event.target.value)} /></label>
        <label>类型<select value={input.type} onChange={(event) => changeType(event.target.value)}><option value="qbittorrent">qBittorrent</option><option value="transmission">Transmission</option><option value="aria2">Aria2</option><option value="pan115">115 网盘</option><option value="pikpak">PikPak</option></select></label>
        {["qbittorrent", "transmission", "aria2"].includes(input.type) && <><label>地址<input required placeholder="127.0.0.1 或 https://下载器地址" value={input.host} onChange={(event) => patch("host", event.target.value)} /></label><label>端口<input required inputMode="numeric" value={input.port} onChange={(event) => patch("port", event.target.value)} /></label></>}
        {input.type === "pikpak" && <label className="editor-wide">代理地址<input placeholder="可选，例如 127.0.0.1:7890" value={input.proxy} onChange={(event) => patch("proxy", event.target.value)} /></label>}
        {input.type === "qbittorrent" && <label>种子管理<select value={input.torrentManagement} onChange={(event) => patch("torrentManagement", event.target.value)}><option value="default">使用客户端设置</option><option value="manual">手动管理</option><option value="auto">自动管理</option></select></label>}
        {supportsMonitor && <label>转移方式<select value={input.rmtMode} onChange={(event) => patch("rmtMode", event.target.value)}><option value="copy">复制</option><option value="link">硬链接</option><option value="softlink">软链接</option><option value="move">移动</option><option value="rclone">Rclone 移动</option><option value="rclonecopy">Rclone 复制</option><option value="minio">MinIO 移动</option><option value="miniocopy">MinIO 复制</option></select></label>}
      </div>

      <section className="secret-fields" aria-labelledby="downloader-secret-title"><div className="secret-fields__heading"><div><h3 id="downloader-secret-title">访问凭据</h3><p>已保存内容不会回显；留空表示保持原值。</p></div><span>只写</span></div>
        {["qbittorrent", "transmission", "pikpak"].includes(input.type) && <CredentialField label={input.type === "pikpak" ? "账号" : "用户名"} configured={localCredentials.username} value={input.username} clear={input.clearUsername} allowClear={false} placeholder="仅在新增或替换时填写" onValue={(value) => patch("username", value)} onClear={(value) => patch("clearUsername", value)} />}
        {["qbittorrent", "transmission", "pikpak"].includes(input.type) && <CredentialField label="密码" configured={localCredentials.password} value={input.password} clear={input.clearPassword} allowClear={input.type !== "pikpak"} placeholder={input.type === "pikpak" ? "PikPak 密码" : "可选"} onValue={(value) => patch("password", value)} onClear={(value) => patch("clearPassword", value)} />}
        {input.type === "aria2" && <CredentialField label="令牌" configured={localCredentials.secret} value={input.secret} clear={input.clearSecret} allowClear={false} placeholder="Aria2 RPC Secret" onValue={(value) => patch("secret", value)} onClear={(value) => patch("clearSecret", value)} />}
        {input.type === "pan115" && <CredentialField label="Cookie" configured={localCredentials.cookie} value={input.cookie} clear={input.clearCookie} allowClear={false} textarea placeholder="USERSESSIONID=…; UID=…" onValue={(value) => patch("cookie", value)} onClear={(value) => patch("clearCookie", value)} />}
      </section>

      <fieldset className="option-checks service-option-checks"><legend>运行方式</legend><label><input type="checkbox" checked={input.enabled} onChange={(event) => patch("enabled", event.target.checked)} />启用下载器</label>
        {supportsMonitor && <><label><input type="checkbox" checked={input.transfer} onChange={(event) => patch("transfer", event.target.checked)} />监控并转移</label><label><input type="checkbox" checked={input.onlyNastool} onChange={(event) => patch("onlyNastool", event.target.checked)} />标签隔离</label><label><input type="checkbox" checked={input.matchPath} onChange={(event) => patch("matchPath", event.target.checked)} />目录隔离</label></>}
      </fieldset>
      {supportsMonitor && <section className="directory-settings" aria-labelledby="directory-settings-title"><header><div><h3 id="directory-settings-title">下载目录规则</h3><p>按媒体类型和二级分类选择保存目录；规则从上到下匹配。</p></div><button type="button" onClick={addDirectory}>＋ 添加规则</button></header>
        {options.warnings.length > 0 && <p className="editor-option-warning">部分二级分类暂时不可用，现有值仍可保存。</p>}
        {input.directories.length === 0 ? <div className="directory-empty">尚未配置目录规则，将使用下载器默认保存目录。</div> : <div className="directory-list">{input.directories.map((directory, index) => {
          const used = new Set(input.directories.filter((_, position) => position !== index && _.type === directory.type).map((item) => item.category));
          const categories = (options.categories[directory.type] || []).filter((item) => !used.has(item));
          return <article className="directory-rule" key={index}><div className="directory-rule__heading"><strong>规则 {index + 1}</strong><button type="button" aria-label={`删除目录规则 ${index + 1}`} onClick={() => removeDirectory(index)}>删除</button></div><div className="directory-rule__grid">
            <label>媒体类型<select value={directory.type} onChange={(event) => patchDirectory(index, "type", event.target.value as DownloadDirectory["type"])}><option value="">全部</option><option value="电影">电影</option><option value="电视剧">电视剧</option><option value="动漫">动漫</option></select></label>
            <label>二级分类<select disabled={!directory.type} value={directory.category} onChange={(event) => patchDirectory(index, "category", event.target.value)}><option value="">全部</option>{directory.category && !categories.includes(directory.category) && <option value={directory.category}>{directory.category}（当前值）</option>}{categories.map((item) => <option key={item} value={item}>{item}</option>)}</select></label>
            {input.type === "qbittorrent" && <label>qBittorrent 标签<input maxLength={100} value={directory.label} onChange={(event) => patchDirectory(index, "label", event.target.value)} placeholder="可选，自动管理时作为分类" /></label>}
            <label className={input.type === "qbittorrent" ? "" : "directory-wide"}>下载保存目录或 ID<input maxLength={2048} value={directory.savePath} onChange={(event) => patchDirectory(index, "savePath", event.target.value)} placeholder="例如 /downloads/movies" /></label>
            <label className="directory-wide">NAStool 访问目录<input maxLength={2048} value={directory.containerPath} onChange={(event) => patchDirectory(index, "containerPath", event.target.value)} placeholder="容器部署时填写映射后的路径" /></label>
          </div></article>;
        })}</div>}
      </section>}
    </>}
    <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={loading || saving}>{saving ? "保存中…" : "保存下载器"}</button></footer>
  </form></div>;
}

function MediaEditor({ session, service, onClose, onSaved, onSessionExpired }: ServiceEditorProps & { service: ManagedService }) {
  const id = service.id as MediaConfigDetail["id"];
  const [detail, setDetail] = useState<MediaConfigDetail | undefined>();
  const [input, setInput] = useState<MediaConfigInput | undefined>();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    getMediaConfig(session.token, id).then((value) => {
      if (!active) return;
      setDetail(value);
      setInput(mediaInput(value));
    }).catch((reason) => {
      if (!active) return;
      if (reason instanceof ApiError && reason.code === 401) return onSessionExpired();
      setError(reason instanceof Error ? reason.message : "媒体服务器配置加载失败");
    });
    return () => { active = false; };
  }, [id, session.token]);

  function patch<K extends keyof MediaConfigInput>(key: K, value: MediaConfigInput[K]) {
    setInput((current) => current ? { ...current, [key]: value } : current);
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!input || !detail) return;
    setError("");
    if (!/^https?:\/\//i.test(input.host)) return setError("服务器地址需要以 http:// 或 https:// 开头");
    if (input.playHost && !/^https?:\/\//i.test(input.playHost)) return setError("播放地址需要以 http:// 或 https:// 开头");
    if (id !== "plex" && !input.apiKey && (!detail.apiKeyConfigured || input.clearApiKey)) return setError("请输入 API Key");
    const hasToken = Boolean(input.token || (detail.tokenConfigured && !input.clearToken));
    const hasAccount = Boolean(input.serverName && (input.username || (detail.usernameConfigured && !input.clearUsername)) && (input.password || (detail.passwordConfigured && !input.clearPassword)));
    if (id === "plex" && !hasToken && !hasAccount) return setError("请填写 Plex Token，或完整填写服务器名称、账户和密码");
    setSaving(true);
    try {
      await saveMediaConfig(session.token, id, { ...input, host: input.host.trim(), playHost: input.playHost.trim(), serverName: input.serverName.trim(), username: input.username.trim() });
      onSaved();
    } catch (reason) {
      if (reason instanceof ApiError && reason.code === 401) return onSessionExpired();
      setError(reason instanceof Error ? reason.message : "媒体服务器保存失败");
    } finally {
      setSaving(false);
    }
  }

  return <div className="editor-backdrop" role="presentation"><form className="service-editor" aria-label={`编辑媒体服务器 ${service.name}`} onSubmit={(event) => void submit(event)}>
    <header><div><p className="eyebrow">MEDIA SERVER SETTINGS</p><h2>配置 {service.name}</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
    {!detail || !input ? <>{error && <div className="form-error" role="alert">{error}</div>}<div className="site-editor-loading">正在读取媒体服务器设置…</div></> : <>
      {error && <div className="form-error" role="alert">{error}</div>}
      <div className="editor-grid"><label className="editor-wide">服务器地址<input required type="url" placeholder={id === "plex" ? "http://127.0.0.1:32400" : "http://127.0.0.1:8096"} value={input.host} onChange={(event) => patch("host", event.target.value)} /></label><label className="editor-wide">媒体播放地址<input type="url" placeholder="可选，留空则使用服务器地址" value={input.playHost} onChange={(event) => patch("playHost", event.target.value)} /></label>{id === "plex" && <label className="editor-wide">Plex 服务器名称<input value={input.serverName} onChange={(event) => patch("serverName", event.target.value)} placeholder="使用 Token 时可留空" /></label>}</div>
      <section className="secret-fields" aria-labelledby="media-secret-title"><div className="secret-fields__heading"><div><h3 id="media-secret-title">访问凭据</h3><p>已保存内容不会回显；留空表示保持原值。</p></div><span>只写</span></div>
        {id !== "plex" ? <CredentialField label="API Key" configured={detail.apiKeyConfigured} value={input.apiKey} clear={input.clearApiKey} allowClear={false} placeholder="仅在新增或替换时填写" onValue={(value) => patch("apiKey", value)} onClear={(value) => patch("clearApiKey", value)} /> : <>
          <CredentialField label="X-Plex-Token" configured={detail.tokenConfigured} value={input.token} clear={input.clearToken} placeholder="推荐使用 Token" onValue={(value) => patch("token", value)} onClear={(value) => patch("clearToken", value)} />
          <div className="secret-pair"><CredentialField label="Plex 账户" configured={detail.usernameConfigured} value={input.username} clear={input.clearUsername} placeholder="Token 已配置时可留空" onValue={(value) => patch("username", value)} onClear={(value) => patch("clearUsername", value)} /><CredentialField label="Plex 密码" configured={detail.passwordConfigured} value={input.password} clear={input.clearPassword} placeholder="Token 已配置时可留空" onValue={(value) => patch("password", value)} onClear={(value) => patch("clearPassword", value)} /></div>
        </>}
      </section>
      <fieldset className="option-checks service-option-checks"><legend>使用状态</legend><label><input type="checkbox" checked={input.activate} disabled={detail.active} onChange={(event) => patch("activate", event.target.checked)} />{detail.active ? "当前媒体服务器" : "设为当前媒体服务器"}</label></fieldset>
      {detail.active && <p className="editor-note">这是当前使用的媒体服务器；取消勾选不会停用它，请选择并启用另一台服务器进行切换。</p>}
    </>}
    <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={!input || saving}>{saving ? "保存中…" : "保存媒体服务器"}</button></footer>
  </form></div>;
}

function CredentialField({ label, configured, value, clear, placeholder, textarea, allowClear = true, onValue, onClear }: {
  label: string; configured: boolean; value: string; clear: boolean; placeholder: string; textarea?: boolean; allowClear?: boolean;
  onValue: (value: string) => void; onClear: (value: boolean) => void;
}) {
  return <div className="secret-field"><label className="secret-value"><span>{label}{configured && <small>已配置</small>}</span>{textarea
    ? <textarea value={value} disabled={clear} rows={3} placeholder={placeholder} autoComplete="off" spellCheck={false} onChange={(event) => onValue(event.target.value)} />
    : <input value={value} disabled={clear} type="password" placeholder={placeholder} autoComplete="new-password" spellCheck={false} onChange={(event) => onValue(event.target.value)} />}</label>
    {configured && allowClear && <label className="secret-clear"><input type="checkbox" checked={clear} onChange={(event) => onClear(event.target.checked)} />清除已保存的{label}</label>}
  </div>;
}
