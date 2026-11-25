package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConcurrentFirstStartsPublishOneConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 6)
	for i := 0; i < cap(results); i++ {
		go func() {
			created, err := InitializeApplication(path)
			results <- result{created, err}
		}()
	}
	creators := 0
	for i := 0; i < cap(results); i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.created {
			creators++
		}
	}
	if _, err := LoadApplication(path); err != nil || creators != 1 {
		t.Fatal(creators, err)
	}
}

func TestUpgradeRefusesToReplaceBackupOrSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const original = "app: {login_user: fixture, login_password: fixture}\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pre-go-key.bak", []byte("prior backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeCredentials(path); err == nil {
		t.Fatal("overwrote existing backup")
	}
	link := filepath.Join(dir, "linked.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Skip(err)
	}
	if err := InitializeCredentials(link); err == nil {
		t.Fatal("replaced linked config during automatic upgrade")
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != original {
		t.Fatal("configuration changed")
	}
}

func TestCredentialUpgradeResumesAfterBackupAndPreservesAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("defaults: &user {login_user: fixture, login_password: 'an$old$password'}\napp: *user\nother: *user\nsecurity: {api_key: null, extra: keep} # retained\n")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivate(path+".pre-go-key.bak", original); err != nil {
		t.Fatal(err)
	}
	if err := InitializeCredentials(path); err != nil {
		t.Fatal(err)
	}
	upgraded, err := LoadApplication(path)
	if err != nil || !strings.HasPrefix(upgraded.App.LoginPassword, "[hash]pbkdf2:") || len(upgraded.Security.APIKey) != 48 {
		t.Fatal("credentials not upgraded", err)
	}
	root, first, _, err := NewStore(path).read()
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]map[string]string
	if err := root.Decode(&values); err != nil || values["other"]["login_password"] != "an$old$password" || values["security"]["extra"] != "keep" || !strings.Contains(string(first), "# retained") {
		t.Fatal("unrelated settings changed", err)
	}
	if err := InitializeCredentials(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("credentials changed on retry", err)
	}
	backup, err := os.Stat(path + ".pre-go-key.bak")
	if err != nil || (runtime.GOOS != "windows" && backup.Mode().Perm() != 0600) {
		t.Fatal("credential backup not private", err)
	}
	if _, err := os.Lstat(path + InitialPasswordSuffix); !os.IsNotExist(err) {
		t.Fatal("unexpected first-login file for an existing account")
	}
}

func TestFirstStartResumesWithPublishedPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	saved := strings.Repeat("ab", 24) + "\n"
	if err := publishPrivate(path+InitialPasswordSuffix, []byte(saved)); err != nil {
		t.Fatal(err)
	}
	if created, err := InitializeApplication(path); err != nil || !created {
		t.Fatal(created, err)
	}
	data, err := os.ReadFile(path + InitialPasswordSuffix)
	if err != nil || string(data) != saved {
		t.Fatal("published password changed", err)
	}
}
