package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestNativeFirstStartAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "config.yaml")
	t.Setenv("NASTOOL_CONFIG", path)
	cfg := config.Config{DisableLegacy: true, ApplicationConfigPath: path}
	start := func() config.Application {
		t.Helper()
		release, err := holdApplication(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		app, err := config.LoadApplication(path)
		if err != nil {
			t.Fatal(err)
		}
		return app
	}
	app := start()
	if app.App.LoginUser != "admin" || len(app.App.LoginPassword) != 48 || len(app.Security.APIKey) != 48 || app.App.LoginPassword == app.Security.APIKey {
		t.Fatal("expected a private password and a distinct signing key")
	}
	info, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("initial configuration permissions", err)
	}
	if app != start() {
		t.Fatal("credentials changed on restart")
	}
}

func TestNativeUpgradePreservesOldConfiguration(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("startup locking requires flock")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("# retained comment\napp: {login_user: fixture, login_password: '[hash]old'}\nsecurity: {api_key: null, extra: keep}\ncustom: original\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak", []byte("older backup"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ApplicationConfigPath: path, DisableLegacy: true}
	reader, err := backuprestore.HoldStartup(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = holdApplication(cfg)
	reader()
	if !errors.Is(err, backuprestore.ErrBusy) {
		t.Fatalf("upgrade during active service: %v", err)
	}
	if _, err := os.Lstat(path + ".pre-go-key.bak"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("active service allowed upgrade")
	}
	release, err := holdApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	app, err := config.LoadApplication(path)
	if err != nil || app.App.LoginPassword != "[hash]old" || app.App.LoginUser != "fixture" || len(app.Security.APIKey) != 48 {
		t.Fatal("existing credentials lost or signing key missing", err)
	}
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if !bytes.Equal(read(path+".pre-go-key.bak"), original) || string(read(path+".bak")) != "older backup" {
		t.Fatal("previous config or backup changed")
	}
	updated := string(read(path))
	for _, part := range []string{"# retained comment", "extra: keep", "custom: original"} {
		if !strings.Contains(updated, part) {
			t.Fatalf("upgrade lost %q", part)
		}
	}
	second, err := holdApplication(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second()
	if string(read(path)) != updated {
		t.Fatal("second startup modified the configuration")
	}
}

func TestStartupRejectsOrphanedData(t *testing.T) {
	for _, name := range []string{"user.db", "media.db-wal", "config.yaml.pre-go-key.bak", "user.db.pre-go.bak"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(filepath.Join(dir, name), []byte("existing data"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := holdApplication(config.Config{ApplicationConfigPath: path, DisableLegacy: true}); err == nil {
				t.Fatal("new credentials issued alongside orphaned data")
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("configuration unexpectedly created")
			}
		})
	}
}
