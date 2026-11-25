package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/initialsql"
)

var initialScripts = []string{
	"init_filter.sql", "init_userrss_v3.sql", "stop_all_service.sql", "update_downloader.sql",
	"update_subscribe.sql", "update_systemdict.sql", "update_userpris.sql", "update_userrss.sql",
}

// InitializeUserData runs after the domain stores have created their tables
// and before any workers or requests may consume the presets and settings.
func InitializeUserData(databasePath, configPath string) error {
	physical, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		return err
	}
	cfg := config.NewStore(physical)
	dsn, err := sqliteFileURL(databasePath)
	if err != nil {
		return err
	}
	dsn.RawQuery = "mode=rw&_txlock=immediate&_pragma=busy_timeout(10000)"
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	previous, err := cfg.SyncInitializationFiles()
	if err != nil {
		return err
	}
	plugins, err := cfg.LegacyPluginSettings()
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS GO_INITIAL_DATA_SCRIPTS(NAME TEXT PRIMARY KEY, SOURCE TEXT NOT NULL)`); err != nil {
		return err
	}
	needsMirror := false
	for _, name := range initialScripts {
		marked := slices.Contains(previous, name)
		needsMirror = needsMirror || !marked
		var recorded bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM GO_INITIAL_DATA_SCRIPTS WHERE NAME=?)", name).Scan(&recorded); err != nil {
			return err
		}
		if recorded {
			continue
		}
		source := "config"
		if !marked {
			if err := applyInitialScript(tx, name); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			source = "go"
		}
		result, err := tx.Exec("INSERT OR ABORT INTO GO_INITIAL_DATA_SCRIPTS(NAME,SOURCE) VALUES(?,?)", name, source)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("cannot record completion of %s", name)
		}
	}
	for key, fields := range plugins {
		if err := importLegacyPlugin(tx, key, fields); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if !needsMirror && len(plugins) == 0 {
		return nil
	}
	// A crash after the commit leaves receipts in user.db. Re-entering this
	// function repairs YAML without repeating any of the data changes.
	guard, err := db.Begin()
	if err != nil {
		return err
	}
	defer guard.Rollback()
	if _, err := cfg.SyncInitializationFiles(initialScripts...); err != nil {
		return fmt.Errorf("record completed scripts in app.init_files: %w", err)
	}
	if len(plugins) != 0 {
		if err := cfg.RetireLegacyPluginSettings(plugins); err != nil {
			return err
		}
	}
	return guard.Commit()
}

// A saved plugin configuration takes precedence over legacy YAML. The caller
// holds an immediate write transaction, including during the existence check.
func importLegacyPlugin(tx *sql.Tx, key string, fields map[string]any) error {
	var previous sql.NullString
	err := tx.QueryRow("SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1", key).Scan(&previous)
	if err == nil {
		var object map[string]json.RawMessage
		if !previous.Valid || json.Unmarshal([]byte(previous.String), &object) != nil || object == nil {
			return fmt.Errorf("invalid saved configuration for %s; legacy YAML retained", key)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	value, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("invalid legacy configuration for %s", key)
	}
	result, err := tx.Exec("INSERT INTO SYSTEM_DICT (TYPE,KEY,VALUE,NOTE) VALUES ('SystemConfig', ?, ?, '')", key, string(value))
	if err != nil {
		return fmt.Errorf("import %s: %w", key, err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("could not import configuration for %s", key)
	}
	return nil
}

func applyInitialScript(tx *sql.Tx, name string) error {
	switch name {
	case "init_filter.sql":
		return seedFilters(tx)
	case "init_userrss_v3.sql":
		return seedParsers(tx)
	case "stop_all_service.sql":
		// Retain old task IDs and switch settings alongside the migration
		// receipt. Go worker authorizations live in separate tables.
		for _, task := range []struct{ table, field, stopped string }{
			{"SITE_BRUSH_TASK", "STATE", "'N'"}, {"TORRENT_REMOVE_TASK", "ENABLED", "0"},
		} {
			present, err := hasInitialTable(tx, task.table)
			if err != nil {
				return err
			}
			if !present {
				continue
			}
			if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS GO_INITIAL_TASK_STATES (TABLE_NAME TEXT, TASK_ID INTEGER, PREVIOUS, PRIMARY KEY(TABLE_NAME,TASK_ID))"); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO GO_INITIAL_TASK_STATES SELECT ?,ID,"+task.field+" FROM "+task.table+" WHERE "+task.field+" IS NOT "+task.stopped, task.table); err != nil {
				return err
			}
			if err := updateInitialRows(tx, task.table, task.field+"="+task.stopped, task.field+" IS NOT "+task.stopped); err != nil {
				return err
			}
		}
	case "update_downloader.sql":
		return updateInitialRows(tx, "DOWNLOADER", "MATCH_PATH=0", "MATCH_PATH IS NULL")
	case "update_subscribe.sql":
		for _, table := range []string{"RSS_MOVIES", "RSS_TVS"} {
			present, err := hasInitialTable(tx, table)
			if err != nil {
				return err
			}
			if present {
				if err := updateInitialRows(tx, table, "DOWNLOAD_SETTING=NULL", "DOWNLOAD_SETTING=-1"); err != nil {
					return err
				}
			}
		}
	case "update_systemdict.sql":
		var count int64
		if err := tx.QueryRow("SELECT count(*) FROM SYSTEM_DICT WHERE TYPE LIKE '刷流%'").Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		if _, err := tx.Exec(`CREATE TABLE GO_INITIAL_RETIRED_DICT AS SELECT * FROM SYSTEM_DICT WHERE TYPE LIKE '刷流%'`); err != nil {
			return err
		}
		result, err := tx.Exec("DELETE FROM SYSTEM_DICT WHERE TYPE LIKE '刷流%'")
		if err != nil {
			return err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if deleted != count {
			return errors.New("retired settings changed during migration")
		}
	case "update_userpris.sql":
		return updateInitialRows(tx, "CONFIG_USERS", "PRIS=replace(PRIS,'推荐','探索')", "instr(PRIS,'推荐')>0")
	case "update_userrss.sql":
		return updateInitialRows(tx, "CONFIG_USER_RSS", "PROCESS_COUNT='0'", "PROCESS_COUNT IS NULL")
	default:
		return fmt.Errorf("unknown initial data script %q", name)
	}
	return nil
}

// A legacy trigger can ignore an UPDATE without returning a SQLite error.
// All expressions here are fixed by the original scripts.
func updateInitialRows(tx *sql.Tx, table, assignment, condition string) error {
	if _, err := tx.Exec("UPDATE " + table + " SET " + assignment + " WHERE " + condition); err != nil {
		return err
	}
	var remaining bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM " + table + " WHERE " + condition + ")").Scan(&remaining); err != nil {
		return err
	}
	if remaining {
		return fmt.Errorf("incomplete initial update in %s", table)
	}
	return nil
}

func hasInitialTable(tx *sql.Tx, table string) (bool, error) {
	var kind string
	err := tx.QueryRow("SELECT type FROM main.sqlite_schema WHERE name=? COLLATE NOCASE AND type IN ('table','view','index')", table).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if kind != "table" {
		return false, fmt.Errorf("%s is a %s", table, kind)
	}
	return true, nil
}

func seedParsers(tx *sql.Tx) error {
	// Stage the original REPLACE statements. On a Go installation any
	// occupied ID can represent a customized parser, including SYSDEF='Y'.
	_, err := tx.Exec(`CREATE TEMP TABLE CONFIG_RSS_PARSER (ID INTEGER PRIMARY KEY, NAME, TYPE, FORMAT, PARAMS, NOTE, SYSDEF);` +
		initialsql.RSSParsers + `
		INSERT OR ABORT INTO main.CONFIG_RSS_PARSER(ID,NAME,TYPE,FORMAT,PARAMS,NOTE,SYSDEF)
		SELECT ID,NAME,TYPE,FORMAT,PARAMS,NOTE,SYSDEF FROM temp.CONFIG_RSS_PARSER src
		WHERE NOT EXISTS (SELECT 1 FROM main.CONFIG_RSS_PARSER dst WHERE dst.ID=src.ID);
		DROP TABLE temp.CONFIG_RSS_PARSER;`)
	return err
}

func seedFilters(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TEMP TABLE CONFIG_FILTER_GROUP(ID INTEGER PRIMARY KEY, GROUP_NAME, IS_DEFAULT, NOTE);
		CREATE TEMP TABLE CONFIG_FILTER_RULES(ID INTEGER PRIMARY KEY, GROUP_ID, ROLE_NAME, PRIORITY, INCLUDE, EXCLUDE, SIZE_LIMIT, NOTE);` +
		initialsql.Filters + `
		INSERT OR ABORT INTO main.CONFIG_FILTER_GROUP(ID,GROUP_NAME,IS_DEFAULT,NOTE)
		SELECT ID,GROUP_NAME,CASE WHEN EXISTS(SELECT 1 FROM main.CONFIG_FILTER_GROUP WHERE IS_DEFAULT='Y')
			THEN 'N' ELSE IS_DEFAULT END,NOTE FROM temp.CONFIG_FILTER_GROUP src
		WHERE NOT EXISTS(SELECT 1 FROM main.CONFIG_FILTER_GROUP dst WHERE dst.ID=src.ID);
		INSERT OR ABORT INTO main.CONFIG_FILTER_RULES(ID,GROUP_ID,ROLE_NAME,PRIORITY,INCLUDE,EXCLUDE,SIZE_LIMIT,NOTE)
		SELECT r.ID,r.GROUP_ID,r.ROLE_NAME,r.PRIORITY,r.INCLUDE,r.EXCLUDE,r.SIZE_LIMIT,r.NOTE FROM temp.CONFIG_FILTER_RULES r
		JOIN temp.CONFIG_FILTER_GROUP g ON g.ID=r.GROUP_ID
		JOIN main.CONFIG_FILTER_GROUP existing ON existing.ID=g.ID AND existing.GROUP_NAME=g.GROUP_NAME
		WHERE NOT EXISTS(SELECT 1 FROM main.CONFIG_FILTER_RULES dst WHERE dst.ID=r.ID);
		DROP TABLE temp.CONFIG_FILTER_RULES; DROP TABLE temp.CONFIG_FILTER_GROUP;`)
	return err
}
