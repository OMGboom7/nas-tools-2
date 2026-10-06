package httpserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func (service downloadService) fetchMTeamSearchTorrent(ctx context.Context, detail string, sites []siteconfig.Site, configuration map[string]any) ([]byte, error) {
	var matched *siteconfig.Site
	for index := range sites {
		address := sites[index].SignURL
		if address == "" {
			address = sites[index].RSSURL
		}
		if indexercatalog.Domain(address) == indexercatalog.Domain(detail) {
			if matched != nil {
				return nil, errors.New("ambiguous MTeam configuration")
			}
			matched = &sites[index]
		}
	}
	if matched == nil || matched.APIKey == "" {
		return nil, errors.New("MTeam API key is unavailable")
	}
	var options map[string]any
	if matched.Note != "" && json.Unmarshal([]byte(matched.Note), &options) != nil {
		return nil, errors.New("invalid MTeam settings")
	}
	app := objectValue(configuration["app"])
	ua := text(options["ua"])
	if ua == "" {
		ua = text(app["user_agent"])
	}
	if ua == "" {
		ua = defaultSiteUserAgent
	}
	transport := service.client.Transport
	if truthy(options["proxy"]) || options["proxy"] == "Y" {
		var err error
		transport, err = siteProxyTransport(transport, objectValue(app["proxies"]), "https")
		if err != nil {
			return nil, err
		}
	}
	return builtinindexer.DownloadMTeam(ctx, detail, matched.APIKey, ua, transport)
}
