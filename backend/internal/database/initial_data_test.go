package database

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/initialsql"
)

func initialFixture(t *testing.T, yaml string) (*sql.DB, string, string) {
	t.Helper()
	dir := t.TempDir()
	path, cfg := filepath.Join(dir, "user.db"), filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	db := userSchemaFixture(t, path, `
		CREATE TABLE CONFIG_FILTER_GROUP(ID INTEGER PRIMARY KEY, GROUP_NAME, IS_DEFAULT, NOTE);
		CREATE TABLE CONFIG_FILTER_RULES(ID INTEGER PRIMARY KEY, GROUP_ID TEXT, ROLE_NAME, PRIORITY, INCLUDE, EXCLUDE, SIZE_LIMIT, NOTE);
		CREATE TABLE CONFIG_RSS_PARSER(ID INTEGER PRIMARY KEY, NAME, TYPE, FORMAT, PARAMS, NOTE, SYSDEF);
		CREATE TABLE DOWNLOADER(ID INTEGER PRIMARY KEY, MATCH_PATH INTEGER);
		CREATE TABLE CONFIG_USERS(ID INTEGER PRIMARY KEY, PRIS TEXT);
		CREATE TABLE CONFIG_USER_RSS(ID INTEGER PRIMARY KEY, PROCESS_COUNT TEXT);
		CREATE TABLE SITE_BRUSH_TASK(ID INTEGER PRIMARY KEY, STATE TEXT);
		CREATE TABLE TORRENT_REMOVE_TASK(ID INTEGER PRIMARY KEY, ENABLED INTEGER);
		CREATE TABLE RSS_MOVIES(ID INTEGER PRIMARY KEY, DOWNLOAD_SETTING TEXT);
		CREATE TABLE RSS_TVS(ID INTEGER PRIMARY KEY, DOWNLOAD_SETTING TEXT);
		CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY, TYPE TEXT, EXTRA BLOB, KEY TEXT, VALUE TEXT, NOTE TEXT);
		INSERT INTO SITE_BRUSH_TASK VALUES(11,'Y');
		INSERT INTO TORRENT_REMOVE_TASK VALUES(12,1);
		INSERT INTO SYSTEM_DICT(ID,TYPE,EXTRA) VALUES(1,'刷流旧配置',x'00FF'),(2,'SystemConfig',x'A0');
		INSERT INTO RSS_MOVIES VALUES(1,'-1');
		INSERT INTO RSS_TVS VALUES(2,'7');
		INSERT INTO CONFIG_USERS VALUES(1,'推荐,其他');
		INSERT INTO DOWNLOADER VALUES(1,NULL),(2,1);
		INSERT INTO CONFIG_USER_RSS VALUES(1,NULL);
	`)
	return db, path, cfg
}

func TestInitialDataMatchesShippedScripts(t *testing.T) {
	paths, err := filepath.Glob("../../../scripts/sqls/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	if !slices.Equal(names, initialScripts) {
		t.Fatalf("shipped %v, implemented %v", names, initialScripts)
	}
	body, err := os.ReadFile("../../../scripts/sqls/init_userrss_v3.sql")
	if err != nil || string(body) != initialsql.RSSParsers {
		t.Fatalf("parser presets differ: %v", err)
	}
}

