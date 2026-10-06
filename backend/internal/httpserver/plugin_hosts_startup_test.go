package httpserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestRestoreNativeHosts(t *testing.T) {
	for _, fixture := range []struct {
		name, installed, values string
		applied, fails          bool
	}{
		{"text", `["CustomHosts"]`, `{"enable":true,"hosts":"127.0.0.2 test.local\nwrong line"}`, true, false},
		{"legacy array", `["CustomHosts"]`, `{"enable":true,"hosts":["127.0.0.2 test.local\n","::1 ipv6.local\n"]}`, true, false},
		{"disabled", `["CustomHosts"]`, `{"enable":false,"hosts":42}`, false, false},
		{"uninstalled", `[]`, `{"enable":true,"hosts":"127.0.0.2 test.local"}`, false, false},
		{"empty", `["CustomHosts"]`, `{"enable":true}`, false, true},
		{"invalid only", `["CustomHosts"]`, `{"enable":true,"hosts":"wrong line"}`, false, true},
		{"bad enable", `["CustomHosts"]`, `{"enable":"true"}`, false, true},
		{"bad array", `["CustomHosts"]`, `{"enable":true,"hosts":[42]}`, false, true},
		{"bad registry", `{}`, `{}`, false, true},
		{"bad config", `["CustomHosts"]`, `null`, false, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store, err := systemconfig.Open(filepath.Join(dir, "user.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			for key, value := range map[string]string{"UserInstalledPlugins": fixture.installed, "plugin.CustomHosts": fixture.values} {
				if err := store.Set(ctx, key, value); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "hosts")
			original := "127.0.0.1 localhost\n"
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := restoreNativeHosts(ctx, store, path)
			if (err != nil) != fixture.fails || result.Applied != fixture.applied {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.applied && !strings.Contains(string(after), "test.local") {
				t.Fatalf("mapping missing: %s", after)
			}
			if !fixture.applied && string(after) != original {
				t.Fatalf("unexpected write: %s", after)
			}
			// Startup restoration never changes saved enable/configuration.
			saved, err := store.Get(ctx, "plugin.CustomHosts")
			if err != nil || saved != fixture.values {
				t.Fatalf("saved=%s err=%v", saved, err)
			}
		})
	}
}

func TestRestoreNativeHostsSafety(t *testing.T) {
	if _, err := restoreNativeHosts(context.Background(), nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreNativeHosts(context.Background(), nil, filepath.Join(t.TempDir(), "hosts")); err == nil {
		t.Fatal("missing store accepted")
	}
	for _, cfg := range []config.Config{{HostsPath: "relative"}, {HostsPath: filepath.Join(t.TempDir(), "hosts")}} {
		if _, err := newHandler(cfg, nil); err == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
	store, err := systemconfig.Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Set(ctx, "UserInstalledPlugins", `["CustomHosts"]`); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, "plugin.CustomHosts", `{"enable":true,"hosts":"127.0.0.2 test.local"}`); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := restoreNativeHosts(ctx, store, missing); err == nil {
		t.Fatal("missing target accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("target was created")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := restoreNativeHosts(cancelled, store, missing); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestServerRestoresHostsBeforeServing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: hosts-test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := systemconfig.Open(filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), "UserInstalledPlugins", `["CustomHosts"]`); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), "plugin.CustomHosts", `{"enable":true,"hosts":"127.0.0.2 test.local"}`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ApplicationConfigPath: configPath, DisableLegacy: true, HostsPath: path}
	if _, err := newHandler(cfg, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(after), "test.local") {
		t.Fatalf("hosts=%s err=%v", after, err)
	}
	cfg.HostsPath = filepath.Join(dir, "missing")
	if handler, err := newHandler(cfg, nil); err == nil || handler != nil {
		t.Fatal("server accepted failed hosts restoration")
	}
}
