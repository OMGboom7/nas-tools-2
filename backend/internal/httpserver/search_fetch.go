package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/searchcache"
)

var errNativeSearchSelection = errors.New("requested search indexer is not enabled")

// fetchResources is a private execution layer for interactive search and future
// native background planners. It does not mint user tokens, populate browser
// caches, perform per-result display recognition or expose private download URLs.
// Callers must enforce their own HTTP permission or native-worker lifecycle.
// Empty requested means the currently enabled global set, as in legacy RSS.
// Nonempty requested is a strict subset; unavailable selections never widen it.
func (service *nativeExternalResourceSearch) fetchResources(ctx context.Context, keyword string, requested []string) ([]externalindexer.Resource, error) {
	if service.system == nil {
		return nil, errors.New("native indexer configuration is unavailable")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if strings.TrimSpace(keyword) == "" || len([]rune(keyword)) > 120 {
		return nil, errors.New("invalid search keyword")
	}
	raw, err := service.system.Get(ctx, "UserIndexerSites")
	if err != nil {
		return nil, err
	}
	selected := []string{}
	if raw != "" && (len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &selected) != nil || len(selected) > 1024) {
		return nil, errors.New("invalid selected indexers")
	}
	if len(requested) > 1024 {
		return nil, errNativeSearchSelection
	}
	if len(requested) > 0 {
		enabled := map[string]bool{}
		for _, id := range selected {
			enabled[id] = true
		}
		selected = []string{}
		seen := map[string]bool{}
		for _, id := range requested {
			if id == "" || len(id) > 512 || !enabled[id] {
				return nil, errNativeSearchSelection
			}
			if !seen[id] {
				selected = append(selected, id)
				seen[id] = true
			}
		}
	}
	if len(selected) == 0 {
		return []externalindexer.Resource{}, nil
	}
	needed := map[string]bool{}
	allowed := map[string]bool{}
	builtin := map[string]bool{}
	for _, id := range selected {
		kind := "Jackett"
		if strings.HasSuffix(id, "-prowlarr") {
			kind = "Prowlarr"
		} else if !strings.HasSuffix(id, "-jackett") {
			builtin[id] = true
			continue
		}
		needed[kind] = true
		allowed[id] = true
	}
	raw, err = service.system.Get(ctx, "UserInstalledPlugins")
	if err != nil {
		return nil, err
	}
	installed, err := decodeInstalledPlugins(raw)
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for _, id := range installed {
		if id != "Jackett" && id != "Prowlarr" && id != "CustomReleaseGroups" && id != "Customization" && id != "CustomHosts" {
			return nil, errors.New("installed search plugin is not supported natively")
		}
		active[id] = true
	}
	resources := []externalindexer.Resource{}
	if len(builtin) != 0 {
		resources, err = service.searchBuiltin(ctx, builtin, keyword)
		if err != nil {
			return nil, err
		}
	}
	for _, kind := range []string{"Jackett", "Prowlarr"} {
		if !needed[kind] {
			continue
		}
		if !active[kind] {
			return nil, errors.New("selected provider is not installed")
		}
		raw, err := service.system.Get(ctx, "plugin."+kind)
		if err != nil {
			return nil, err
		}
		values, err := decodeMetadataConfig(raw)
		if err != nil {
			return nil, err
		}
		configuration := externalindexer.Config{Kind: kind}
		for key, target := range map[string]*string{"host": &configuration.Host, "api_key": &configuration.APIKey, "password": &configuration.Password} {
			if raw, exists := values[key]; exists {
				value, ok := raw.(string)
				if !ok {
					return nil, externalindexer.ErrConfig
				}
				*target = value
			}
		}
		indexers, err := externalindexer.Discover(ctx, configuration, service.transport)
		if err != nil {
			return nil, err
		}
		for _, indexer := range indexers {
			if !allowed[indexer.ID] {
				continue
			}
			delete(allowed, indexer.ID)
			items, err := externalindexer.Search(ctx, configuration, indexer, keyword, service.transport)
			if err != nil {
				return nil, err
			}
			resources = append(resources, items...)
			if len(resources) > 2000 {
				return nil, searchcache.ErrCapacity
			}
		}
	}
	if len(allowed) > 0 {
		return nil, errors.New("selected indexer is unavailable")
	}
	return resources, nil
}
