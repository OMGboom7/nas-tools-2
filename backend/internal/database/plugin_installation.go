package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

// PluginInstallation is used by source and packaged plugin managers while
// they still instantiate third-party classes. It never creates a missing DB.
func PluginInstallation(ctx context.Context, configPath string, args []string, out io.Writer) error {
	const usage = "usage: plugin-installation list | restore ID | install ID | uninstall ID | apply install|uninstall ID idle|running|unavailable | ack ID receipt"
	if len(args) == 0 {
		return errors.New(usage)
	}
	op := args[0]
	var runtime string
	if op == "apply" {
		if len(args) != 4 || (args[1] != "install" && args[1] != "uninstall") {
			return errors.New(usage)
		}
		runtime = args[3]
		if runtime != "idle" && runtime != "running" && runtime != "unavailable" {
			return errors.New("invalid plugin runtime state")
		}
		args = args[1:3]
	}
	switch op {
	case "list":
		if len(args) != 1 {
			return errors.New(usage)
		}
	case "ack":
		if len(args) != 3 || !systemconfig.ValidStoredPluginID(args[1]) || args[2] == "" {
			return errors.New(usage)
		}
	case "apply", "restore", "install", "uninstall":
		if len(args) != 2 || !systemconfig.ValidStoredPluginID(args[1]) {
			return errors.New(usage)
		}
	default:
		return errors.New(usage)
	}
	db, closeDB, err := openPluginDatabase(configPath, op == "list")
	if err != nil {
		return err
	}
	defer closeDB()
	if op == "list" {
		ids, err := readPluginInstallation(ctx, db)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(ids)
	}
	if op == "ack" {
		result, err := db.ExecContext(ctx, "DELETE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? AND VALUE=?", systemconfig.PluginLifecycleKey(args[1]), args[2])
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count == 0 {
			return errors.New("plugin lifecycle receipt changed")
		}
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, install := args[1], args[0] == "install"
	if op == "restore" {
		ids, err := readPluginInstallation(ctx, tx)
		if err != nil {
			return err
		}
		install, runtime = slices.Contains(ids, id), "idle"
	} else if _, err := systemconfig.SetPluginInstalledTx(ctx, tx, id, install); err != nil {
		return err
	}
	action := "keep"
	var config any
	switch {
	case install && runtime == "idle":
		action = "start"
		config, _, err = readPluginValue(ctx, tx, "plugin."+id)
		if err != nil {
			return err
		}
	case !install && runtime == "running":
		action = "stop"
	}
	if op != "restore" && op != "apply" {
		return tx.Commit()
	}

	// A failed callback or lost reply must remain retryable even when the
	// installed list already matches. A stale worker cannot acknowledge a
	// newer transition. No plugin settings are stored in this receipt.
	key := systemconfig.PluginLifecycleKey(id)
	var receipt string
	if action == "keep" {
		_, err = tx.ExecContext(ctx, "DELETE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=?", key)
	} else {
		receipt = rand.Text()
		err = systemconfig.UpdateManyTx(ctx, tx, []string{key}, func(map[string]string) (map[string]string, error) {
			return map[string]string{key: receipt}, nil
		})
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"action": action, "config": config, "receipt": receipt})
}

type pluginQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readPluginInstallation(ctx context.Context, db pluginQuery) ([]string, error) {
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
