package database

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestPluginMarketPreservesDiscoveryOrderAndReadsMembership(t *testing.T) {
	_, _, cfg := initialFixture(t, "app: {}\n")
	if err := PluginInstallation(t.Context(), cfg, []string{"install", "外部"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	const catalog = `[{"level":1,"fields":{"id":"外部","version":42,"script":"private","installed":false}},{"level":2,"fields":{"id":"Admin","name":"hidden"}},{"level":1,"fields":{"id":"First"}}]`
	for _, tc := range []struct {
		level string
		ids   []string
	}{{"1", []string{"外部", "First"}}, {"2", []string{"外部", "Admin", "First"}}} {
		var out bytes.Buffer
		if err := PluginMarket(t.Context(), cfg, tc.level, strings.NewReader(catalog), &out); err != nil {
			t.Fatal(err)
		}
		var rows []map[string]any
		if json.Unmarshal(out.Bytes(), &rows) != nil || len(rows) != len(tc.ids) || rows[0]["installed"] != true || rows[0]["version"] != float64(42) || strings.Contains(out.String(), "private") {
			t.Fatal(out.String())
		}
		for i, id := range tc.ids {
			if rows[i]["id"] != id {
				t.Fatal(rows)
			}
		}
	}
}
