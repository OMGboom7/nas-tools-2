// Package dbinit prepares the application volume before either web service starts.
package dbinit

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/database"
)

func Run(path, command string) error {
	if command != "init-db" && command != "init-data" {
		return fmt.Errorf("unknown database command %q", command)
	}
	dir := filepath.Dir(path)
	if command == "init-db" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	dbPath := filepath.Join(dir, "user.db")
	var created bool
	if command == "init-db" {
		created, err = config.InitializeApplication(path)
		if err != nil {
			return err
		}
		if _, err := config.NewStore(path).SyncInitializationFiles(); err != nil {
			return err
		}
		if err := database.PrepareApplicationDatabases(dir); err != nil {
			return err
		}
	} else if _, err := database.EnsureMigrationBackup(dbPath); err != nil {
		return err
	}
	if err := database.InitializeUserData(dbPath, path); err != nil {
		return err
	}
	if created {
		slog.Info("initial configuration created; read the initial sign-in settings from the private config backup", "path", path+".bak")
	}
	return nil
}
