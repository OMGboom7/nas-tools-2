package database

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func userSchemaFixture(t *testing.T, path string, ddl string) *sql.DB {
	t.Helper()
	dsn, err := sqliteFileURL(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	return db
}

func requireSchemaValue(t *testing.T, db *sql.DB, query, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(query).Scan(&got); err != nil || got != want {
		t.Fatalf("%s: got %q, want %q, error %v", query, got, want, err)
	}
}

func TestPrepareNativeUserDatabaseUpgradesHistoricalFieldsAcrossRevisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user #1's.db")
	db := userSchemaFixture(t, path, `
		CREATE TABLE CONFIG_USER_RSS (ID INTEGER PRIMARY KEY);
		CREATE TABLE RSS_MOVIES (ID INTEGER PRIMARY KEY, FILTER_INCLUDE TEXT);
		CREATE TABLE RSS_TVS (ID INTEGER PRIMARY KEY);
		CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY, FILE_PATH TEXT, FILE_NAME TEXT, SE TEXT, DATE TEXT, CUSTOM BLOB);
		CREATE VIEW extension_view AS SELECT FILE_PATH, CUSTOM FROM TRANSFER_HISTORY;
		INSERT INTO TRANSFER_HISTORY VALUES (3,'/library','file.mkv','S2E01','2023',x'00FF');
		CREATE TABLE DOWNLOAD_HISTORY (ID INTEGER PRIMARY KEY);
		CREATE TABLE DOWNLOAD_SETTING (ID INTEGER PRIMARY KEY, CONTENT_LAYOUT TEXT);
		INSERT INTO DOWNLOAD_SETTING VALUES (1,'original');
		CREATE TABLE CONFIG_SYNC_PATHS (ID INTEGER PRIMARY KEY);
		CREATE TABLE SITE_BRUSH_TASK (ID INTEGER PRIMARY KEY);
		CREATE TABLE SITE_BRUSH_TORRENTS (ID INTEGER PRIMARY KEY);
		CREATE TABLE DOWNLOADER (ID INTEGER PRIMARY KEY);
		CREATE TABLE TORRENT_REMOVE_TASK (ID INTEGER PRIMARY KEY);
		CREATE TABLE CONFIG_SITE (ID INTEGER PRIMARY KEY);
		CREATE TABLE SITE_USER_INFO_STATS (ID INTEGER PRIMARY KEY, FAVICON BLOB);
		CREATE TABLE DOUBAN_MEDIAS (ID INTEGER PRIMARY KEY);
		CREATE TABLE TRANSFER_UNKNOWN (ID INTEGER PRIMARY KEY);
		CREATE TABLE IGNORED_WORDS (WORD TEXT);
		INSERT INTO IGNORED_WORDS VALUES ('keep');
		CREATE TABLE alembic_version (version_num TEXT);
		INSERT INTO alembic_version VALUES ('d116d793ba9f');
	`)
	errs := make(chan error, 4)
	for i := 0; i < cap(errs); i++ {
		go func() { errs <- PrepareNativeUserDatabase(path) }()
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	requireSchemaValue(t, db, "SELECT SOURCE_PATH || '/' || SOURCE_FILENAME || ':' || SEASON_EPISODE || ':' || hex(CUSTOM) FROM TRANSFER_HISTORY", "/library/file.mkv:S2E01:00FF")
	requireSchemaValue(t, db, "SELECT hex(CUSTOM) FROM extension_view", "00FF")
	requireSchemaValue(t, db, "SELECT CONTENT_LAYOUT FROM DOWNLOAD_SETTING", "original")
	requireSchemaValue(t, db, "SELECT WORD FROM IGNORED_WORDS", "keep")
	requireSchemaValue(t, db, "SELECT version_num FROM alembic_version", "d116d793ba9f")
	for _, tt := range []struct{ table, column, typ string }{
		{"CONFIG_USER_RSS", "MEDIAINFOS", "TEXT"}, {"RSS_MOVIES", "FILTER_INCLUDE", "TEXT"}, {"RSS_TVS", "TOTAL_EP", "INTEGER"},
		{"DOWNLOAD_HISTORY", "SAVE_PATH", "TEXT"}, {"DOWNLOAD_SETTING", "DOWNLOADER", "TEXT"}, {"CONFIG_SYNC_PATHS", "COMPATIBILITY", "INTEGER"},
		{"CONFIG_SYNC_PATHS", "LOCATING", "INTEGER"}, {"SITE_BRUSH_TASK", "RSSURL", "TEXT"}, {"SITE_BRUSH_TASK", "SAVEPATH", "TEXT"},
		{"SITE_BRUSH_TASK", "UP_LIMIT", "TEXT"}, {"SITE_BRUSH_TASK", "BRUSHTASK_FREE_DDL_DELETE", "TEXT"}, {"SITE_BRUSH_TORRENTS", "FREE_DEADLINE", "TEXT"},
		{"DOWNLOADER", "ONLY_NASTOOL", "INTEGER"}, {"TORRENT_REMOVE_TASK", "ONLY_NASTOOL", "INTEGER"}, {"CONFIG_SITE", "APIKEY", "TEXT"},
		{"SITE_USER_INFO_STATS", "LAST_SEEN", "TEXT"}, {"DOUBAN_MEDIAS", "ADD_TIME", "TEXT"}, {"TRANSFER_UNKNOWN", "MODE", "TEXT"},
	} {
		requireSchemaValue(t, db, "SELECT type FROM pragma_table_info('"+tt.table+"') WHERE name='"+tt.column+"'", tt.typ)
	}
	requireSchemaValue(t, db, "SELECT name FROM pragma_index_info('ix_DOWNLOAD_HISTORY_SAVE_PATH')", "SAVE_PATH")
	requireSchemaValue(t, db, "SELECT name FROM pragma_index_info('ix_TRANSFER_HISTORY_DATE')", "DATE")
	backup := userSchemaFixture(t, path+MigrationBackupSuffix, "")
	requireSchemaValue(t, backup, "SELECT FILE_NAME FROM TRANSFER_HISTORY", "file.mkv")
	requireSchemaValue(t, backup, "SELECT count(*) FROM pragma_table_info('CONFIG_SITE') WHERE name='APIKEY'", "0")
}

func TestPrepareNativeUserDatabaseRollsBackConflictingHistoryAndRejectsEmptyBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	db := userSchemaFixture(t, path, `
		CREATE TABLE RSS_MOVIES (ID INTEGER PRIMARY KEY);
		CREATE TABLE TRANSFER_HISTORY (FILE_PATH TEXT, FILE_NAME TEXT, SOURCE_FILENAME TEXT);
		INSERT INTO TRANSFER_HISTORY VALUES ('original','first','second');
	`)
	if err := PrepareNativeUserDatabase(path); err == nil || !strings.Contains(err.Error(), "SOURCE_FILENAME") {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT FILE_PATH || ':' || FILE_NAME || ':' || SOURCE_FILENAME FROM TRANSFER_HISTORY", "original:first:second")
	requireSchemaValue(t, db, "SELECT COUNT(*) FROM pragma_table_info('RSS_MOVIES')", "1")
	if err := os.WriteFile(path+MigrationBackupSuffix, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareNativeUserDatabase(path); err == nil || !strings.Contains(err.Error(), "empty SQLite snapshot") {
		t.Fatal(err)
	}
}

func TestPrepareNativeUserDatabasePreservesConflictingExtensionIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	db := userSchemaFixture(t, path, `
		CREATE TABLE DOWNLOAD_HISTORY (ID INTEGER PRIMARY KEY);
		CREATE TABLE extension (value TEXT);
		CREATE INDEX ix_DOWNLOAD_HISTORY_SAVE_PATH ON extension(value);
	`)
	if err := PrepareNativeUserDatabase(path); err == nil || !strings.Contains(err.Error(), "does not cover") {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT tbl_name FROM sqlite_schema WHERE name='ix_DOWNLOAD_HISTORY_SAVE_PATH'", "extension")
	requireSchemaValue(t, db, "SELECT count(*) FROM pragma_table_info('DOWNLOAD_HISTORY')", "1")
}
