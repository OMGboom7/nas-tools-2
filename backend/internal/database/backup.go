package database

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const MigrationBackupSuffix = ".pre-go.bak"

// EnsureMigrationBackup creates one transactionally consistent SQLite
// snapshot before Go starts mutating an existing Python-era database. The
// snapshot is intentionally retained across restarts and never overwritten.
func EnsureMigrationBackup(databasePath string) (string, error) {
	backupPath := databasePath + MigrationBackupSuffix
	if _, err := os.Stat(backupPath); err == nil {
		if err := verifySQLite(backupPath); err != nil {
			return "", fmt.Errorf("verify existing migration backup: %w", err)
		}
		return backupPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat migration backup: %w", err)
	}
	if _, err := os.Stat(databasePath); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("stat database before migration backup: %w", err)
	}

	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return "", fmt.Errorf("open database for migration backup: %w", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	escaped := strings.ReplaceAll(filepath.Clean(backupPath), "'", "''")
	if _, err := database.Exec("VACUUM INTO '" + escaped + "'"); err != nil {
		return "", fmt.Errorf("create migration backup: %w", err)
	}
	if err := os.Chmod(backupPath, 0o600); err != nil {
		return "", fmt.Errorf("secure migration backup: %w", err)
	}
	if err := verifySQLite(backupPath); err != nil {
		return "", fmt.Errorf("verify migration backup: %w", err)
	}
	return backupPath, nil
}

func verifySQLite(path string) error {
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return err
	}
	defer database.Close()
	var result string
	if err := database.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite quick_check returned %q", result)
	}
	return nil
}
