package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func (service *nativeExternalResourceSearch) searchBuiltin(ctx context.Context, selected map[string]bool, keyword string) ([]externalindexer.Resource, error) {
	return service.searchBuiltinPage(ctx, selected, keyword, nil)
}

func (service *nativeExternalResourceSearch) searchBuiltinPage(ctx context.Context, selected map[string]bool, keyword string, page *int) (resources []externalindexer.Resource, retErr error) {
	if page != nil && len(selected) != 1 {
		return nil, errNativeSearchSelection
	}
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
		originalSite, configured, err := configuredIndexerSite(definition, sites)
		if err != nil {
			return nil, err
		}
		cookie, key, siteID := originalSite.Cookie, originalSite.APIKey, originalSite.ID
		if configured {
			definition.Name = originalSite.Name
		}
		authenticated := cookie != ""
		if definition.Parser == "MTeamSpider" {
			authenticated = key != ""
		}
		if page != nil {
			if !configured || (definition.Parser != "" && definition.Parser != "HaiDanSpider") {
				return nil, builtinindexer.ErrConfig
			}
			started := time.Now()
			defer func() {
				if err := service.sites.RecordBuiltinRequest(ctx, originalSite.Name, started, retErr == nil); err != nil {
					slog.Warn("could not record site resource request statistics")
				}
			}()
		}
		if (!definition.Public || !public) && (!configured || !authenticated) {
			return nil, errors.New("selected tracker requires a configured session")
		}
		options, ua, err := indexerSiteSettings(originalSite.Note, app)
		if err != nil {
			return nil, err
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
			for i := range items {
				items[i].Public = definition.Public
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
		var plan builtinindexer.Plan
		limit := 2000 - len(results)
		if page == nil {
			plan, err = builtinindexer.Build(definition, keyword, 0, "")
		} else {
			limit = 100
			if raw := text(objectValue(configuration["pt"])["site_search_result_num"]); raw != "" && raw != "0" {
				limit, err = strconv.Atoi(raw)
				if err != nil || limit < 1 || limit > 2000 {
					return nil, builtinindexer.ErrConfig
				}
			}
			plan, err = builtinindexer.BuildResourcePage(definition, keyword, *page)
		}
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
		var body []byte
		var session *browserruntime.Session
		if plan.Form != nil || options["chrome"] == "Y" || truthy(options["chrome"]) {
			var rules struct {
				List struct {
					Selector string `json:"selector"`
				} `json:"list"`
			}
			if json.Unmarshal(definition.Torrents, &rules) != nil {
				return nil, builtinindexer.ErrConfig
			}
			catalogHash := browserruntime.Hash(catalog)
			guard := func(ctx context.Context) error {
				current, e := indexercatalog.Load(service.catalogPath)
				if e != nil || browserruntime.Hash(current) != catalogHash {
					return browserruntime.ErrScope
				}
				return ctx.Err()
			}
			page, private, e := (siteBrowser{renderer: service.browser, sites: service.sites, config: service.config, limits: service.siteLimits, transport: service.transport}).render(ctx, originalSite, configuration, plan.URL.String(), plan.Form, rules.List.Selector, empty, guard)
			if e != nil {
				return nil, e
			}
			body, session = page.HTML, private
		} else {
			if configured {
				if err := service.siteLimits.Wait(ctx, service.sites, siteID); err != nil {
					return nil, err
				}
			}
			body, err = builtinindexer.Fetch(ctx, plan, cookie, ua, transport)
		}
		if err != nil {
			return nil, err
		}
		items, err := builtinindexer.ParseResults(ctx, plan, body, builtinindexer.ResultOptions{Now: now, Limit: limit, EmptySelector: empty})
		if err != nil {
			return nil, err
		}
		for i := range items {
			items[i].Public = definition.Public
			items[i].Browser = session
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

func configuredIndexerSite(definition indexercatalog.Definition, sites []siteconfig.Site) (siteconfig.Site, bool, error) {
	domain := indexercatalog.Domain(definition.Domain)
	var matched siteconfig.Site
	for _, site := range sites {
		address := site.SignURL
		if address == "" {
			address = site.RSSURL
		}
		if indexercatalog.Domain(address) != domain {
			continue
		}
		if matched.ID != 0 {
			return siteconfig.Site{}, false, errors.New("multiple configured sites match selected indexer")
		}
		matched = site
	}
	return matched, matched.ID != 0, nil
}

func indexerSiteSettings(note string, app map[string]any) (map[string]any, string, error) {
	var options map[string]any
	if note != "" && (len(note) > 64<<10 || json.Unmarshal([]byte(note), &options) != nil) {
		return nil, "", errors.New("invalid tracker request settings")
	}
	ua := text(app["user_agent"])
	if value, exists := options["ua"]; exists {
		var ok bool
		ua, ok = value.(string)
		if !ok {
			return nil, "", builtinindexer.ErrConfig
		}
	}
	if ua == "" {
		ua = "Mozilla/5.0 (compatible; NAS-Tools-Go)"
	}
	return options, ua, nil
}
