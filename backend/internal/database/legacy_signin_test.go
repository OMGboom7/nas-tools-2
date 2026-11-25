package database

import (
	"os"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

func TestLegacySigninSchedulesAndInstallation(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"6", "0 */6 * * *"}, {"'08:03'", "3 8 * * *"}, {"'7:05-09:30'", "5 7 * * *"}, {"'5 12 * * mon-fri'", "5 12 * * mon-fri"}, {"daily", "30 8 * * *"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			db, path, cfg := initialFixture(t, "app: {}\npt: &pt {ptsignin_cron: "+tc.source+", other: keep}\ncopy: *pt\n")
			if _, err := db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','UserInstalledPlugins','["Other"]')`); err != nil {
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
			var raw string
			if err := db.QueryRow("SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.AutoSignIn'").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			settings, err := sitesignin.ParseSettings(raw)
			if err != nil || !settings.Enabled || !settings.Notify || settings.Once || settings.Concurrency != 10 || settings.Cron != tc.want || len(settings.Sites)+len(settings.Special) != 0 {
				t.Fatal("incorrect converted settings", err)
			}
			requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='UserInstalledPlugins'", `["Other","AutoSignIn"]`)
			data, err := config.NewStore(cfg).Snapshot()
			if err != nil || data["pt"].(map[string]any)["ptsignin_cron"] != nil || data["copy"].(map[string]any)["ptsignin_cron"] == nil {
				t.Fatal("incorrect source cleanup", err)
			}
		})
	}
}

func TestLegacySigninDoesNotReinstallOnRetry(t *testing.T) {
	input := "app: {init_files: [" + strings.Join(initialScripts, ", ") + "]}\npt: {ptsignin_cron: 4}\n"
	db, path, cfg := initialFixture(t, input)
	if err := os.Mkdir(cfg+".bak", 0700); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err == nil {
		t.Fatal("backup failure ignored")
	}
	requireSchemaValue(t, db, "SELECT COUNT(*) FROM GO_LEGACY_PLUGIN_INSTALLS", "1")
	if _, err := db.Exec(`UPDATE SYSTEM_DICT SET VALUE='[]' WHERE KEY='UserInstalledPlugins'; UPDATE SYSTEM_DICT SET VALUE='{"enabled":false,"future":true}' WHERE KEY='plugin.AutoSignIn'`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg + ".bak"); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='UserInstalledPlugins'", "[]")
	requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.AutoSignIn'", `{"enabled":false,"future":true}`)
}

func TestLegacySigninExistingPreferenceAndAtomicFailure(t *testing.T) {
	for _, tc := range []struct {
		name, cron, setup string
		reject            bool
	}{
		{"already removed", "3", `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES ('SystemConfig','UserInstalledPlugins','["Other"]'),('SystemConfig','plugin.AutoSignIn','{"enabled":false,"future":true}')`, false},
		{"invalid list", "3", `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES ('SystemConfig','UserInstalledPlugins','["invalid id"]')`, true},
		{"rejected receipt", "3", `CREATE TABLE GO_LEGACY_PLUGIN_INSTALLS(PLUGIN TEXT PRIMARY KEY); CREATE TRIGGER refuse BEFORE INSERT ON GO_LEGACY_PLUGIN_INSTALLS BEGIN SELECT RAISE(IGNORE); END`, true},
		{"invalid time", "'26:09'", "", true}, {"invalid cron", "'0 0 * 15 *'", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "app: {}\npt: {ptsignin_cron: " + tc.cron + "}\n"
			db, path, cfg := initialFixture(t, input)
			if tc.setup != "" {
				if _, err := db.Exec(tc.setup); err != nil {
					t.Fatal(err)
				}
			}
			err := InitializeUserData(path, cfg)
			if (err != nil) != tc.reject {
				t.Fatal("unexpected migration result", err)
			}
			if tc.reject {
				got, err := os.ReadFile(cfg)
				if err != nil || string(got) != input {
					t.Fatal("lost source settings")
				}
				requireSchemaValue(t, db, "SELECT count(*) FROM SYSTEM_DICT WHERE KEY='plugin.AutoSignIn'", "0")
			} else {
				requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='UserInstalledPlugins'", `["Other"]`)
				requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.AutoSignIn'", `{"enabled":false,"future":true}`)
			}
		})
	}
}
