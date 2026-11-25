package database

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestInstalledPluginEditorsSnapshotAndConflict(t *testing.T) {
	db, _, cfg := initialFixture(t, "app: {}\n")
	id := strings.Repeat("扩展", 30)
	for _, name := range []string{id, "Empty", "Admin"} {
		if err := PluginInstallation(t.Context(), cfg, []string{"install", name}, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	const original = `{"big":9007199254740993,"private":"saved"}`
	if _, err := db.Exec("INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig',?,?),('SystemConfig','plugin.Empty','[ ]  ')", "plugin."+id, original); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	rev := pluginConfigRevision(original)
	dynamic := map[string]any{"id": id, "level": 1, "digest": digest, "revision": rev, "detail": map[string]any{"fields": []any{map[string]any{"content": map[string]any{"id": "private", "placeholder": "discard-me"}}}}}
	forms := []any{map[string]any{"id": "Empty", "level": 1, "detail": map[string]any{}}, dynamic, map[string]any{"id": "Admin", "level": 2, "detail": map[string]any{}}, map[string]any{"id": "Removed", "level": 1, "detail": map[string]any{}}}
	run := func(level string, selected any) (string, error) {
		b, err := json.Marshal(map[string]any{"selected": selected, "forms": forms})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = PluginEditors(t.Context(), cfg, level, bytes.NewReader(b), &out)
		return out.String(), err
	}
	preflight, err := json.Marshal(map[string]any{"revisionsOnly": true, "forms": forms})
	if err != nil {
		t.Fatal(err)
	}
	var hashes bytes.Buffer
	if err := PluginEditors(t.Context(), cfg, "1", bytes.NewReader(preflight), &hashes); err == nil || hashes.Len() != 0 {
		t.Fatal("viewer accessed external revisions")
	}
	if err := PluginEditors(t.Context(), cfg, "2", bytes.NewReader(preflight), &hashes); err != nil {
		t.Fatal(err)
	}
	var revisions map[string]string
	if json.Unmarshal(hashes.Bytes(), &revisions) != nil || len(revisions) != 3 || revisions[id] != rev || strings.Contains(hashes.String(), "saved") {
		t.Fatal(hashes.String())
	}
	result, err := run("1", nil)
	if err != nil || strings.Contains(result, "Admin") || strings.Contains(result, "Removed") || strings.Contains(result, "config_revision") || !strings.HasPrefix(result, `[{"id":"Empty","detail":{"config":{}}}`) || !strings.Contains(result, "9007199254740993") {
		t.Fatal(result, err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM SYSTEM_DICT WHERE KEY=?", "GoExternalPluginFields."+id).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	result, err = run("2", "Empty")
	if err != nil || !strings.Contains(result, `"config":[]`) {
		t.Fatal(result, err)
	}
	result, err = run("2", id)
	if err != nil || !strings.Contains(result, rev) {
		t.Fatal(result, err)
	}
	var saved string
	if err := db.QueryRow("SELECT VALUE FROM SYSTEM_DICT WHERE KEY=?", "GoExternalPluginFields."+id).Scan(&saved); err != nil || !strings.Contains(saved, `"keys":["private"]`) || strings.Contains(saved, "discard-me") || strings.Contains(saved, "saved") {
		t.Fatal(saved, err)
	}
	if _, err := db.Exec("UPDATE SYSTEM_DICT SET VALUE='{}' WHERE KEY=?", "plugin."+id); err != nil {
		t.Fatal(err)
	}
	result, err = run("2", id)
	if !errors.Is(err, ErrPluginConfigChanged) || result != "" {
		t.Fatal(result, err)
	}
}
