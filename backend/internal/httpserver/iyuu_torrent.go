package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/iyuu"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

var errIyuuMissingLink = errors.New("no torrent link in site details")

// Only cache a failure tied to this target. A lost downloader response belongs
// in its receipt, and a local layout mismatch may depend on the source task.
func iyuuCacheKey(err error) string {
	var status siteTorrentHTTPStatus
	if errors.As(err, &status) {
		switch int(status) {
		case http.StatusGone:
			return "permanent_error_caches"
		case http.StatusNotFound:
			return "error_caches"
		}
	}
	if errors.Is(err, errIyuuMissingLink) {
		return "error_caches"
	}
	return ""
}

// The selected local account supplies the origin. A provider template or
// detail page may supply a path, but cannot select the recipient of credentials.
func iyuuDownloadLink(origin, raw string) (string, error) {
	base, err := url.Parse(origin)
	if err != nil {
		return "", iyuu.ErrInput
	}
	ref, err := url.Parse(raw)
	if err != nil || raw == "" || strings.Contains(raw, "\\") || ref.User != nil || ref.Fragment != "" {
		return "", iyuu.ErrInput
	}
	u := base.ResolveReference(ref)
	if u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) || u.Path == "" || u.Path == "/" {
		return "", iyuu.ErrInput
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", iyuu.ErrInput
	}
	// Keep the byte representation of signed parameters from the details page.
	if !q.Has("https") {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += "https=1"
	}
	return u.String(), nil
}

func iyuuTemplate(origin, pattern, id string) (string, bool, error) {
	if pattern == "" {
		return "", true, nil
	}
	u, _ := url.Parse(origin)
	special := []string{"hdchina.org", "hdsky.me", "hdcity.in", "totheglory.im"}
	for _, domain := range special {
		if u.Hostname() == domain || strings.HasSuffix(u.Hostname(), "."+domain) {
			return "", true, nil
		}
	}
	if strings.Contains(pattern, "hash=") || strings.Contains(pattern, "authkey=") {
		return "", true, nil
	}
	tmpl := strings.NewReplacer("id={}", "id={id}", "/{}", "/{id}", "/{torrent_key}", "").Replace(pattern)
	if !strings.Contains(tmpl, "{id}") {
		return "", true, nil
	}
	raw := strings.NewReplacer("{id}", id, "{passkey}", "", "{uid}", "").Replace(tmpl)
	if strings.ContainsAny(raw, "{}") {
		return "", true, nil
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", false, iyuu.ErrInput
	}
	q, err := url.ParseQuery(ref.RawQuery)
	if err != nil {
		return "", false, iyuu.ErrInput
	}
	for k, vs := range q {
		if (strings.EqualFold(k, "passkey") || strings.EqualFold(k, "uid")) && len(vs) == 1 && vs[0] == "" {
			q.Del(k)
		}
	}
	ref.RawQuery = q.Encode()
	link, err := iyuuDownloadLink(origin, ref.String())
	return link, false, err
}

func iyuuExtract(ctx context.Context, body []byte, origin string) (string, error) {
	doc, err := builtinindexer.ParseDocument(ctx, body)
	if err != nil {
		return "", err
	}
	for _, s := range []struct{ query, attr string }{
		{`form[action*="download.php?id="]`, "action"}, {`a[href*="download.php?hash="]`, "href"}, {`a[href*="download.php?id="]`, "href"}, {`a.index[href*="/dl/"]`, "href"}, {`input[title="DirectLink"]`, "value"},
	} {
		nodes, err := builtinindexer.Select(ctx, doc.Root, s.query)
		if err != nil {
			return "", err
		}
		if len(nodes) > 0 {
			for _, v := range nodes[0].Attr {
				if v.Key == s.attr {
					return iyuuDownloadLink(origin, v.Val)
				}
			}
		}
	}
	return "", errIyuuMissingLink
}
func (api *iyuuAPI) seedContent(ctx context.Context, site siteconfig.Site, provider iyuu.Site, id string, guard func(context.Context) error) ([]byte, error) {
	address := site.SignURL
	if address == "" {
		address = site.RSSURL
	}
	u, err := url.Parse(address)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || iyuuHost(address) != iyuuHost(provider.Domain) {
		return nil, iyuu.ErrInput
	}
	origin := u.Scheme + "://" + u.Host + "/"
	service := api.downloads
	cfg, err := service.configStore.Snapshot()
	if err != nil {
		return nil, err
	}
	if err := guard(ctx); err != nil {
		return nil, err
	}
	if _, ok := builtinindexer.MTeamFeedOrigin(address); ok {
		return service.fetchMTeamSearchTorrent(ctx, origin+"detail/"+id, []siteconfig.Site{site}, cfg)
	}
	raw, detail, err := iyuuTemplate(origin, provider.DownloadPage, id)
	if err != nil {
		return nil, err
	}
	var opt map[string]any
	if site.Note != "" && json.Unmarshal([]byte(site.Note), &opt) != nil {
		return nil, iyuu.ErrInput
	}
	page := origin + "details.php?id=" + id + "&hit=1"
	var session *browserruntime.Session
	if truthy(opt["chrome"]) || opt["chrome"] == "Y" {
		home := origin
		if detail {
			home = page
		}
		browser := siteBrowser{renderer: service.browser, sites: service.sites, config: service.configStore, limits: service.siteLimits, transport: service.client.Transport}
		result, s, err := browser.render(ctx, site, cfg, home, nil, "", "", guard)
		if err != nil {
			return nil, err
		}
		session = s
		if detail {
			raw, err = iyuuExtract(ctx, result.HTML, origin)
			if err != nil {
				return nil, err
			}
		}
	} else if detail {
		transport := service.client.Transport
		if truthy(opt["proxy"]) || opt["proxy"] == "Y" {
			transport, err = siteProxyTransport(transport, objectValue(objectValue(cfg["app"])["proxies"]), u.Scheme)
			if err != nil {
				return nil, err
			}
		}
		if err = service.siteLimits.Wait(ctx, service.sites, site.ID); err != nil {
			return nil, err
		}
		if err = guard(ctx); err != nil {
			return nil, err
		}
		p, _ := url.Parse(page)
		body, err := builtinindexer.Fetch(ctx, builtinindexer.Plan{URL: p}, site.Cookie, defaultString(text(opt["ua"]), defaultSiteUserAgent), transport)
		if err != nil {
			return nil, err
		}
		raw, err = iyuuExtract(ctx, body, origin)
		if err != nil {
			return nil, err
		}
	}
	if err := guard(ctx); err != nil {
		return nil, err
	}
	if session != nil {
		_, body, err := service.prepareBrowserSearchTorrent(ctx, externalindexer.Resource{DownloadURL: raw, Browser: session})
		return body, err
	}
	return service.fetchSiteTorrentBound(ctx, site, raw, guard)
}
