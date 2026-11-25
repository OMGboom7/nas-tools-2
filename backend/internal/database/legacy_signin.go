package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

// The persistent receipt belongs to user.db. Replaying old YAML after a crash
// or a restore must not reinstall a plugin the user subsequently removed.
func importLegacySignin(tx *sql.Tx, cron string) error {
	if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS GO_LEGACY_PLUGIN_INSTALLS (PLUGIN TEXT PRIMARY KEY)"); err != nil {
		return err
	}
	var recorded bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM GO_LEGACY_PLUGIN_INSTALLS WHERE PLUGIN=?)", sitesignin.Plugin).Scan(&recorded); err != nil {
		return err
	}
	if recorded {
		return nil
	}
	const setting = "plugin." + sitesignin.Plugin
	const list = "UserInstalledPlugins"
	err := systemconfig.UpdateManyTx(context.Background(), tx, []string{setting, list}, func(current map[string]string) (map[string]string, error) {
		next := map[string]string{}
		installed, err := systemconfig.DecodeInstalledPlugins(current[list])
		if err != nil {
			return nil, err
		}
		if current[setting] != "" {
			if _, err := sitesignin.ParseSettings(current[setting]); err != nil {
				return nil, errors.New("invalid stored AutoSignIn configuration")
			}
			// An already saved configuration and explicit list together retain
			// the user's installed or uninstalled state.
			if current[list] != "" {
				return next, nil
			}
		} else {
			raw, err := json.Marshal(map[string]any{"enabled": true, "cron": cron, "retry_keyword": "", "sign_sites": []string{}, "special_sites": []string{}, "notify": true, "onlyonce": false, "queue_cnt": 10})
			if err != nil {
				return nil, err
			}
			next[setting] = string(raw)
		}
		if !slices.Contains(installed, sitesignin.Plugin) {
			value, err := json.Marshal(append(installed, sitesignin.Plugin))
			if err != nil {
				return nil, err
			}
			if _, err := systemconfig.DecodeInstalledPlugins(string(value)); err != nil {
				return nil, err
			}
			next[list] = string(value)
		}
		return next, nil
	})
	if err != nil {
		return err
	}
	result, err := tx.Exec("INSERT INTO GO_LEGACY_PLUGIN_INSTALLS(PLUGIN) VALUES(?)", sitesignin.Plugin)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return errors.New("could not record AutoSignIn migration")
	}
	return nil
}
