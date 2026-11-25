package database

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareApplicationDatabasesFreshAndRestart(t *testing.T) {
	dir := t.TempDir()
	if err := PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	user := userSchemaFixture(t, filepath.Join(dir, "user.db"), "")
	media := userSchemaFixture(t, filepath.Join(dir, "media.db"), "")
	requireSchemaValue(t, user, "SELECT count(*) FROM sqlite_schema WHERE type='table'", "34")
	requireSchemaValue(t, media, "SELECT count(*) FROM sqlite_schema WHERE type='table'", "2")
	requireSchemaValue(t, media, "SELECT group_concat(name,',') FROM pragma_index_info('INDX_MEDIASYNC_ITEMS_SL')", "SERVER,LIBRARY")
	requireSchemaValue(t, user, "SELECT group_concat(name,',') FROM pragma_index_info('INDX_RSS_TORRENTS_NAME')", "TITLE,YEAR,SEASON,EPISODE")
	requireSchemaValue(t, user, `SELECT "unique" FROM pragma_index_list('SITE_STATISTICS_HISTORY') WHERE name='UN_INDX_SITE_STATISTICS_HISTORY_DS'`, "1")
	if _, err := user.Exec("INSERT INTO SITE_USER_SEEDING_INFO(URL) VALUES ('fixture')"); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, user, "SELECT SEEDING_INFO FROM SITE_USER_SEEDING_INFO", "[]")
	if _, err := media.Exec("INSERT INTO MEDIASYNC_ITEMS(TITLE) VALUES ('fixture')"); err != nil {
		t.Fatal(err)
	}
	if err := PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, media, "SELECT TITLE FROM MEDIASYNC_ITEMS", "fixture")
	requireSchemaValue(t, user, "SELECT count(*) FROM sqlite_schema WHERE name='alembic_version'", "0")
}

func TestPrepareApplicationDatabasesOldColumnsAndBothSnapshots(t *testing.T) {
	dir := t.TempDir()
	userPath, mediaPath := filepath.Join(dir, "user.db"), filepath.Join(dir, "media.db")
	user := userSchemaFixture(t, userPath, `CREATE TABLE TRANSFER_HISTORY(ID INTEGER PRIMARY KEY, FILE_NAME TEXT, FILE_PATH TEXT, SE TEXT);
	INSERT INTO TRANSFER_HISTORY VALUES(1,'fixture.mkv','/tmp','S1E1');
	CREATE TABLE alembic_version(version_num TEXT); INSERT INTO alembic_version VALUES('original');`)
	media := userSchemaFixture(t, mediaPath, `CREATE TABLE CUSTOM_MEDIA (extra BLOB);
		INSERT INTO CUSTOM_MEDIA VALUES (x'00FF');
		CREATE TRIGGER MEDIASYNC_ITEMS BEFORE DELETE ON CUSTOM_MEDIA BEGIN SELECT RAISE(ABORT,'extension protected'); END;`)
	if err := PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, user, "SELECT SOURCE_FILENAME FROM TRANSFER_HISTORY", "fixture.mkv")
	requireSchemaValue(t, user, "SELECT version_num FROM alembic_version", "original")
	requireSchemaValue(t, media, "SELECT hex(extra) FROM CUSTOM_MEDIA", "00FF")
	if _, err := media.Exec("DELETE FROM CUSTOM_MEDIA"); err == nil || !strings.Contains(err.Error(), "extension protected") {
		t.Fatal("lost user trigger sharing the new table name", err)
	}
	for _, p := range []string{userPath, mediaPath} {
		db := userSchemaFixture(t, p+MigrationBackupSuffix, "")
		requireSchemaValue(t, db, "PRAGMA quick_check", "ok")
		requireSchemaValue(t, db, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name IN ('CONFIG_USERS','MEDIASYNC_ITEMS')", "0")
	}
	userBackup := userSchemaFixture(t, userPath+MigrationBackupSuffix, "")
	requireSchemaValue(t, userBackup, "SELECT FILE_NAME FROM TRANSFER_HISTORY", "fixture.mkv")
}

func TestPrepareApplicationDatabasesFailureBeforeUpgradeAndRetry(t *testing.T) {
	dir := t.TempDir()
	userPath, mediaPath := filepath.Join(dir, "user.db"), filepath.Join(dir, "media.db")
	user := userSchemaFixture(t, userPath, "CREATE TABLE history_extension (val TEXT); CREATE VIEW CONFIG_SITE AS SELECT val FROM history_extension;")
	media := userSchemaFixture(t, mediaPath, "CREATE TABLE extension(value BLOB)")
	if err := os.WriteFile(mediaPath+MigrationBackupSuffix, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareApplicationDatabases(dir); err == nil || !strings.Contains(err.Error(), "back up media.db") {
		t.Fatal(err)
	}
	requireSchemaValue(t, user, "SELECT count(*) FROM sqlite_schema WHERE name='CONFIG_USERS'", "0")
	if err := os.Rename(mediaPath+MigrationBackupSuffix, mediaPath+".bad-fixture"); err != nil {
		t.Fatal(err)
	}
	if err := PrepareApplicationDatabases(dir); err == nil || !strings.Contains(err.Error(), "CONFIG_SITE is a view") {
		t.Fatal(err)
	}
	requireSchemaValue(t, user, "SELECT count(*) FROM sqlite_schema WHERE name='CONFIG_FILTER_GROUP'", "0")
	requireSchemaValue(t, media, "SELECT count(*) FROM sqlite_schema WHERE name='MEDIASYNC_ITEMS'", "0")
	if _, err := user.Exec("DROP VIEW CONFIG_SITE"); err != nil {
		t.Fatal(err)
	}
	if err := PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, user, "SELECT type FROM sqlite_schema WHERE name='CONFIG_SITE'", "table")
}
