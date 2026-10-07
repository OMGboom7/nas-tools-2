package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func (service *nativeExternalResourceSearch) searchBuiltin(ctx context.Context, selected map[string]bool, keyword string) ([]externalindexer.Resource, error) {
	if service.catalogPath == "" || service.sites == nil || service.config == nil {
		return nil, errors.New("native builtin indexers are unavailable")
	}
	catalog, err := indexercatalog.Load(service.catalogPath)
	if err != nil {
		return nil, err
	}
	sites, err := service.sites.List(ctx)
	if err != nil {
		return nil, err
	}
	configuration, err := service.config.Snapshot()
	if err != nil {
		return nil, err
	}
	app := objectValue(configuration["app"])
	public := truthy(objectValue(configuration["laboratory"])["show_more_sites"])
	results := []externalindexer.Resource{}
	seen := map[string]bool{}
	now := time.Now()
	for _, definition := range catalog.Indexers {
		if !selected[definition.ID] || seen[definition.ID] {
			continue
		}
		seen[definition.ID] = true
		cookie, note, key := "", "", ""
		configured := false
		var siteID int64
		for _, site := range sites {
			address := site.SignURL
			if address == "" {
				address = site.RSSURL
			}
			if indexercatalog.Domain(address) != indexercatalog.Domain(definition.Domain) {
				continue
			}
			if configured {
				return nil, errors.New("multiple configured sites match selected indexer")
			}
			configured = true
			siteID = site.ID
			cookie, note = site.Cookie, site.Note
			key = site.APIKey
			definition.Name = site.Name
		}
		authenticated := cookie != ""
		if definition.Parser == "MTeamSpider" {
			authenticated = key != ""
		}
		if (!definition.Public || !public) && (!configured || !authenticated) {
			return nil, errors.New("selected tracker requires a configured session")
		}
		var options map[string]any
		if note != "" && (len(note) > 64<<10 || json.Unmarshal([]byte(note), &options) != nil) {
			return nil, errors.New("invalid tracker request settings")
		}
		ua := text(app["user_agent"])
		if value, exists := options["ua"]; exists {
			var ok bool
			ua, ok = value.(string)
			if !ok {
				return nil, builtinindexer.ErrConfig
			}
		}
		if ua == "" {
			ua = "Mozilla/5.0 (compatible; NAS-Tools-Go)"
		}
		if definition.Parser == "MTeamSpider" || definition.Parser == "TNodeSpider" {
			transport := service.transport
			if truthy(options["proxy"]) || options["proxy"] == "Y" {
				transport, err = siteProxyTransport(transport, objectValue(app["proxies"]), "https")
				if err != nil {
					return nil, err
				}
			}
			var items []externalindexer.Resource
			if configured {
				if err := service.siteLimits.Wait(ctx, service.sites, siteID); err != nil {
					return nil, err
				}
			}
			if definition.Parser == "TNodeSpider" {
				items, err = builtinindexer.SearchTNode(ctx, definition, cookie, ua, keyword, 0, 100, transport)
			} else {
				items, err = builtinindexer.SearchMTeam(ctx, definition, key, ua, keyword, 0, transport)
			}
			if err != nil {
				return nil, err
			}
			results = append(results, items...)
			if len(results) > 2000 {
				return nil, errors.New("builtin search capacity exceeded")
			}
			continue
		}
		empty := ""
		if value, exists := options["go_indexer_empty_selector"]; exists {
			var ok bool
			empty, ok = value.(string)
			if !ok {
				return nil, builtinindexer.ErrConfig
			}
		}
		plan, err := builtinindexer.Build(definition, keyword, 0, "")
		if err != nil {
			return nil, err
		}
		transport := service.transport
		if truthy(options["proxy"]) || options["proxy"] == "Y" {
			transport, err = siteProxyTransport(transport, objectValue(app["proxies"]), plan.URL.Scheme)
			if err != nil {
				return nil, err
			}
		}
		// Cookies/UA come only from the saved site. Never use browser/client credentials.
		if configured {
			if err := service.siteLimits.Wait(ctx, service.sites, siteID); err != nil {
				return nil, err
			}
		}
		body, err := builtinindexer.Fetch(ctx, plan, cookie, ua, transport)
		if err != nil {
			return nil, err
		}
		items, err := builtinindexer.ParseResults(ctx, plan, body, builtinindexer.ResultOptions{Now: now, Limit: 2000 - len(results), EmptySelector: empty})
		if err != nil {
			return nil, err
		}
		results = append(results, items...)
		if len(results) >= 2000 && len(seen) < len(selected) {
			return nil, errors.New("builtin search capacity exceeded")
		}
	}
	if len(seen) != len(selected) {
		return nil, errors.New("selected builtin indexer is unavailable")
	}
	return results, nil
}
