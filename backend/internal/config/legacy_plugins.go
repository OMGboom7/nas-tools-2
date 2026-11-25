package config

import (
	"errors"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

func legacySystemSettings(root *yaml.Node) (map[string]any, error) {
	var data struct {
		Douban     any `yaml:"douban"`
		NFO        any `yaml:"scraper_nfo"`
		Pictures   any `yaml:"scraper_pic"`
		PT         any `yaml:"pt"`
		Subtitle   any `yaml:"subtitle"`
		Laboratory struct {
			Groups any `yaml:"release_groups"`
		} `yaml:"laboratory"`
	}
	if err := root.Decode(&data); err != nil {
		return nil, errors.New("invalid legacy system settings")
	}
	settings := make(map[string]any)
	if groups := data.Laboratory.Groups; !emptyLegacySetting(groups) {
		value, ok := groups.(string)
		if !ok {
			return nil, errors.New("invalid legacy release groups")
		}
		settings["plugin.CustomReleaseGroups"] = map[string]any{"release_groups": value}
	}
	if !emptyLegacySetting(data.Subtitle) {
		subtitle, ok := data.Subtitle.(map[string]any)
		if !ok {
			return nil, errors.New("invalid legacy subtitle configuration")
		}
		provider, plugin := "chinesesubfinder", "plugin.ChineseSubFinder"
		keys := []string{"host", "api_key", "local_path", "remote_path"}
		if subtitle["server"] == "opensubtitles" {
			provider, plugin, keys = "opensubtitles", "plugin.OpenSubtitles", []string{"enable"}
		}
		fields, ok := subtitle[provider].(map[string]any)
		if !ok && subtitle[provider] != nil {
			return nil, fmt.Errorf("invalid legacy %s configuration", provider)
		}
		values := make(map[string]any, len(keys))
		for _, key := range keys {
			values[key] = fields[key]
		}
		settings[plugin] = values
	}
	if !emptyLegacySetting(data.Douban) {
		source, ok := data.Douban.(map[string]any)
		if !ok {
			return nil, errors.New("invalid legacy DoubanSync configuration")
		}
		fields := map[string]any{"onlyonce": false, "enable": !emptyLegacySetting(source["users"]) && !emptyLegacySetting(source["interval"]) && !emptyLegacySetting(source["types"])}
		for _, key := range []string{"interval", "auto_search", "auto_rss", "cookie", "users", "days", "types"} {
			fields[key] = source[key]
		}
		// Unquoted numeric YAML account IDs must reach the Go consumer as
		// exact decimal strings, including IDs too large for a JSON float.
		switch ids := source["users"].(type) {
		case int, uint64:
			fields["users"] = fmt.Sprint(ids)
		case []any:
			for i, id := range ids {
				switch id.(type) {
				case int, uint64:
					ids[i] = fmt.Sprint(id)
				}
			}
		}
		settings["plugin.DoubanSync"] = fields
	}
	scraper := map[string]any{}
	for key, raw := range map[string]any{"scraper_nfo": data.NFO, "scraper_pic": data.Pictures} {
		if !emptyLegacySetting(raw) {
			scraper[key] = raw
		}
	}
	if len(scraper) > 0 {
		settings["UserScraperConf"] = scraper
	}
	pt, _ := data.PT.(map[string]any)
	if raw := pt["indexer_sites"]; !emptyLegacySetting(raw) {
		sites, ok := raw.([]any)
		if !ok {
			return nil, errors.New("invalid legacy pt.indexer_sites list")
		}
		selected := make([]string, len(sites))
		for i, site := range sites {
			var ok bool
			if selected[i], ok = site.(string); !ok {
				return nil, errors.New("invalid legacy pt.indexer_sites entry")
			}
		}
		settings["UserIndexerSites"] = selected
	}
	return settings, nil
}

// Older configurations also used false, zero, or an empty sequence for a
// disabled section. Leave those values alone, as the old initializer did.
func emptyLegacySetting(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case bool:
		return !v
	case int:
		return v == 0
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

func restoreYAMLDefinitions(node *yaml.Node, emitted map[*yaml.Node]bool) {
	if node.Anchor != "" {
		emitted[node] = true
	}
	for i, child := range node.Content {
		if child.Kind == yaml.AliasNode && !emitted[child.Alias] {
			child = child.Alias
			node.Content[i] = child
		}
		restoreYAMLDefinitions(child, emitted)
	}
}

func removeMappingKey(mapping *yaml.Node, name string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			mapping.Content = slices.Delete(mapping.Content, i, i+2)
			return
		}
	}
}
