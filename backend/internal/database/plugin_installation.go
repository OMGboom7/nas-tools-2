package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

// PluginInstallation is used by source and packaged plugin managers while
// they still instantiate third-party classes. It never creates a missing DB.
func PluginInstallation(ctx context.Context, configPath string, args []string, out io.Writer) error {
	list := len(args) == 1 && args[0] == "list"
	if !list && (len(args) != 2 || args[0] != "install" && args[0] != "uninstall" || !systemconfig.ValidStoredPluginID(args[1])) {
		return errors.New("usage: plugin-installation list | install ID | uninstall ID")
	}
	db, closeDB, err := openPluginDatabase(configPath, list)
	if err != nil {
		return err
	}
	defer closeDB()
	if list {
		ids, err := readPluginInstallation(ctx, db)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(ids)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := systemconfig.SetPluginInstalledTx(ctx, tx, args[1], args[0] == "install"); err != nil {
		return err
	}
	return tx.Commit()
}

func readPluginInstallation(ctx context.Context, db *sql.DB) ([]string, error) {
	var raw string
	err := db.QueryRowContext(ctx, "SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY='UserInstalledPlugins' ORDER BY ID DESC LIMIT 1").Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return systemconfig.DecodeInstalledPlugins(raw)
}

func openPluginDatabase(configPath string, readOnly bool) (*sql.DB, func(), error) {
	release, err := backuprestore.HoldStartup(configPath)
	if err != nil {
		return nil, nil, err
	}
	dsn, err := sqliteFileURL(filepath.Join(filepath.Dir(configPath), "user.db"))
	if err != nil {
		release()
		return nil, nil, err
	}
	dsn.RawQuery = "mode=rw&_pragma=busy_timeout(5000)&_txlock=immediate"
	if readOnly {
		dsn.RawQuery = "mode=ro&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		release()
		return nil, nil, err
	}
	return db, func() { db.Close(); release() }, nil
}
