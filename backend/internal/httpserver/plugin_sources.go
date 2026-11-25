package httpserver

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type pluginSourceInventory struct {
	Classes    map[string][]string `json:"classes,omitempty"`
	Files      []string            `json:"files"`
	Incomplete bool                `json:"incomplete,omitempty"`
}

// The old loader copies top-level files, including links to regular files.
// Installation IDs come from classes loaded by that runtime, not from filenames.
func readPluginSourceInventory(configPath string) *pluginSourceInventory {
	inventory := &pluginSourceInventory{Files: []string{}}
	directory := filepath.Join(filepath.Dir(configPath), "plugins")
	entries, err := os.ReadDir(directory)
	if err != nil {
		inventory.Incomplete = !errors.Is(err, fs.ErrNotExist)
		return inventory
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.EqualFold(filepath.Ext(strings.TrimLeft(name, ".")), ".py") {
			continue
		}
		mode := entry.Type()
		if mode&fs.ModeSymlink != 0 {
			info, err := os.Stat(filepath.Join(directory, name))
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					inventory.Incomplete = true
				}
				continue
			}
			mode = info.Mode()
		}
		if mode.IsRegular() {
			inventory.Files = append(inventory.Files, name)
		}
	}
	return inventory
}

// Only associate files still present with identifiers observed by the old loader.
// This snapshot never grants permissions or selects an executor.
func (inventory *pluginSourceInventory) attachClasses(raw string) {
	if raw == "" {
		return
	}
	var known map[string][]string
	if len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &known) != nil {
		inventory.Incomplete = true
		return
	}
	for _, file := range inventory.Files {
		ids := known[file]
		if len(ids) == 0 {
			continue
		}
		if len(ids) > 1024 || slices.ContainsFunc(ids, func(id string) bool { return !systemconfig.ValidStoredPluginID(id) }) {
			inventory.Incomplete = true
			continue
		}
		if inventory.Classes == nil {
			inventory.Classes = make(map[string][]string)
		}
		slices.Sort(ids)
		inventory.Classes[file] = slices.Compact(ids)
	}
}
