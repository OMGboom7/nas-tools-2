import { useEffect, useState, type FormEvent } from "react";
import { ApiError, getSite, getSiteOptions, saveSite, type AuthSession, type SiteDetail, type SiteInput, type SiteOptions, type SiteSummary } from "../api/client";

type SiteEditorProps = {
  session: AuthSession;
  site?: SiteSummary;
  onClose: () => void;
  onSaved: () => void;
  onSessionExpired: () => void;
};

const emptyDetail: SiteDetail = {
  id: "", name: "", priority: 1, siteUrl: "", rssEnabled: true, brushEnabled: false,
  statisticEnabled: true, parseEnabled: true, messageEnabled: true, browserEnabled: false,
  proxyEnabled: false, subtitleEnabled: false, tags: "", filterRule: "", downloadSetting: "",
  limitInterval: "", limitCount: "", limitSeconds: "", rssConfigured: false,
  cookieConfigured: false, apiKeyConfigured: false, userAgentConfigured: false,
};

function inputFromDetail(detail: SiteDetail): SiteInput {
  return {
    name: detail.name, priority: detail.priority, siteUrl: detail.siteUrl,
    rssEnabled: detail.rssEnabled, brushEnabled: detail.brushEnabled, statisticEnabled: detail.statisticEnabled,
    parseEnabled: detail.parseEnabled, messageEnabled: detail.messageEnabled,
    browserEnabled: detail.browserEnabled, proxyEnabled: detail.proxyEnabled, subtitleEnabled: detail.subtitleEnabled,
    tags: detail.tags, filterRule: detail.filterRule, downloadSetting: detail.downloadSetting,
    limitInterval: detail.limitInterval, limitCount: detail.limitCount, limitSeconds: detail.limitSeconds,
    rssUrl: "", cookie: "", apiKey: "", userAgent: "",
    clearRssUrl: false, clearCookie: false, clearApiKey: false, clearUserAgent: false,
  };
}

