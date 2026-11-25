package database

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestPluginConfigStorage(t *testing.T) {
	db, _, cfg := initialFixture(t, "app: {}\n")
	id := strings.Repeat("配置", 28)
	key := "plugin." + id
	old := `{"large":9007199254740993123,"private":{"nested":"keep"}}`
	if _, err := db.Exec("INSERT INTO SYSTEM_DICT(TYPE, KEY, VALUE, NOTE) VALUES('SystemConfig',?,?, 'old note')", key, old); err != nil {
		t.Fatal(err)
	}
	if err := PluginInstallation(t.Context(), cfg, []string{"install", id}, io.Discard); err != nil {
		t.Fatal(err)
	}
	read := func() (string, string) {
		t.Helper()
		var out bytes.Buffer
		if err := PluginConfig(t.Context(), cfg, []string{"read", id}, nil, &out); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Value    json.RawMessage
			Revision string
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return string(got.Value), got.Revision
	}
	// Other system settings retain their existing NULL handling.
	if _, err := db.Exec("INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','plugin.Cleared',NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := PluginConfig(t.Context(), cfg, []string{"save", "Cleared"}, strings.NewReader(`{"value":{}}`), io.Discard); err != nil {
		t.Fatal(err)
	}
	value, rev := read()
	if value != old || rev != pluginConfigRevision(old) {
		t.Fatal(value, rev)
	}
	save := func(payload string) error {
		return PluginConfig(t.Context(), cfg, []string{"save", id}, strings.NewReader(payload), io.Discard)
	}
	errorsOut := make(chan error, 2)
	for i := range 2 {
		go func() { errorsOut <- save(fmt.Sprintf(`{"value":{"n":%d},"revision":%q}`, i, rev)) }()
	}
	var success, conflict int
	for range 2 {
		err := <-errorsOut
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrPluginConfigChanged):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
	value, rev = read()
	if err := PluginInstallation(t.Context(), cfg, []string{"uninstall", id}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := save(fmt.Sprintf(`{"value":{},"revision":%q}`, rev)); !errors.Is(err, ErrPluginConfigChanged) {
		t.Fatal(err)
	}
	for _, invalid := range []string{`{"value":[]}`, `{"value":{}}{}`, `{"value":{},"revision":""}`} {
		if err := save(invalid); err == nil {
			t.Fatal(invalid)
		}
	}
	if _, err := db.Exec("CREATE TRIGGER no_config BEFORE UPDATE ON SYSTEM_DICT BEGIN SELECT RAISE(IGNORE); END"); err != nil {
		t.Fatal(err)
	}
	if err := save(`{"value":{}}`); err == nil {
		t.Fatal("ignored write succeeded")
	}
	if after, _ := read(); after != value {
		t.Fatal(after)
	}
	requireSchemaValue(t, db, "SELECT NOTE FROM SYSTEM_DICT WHERE NOTE='old note'", "old note")
}
