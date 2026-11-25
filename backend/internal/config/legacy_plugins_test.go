package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetireInheritedPluginSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const original = "base: &defaults\n  laboratory: {release_groups: Teams, extra: keep}\n  subtitle: {server: opensubtitles}\n<<: *defaults # inherited\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	plans, err := store.LegacySettings()
	if err != nil || len(plans.Records) != 2 {
		t.Fatal("missing inherited settings", err)
	}
	if err := store.RetireLegacySettings(plans, false); err != nil {
		t.Fatal(err)
	}
	values, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if values["subtitle"] != nil || values["laboratory"].(map[string]any)["extra"] != "keep" || values["base"].(map[string]any)["subtitle"] == nil {
		t.Fatal("lost inherited fields")
	}
	next, err := store.LegacySettings()
	if err != nil || len(next.Records) != 0 {
		t.Fatal("legacy fields remain active", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(current), "# inherited") {
		t.Fatal("lost unrelated comment")
	}
}

func TestLegacyPluginCleanupRequiresMatchingSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	store := NewStore(path)
	for _, disabled := range []string{"false", "[]", "null", "''", "0"} {
		if err := os.WriteFile(path, []byte("subtitle: "+disabled+"\nlaboratory: {release_groups: "+disabled+"}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		fields, err := store.LegacySettings()
		if err != nil || len(fields.Records) != 0 {
			t.Fatal("disabled legacy setting", err)
		}
	}
	if err := os.WriteFile(path, []byte("laboratory: {release_groups: A}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fields, err := store.LegacySettings()
	if err != nil {
		t.Fatal(err)
	}
	const changed = "laboratory: {release_groups: B}\n"
	if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireLegacySettings(fields, false); err == nil {
		t.Fatal("removed edited settings")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != changed {
		t.Fatal("changed YAML lost")
	}
}
