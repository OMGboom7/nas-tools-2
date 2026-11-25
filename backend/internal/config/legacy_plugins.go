package config

import (
	"errors"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

func legacyPluginSettings(root *yaml.Node) (map[string]map[string]any, error) {
	var data struct {
		Subtitle   any `yaml:"subtitle"`
		Laboratory struct {
			Groups any `yaml:"release_groups"`
		} `yaml:"laboratory"`
	}
	if err := root.Decode(&data); err != nil {
		return nil, errors.New("invalid legacy subtitle or release group configuration")
	}
	settings := make(map[string]map[string]any)
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
		settings[plugin] = make(map[string]any, len(keys))
		for _, key := range keys {
			settings[plugin][key] = fields[key]
		}
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
