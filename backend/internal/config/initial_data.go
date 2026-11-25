package config

import (
	"fmt"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// SyncInitializationFiles reads the original execution record and appends any
// missing completed script names. A read or an already complete list is read-only.
func (store *Store) SyncInitializationFiles(completed ...string) ([]string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	root, original, mode, err := store.read()
	if err != nil {
		return nil, err
	}
	var data struct {
		App struct {
			Files []string `yaml:"init_files"`
		} `yaml:"app"`
	}
	if err := root.Decode(&data); err != nil {
		return nil, fmt.Errorf("read app.init_files: %w", err)
	}
	files := data.App.Files
	for _, name := range completed {
		if !slices.Contains(files, name) {
			files = append(files, name)
		}
	}
	if len(files) == len(data.App.Files) {
		return files, nil
	}
	app, err := findNodePath(root, "app", false)
	if err != nil {
		return nil, err
	}
	if app == nil {
		// A root-level merge may supply the app mapping. Materialize an
		// independent override without copying anchors or modifying its source.
		var effective struct {
			App map[string]any `yaml:"app"`
		}
		if err := root.Decode(&effective); err != nil {
			return nil, err
		}
		if err := setNodePath(root, "app", effective.App); err != nil {
			return nil, err
		}
		app, err = findNodePath(root, "app", false)
		if err != nil {
			return nil, err
		}
	}
	if app.Kind == yaml.AliasNode {
		alias := *app
		*app = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, &alias,
		}}
	}
	if app.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid app configuration")
	}
	list, err := findNodePath(app, "init_files", false)
	if err != nil {
		return nil, err
	}
	if list != nil && list.Kind == yaml.SequenceNode {
		for _, name := range files[len(data.App.Files):] {
			list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name})
		}
	} else if err := setNodePath(app, "init_files", files); err != nil {
		return nil, err
	}
	if err := store.write(root, original, mode); err != nil {
		return nil, err
	}
	return files, syncConfigDirectory(filepath.Dir(store.path))
}
