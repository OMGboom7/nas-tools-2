package database

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestLegacyPluginSettingsUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, key string
		want            map[string]any
	}{
		{"open", "subtitle: {server: opensubtitles, opensubtitles: {enable: false}}", "plugin.OpenSubtitles", map[string]any{"enable": false}},
		{"chinese", "subtitle: {server: other, chinesesubfinder: {host: fixture, api_key: fixture-key, remote_path: /before}}", "plugin.ChineseSubFinder", map[string]any{"host": "fixture", "api_key": "fixture-key", "remote_path": "/before", "local_path": nil}},
		{"empty-provider", "subtitle: {server: opensubtitles}", "plugin.OpenSubtitles", map[string]any{"enable": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, path, cfg := initialFixture(t, "app: {}\nunknown: value # kept\nlaboratory: {release_groups: MyTeam, sibling: keep}\n"+tc.yaml+"\n")
			if err := InitializeUserData(path, cfg); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := db.QueryRow("SELECT VALUE FROM SYSTEM_DICT WHERE KEY=?", tc.key).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if json.Unmarshal([]byte(raw), &fields) != nil || !reflect.DeepEqual(fields, tc.want) {
				t.Fatal("incorrect plugin fields")
			}
			requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.CustomReleaseGroups'", `{"release_groups":"MyTeam"}`)
			requireSchemaValue(t, db, "SELECT count(*) FROM SYSTEM_DICT WHERE KEY='UserInstalledPlugins'", "0")
			current, err := os.ReadFile(cfg)
			if err != nil || strings.Contains(string(current), "subtitle:") || strings.Contains(string(current), "release_groups:") || !strings.Contains(string(current), "# kept") || !strings.Contains(string(current), "sibling: keep") {
				t.Fatal("incorrect YAML cleanup", err)
			}
			original, err := os.ReadFile(cfg + ".bak")
			if err != nil || !strings.Contains(string(original), "subtitle:") {
				t.Fatal("missing original subtitle settings")
			}
			if err := InitializeUserData(path, cfg); err != nil {
				t.Fatal(err)
			}
			again, err := os.ReadFile(cfg)
			if err != nil || string(current) != string(again) {
				t.Fatal("restart rewrote YAML")
			}
		})
	}
}

func TestLegacyPluginSettingsResumeAndSharedYAML(t *testing.T) {
	input := "app: {init_files: [" + strings.Join(initialScripts, ", ") + "]}\nbase: &old {release_groups: KeepTeam, other: keep}\nlaboratory: *old\ncopy: *old\nsubtitle: &sub {server: opensubtitles, opensubtitles: {enable: true}}\ncopy_sub: *sub\n"
	db, path, cfg := initialFixture(t, input)
	if _, err := db.Exec(`CREATE TRIGGER reject_plugin BEFORE INSERT ON SYSTEM_DICT WHEN NEW.KEY='plugin.OpenSubtitles' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err == nil {
		t.Fatal("ignored insert accepted")
	}
	requireSchemaValue(t, db, "SELECT count(*) FROM SYSTEM_DICT WHERE KEY LIKE 'plugin.%'", "0")
	if err := os.Mkdir(cfg+".bak", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_plugin"); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err == nil {
		t.Fatal("backup failure accepted")
	}
	current, err := os.ReadFile(cfg)
	if err != nil || string(current) != input {
		t.Fatal("source changed before cleanup")
	}
	requireSchemaValue(t, db, "SELECT count(*) FROM SYSTEM_DICT WHERE KEY LIKE 'plugin.%'", "2")
	if _, err := db.Exec(`UPDATE SYSTEM_DICT SET VALUE='{"enable":false,"future":1}' WHERE KEY='plugin.OpenSubtitles'`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg + ".bak"); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err != nil {
		t.Fatal(err)
	}
	requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.OpenSubtitles'", `{"enable":false,"future":1}`)
	values, err := config.NewStore(cfg).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := values["subtitle"]; exists || values["laboratory"].(map[string]any)["release_groups"] != nil || values["copy"].(map[string]any)["release_groups"] != "KeepTeam" || values["copy_sub"].(map[string]any)["server"] != "opensubtitles" {
		t.Fatal("shared YAML alias changed")
	}
}

func TestLegacyPluginSettingsPreserveInvalidDestination(t *testing.T) {
	input := "app: {}\nsubtitle: {server: opensubtitles, opensubtitles: {enable: false}}\n"
	db, path, cfg := initialFixture(t, input)
	if _, err := db.Exec("INSERT INTO SYSTEM_DICT(TYPE, KEY, VALUE) VALUES('SystemConfig','plugin.OpenSubtitles','broken-private-fixture')"); err != nil {
		t.Fatal(err)
	}
	if err := InitializeUserData(path, cfg); err == nil || strings.Contains(err.Error(), "private-fixture") {
		t.Fatal("expected a redacted error")
	}
	contents, err := os.ReadFile(cfg)
	if err != nil || string(contents) != input {
		t.Fatal("removed original YAML settings")
	}
	requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.OpenSubtitles'", "broken-private-fixture")
}
