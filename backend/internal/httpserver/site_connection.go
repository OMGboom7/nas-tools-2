package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

const defaultSiteUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/98.0.4758.102 Safari/537.36"

var (
	siteTag       = regexp.MustCompile(`(?is)<\s*(input|a|form|div)\b[^>]*>`)
	siteAttribute = regexp.MustCompile(`(?is)\b([a-z][a-z0-9_-]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// The site check deliberately returns only stable messages: URLs, cookies and
// server response bodies can contain private tracker credentials.
func checkSiteConnection(ctx context.Context, site siteconfig.Site, app map[string]any, transport http.RoundTripper) (bool, string, bool) {
	return checkSiteConnectionAdmitted(ctx, site, app, transport, nil)
}

func checkSiteConnectionAdmitted(ctx context.Context, site siteconfig.Site, app map[string]any, transport http.RoundTripper, admit func(context.Context) error, renderers ...browserruntime.Renderer) (bool, string, bool) {
	note := map[string]any{}
	if site.Note != "" && site.Note != "null" && json.Unmarshal([]byte(site.Note), &note) != nil {
		return false, "站点配置无效", false
	}
	raw := strings.TrimSpace(site.SignURL)
	if raw == "" {
		raw = strings.TrimSpace(site.RSSURL)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false, "未配置有效站点地址", false
	}
	origin, isMTeam := builtinindexer.MTeamFeedOrigin(raw)
	if isMTeam && strings.TrimSpace(site.APIKey) == "" {
		return false, "未配置站点API Key", false
	}
	if !isMTeam {
		// A tracker configured with an unsupported scheme or port must not fall
		// through to the ordinary HTML check and send its session cookie.
		if _, knownHost := builtinindexer.MTeamFeedOrigin("https://" + parsed.Hostname()); knownHost {
			return false, "未配置有效站点地址", false
		}
	}
	if !isMTeam && strings.TrimSpace(site.Cookie) == "" {
		return false, "未配置站点Cookie", false
	}
	if text(note["proxy"]) == "Y" {
		transport, err = siteProxyTransport(transport, objectValue(app["proxies"]), parsed.Scheme)
		if err != nil {
			return false, "站点代理配置无效或不受支持", true
		}
	}
	userAgent := strings.TrimSpace(text(note["ua"]))
	if userAgent == "" {
		userAgent = strings.TrimSpace(text(app["user_agent"]))
	}
	if userAgent == "" {
		userAgent = defaultSiteUserAgent
	}
	if isMTeam {
		if admit != nil && admit(ctx) != nil {
			return false, "站点限流等待被取消或配置无效", false
		}
		err := builtinindexer.CheckMTeamProfile(ctx, origin, site.APIKey, userAgent, transport)
		var status builtinindexer.MTeamHTTPStatus
		if errors.As(err, &status) {
			return false, "连接失败，状态码：" + text(int(status)), false
		}
		if err != nil {
			return false, "连接失败", false
		}
		return true, "连接成功", false
	}
	parsed.Path = "/"
	if strings.Contains(strings.ToLower(parsed.Hostname()), "1ptba") || strings.Contains(strings.ToLower(parsed.Hostname()), "zmpt") {
		parsed.Path = "/index.php"
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	if note["chrome"] == "Y" || truthy(note["chrome"]) {
		if len(renderers) == 0 || renderers[0] == nil || !renderers[0].Available() {
			return false, "原生浏览器不可用，请检查浏览器安装和运行权限", true
		}
		page, e := renderers[0].Render(ctx, browserruntime.Request{URL: parsed.String(), Cookie: site.Cookie, UserAgent: userAgent, Transport: transport, ReadOnly: true, Assets: true, Guard: func(ctx context.Context) error { return ctx.Err() },
			Before: func(ctx context.Context, _ browserruntime.Exchange) (func(string, string) error, error) {
				if admit != nil {
					if e := admit(ctx); e != nil {
						return nil, e
					}
				}
				return func(string, string) error { return nil }, nil
			}})
		if e != nil {
			if e == browserruntime.ErrVerification {
				return false, "站点需要人工验证，请在站点完成验证后更新Cookie", false
			}
			return false, "原生浏览器未能完成站点页面", false
		}
		if !siteHTMLLoggedIn(page.HTML) {
			return false, "Cookie失效", false
		}
		return true, "连接成功（原生浏览器）", false
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return false, "站点地址无效", false
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Cookie", site.Cookie)
	if admit != nil {
		if err := admit(ctx); err != nil {
			return false, "站点限流等待被取消或配置无效", false
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return false, "无法打开网站", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, "连接失败，状态码：" + text(response.StatusCode), false
	}
	const limit = 2 << 20
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(contents) > limit {
		return false, "站点响应过大或读取失败", false
	}
	if !siteHTMLLoggedIn(contents) {
		return false, "Cookie失效", false
	}
	return true, "连接成功", false
}

func siteProxyTransport(transport http.RoundTripper, proxies map[string]any, scheme string) (http.RoundTripper, error) {
	return siteconfig.ProxyTransport(transport, text(proxies[scheme]))
}

func siteHTMLLoggedIn(contents []byte) bool {
	loginMarker := false
	for _, raw := range siteTag.FindAll(contents, -1) {
		found := siteTag.FindSubmatch(raw)
		if len(found) < 2 {
			continue
		}
		attributes := map[string]string{}
		for _, match := range siteAttribute.FindAllSubmatch(raw, -1) {
			value := match[2]
			if len(value) == 0 {
				value = match[3]
			}
			if len(value) == 0 {
				value = match[4]
			}
			attributes[strings.ToLower(string(match[1]))] = strings.ToLower(string(value))
		}
		switch strings.ToLower(string(found[1])) {
		case "input":
			if attributes["type"] == "password" {
				return false
			}
		case "a":
			if strings.Contains(attributes["href"], "logout") || strings.Contains(attributes["data-url"], "logout") || strings.Contains(attributes["onclick"], "logout") || strings.Contains(attributes["href"], "mybonus") || strings.Contains(attributes["href"], "usercp") {
				loginMarker = true
			}
		case "form":
			loginMarker = loginMarker || strings.Contains(attributes["action"], "logout")
		case "div":
			loginMarker = loginMarker || attributes["class"] == "user-info-side"
		}
	}
	return loginMarker
}
