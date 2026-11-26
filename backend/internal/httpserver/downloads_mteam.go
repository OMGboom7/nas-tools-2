package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func siteMatchesMTeamDownload(site siteconfig.Site, raw string) bool {
	origin := site.SignURL
	if origin == "" {
		origin = site.RSSURL
	}
	return builtinindexer.MTeamDownloadMatches(origin, raw) || (builtinindexer.IsSignedMTeamDownload(raw) && builtinindexer.MTeamDownloadMatches(site.RSSURL, raw))
}

func mteamDownloadSite(sites []siteconfig.Site, raw string) (siteconfig.Site, error) {
	var matched siteconfig.Site
	link, err := url.Parse(raw)
	if err != nil {
		return matched, err
	}
	best, count := 0, 0
	for _, site := range sites {
		if !siteMatchesMTeamDownload(site, raw) {
			continue
		}
		// Prefer an explicitly configured origin over a derived API alias.
		score := 1
		for _, address := range []string{site.SignURL, site.RSSURL} {
			u, e := url.Parse(address)
			if e == nil && u.User == nil && u.Scheme == link.Scheme && strings.EqualFold(u.Host, link.Host) {
				score = 2
				break
			}
		}
		if score > best {
			matched, best, count = site, score, 1
		} else if score == best {
			count++
		}
	}
	if count > 1 {
		return siteconfig.Site{}, errors.New("ambiguous M-Team configuration")
	}
	return matched, nil
}

func (service downloadService) fetchMTeamSearchTorrent(ctx context.Context, raw string, sites []siteconfig.Site, configuration map[string]any) ([]byte, error) {
	site, err := mteamDownloadSite(sites, raw)
	if err != nil {
		return nil, err
	}
	if site.APIKey == "" {
		return nil, errors.New("M-Team API key is unavailable")
	}
	ua, transport, err := service.siteReader().mteamSiteRequest(ctx, site, configuration, false)
	if err != nil {
		return nil, err
	}
	return builtinindexer.DownloadMTeam(ctx, raw, site.APIKey, ua, transport)
}

func (service downloadService) fetchMTeamSiteLink(ctx context.Context, raw string, site siteconfig.Site, configuration map[string]any, taskProxy bool) ([]byte, error) {
	ua, transport, err := service.siteReader().mteamSiteRequest(ctx, site, configuration, taskProxy)
	if err != nil {
		return nil, err
	}
	contents, err := builtinindexer.FetchMTeamLink(ctx, raw, site.APIKey, ua, transport)
	if err == nil && bytes.HasPrefix(contents, []byte("magnet:?")) && !validMagnet(string(contents)) {
		return nil, builtinindexer.ErrResponse
	}
	return contents, err
}

func (reader siteBrowser) mteamSiteRequest(ctx context.Context, site siteconfig.Site, configuration map[string]any, taskProxy bool) (string, http.RoundTripper, error) {
	app := objectValue(configuration["app"])
	var options map[string]any
	if site.Note != "" && json.Unmarshal([]byte(site.Note), &options) != nil {
		return "", nil, errors.New("invalid M-Team settings")
	}
	ua := text(options["ua"])
	if ua == "" {
		ua = text(app["user_agent"])
	}
	if ua == "" {
		ua = defaultSiteUserAgent
	}
	transport := reader.transport
	if taskProxy || truthy(options["proxy"]) || options["proxy"] == "Y" {
		var err error
		transport, err = siteProxyTransport(transport, objectValue(app["proxies"]), "https")
		if err != nil {
			return "", nil, err
		}
	}
	if site.ID > 0 {
		if err := reader.waitForSite(ctx, site.ID); err != nil {
			return "", nil, err
		}
	}
	return ua, transport, nil
}
