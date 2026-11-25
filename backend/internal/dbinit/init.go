// Package dbinit prepares the application volume before either web service starts.
package dbinit

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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
	var release func()
	var err error
	if command == "init-db" {
		release, err = HoldApplication(path)
	} else {
		release, err = backuprestore.HoldStartup(path)
	}
	if err != nil {
		return err
	}
	defer release()
	dbPath := filepath.Join(dir, "user.db")
	if command == "init-db" {
		if err := database.PrepareApplicationDatabases(dir); err != nil {
			return err
		}
	} else if _, err := database.EnsureMigrationBackup(dbPath); err != nil {
		return err
	}
	return database.InitializeUserData(dbPath, path)
}

// HoldApplication prepares credentials before either service opens the
// databases. Configured volumes only need the shared serving lock.
func HoldApplication(path string) (func(), error) {
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return nil, err
	}
	created, err := config.InitializeApplication(path)
	if err == nil {
		_, err = config.NewStore(path).SyncInitializationFiles()
	}
	if err != nil {
		release()
		return nil, err
	}
	app, err := config.LoadApplication(path)
	if err == nil && strings.HasPrefix(app.App.LoginPassword, "[hash]") {
		if created {
			slog.Info("initial administrator password saved", "path", path+config.InitialPasswordSuffix)
		}
		return release, nil
	}
	release()
	if err != nil && !errors.Is(err, config.ErrMissingAPIKey) {
		return nil, err
	}
	if err := backuprestore.PrepareStartup(path, func() error { return config.InitializeCredentials(path) }); err != nil {
		return nil, fmt.Errorf("initialize application credentials: %w", err)
	}
	return backuprestore.HoldStartup(path)
}
