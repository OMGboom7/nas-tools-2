package config

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"gopkg.in/yaml.v3"
)

type LegacySettings struct {
	Records     map[string]any
	Downloaders []LegacyDownloader
	// Only consume shared settings when the selected client has been imported.
	RetireShared bool
	// The client section may already have been moved by an older installation.
	UnresolvedClient string
	SigninCron       string
	application      legacyApplication
}

type LegacyDownloader struct {
	Type                          string
	Enabled, Monitor, OnlyNastool bool
	Mode                          any
	Connection                    map[string]any
	Directories                   []map[string]any
}

func (s LegacySettings) Empty() bool {
	return len(s.Records) == 0 && len(s.Downloaders) == 0 && s.SigninCron == "" && s.application.empty()
}

func (store *Store) LegacySettings() (LegacySettings, error) {
	root, _, _, err := store.read()
	if err != nil {
		return LegacySettings{}, err
	}
	return readLegacySettings(root)
}

var legacyPTKeys = []string{"pt_client", "pt_monitor", "pt_monitor_only", "rmt_mode"}

func readLegacySettings(root *yaml.Node) (LegacySettings, error) {
	var settings LegacySettings
	records, err := legacySystemSettings(root)
	if err != nil {
		return settings, err
	}
	settings.Records = records
	settings.application, err = readLegacyApplication(root)
	if err != nil {
		return settings, err
	}
	var source struct {
		QB   any       `yaml:"qbittorrent"`
		TR   any       `yaml:"transmission"`
		PT   yaml.Node `yaml:"pt"`
		Dirs yaml.Node `yaml:"downloaddir"`
	}
	if err := root.Decode(&source); err != nil {
		return settings, errors.New("invalid legacy downloader configuration")
	}
	var selection struct {
		Client any `yaml:"pt_client"`
		Signin any `yaml:"ptsignin_cron"`
	}
	// Inspect the selector independently: old partially upgraded installations
	// can have a database row and only the original pt_client left in YAML.
	if err := source.PT.Decode(&selection); err == nil {
		if kind, ok := selection.Client.(string); ok && (kind == "qbittorrent" || kind == "transmission") {
			settings.UnresolvedClient = kind
		}
		if !emptyLegacySetting(selection.Signin) {
			settings.SigninCron, err = legacySigninCron(selection.Signin)
			if err != nil {
				return settings, err
			}
		}
	}
	if emptyLegacySetting(source.QB) && emptyLegacySetting(source.TR) {
		return settings, nil
	}
	var pt map[string]any
	if err := source.PT.Decode(&pt); err != nil {
		return settings, errors.New("invalid legacy pt configuration")
	}
	var dirs []map[string]any
	if err := source.Dirs.Decode(&dirs); err != nil {
		return settings, errors.New("invalid legacy download directories")
	}
	mappings := make([]map[string]any, 0, len(dirs))
	for _, dir := range dirs {
		if dir == nil {
			return settings, errors.New("invalid legacy download directory")
		}
		// Keep extension fields as well as the original save/container paths.
		for _, key := range []string{"save_path", "type", "category", "container_path", "label"} {
			if _, exists := dir[key]; !exists {
				dir[key] = nil
			}
		}
		mappings = append(mappings, dir)
	}
	for _, client := range []struct {
		kind, prefix string
		section      any
	}{
		{"qbittorrent", "qb", source.QB}, {"transmission", "tr", source.TR},
	} {
		if emptyLegacySetting(client.section) {
			continue
		}
		fields, ok := client.section.(map[string]any)
		if !ok {
			return settings, errors.New("invalid legacy " + client.kind + " configuration")
		}
		connection := make(map[string]any)
		for _, key := range []string{"host", "port", "username", "password"} {
			connection[key] = fields[client.prefix+key]
		}
		selected := pt["pt_client"] == client.kind
		if selected {
			settings.UnresolvedClient = ""
		}
		settings.Downloaders = append(settings.Downloaders, LegacyDownloader{
			Type: client.kind, Enabled: selected, Monitor: !emptyLegacySetting(pt["pt_monitor"]), OnlyNastool: !emptyLegacySetting(pt["pt_monitor_only"]),
			Mode: pt["rmt_mode"], Connection: connection, Directories: mappings,
		})
		settings.RetireShared = settings.RetireShared || selected || emptyLegacySetting(pt["pt_client"])
	}
	return settings, nil
}

