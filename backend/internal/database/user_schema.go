package database

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// The legacy initializer cleared alembic_version before replaying upgrades,
// and tolerated partially applied revisions. Inspect the schema of each
// existing table. Domain stores still initialize absent tables themselves.
type userTableUpgrade struct {
	name, textColumns, integerColumns string
}

const rssText = "RSS_SITES SEARCH_SITES FILTER_RESTYPE FILTER_PIX FILTER_TEAM SAVE_PATH NOTE KEYWORD FILTER_INCLUDE FILTER_EXCLUDE"
const rssIntegers = "OVER_EDITION FILTER_RULE DOWNLOAD_SETTING FUZZY_MATCH FILTER_ORDER"

var userTableUpgrades = []userTableUpgrade{
	{"CONFIG_USER_RSS", "SAVE_PATH RECOGNIZATION MEDIAINFOS SITES FILTER_ARGS", "DOWNLOAD_SETTING OVER_EDITION"},
	{"RSS_MOVIES", rssText, rssIntegers},
	{"RSS_TVS", rssText, rssIntegers + " TOTAL_EP CURRENT_EP"},
	{"TRANSFER_HISTORY", "DEST_PATH DEST_FILENAME DATE", "TMDBID"},
	{"DOWNLOAD_SETTING", "DOWNLOADER", ""},
	{"TRANSFER_UNKNOWN", "MODE", ""},
	{"DOUBAN_MEDIAS", "ADD_TIME", ""},
	{"SITE_BRUSH_TASK", "SENDMESSAGE LABEL RSSURL SAVEPATH UP_LIMIT DL_LIMIT FRACTION_RULE BRUSHTASK_FREE_LIMIT_SPEED BRUSHTASK_FREE_DDL_DELETE", ""},
	{"DOWNLOAD_HISTORY", "DOWNLOADER DOWNLOAD_ID SE SAVE_PATH", ""},
	{"DOWNLOADER", "", "MATCH_PATH ONLY_NASTOOL"},
	{"CONFIG_SYNC_PATHS", "", "COMPATIBILITY LOCATING"},
	{"TORRENT_REMOVE_TASK", "", "ONLY_NASTOOL"},
	{"SITE_BRUSH_TORRENTS", "FREE_DEADLINE", ""},
	{"CONFIG_SITE", "APIKEY", ""},
	{"SITE_USER_INFO_STATS", "LAST_SEEN", ""},
}

// PrepareNativeUserDatabase runs before any Go store or worker opens user.db.
// Keep retired columns and tables, including user extensions, in the live DB.
func PrepareNativeUserDatabase(path string) error {
	if _, err := EnsureMigrationBackup(path); err != nil {
		return fmt.Errorf("create pre-Go database backup: %w", err)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return prepareSchema(path, "rw", nil, true)
}

// PrepareApplicationDatabases replaces the original ORM create_all followed
// by Alembic in deployments that still serve legacy clients. Both original
// databases are backed up before either can be changed.
func PrepareApplicationDatabases(dir string) error {
	user := filepath.Join(dir, "user.db")
	media := filepath.Join(dir, "media.db")
	for _, path := range []string{user, media} {
		if _, err := EnsureMigrationBackup(path); err != nil {
			return fmt.Errorf("back up %s: %w", filepath.Base(path), err)
		}
	}
	if err := prepareSchema(user, "rwc", userTables, true); err != nil {
		return fmt.Errorf("prepare user.db: %w", err)
	}
	if err := prepareSchema(media, "rwc", mediaTables, false); err != nil {
		return fmt.Errorf("prepare media.db: %w", err)
	}
	return nil
}

func prepareSchema(path, mode string, tables []initialTable, upgradeUser bool) error {
	dsn, err := sqliteFileURL(path)
	if err != nil {
		return err
	}
	// Dropping a table during a type rebuild must not cascade into child
	// rows. The private migration connection checks the affected keys itself.
	dsn.RawQuery = "mode=" + mode + "&_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=foreign_keys(OFF)"
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
	for _, table := range tables {
		present, err := hasInitialTable(tx, table.name)
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(table.ddl); err != nil {
				return fmt.Errorf("create %s: %w", table.name, err)
			}
		}
	}
	if upgradeUser {
		if err := upgradeUserTables(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func upgradeUserTables(tx *sql.Tx) error {
	for _, table := range userTableUpgrades {
		if err := table.apply(tx); err != nil {
			return fmt.Errorf("upgrade %s: %w", table.name, err)
		}
	}
	for _, change := range historicalTypeChanges {
		if err := change.apply(tx); err != nil {
			return fmt.Errorf("upgrade %s column types: %w", change.table, err)
		}
	}
	return nil
}

func sqliteFileURL(path string) (*url.URL, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}, nil
}

func (table userTableUpgrade) apply(tx *sql.Tx) error {
	rows, err := tx.Query("SELECT upper(name) FROM pragma_table_xinfo(?)", table.name)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(columns) == 0 {
		return err
	}
	// All identifiers interpolated below are fixed application schema names.
	if table.name == "TRANSFER_HISTORY" {
		for _, pair := range [][2]string{{"FILE_PATH", "SOURCE_PATH"}, {"FILE_NAME", "SOURCE_FILENAME"}, {"SE", "SEASON_EPISODE"}} {
			if !columns[pair[0]] {
				continue
			}
			if columns[pair[1]] {
				return fmt.Errorf("both %s and %s exist; cannot choose the original history field", pair[0], pair[1])
			}
			if _, err := tx.Exec("ALTER TABLE TRANSFER_HISTORY RENAME COLUMN " + pair[0] + " TO " + pair[1]); err != nil {
				return err
			}
		}
	}
	for _, group := range []struct{ names, sqlType string }{{table.textColumns, "TEXT"}, {table.integerColumns, "INTEGER"}} {
		for _, name := range strings.Fields(group.names) {
			if !columns[name] {
				if _, err := tx.Exec("ALTER TABLE " + table.name + " ADD COLUMN " + name + " " + group.sqlType); err != nil {
					return err
				}
			}
		}
	}
	switch table.name {
	case "DOWNLOAD_HISTORY":
		return ensureHistoryIndex(tx, table.name, "SAVE_PATH")
	case "TRANSFER_HISTORY":
		return ensureHistoryIndex(tx, table.name, "DATE")
	}
	return nil
}

func ensureHistoryIndex(tx *sql.Tx, table, column string) error {
	name := "ix_" + table + "_" + column
	if _, err := tx.Exec("CREATE INDEX IF NOT EXISTS " + name + " ON " + table + "(" + column + ")"); err != nil {
		return err
	}
	// IF NOT EXISTS also succeeds for a same-named index on another table
	// or column. Keep the user's index intact and roll back the upgrade.
	var matches bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_index_list(?) WHERE name=? COLLATE NOCASE AND partial=0)
		AND (SELECT count(*) FROM pragma_index_info(?))=1
		AND EXISTS(SELECT 1 FROM pragma_index_info(?) WHERE name=? COLLATE NOCASE)`,
		table, name, name, name, column).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return fmt.Errorf("existing index %s does not cover %s(%s)", name, table, column)
	}
	return nil
}
