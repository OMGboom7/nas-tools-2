package config

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/crypto/pbkdf2"
	"gopkg.in/yaml.v3"
)

//go:embed default_config.yaml
var initialTemplate []byte

// InitialPasswordSuffix identifies the private first-login file on a new volume.
const InitialPasswordSuffix = ".initial-password"

func randomSecret() (string, error) {
	var secret [24]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(secret[:]), nil
}

// HashAdminPassword produces a configuration value accepted by both legacy
// distributions (Werkzeug 2.2 and 2.3) and by native authentication.
func HashAdminPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("administrator password is required")
	}
	salt, err := randomSecret()
	if err != nil {
		return "", err
	}
	const rounds = 600000
	digest := pbkdf2.Key([]byte(password), []byte(salt), rounds, sha256.Size, sha256.New)
	return fmt.Sprintf("[hash]pbkdf2:sha256:%d$%s$%x", rounds, salt, digest), nil
}

// InitializeCredentials re-reads the effective settings under the exclusive
// startup lock. One replacement publishes both values or neither.
func InitializeCredentials(path string) error {
	root, original, mode, err := NewStore(path).read()
	if err != nil {
		return err
	}
	var app Application
	if err := root.Decode(&app); err != nil {
		return errors.New("invalid application authentication settings")
	}
	if app.App.LoginUser == "" || app.App.LoginPassword == "" {
		return errors.New("set app.login_user and app.login_password in the existing configuration")
	}
	plain := !strings.HasPrefix(app.App.LoginPassword, "[hash]")
	missingKey := app.Security.APIKey == ""
	if !plain && !missingKey {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("update credentials in the original regular configuration file")
	}
	if plain {
		hash, err := HashAdminPassword(app.App.LoginPassword)
		if err != nil {
			return err
		}
		if err := modifyLegacyMapping(root, "app", nil, map[string]any{"login_password": hash}); err != nil {
			return err
		}
	}
	if missingKey {
		key, err := randomSecret()
		if err != nil {
			return err
		}
		if err := modifyLegacyMapping(root, "security", nil, map[string]any{"api_key": key}); err != nil {
			return err
		}
	}
	restoreYAMLDefinitions(root, make(map[*yaml.Node]bool))
	updated, err := yaml.Marshal(root)
	if err != nil {
		return err
	}
	if missingKey {
		backup := path + ".pre-go-key.bak"
		if err := publishPrivate(backup, original); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			prior, err := readPrivate(backup)
			if err != nil || !bytes.Equal(prior, original) {
				return fmt.Errorf("a different or inaccessible pre-migration configuration exists at %s", backup)
			}
		}
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		return errors.New("configuration changed during initialization")
	}
	if err := atomicWrite(path, updated, mode); err != nil {
		return err
	}
	return syncConfigDirectory(filepath.Dir(path))
}

func readPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return nil, fmt.Errorf("expected a private regular file at %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("private file changed during initialization")
	}
	return io.ReadAll(file)
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
	// Publish the generated password first so a crash before publishing the
	// config can resume with the same first-login information.
	password, err := randomSecret()
	if err != nil {
		return false, err
	}
	initialPath := path + InitialPasswordSuffix
	if err := publishPrivate(initialPath, []byte(password+"\n")); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return false, err
		}
		saved, err := readPrivate(initialPath)
		if err != nil {
			return false, err
		}
		password = strings.TrimSuffix(string(saved), "\n")
		raw, err := hex.DecodeString(password)
		if err != nil || len(raw) != 24 {
			return false, errors.New("invalid initial-password file; restore the original configuration")
		}
	}
	hash, err := HashAdminPassword(password)
	if err != nil {
		return false, err
	}
	key, err := randomSecret()
	if err != nil {
		return false, err
	}
	if err := setNodePath(root, "app.login_password", hash); err != nil {
		return false, err
	}
	if err := setNodePath(root, "security.api_key", key); err != nil {
		return false, err
	}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return false, err
	}
	if err := publishPrivate(path, data); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("publish initial configuration: %w", err)
	}
	return true, nil
}

// Link a fully synced mode-0600 file without replacing an existing name.
func publishPrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".nastool-init-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if err := os.Link(file.Name(), path); err != nil {
		return err
	}
	return syncConfigDirectory(dir)
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
