package config

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed default_config.yaml
var initialTemplate []byte

// InitializeSigningKey runs under the startup directory's exclusive lock.
// Re-read the file under the lock and keep all existing account settings.
func InitializeSigningKey(path string) error {
	if _, err := LoadApplication(path); !errors.Is(err, ErrMissingAPIKey) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("set security.api_key in the original regular configuration file")
	}
	store := NewStore(path)
	root, original, mode, err := store.read()
	if err != nil {
		return err
	}
	security, err := findNodePath(root, "security", false)
	if err != nil {
		return err
	}
	if security != nil && security.Kind != yaml.MappingNode && security.ShortTag() != "!!null" {
		return errors.New("set security.api_key in the original security mapping")
	}
	var secret [24]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return err
	}
	if err := setNodePath(root, "security.api_key", hex.EncodeToString(secret[:])); err != nil {
		return err
	}
	doc := yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	updated, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	// Keep the exact pre-migration document without consuming an existing .bak.
	backup := path + ".pre-go-key.bak"
	if err := os.Link(path, backup); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		prior, statErr := os.Lstat(backup)
		if statErr != nil || !os.SameFile(info, prior) {
			return fmt.Errorf("a different pre-migration configuration already exists at %s", backup)
		}
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		return errors.New("configuration changed during initialization")
	}
	if err := syncConfigDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := atomicWrite(path, updated, mode); err != nil {
		return err
	}
	return syncConfigDirectory(filepath.Dir(path))
}

func priorData(name, configName string) bool {
	for _, db := range []string{"media.db", "user.db"} {
		if name == db || strings.HasPrefix(name, db+".") || strings.HasPrefix(name, db+"-") {
			return true
		}
	}
	return name == configName+".bak" || name == configName+".pre-go-key.bak" || name == ".nastool-restore"
}

// InitializeApplication publishes an entire private config only on first use.
// A missing config alongside an existing database must be recovered with the
// original credentials. Creating another administrator would change ownership.
func InitializeApplication(path string) (bool, error) {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if priorData(entry.Name(), filepath.Base(path)) {
			if _, err := os.Lstat(path); err == nil {
				return false, nil // Another first start already published it.
			}
			return false, fmt.Errorf("existing NAS Tools data in %s has no application config: restore config.yaml or set NASTOOL_CONFIG to its location", dir)
		}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(initialTemplate, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return false, errors.New("initial configuration template must contain a YAML mapping")
	}
	root := doc.Content[0]
	var app Application
	if err := root.Decode(&app); err != nil || app.App.LoginUser == "" {
		return false, errors.New("initial configuration template requires app.login_user")
	}
	for _, field := range []string{"app.login_password", "security.api_key"} {
		var secret [24]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return false, err
		}
		if err := setNodePath(root, field, hex.EncodeToString(secret[:])); err != nil {
			return false, err
		}
	}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return false, err
	}
	file, err := os.CreateTemp(dir, ".nastool-init-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return false, errors.Join(err, closeErr)
	}
	// A simultaneous initializer, restored file or dangling symlink wins.
	if err := os.Link(file.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("publish initial configuration: %w", err)
	}
	return true, syncConfigDirectory(dir)
}

func syncConfigDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
