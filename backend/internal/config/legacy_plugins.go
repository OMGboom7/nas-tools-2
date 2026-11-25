package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"gopkg.in/yaml.v3"
)

// LegacyPluginSettings returns the effective values, including YAML merges.
// The installer writes these to user.db before removing their original keys.
func (store *Store) LegacyPluginSettings() (map[string]map[string]any, error) {
	root, _, _, err := store.read()
	if err != nil {
		return nil, err
	}
	return legacyPluginSettings(root)
}

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

// RetireLegacyPluginSettings runs under the database initialization write lock.
// Re-read before cleanup so unrelated edits survive and changed source settings
// cannot be removed based on an earlier snapshot.
func (store *Store) RetireLegacyPluginSettings(saved map[string]map[string]any) error {
	root, original, mode, err := store.read()
	if err != nil {
		return err
	}
	current, err := legacyPluginSettings(root)
	if err != nil || len(current) == 0 {
		return err
	}
	if !reflect.DeepEqual(current, saved) {
		return errors.New("legacy plugin configuration changed during initialization")
	}
	if current["plugin.OpenSubtitles"] != nil || current["plugin.ChineseSubFinder"] != nil {
		removeMappingKey(root, "subtitle")
		// A merge can supply the old value again after deleting an override.
		var inherited struct {
			Subtitle any `yaml:"subtitle"`
		}
		if err := root.Decode(&inherited); err != nil {
			return err
		}
		if inherited.Subtitle != nil {
			if err := setNodePath(root, "subtitle", nil); err != nil {
				return err
			}
		}
	}
	if current["plugin.CustomReleaseGroups"] != nil {
		if err := retireReleaseGroups(root); err != nil {
			return err
		}
	}
	// Removing an old setting can remove the definition of an anchor still
	// used by unrelated settings. Promote its first surviving alias in place.
	restoreYAMLDefinitions(root, make(map[*yaml.Node]bool))
	if latest, err := os.ReadFile(store.path); err != nil || !bytes.Equal(original, latest) {
		return errors.New("configuration changed before legacy plugin cleanup")
	}
	if err := store.write(root, original, mode); err != nil {
		return err
	}
	return syncConfigDirectory(filepath.Dir(store.path))
}

func retireReleaseGroups(root *yaml.Node) error {
	lab, err := findNodePath(root, "laboratory", false)
	if err != nil {
		return err
	}
	if lab == nil {
		// A root merge supplied the entire laboratory mapping. Override its
		// effective values locally, leaving the source of the merge intact.
		var inherited struct {
			Laboratory any `yaml:"laboratory"`
		}
		if err := root.Decode(&inherited); err != nil {
			return err
		}
		switch values := inherited.Laboratory.(type) {
		case map[string]any:
			delete(values, "release_groups")
		case map[any]any:
			delete(values, "release_groups")
		default:
			return errors.New("invalid laboratory configuration")
		}
		return setNodePath(root, "laboratory", inherited.Laboratory)
	}
	if lab.Kind == yaml.AliasNode || lab.Anchor != "" {
		// Preserve the shared mapping in its original position and mask the
		// old key locally. Other aliases continue to see their original data.
		override := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, lab,
		}}
		for i := 1; i < len(root.Content); i += 2 {
			if root.Content[i] == lab {
				root.Content[i] = override
				break
			}
		}
		return setNodePath(override, "release_groups", nil)
	}
	removeMappingKey(lab, "release_groups")
	var inherited struct {
		Groups any `yaml:"release_groups"`
	}
	if err := lab.Decode(&inherited); err != nil {
		return err
	}
	if inherited.Groups != nil {
		return setNodePath(lab, "release_groups", nil)
	}
	return nil
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
