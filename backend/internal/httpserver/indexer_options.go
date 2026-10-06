package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

// The boolean reports whether all installed indexer providers are supported.
func (service subscriptionService) nativeIndexerOptions(ctx context.Context) (map[string]any, bool, error) {
	if service.catalogPath == "" || service.configStore == nil || service.systemConfig == nil || service.sites == nil {
		return nil, false, nil
	}
	plugins, err := service.systemConfig.Get(ctx, "UserInstalledPlugins")
	if err != nil {
		return nil, true, err
	}
	installed, err := decodeInstalledPlugins(plugins)
	if err != nil {
		return nil, true, err
	}
	if plugins != "" {
		for _, id := range installed {
			// These metadata plugins are consumed directly by Go and do not
			// contribute indexers. Other plugin capabilities remain unmigrated.
			if id != "CustomReleaseGroups" && id != "Customization" && id != "CustomHosts" && id != "Jackett" && id != "Prowlarr" {
				return nil, false, nil
			}
		}
	}
	selectedValue, err := service.systemConfig.Get(ctx, "UserIndexerSites")
	if err != nil {
		return nil, true, err
	}
	var selected []string
	if selectedValue != "" {
		if err := json.Unmarshal([]byte(selectedValue), &selected); err != nil {
			return nil, true, err
		}
	}
	catalog, err := indexercatalog.Load(service.catalogPath)
	if err != nil {
		return nil, true, err
	}
	sites, err := service.sites.List(ctx)
	if err != nil {
		return nil, true, err
	}
	configuration, err := service.configStore.Snapshot()
	if err != nil {
		return nil, true, err
	}
	laboratory, _ := configuration["laboratory"].(map[string]any)
	items := catalog.Selected(sites, selected, truthy(laboratory["show_more_sites"]))
	values := make([]any, 0, len(items))
	for _, item := range items {
		values = append(values, map[string]any{"id": item.ID, "name": item.Name, "domain": item.Domain, "public": item.Public})
	}
	allowed := map[string]bool{}
	for _, id := range selected {
		allowed[id] = true
	}
	seen := map[string]bool{}
	for _, id := range installed {
		if (id != "Jackett" && id != "Prowlarr") || seen[id] {
			continue
		}
		seen[id] = true
		raw, err := service.systemConfig.Get(ctx, "plugin."+id)
		if err != nil {
			return nil, true, err
		}
		pluginValues, err := decodeMetadataConfig(raw)
		if err != nil {
			return nil, true, err
		}
		configuration := externalindexer.Config{Kind: id}
		for key, target := range map[string]*string{"host": &configuration.Host, "api_key": &configuration.APIKey, "password": &configuration.Password} {
			if value, exists := pluginValues[key]; exists {
				parsed, ok := value.(string)
				if !ok {
					return nil, true, externalindexer.ErrConfig
				}
				*target = parsed
			}
		}
		if configuration.Host == "" && configuration.APIKey == "" {
			continue
		}
		var transport http.RoundTripper
		if service.client != nil {
			transport = service.client.Transport
		}
		indexers, err := externalindexer.Discover(ctx, configuration, transport)
		if err != nil {
			return nil, true, err
		}
		for _, item := range indexers {
			if allowed[item.ID] {
				values = appendIndexerOption(values, item)
			}
		}
	}
	return map[string]any{"code": 0, "data": map[string]any{"indexers": values}}, true, nil
}

func appendIndexerOption(options []any, item externalindexer.Indexer) []any {
	return append(options, map[string]any{"id": item.ID, "name": item.Name, "domain": indexercatalog.Domain(item.Domain), "public": item.Public})
}

func (service subscriptionService) serveCompatIndexers(response http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	result, handled, err := service.nativeIndexerOptions(ctx)
	if !handled {
		writeAPIError(response, http.StatusNotImplemented, 501, "plugin indexers or site catalog are not migrated")
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 500, "indexer options could not be loaded")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "indexers": legacyPayload(result)["indexers"]})
}