func TestInitialDataConcurrentStartsPreserveOriginalsAndRunOnce(t *testing.T) {
	db, path, cfg := initialFixture(t, "app: {init_files: [vendor.sql]}\n")
	results := make(chan error, 4)
	for i := 0; i < cap(results); i++ {
		go func() { results <- InitializeUserData(path, cfg) }()
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for query, want := range map[string]string{
		"SELECT count(*) FROM CONFIG_RSS_PARSER":                                         "5",
		"SELECT count(*) FROM CONFIG_FILTER_GROUP":                                       "3",
		"SELECT count(*) FROM CONFIG_FILTER_RULES":                                       "34",
		"SELECT ID FROM CONFIG_FILTER_GROUP WHERE IS_DEFAULT='Y'":                        "9999",
		"SELECT count(*) FROM GO_INITIAL_DATA_SCRIPTS WHERE SOURCE='go'":                 "8",
		"SELECT STATE FROM SITE_BRUSH_TASK":                                              "N",
		"SELECT ENABLED FROM TORRENT_REMOVE_TASK":                                        "0",
		"SELECT PREVIOUS FROM GO_INITIAL_TASK_STATES WHERE TASK_ID=11":                   "Y",
		"SELECT typeof(PREVIOUS)||PREVIOUS FROM GO_INITIAL_TASK_STATES WHERE TASK_ID=12": "integer1",
		"SELECT hex(EXTRA) FROM GO_INITIAL_RETIRED_DICT":                                 "00FF",
		"SELECT hex(EXTRA) FROM SYSTEM_DICT":                                             "A0",
		"SELECT PRIS FROM CONFIG_USERS":                                                  "探索,其他",
		"SELECT count(*) FROM RSS_MOVIES WHERE DOWNLOAD_SETTING IS NULL":                 "1",
		"SELECT DOWNLOAD_SETTING FROM RSS_TVS":                                           "7",
		"SELECT group_concat(MATCH_PATH,',') FROM DOWNLOADER ORDER BY ID":                "0,1",
		"SELECT PROCESS_COUNT FROM CONFIG_USER_RSS":                                      "0",
	} {
		requireSchemaValue(t, db, query, want)
	}
	files, err := config.NewStore(cfg).SyncInitializationFiles()
	if err != nil || !slices.Equal(files, append([]string{"vendor.sql"}, initialScripts...)) {
		t.Fatalf("records %v: %v", files, err)
	}
	if _, err := db.Exec("UPDATE SITE_BRUSH_TASK SET STATE='Y'; DELETE FROM CONFIG_RSS_PARSER WHERE ID=1"); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT STATE FROM SITE_BRUSH_TASK", "Y")
	requireSchemaValue(t, db, "SELECT count(*) FROM CONFIG_RSS_PARSER", "4")
}

func TestInitialDataHonorsOldMarkersAndOccupiedPresets(t *testing.T) {
	db, path, cfg := initialFixture(t, "app:\n  init_files: [stop_all_service.sql, update_systemdict.sql]\n")
	if _, err := db.Exec(`INSERT INTO CONFIG_FILTER_GROUP VALUES(1000,'Custom','Y',NULL);
		INSERT INTO CONFIG_RSS_PARSER(ID,NAME,SYSDEF) VALUES(1,'Edited','Y');
		INSERT INTO CONFIG_FILTER_RULES(ID,GROUP_ID,ROLE_NAME) VALUES(10000,'1000','Mine')`); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]string{
		"SELECT count(*) FROM GO_INITIAL_DATA_SCRIPTS WHERE SOURCE='config'": "2",
		"SELECT NAME FROM CONFIG_RSS_PARSER WHERE ID=1":                      "Edited",
		"SELECT count(*) FROM CONFIG_RSS_PARSER":                             "5",
		"SELECT count(*) FROM CONFIG_FILTER_RULES WHERE GROUP_ID='1000'":     "1",
		"SELECT ROLE_NAME FROM CONFIG_FILTER_RULES WHERE ID=10000":           "Mine",
		"SELECT ID FROM CONFIG_FILTER_GROUP WHERE IS_DEFAULT='Y'":            "1000",
		"SELECT IS_DEFAULT FROM CONFIG_FILTER_GROUP WHERE ID=9999":           "N",
		"SELECT STATE FROM SITE_BRUSH_TASK":                                  "Y",
		"SELECT count(*) FROM SYSTEM_DICT":                                   "2",
	} {
		requireSchemaValue(t, db, query, want)
	}
}

func TestInitialDataImportsFullyInitializedLegacyInstall(t *testing.T) {
	original := "app: {init_files: [" + strings.Join(initialScripts, ", ") + "]} # old installation\nocr: {custom_ocr_url: ''}\n"
	db, path, cfg := initialFixture(t, original)
	if err := InitializeUserData(path, cfg); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT count(*) FROM GO_INITIAL_DATA_SCRIPTS WHERE SOURCE='config'", "8")
	requireSchemaValue(t, db, "SELECT count(*) FROM CONFIG_RSS_PARSER", "0")
	requireSchemaValue(t, db, "SELECT STATE FROM SITE_BRUSH_TASK", "Y")
	data, err := os.ReadFile(cfg)
	if err != nil || string(data) != original {
		t.Fatalf("changed legacy configuration: %s, %v", data, err)
	}
	if _, err := os.Stat(cfg + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("unexpected configuration write: %v", err)
	}
}

func TestInitialDataRollbackAndRepairAfterConfigWriteFailure(t *testing.T) {
	const original = "app: {}\n"
	db, path, cfg := initialFixture(t, original)
	if _, err := db.Exec("CREATE TRIGGER fail_rss BEFORE UPDATE ON CONFIG_USER_RSS BEGIN SELECT RAISE(IGNORE); END"); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err == nil || !strings.Contains(err.Error(), "incomplete initial update in CONFIG_USER_RSS") {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT count(*) FROM CONFIG_FILTER_GROUP", "0")
	requireSchemaValue(t, db, "SELECT count(*) FROM sqlite_schema WHERE name LIKE 'GO_INITIAL_%'", "0")
	requireSchemaValue(t, db, "SELECT STATE FROM SITE_BRUSH_TASK", "Y")
	if _, err := db.Exec("DROP TRIGGER fail_rss"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg+".bak", 0700); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err == nil || !strings.Contains(err.Error(), "record completed scripts") {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg)
	if err != nil || !bytes.Equal(data, []byte(original)) {
		t.Fatalf("%q %v", data, err)
	}
	requireSchemaValue(t, db, "SELECT count(*) FROM GO_INITIAL_DATA_SCRIPTS", "8")
	if _, err := db.Exec("UPDATE SITE_BRUSH_TASK SET STATE='Y'"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cfg+".bak", cfg+".obstruction"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(cfg), "linked.yaml")
	if err := os.Symlink(cfg, link); err != nil {
		t.Skip(err)
	}
	if err := InitializeUserData(path, link); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT STATE FROM SITE_BRUSH_TASK", "Y")
	files, err := config.NewStore(cfg).SyncInitializationFiles()
	if err != nil || !slices.Equal(files, initialScripts) {
		t.Fatalf("repair: %v %v", files, err)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Fatal(err)
	}
}
