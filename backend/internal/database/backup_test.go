package database

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestEnsureMigrationBackupCreatesVerifiedImmutableSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	if _, err := database.Exec("CREATE TABLE items (value TEXT); INSERT INTO items VALUES ('before')"); err != nil {
		t.Fatalf("create fixture database: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close fixture database: %v", err)
	}

	backup, err := EnsureMigrationBackup(path)
	if err != nil {
		t.Fatalf("EnsureMigrationBackup() error = %v", err)
	}
	if backup != path+MigrationBackupSuffix {
		t.Fatalf("backup path = %q", backup)
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %o", info.Mode().Perm())
	}

	original, _ := sql.Open("sqlite", path)
	if _, err := original.Exec("UPDATE items SET value = 'after'"); err != nil {
		t.Fatalf("update original database: %v", err)
	}
	_ = original.Close()
	if _, err := EnsureMigrationBackup(path); err != nil {
		t.Fatalf("second EnsureMigrationBackup() error = %v", err)
	}
	snapshot, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(backup)+"?mode=ro")
	defer snapshot.Close()
	var value string
	if err := snapshot.QueryRow("SELECT value FROM items").Scan(&value); err != nil || value != "before" {
		t.Fatalf("backup value = %q, %v", value, err)
	}
}

func TestEnsureMigrationBackupSkipsMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	backup, err := EnsureMigrationBackup(path)
	if err != nil || backup != "" {
		t.Fatalf("EnsureMigrationBackup(missing) = %q, %v", backup, err)
	}
}
