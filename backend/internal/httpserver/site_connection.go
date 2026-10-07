package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

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

func checkSiteConnectionAdmitted(ctx context.Context, site siteconfig.Site, app map[string]any, transport http.RoundTripper, admit func(context.Context) error) (bool, string, bool) {
	if strings.TrimSpace(site.Cookie) == "" {
		return false, "未配置站点Cookie", false
	}
	note := map[string]any{}
	if site.Note != "" && site.Note != "null" && json.Unmarshal([]byte(site.Note), &note) != nil {
		return false, "站点配置无效", false
	}
	if text(note["chrome"]) == "Y" {
		return false, "浏览器仿真测试尚未迁移", true
	}
	raw := strings.TrimSpace(site.SignURL)
	if raw == "" {
		raw = strings.TrimSpace(site.RSSURL)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false, "未配置有效站点地址", false
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
	method := http.MethodGet
	if strings.Contains(strings.ToLower(parsed.Hostname()), "m-team") {
		if strings.TrimSpace(site.APIKey) == "" {
			return false, "未配置站点API Key", false
		}
		hostname := parsed.Hostname()
		index := strings.Index(strings.ToLower(hostname), "m-team")
		parsed.Host = strings.Replace(parsed.Host, hostname, "api."+hostname[index:], 1)
		parsed.Path = "/api/member/profile"
		method = http.MethodPost
	} else {
		parsed.Path = "/"
		if strings.Contains(strings.ToLower(parsed.Hostname()), "1ptba") || strings.Contains(strings.ToLower(parsed.Hostname()), "zmpt") {
			parsed.Path = "/index.php"
		}
	}
	parsed.RawQuery, parsed.Fragment = "", ""
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
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), nil)
	if err != nil {
		return false, "站点地址无效", false
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Cookie", site.Cookie)
	if method == http.MethodPost {
		request.Header.Set("X-API-KEY", site.APIKey)
	}
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
	if method == http.MethodPost {
		var payload struct {
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(contents, &payload) != nil || len(payload.Data) == 0 || string(payload.Data) == "null" || string(payload.Data) == "false" || string(payload.Data) == "{}" {
			return false, "连接失败", false
		}
		return true, "连接成功", false
	}
	if !siteHTMLLoggedIn(contents) {
		return false, "Cookie失效", false
	}
	return true, "连接成功", false
}

func siteProxyTransport(transport http.RoundTripper, proxies map[string]any, scheme string) (http.RoundTripper, error) {
	raw := strings.TrimSpace(text(proxies[scheme]))
	proxy, err := url.Parse(raw)
	if raw == "" || err != nil || proxy.Hostname() == "" || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
		return nil, http.ErrNotSupported
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	base, ok := transport.(*http.Transport)
	if !ok {
		return nil, http.ErrNotSupported
	}
	configured := base.Clone()
	configured.Proxy = http.ProxyURL(proxy)
	return configured, nil
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
