package database

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/0xforee/nas-tools/backend/internal/fsutil"
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

	dsn, err := sqliteFileURL(databasePath)
	if err != nil {
		return "", err
	}
	dsn.RawQuery = "mode=ro&_pragma=busy_timeout(10000)"
	database, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return "", fmt.Errorf("open database for migration backup: %w", err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	dir := filepath.Dir(backupPath)
	temp, err := os.CreateTemp(dir, ".nastool-snapshot-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if _, err := database.Exec("VACUUM INTO ?", temp.Name()); err != nil {
		return "", fmt.Errorf("create migration backup: %w", err)
	}
	if err := verifySQLite(temp.Name()); err != nil {
		return "", fmt.Errorf("verify migration backup: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("sync migration backup: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	// Publish only the complete snapshot. A second startup can verify the
	// winner's backup without ever opening another process's partial copy.
	err = fsutil.RenameExclusive(temp.Name(), backupPath)
	if err != nil && !errors.Is(err, os.ErrExist) {
		err = os.Link(temp.Name(), backupPath)
	}
	if errors.Is(err, os.ErrExist) {
		if err := verifySQLite(backupPath); err != nil {
			return "", fmt.Errorf("verify existing migration backup: %w", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("publish migration backup: %w", err)
	}
	if runtime.GOOS != "windows" {
		d, err := os.Open(dir)
		if err != nil {
			return "", err
		}
		defer d.Close()
		if err := d.Sync(); err != nil {
			return "", fmt.Errorf("sync migration backup directory: %w", err)
		}
	}
	return backupPath, nil
}

func verifySQLite(path string) error {
	dsn, err := sqliteFileURL(path)
	if err != nil {
		return err
	}
	dsn.RawQuery = "mode=ro"
	database, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return err
	}
	defer database.Close()
	var pages int
	if err := database.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		return err
	}
	if pages == 0 {
		return errors.New("empty SQLite snapshot")
	}
	var result string
	if err := database.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite quick_check returned %q", result)
	}
	return nil
}
