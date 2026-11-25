package httpserver

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type pluginSourceInventory struct {
	Files      []string `json:"files"`
	Incomplete bool     `json:"incomplete,omitempty"`
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
