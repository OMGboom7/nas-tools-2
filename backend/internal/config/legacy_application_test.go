package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyApplicationUpgradeAndSharedMappings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const source = `app: &app {login_user: kept}
copy_app: *app
laboratory: &lab {tmdb_proxy: true, release_groups: Teams, future: kept}
copy_lab: *lab
ocr_base: &ocr {custom_ocr_url: "https://fixture.invalid/ocr", extra: 1}
ocr: *ocr
baiduocr: &old {api_key: old-id, secret_key: old-secret}
reference: *old
pt: &pt {ptsignin_cron: 6}
copy_pt: *pt
`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	p, err := store.LegacySettings()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RetireLegacySettings(p, false); err != nil {
		t.Fatal(err)
	}
	values, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	section := func(key string) map[string]any { return values[key].(map[string]any) }
	if section("app")["tmdb_domain"] != "api.tmdb.org" || section("copy_app")["tmdb_domain"] != nil || section("laboratory")["tmdb_proxy"] != nil || section("copy_lab")["tmdb_proxy"] != true || section("laboratory")["future"] != "kept" {
		t.Fatal("TMDB settings or shared mappings changed")
	}
	if section("ocr")["baiduocr_api_key"] != "old-id" || section("ocr")["baiduocr_secret_key"] != "old-secret" || section("ocr")["extra"] != 1 || section("ocr_base")["baiduocr_api_key"] != nil || values["baiduocr"] != nil || section("reference")["secret_key"] != "old-secret" {
		t.Fatal("incorrect OCR upgrade")
	}
	if section("pt")["ptrefresh_date_cron"] != "6" || section("copy_pt")["ptrefresh_date_cron"] != nil || section("pt")["ptsignin_cron"] != nil {
		t.Fatal("incorrect refresh schedule")
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil || string(backup) != source {
		t.Fatal("missing complete original configuration")
	}
	again, err := store.LegacySettings()
	if err != nil || !again.Empty() {
		t.Fatal("upgrade not idempotent", err)
	}
}

func TestLegacyApplicationKeepsExplicitModernSettings(t *testing.T) {
	for _, current := range []string{"''", "new-id"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		src := "app: {tmdb_domain: tmdb.fixture}\nlaboratory: {tmdb_proxy: true}\npt: {ptrefresh_date_cron: null}\nocr: {baiduocr_api_key: " + current + ", custom_ocr_url: https://fixture.invalid}\nbaiduocr: {api_key: old-id, secret_key: old-secret}\n"
		if err := os.WriteFile(path, []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
		store := NewStore(path)
		plan, err := store.LegacySettings()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RetireLegacySettings(plan, false); err != nil {
			t.Fatal(err)
		}
		out, err := store.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		ocr := out["ocr"].(map[string]any)
		if ocr["baiduocr_api_key"] != strings.Trim(current, "'") || ocr["baiduocr_secret_key"] != nil || out["app"].(map[string]any)["tmdb_domain"] != "tmdb.fixture" {
			t.Fatal("modern selection overwritten")
		}
		if v, exists := out["pt"].(map[string]any)["ptrefresh_date_cron"]; !exists || v != nil {
			t.Fatal("disabled schedule overwritten")
		}
	}
}

func TestLegacyApplicationInitializesEmptyOCR(t *testing.T) {
	for _, src := range []string{"app: {}\n", "ocr: null\n", "source: &off false\nocr: *off\n", "base: &base {ocr: {}}\n<<: *base\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
		store := NewStore(path)
		plan, err := store.LegacySettings()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RetireLegacySettings(plan, false); err != nil {
			t.Fatal(err)
		}
		values, err := store.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		ocr := values["ocr"].(map[string]any)
		for _, key := range []string{"custom_ocr_url", "baiduocr_api_key", "baiduocr_secret_key"} {
			if ocr[key] != "" {
				t.Fatal("missing empty OCR default")
			}
		}
	}
}
