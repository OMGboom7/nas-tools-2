package dbinit

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestInitializeDatabaseProvisionsEmptyVolume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "新安装 #1%", "config.yaml")
	if err := Run(path, "init-db"); err != nil {
		t.Fatal(err)
	}
	app, err := config.LoadApplication(path)
	if err != nil || len(app.App.LoginPassword) != 48 {
		t.Fatalf("first sign-in unavailable: %v", err)
	}
	before, err := config.LoadApplication(path + ".bak")
	if err != nil || before.App != app.App {
		t.Fatalf("first sign-in backup: %v", err)
	}
	if stat, err := os.Stat(path + ".bak"); err != nil || (runtime.GOOS != "windows" && stat.Mode().Perm()&0077 != 0) {
		t.Fatal("initial configuration backup permissions", err)
	}
	if err := Run(path, "init-db"); err != nil {
		t.Fatal(err)
	}
	files, err := config.NewStore(path).SyncInitializationFiles()
	if err != nil || len(files) != 8 {
		t.Fatalf("initial scripts %v: %v", files, err)
	}
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(path), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM CONFIG_FILTER_RULES").Scan(&count); err != nil || count != 34 {
		t.Fatalf("seeded rules %d: %v", count, err)
	}
}

func TestInitializeDatabaseRequiresConfigForOldVolume(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "media.db")
	if err := os.WriteFile(media, []byte("retained fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	err := Run(filepath.Join(dir, "config.yaml"), "init-db")
	if err == nil || !strings.Contains(err.Error(), "restore config.yaml") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "user.db")); !os.IsNotExist(err) {
		t.Fatal("unexpected new user database", err)
	}
	if got, err := os.ReadFile(media); err != nil || string(got) != "retained fixture" {
		t.Fatal("changed media database", err)
	}
}

func TestInitializeDatabaseRejectsInvalidRecordsBeforeSchemaChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("app: {init_files: {invalid: true}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := Run(path, "init-db")
	if err == nil || !strings.Contains(err.Error(), "init_files") {
		t.Fatal(err)
	}
	for _, name := range []string{"user.db", "media.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected %s after invalid configuration: %v", name, err)
		}
	}
}