// RetireLegacySettings is the second phase of the database transaction. Both
// families of settings share the same YAML backup and atomic replacement.
func (store *Store) RetireLegacySettings(saved LegacySettings, resolvedSelection bool) error {
	root, original, mode, err := store.read()
	if err != nil {
		return err
	}
	current, err := readLegacySettings(root)
	if err != nil || current.Empty() && current.UnresolvedClient == "" {
		return err
	}
	if !reflect.DeepEqual(current, saved) {
		return errors.New("legacy configuration changed during initialization")
	}
	var rootKeys, ptKeys, labKeys []string
	application := current.application
	if application.OldTMDB {
		labKeys = append(labKeys, "tmdb_proxy")
	}
	if application.OldBaidu {
		rootKeys = append(rootKeys, "baiduocr")
	}
	if application.SetTMDB {
		if err := modifyLegacyMapping(root, "app", nil, map[string]any{"tmdb_domain": "api.tmdb.org"}); err != nil {
			return err
		}
	}
	if err := modifyLegacyMapping(root, "ocr", nil, application.OCR); err != nil {
		return err
	}
	if current.SigninCron != "" {
		ptKeys = append(ptKeys, "ptsignin_cron")
	}
	if current.Records["plugin.DoubanSync"] != nil {
		rootKeys = append(rootKeys, "douban")
	}
	if scraper, ok := current.Records["UserScraperConf"].(map[string]any); ok {
		for _, key := range []string{"scraper_nfo", "scraper_pic"} {
			if _, exists := scraper[key]; exists {
				rootKeys = append(rootKeys, key)
			}
		}
	}
	if current.Records["UserIndexerSites"] != nil {
		ptKeys = append(ptKeys, "indexer_sites")
	}
	if current.Records["plugin.OpenSubtitles"] != nil || current.Records["plugin.ChineseSubFinder"] != nil {
		rootKeys = append(rootKeys, "subtitle")
	}
	if current.Records["plugin.CustomReleaseGroups"] != nil {
		labKeys = append(labKeys, "release_groups")
	}
	for _, client := range current.Downloaders {
		rootKeys = append(rootKeys, client.Type)
	}
	if current.RetireShared {
		rootKeys = append(rootKeys, "downloaddir")
		ptKeys = append(ptKeys, legacyPTKeys...)
	} else if resolvedSelection {
		ptKeys = append(ptKeys, "pt_client")
	}
	if err := modifyLegacyMapping(root, "laboratory", labKeys, nil); err != nil {
		return err
	}
	var ptValues map[string]any
	if application.RefreshDefault {
		ptValues = map[string]any{"ptrefresh_date_cron": "6"}
	}
	if err := modifyLegacyMapping(root, "pt", ptKeys, ptValues); err != nil {
		return err
	}
	if err := modifyLegacyMapping(root, "", rootKeys, nil); err != nil {
		return err
	}
	restoreYAMLDefinitions(root, make(map[*yaml.Node]bool))
	if latest, err := os.ReadFile(store.path); err != nil || !bytes.Equal(original, latest) {
		return errors.New("configuration changed before legacy settings cleanup")
	}
	if err := store.write(root, original, mode); err != nil {
		return err
	}
	return syncConfigDirectory(filepath.Dir(store.path))
}

// Apply local changes without modifying other users of an anchor or merge.
func modifyLegacyMapping(root *yaml.Node, section string, keys []string, values map[string]any) error {
	if len(keys) == 0 && len(values) == 0 {
		return nil
	}
	mapping := root
	if section != "" {
		var err error
		mapping, err = findNodePath(root, section, false)
		if err != nil {
			return err
		}
		// A root-level merge can provide an inherited section. Materialize a
		// local mapping only when this section actually needs to change.
		if mapping == nil || mapping.Kind != yaml.MappingNode && (mapping.Kind != yaml.AliasNode || mapping.Alias.Kind != yaml.MappingNode) {
			var effective map[string]any
			if err := root.Decode(&effective); err != nil {
				return err
			}
			source := effective[section]
			if emptyLegacySetting(source) {
				if len(values) == 0 {
					return nil
				}
				source = map[string]any{}
			}
			switch source.(type) {
			case map[string]any, map[any]any:
			default:
				return errors.New("invalid legacy configuration section " + section)
			}
			if err := setNodePath(root, section, source); err != nil {
				return err
			}
			mapping, err = findNodePath(root, section, false)
			if err != nil {
				return err
			}
		}
		if mapping.Kind == yaml.AliasNode || mapping.Anchor != "" {
			override := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, mapping}}
			for i := 1; i < len(root.Content); i += 2 {
				if root.Content[i] == mapping {
					root.Content[i] = override
					break
				}
			}
			mapping = override
		}
	}
	for _, key := range keys {
		removeMappingKey(mapping, key)
	}
	// An inherited value may still be visible after removing a local override.
	var inherited map[string]any
	if err := mapping.Decode(&inherited); err != nil {
		return err
	}
	for _, key := range keys {
		if inherited[key] != nil {
			if err := setNodePath(mapping, key, nil); err != nil {
				return err
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if err := setNodePath(mapping, key, values[key]); err != nil {
			return err
		}
	}
	return nil
}