export function SiteEditor({ session, site, onClose, onSaved, onSessionExpired }: SiteEditorProps) {
  const [detail, setDetail] = useState<SiteDetail>(emptyDetail);
  const [input, setInput] = useState<SiteInput>(() => inputFromDetail(emptyDetail));
  const [loading, setLoading] = useState(Boolean(site));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [options, setOptions] = useState<SiteOptions>({ filterRules: [], downloadSettings: [], warnings: [] });

  useEffect(() => {
    let active = true;
    setLoading(true);
    Promise.all([site ? getSite(session.token, site.id) : Promise.resolve(emptyDetail), getSiteOptions(session.token)]).then(([value, nextOptions]) => {
      if (!active) return;
      setDetail(value);
      setInput(inputFromDetail(value));
      setOptions(nextOptions);
      setLoading(false);
    }).catch((reason) => {
      if (!active) return;
      if (reason instanceof ApiError && reason.code === 401) {
        onSessionExpired();
        return;
      }
      setError(reason instanceof Error ? reason.message : "站点详情加载失败");
      setLoading(false);
    });
    return () => { active = false; };
  }, [session.token, site?.id]);

  function patch<K extends keyof SiteInput>(key: K, value: SiteInput[K]) {
    setInput((current) => ({ ...current, [key]: value }));
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError("");
    if (!input.name.trim()) return setError("请输入站点名称");
    if (!/^https?:\/\//i.test(input.siteUrl)) return setError("站点地址需要以 http:// 或 https:// 开头");
    const needsRSS = input.rssEnabled || input.brushEnabled;
    if (needsRSS && !input.rssUrl.trim() && (!detail.rssConfigured || input.clearRssUrl)) {
      return setError("启用订阅或刷流时需要填写 RSS 地址");
    }
    setSaving(true);
    try {
      await saveSite(session.token, { ...input, name: input.name.trim(), siteUrl: input.siteUrl.trim(), rssUrl: input.rssUrl.trim() }, site?.id);
      onSaved();
    } catch (reason) {
      if (reason instanceof ApiError && reason.code === 401) {
        onSessionExpired();
        return;
      }
      setError(reason instanceof Error ? reason.message : "站点保存失败");
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="editor-backdrop" role="presentation">
      <form className="site-editor" aria-label={site ? `编辑站点 ${site.name}` : "新增站点"} onSubmit={(event) => void submit(event)}>
        <header><div><p className="eyebrow">SITE SETTINGS</p><h2>{site ? "编辑站点" : "新增站点"}</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
        {loading ? <div className="site-editor-loading">正在读取站点设置…</div> : <>
          {error && <div className="form-error" role="alert">{error}</div>}
          <div className="editor-grid">
            <label>站点名称<input required maxLength={80} value={input.name} onChange={(event) => patch("name", event.target.value)} /></label>
            <label>优先级<input required type="number" min={1} max={50} value={input.priority} onChange={(event) => patch("priority", Number(event.target.value))} /></label>
            <label className="editor-wide">站点地址<input required type="url" placeholder="https://tracker.example" value={input.siteUrl} onChange={(event) => patch("siteUrl", event.target.value)} /></label>
          </div>

          <fieldset className="option-checks"><legend>站点用途</legend>
            <label><input type="checkbox" checked={input.rssEnabled} onChange={(event) => patch("rssEnabled", event.target.checked)} />订阅</label>
            <label><input type="checkbox" checked={input.brushEnabled} onChange={(event) => patch("brushEnabled", event.target.checked)} />刷流</label>
            <label><input type="checkbox" checked={input.statisticEnabled} onChange={(event) => patch("statisticEnabled", event.target.checked)} />数据统计</label>
          </fieldset>

          <section className="secret-fields" aria-labelledby="site-secret-title">
            <div className="secret-fields__heading"><div><h3 id="site-secret-title">访问凭据</h3><p>已保存的内容不会回显；留空表示保持原值。</p></div><span>只写</span></div>
            <SecretField label="RSS 地址" configured={detail.rssConfigured} value={input.rssUrl} clear={input.clearRssUrl}
              placeholder="https://tracker.example/rss?passkey=…" onValue={(value) => patch("rssUrl", value)} onClear={(value) => patch("clearRssUrl", value)} />
            <SecretField label="Cookie" configured={detail.cookieConfigured} value={input.cookie} clear={input.clearCookie} textarea
              placeholder="仅在需要替换时填写" onValue={(value) => patch("cookie", value)} onClear={(value) => patch("clearCookie", value)} />
            <div className="secret-pair">
              <SecretField label="API Key" configured={detail.apiKeyConfigured} value={input.apiKey} clear={input.clearApiKey}
                placeholder="仅在需要替换时填写" onValue={(value) => patch("apiKey", value)} onClear={(value) => patch("clearApiKey", value)} />
              <SecretField label="User-Agent" configured={detail.userAgentConfigured} value={input.userAgent} clear={input.clearUserAgent}
                placeholder="留空使用全局设置" onValue={(value) => patch("userAgent", value)} onClear={(value) => patch("clearUserAgent", value)} />
            </div>
          </section>

          <details className="site-advanced"><summary>高级选项</summary>
            {options.warnings.length > 0 && <p className="editor-option-warning">部分配置选项暂时不可用，仍可保存其他设置。</p>}
            <div className="editor-switches site-switches">
              <label><input type="checkbox" checked={input.parseEnabled} onChange={(event) => patch("parseEnabled", event.target.checked)} />解析种子详情</label>
              <label><input type="checkbox" checked={input.messageEnabled} onChange={(event) => patch("messageEnabled", event.target.checked)} />未读消息通知</label>
              <label><input type="checkbox" checked={input.browserEnabled} onChange={(event) => patch("browserEnabled", event.target.checked)} />浏览器仿真</label>
              <label><input type="checkbox" checked={input.proxyEnabled} onChange={(event) => patch("proxyEnabled", event.target.checked)} />使用代理</label>
              <label><input type="checkbox" checked={input.subtitleEnabled} onChange={(event) => patch("subtitleEnabled", event.target.checked)} />下载字幕</label>
            </div>
            <div className="editor-grid">
              <label>过滤规则<select value={input.filterRule} onChange={(event) => patch("filterRule", event.target.value)}><option value="">默认</option>
                {input.filterRule && !options.filterRules.some((item) => item.value === input.filterRule) && <option value={input.filterRule}>当前设置（{input.filterRule}）</option>}
                {options.filterRules.map((item) => <option key={item.value} value={item.value}>{item.label}</option>)}</select></label>
              <label>下载设置<select value={input.downloadSetting} onChange={(event) => patch("downloadSetting", event.target.value)}><option value="">默认</option>
                {input.downloadSetting && !options.downloadSettings.some((item) => item.value === input.downloadSetting) && <option value={input.downloadSetting}>当前设置（{input.downloadSetting}）</option>}
                {options.downloadSettings.map((item) => <option key={item.value} value={item.value}>{item.label}</option>)}</select></label>
              <label className="editor-wide">下载标签<input value={input.tags} onChange={(event) => patch("tags", event.target.value)} placeholder="多个标签使用英文分号分隔" /></label>
              <label>单位时间（分钟）<input inputMode="numeric" value={input.limitInterval} onChange={(event) => patch("limitInterval", event.target.value)} /></label>
              <label>单位时间访问次数<input inputMode="numeric" value={input.limitCount} onChange={(event) => patch("limitCount", event.target.value)} /></label>
              <label>访问间隔（秒）<input inputMode="numeric" value={input.limitSeconds} onChange={(event) => patch("limitSeconds", event.target.value)} /></label>
            </div>
          </details>
        </>}
        <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={loading || saving}>{saving ? "保存中…" : "保存站点"}</button></footer>
      </form>
    </div>
  );
}

function SecretField({ label, configured, value, clear, placeholder, textarea, onValue, onClear }: {
  label: string; configured: boolean; value: string; clear: boolean; placeholder: string; textarea?: boolean;
  onValue: (value: string) => void; onClear: (value: boolean) => void;
}) {
  return <div className="secret-field"><label className="secret-value"><span>{label}{configured && <small>已配置</small>}</span>
    {textarea ? <textarea value={value} disabled={clear} rows={2} placeholder={placeholder} autoComplete="off" spellCheck={false} onChange={(event) => onValue(event.target.value)} />
      : <input value={value} disabled={clear} type="password" placeholder={placeholder} autoComplete="new-password" spellCheck={false} onChange={(event) => onValue(event.target.value)} />}
  </label>{configured && <label className="secret-clear"><input type="checkbox" checked={clear} onChange={(event) => onClear(event.target.checked)} />清除已保存的{label}</label>}
  </div>;
}
