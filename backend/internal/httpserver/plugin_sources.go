package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

// Descriptions and permissions come from classes actually loaded by the old
// application. A changed or removed source needs a fresh declaration.
func (s pluginService) externalPluginCatalog(ctx context.Context, selected string) (map[string]systemconfig.PluginDeclaration, error) {
	raw, err := s.system.Get(ctx, "GoExternalPluginCatalog")
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return map[string]systemconfig.PluginDeclaration{}, nil
	}
	var entries map[string]systemconfig.PluginDeclaration
	if len(raw) > 4<<20 || json.Unmarshal([]byte(raw), &entries) != nil || len(entries) > 4096 {
		return nil, errors.New("invalid external plugin catalog")
	}
	digests := map[string]string{}
	for id, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if selected != "" && selected != id {
			delete(entries, id)
			continue
		}
		name := entry.Source
		if entry.Fields["id"] != id || !systemconfig.ValidStoredPluginID(id) || entry.Level == nil || *entry.Level < 0 || name == "" || !filepath.IsLocal(name) || filepath.Base(name) != name || !strings.EqualFold(filepath.Ext(name), ".py") || len(entry.Digest) != sha256.Size*2 {
			delete(entries, id)
			continue
		}
		digest, checked := digests[name]
		if !checked {
			digest = pluginSourceDigest(filepath.Join(filepath.Dir(s.applicationConfigPath), "plugins", name))
			digests[name] = digest
		}
		if digest != entry.Digest {
			delete(entries, id)
		}
	}
	return entries, nil
}

func pluginSourceDigest(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return ""
	}
	sum := sha256.New()
	if n, err := io.Copy(sum, io.LimitReader(f, (8<<20)+1)); err != nil || n != info.Size() {
		return ""
	}
	current, err := os.Stat(path)
	if err != nil || !os.SameFile(opened, current) || current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func pluginReaderLevel(administrator bool) int {
	if administrator {
		return 2
	}
	return 1
}
