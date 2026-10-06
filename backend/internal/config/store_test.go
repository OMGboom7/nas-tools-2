package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newConfigStoreFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "# top-level comment\napp:\n  login_user: admin\n  login_password: password\n  # keep media comment\n  media_server: emby\n  media_paths:\n    - /media/old@/container/old\nsecurity:\n  api_key: secret\nemby:\n  host: http://old:8096\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	return NewStore(path), path, original
}

func TestStoreUpdatePreservesCommentsAndCreatesBackup(t *testing.T) {
	store, path, original := newConfigStoreFixture(t)
	if err := store.Update(map[string]any{
		"emby.host":        "http://new:8096",
		"emby.play_host":   "http://play:8096",
		"app.media_server": "jellyfin",
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	text := string(contents)
	for _, expected := range []string{"# top-level comment", "# keep media comment", "http://new:8096", "play_host", "jellyfin"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("updated config does not contain %q:\n%s", expected, text)
		}
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read config backup: %v", err)
	}
	if string(backup) != original {
		t.Fatalf("backup changed:\n%s", backup)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot["app"].(map[string]any)["media_server"] != "jellyfin" {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
}

func TestStoreUpdateDirectory(t *testing.T) {
	store, _, _ := newConfigStoreFixture(t)
	if err := store.UpdateDirectory("set", "app.media_paths", "/media/old@/container/old", "/media/new@/container/new"); err != nil {
		t.Fatalf("UpdateDirectory(set) error = %v", err)
	}
	if err := store.UpdateDirectory("add", "app.media_paths", "/media/second", ""); err != nil {
		t.Fatalf("UpdateDirectory(add) error = %v", err)
	}
	if err := store.UpdateDirectory("sub", "app.media_paths", "/media/new@ignored", ""); err != nil {
		t.Fatalf("UpdateDirectory(sub) error = %v", err)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	paths := snapshot["app"].(map[string]any)["media_paths"].([]any)
	if len(paths) != 1 || paths[0] != "/media/second" {
		t.Fatalf("unexpected media paths: %#v", paths)
	}
}

func TestStoreRejectsInvalidPathWithoutChangingFile(t *testing.T) {
	store, path, original := newConfigStoreFixture(t)
	if err := store.Update(map[string]any{"app..host": "bad"}); err == nil {
		t.Fatal("Update() error = nil, want invalid path error")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(contents) != original {
		t.Fatal("invalid update changed config file")
	}
}
