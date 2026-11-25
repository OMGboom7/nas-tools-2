package database

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

const oldClients = `pt: &pt {pt_client: transmission, pt_monitor: true, pt_monitor_only: false, rmt_mode: link, other: keep}
pt_copy: *pt
qbittorrent: {qbhost: box.fixture, qbport: "8080", qbusername: user, qbpassword: old}
transmission: {trhost: https://tr.fixture/rpc-base, trport: 9443, trusername: fixture, trpassword: test-only}
downloaddir: [{save_path: /torrents, container_path: /volume, category: film, type: movie, label: cinema, extra: keep}]
laboratory: {release_groups: MyTeam}
`

func oldDownloaderFixture(t *testing.T, yaml string) (*sql.DB, string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	cfg, path := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "user.db")
	if err := os.WriteFile(cfg, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	return userSchemaFixture(t, path, ""), path, cfg
}

func TestLegacyDownloaderUpgradeAndConcurrentRestart(t *testing.T) {
	db, path, cfg := oldDownloaderFixture(t, oldClients)
	// Existing non-default client has a new name, password and directory. A
	// second account on the same endpoint must keep its own row.
	if _, err := db.Exec(`INSERT INTO DOWNLOADER(ID,TYPE,NAME,ENABLED,MATCH_PATH,CONFIG,DOWNLOAD_DIR)
		VALUES(24,'qbittorrent','renamed',1,1,'{"host":"http://BOX.fixture:8080/","username":"user","password":"new"}','[{"save_path":"/edited"}]'),
		(25,'qbittorrent','another',1,1,'{"host":"box.fixture","port":8080,"username":"second"}','[]')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 3)
	for i := 0; i < cap(done); i++ {
		go func() { done <- InitializeUserData(path, cfg) }()
	}
	for i := 0; i < cap(done); i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	requireSchemaValue(t, db, "SELECT count(*) FROM DOWNLOADER", "3")
	requireSchemaValue(t, db, `SELECT NAME||':'||json_extract(CONFIG,'$.password')||':'||json_extract(DOWNLOAD_DIR,'$[0].save_path') FROM DOWNLOADER WHERE ID=24`, "renamed:new:/edited")
	requireSchemaValue(t, db, `SELECT ENABLED||TRANSFER||ONLY_NASTOOL||MATCH_PATH||':'||RMT_MODE FROM DOWNLOADER WHERE TYPE='transmission'`, "1100:link")
	requireSchemaValue(t, db, `SELECT json_extract(CONFIG,'$.port')||':'||json_extract(DOWNLOAD_DIR,'$[0].container_path')||':'||json_extract(DOWNLOAD_DIR,'$[0].extra') FROM DOWNLOADER WHERE TYPE='transmission'`, "9443:/volume:keep")
	requireSchemaValue(t, db, `SELECT NAME FROM DOWNLOADER WHERE ID=(SELECT VALUE FROM SYSTEM_DICT WHERE KEY='DefaultDownloader')`, "Transmission")
	values, err := config.NewStore(cfg).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if values["qbittorrent"] != nil || values["downloaddir"] != nil || values["transmission"] != nil || values["laboratory"].(map[string]any)["release_groups"] != nil || values["pt"].(map[string]any)["pt_client"] != nil || values["pt"].(map[string]any)["other"] != "keep" || values["pt_copy"].(map[string]any)["pt_client"] != "transmission" {
		t.Fatal("incorrect cleaned configuration")
	}
	before, err := os.ReadFile(cfg + ".bak")
	if err != nil || !strings.Contains(string(before), "qbpassword") || !strings.Contains(string(before), "MyTeam") {
		t.Fatal("incomplete shared backup")
	}
}

func TestLegacyDownloaderRetryHonorsDatabaseEdits(t *testing.T) {
	input := "app: {init_files: [" + strings.Join(initialScripts, ", ") + "]}\n" + oldClients
	db, path, cfg := oldDownloaderFixture(t, input)
	if _, err := db.Exec("INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','DefaultDownloader','77')"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg+".bak", 0700); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err == nil {
		t.Fatal("backup failure ignored")
	}
	if _, err := db.Exec(`UPDATE DOWNLOADER SET CONFIG='{"host":"replacement"}',NAME='moved' WHERE TYPE='qbittorrent'; DELETE FROM DOWNLOADER WHERE TYPE='transmission'`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg + ".bak"); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(input, "qbpassword: old", "qbpassword: changed-secret", 1)
	if err := os.WriteFile(cfg, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err == nil || strings.Contains(err.Error(), "changed-secret") {
		t.Fatal("changed source not detected safely")
	}
	if err := os.WriteFile(cfg, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT count(*) FROM DOWNLOADER", "1")
	requireSchemaValue(t, db, "SELECT NAME FROM DOWNLOADER", "moved")
	requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='DefaultDownloader'", "77")
	requireSchemaValue(t, db, "SELECT count(*) FROM GO_LEGACY_DOWNLOADER_IMPORT", "0")
}

func TestLegacyDownloaderAmbiguousAndRejectedWritesRollBack(t *testing.T) {
	for _, stmt := range []string{
		`INSERT INTO DOWNLOADER(TYPE,CONFIG) VALUES('transmission','{"host":"https://tr.fixture:9443/rpc-base","username":"fixture"}'),('transmission','{"host":"https://tr.fixture:9443/rpc-base","username":"fixture"}')`,
		`CREATE TRIGGER reject_client BEFORE INSERT ON DOWNLOADER WHEN NEW.TYPE='transmission' BEGIN SELECT RAISE(IGNORE); END`,
	} {
		db, path, cfg := oldDownloaderFixture(t, oldClients)
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
		if err := InitializeUserData(path, cfg); err == nil {
			t.Fatal("incomplete import accepted")
		}
		requireSchemaValue(t, db, "SELECT count(*) FROM DOWNLOADER WHERE TYPE='qbittorrent'", "0")
		original, err := os.ReadFile(cfg)
		if err != nil || string(original) != oldClients {
			t.Fatal("YAML changed after rollback")
		}
	}
}

func TestPartiallyMigratedDownloaderSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows int
	}{
		{"unique", 1}, {"ambiguous", 2}, {"absent", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, path, cfg := oldDownloaderFixture(t, "pt: {pt_client: transmission, pt_monitor: true, untouched: keep}\ndownloaddir: [{save_path: /original}]\n")
			for i := 0; i < tc.rows; i++ {
				if _, err := db.Exec("INSERT INTO DOWNLOADER(TYPE,ENABLED) VALUES('transmission',1)"); err != nil {
					t.Fatal(err)
				}
			}
			if err := InitializeUserData(path, cfg); err != nil {
				t.Fatal(err)
			}
			data, err := config.NewStore(cfg).Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			pt := data["pt"].(map[string]any)
			if pt["pt_monitor"] != true || data["downloaddir"] == nil {
				t.Fatal("unassigned options removed")
			}
			if tc.rows == 1 {
				requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='DefaultDownloader'", "1")
				if pt["pt_client"] != nil {
					t.Fatal("selector not retired")
				}
			} else {
				requireSchemaValue(t, db, "SELECT COUNT(*) FROM SYSTEM_DICT WHERE KEY='DefaultDownloader'", "0")
				if pt["pt_client"] != "transmission" {
					t.Fatal("unresolved selector lost")
				}
			}
		})
	}
}
