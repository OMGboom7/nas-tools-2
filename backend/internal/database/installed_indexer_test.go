package database

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInstalledIndexerRechecksSavedAccountAndMembership(t *testing.T) {
	_, _, cfg := initialFixture(t, "app: {}\n")
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/indexerstats" || r.Header.Get("X-Api-Key") != "fixture-key" {
			t.Error("unexpected discovery request")
		}
		calls.Add(1)
		io.WriteString(w, `{"indexers":[{"indexerId":12,"indexerName":"Test"}]}`)
	}))
	defer up.Close()
	payload, _ := json.Marshal(map[string]any{"value": map[string]string{"host": up.URL, "api_key": "fixture-key"}})
	if err := PluginConfig(t.Context(), cfg, []string{"save", "Prowlarr"}, bytes.NewReader(payload), io.Discard); err != nil {
		t.Fatal(err)
	}
	query := func(action, body string) (string, error) {
		var out bytes.Buffer
		err := InstalledIndexerCommand(t.Context(), cfg, "Prowlarr", action, strings.NewReader(body), &out)
		return out.String(), err
	}
	if _, err := query("discover", "{}"); err == nil {
		t.Fatal("used settings of uninstalled provider")
	}
	if err := PluginInstallation(t.Context(), cfg, []string{"install", "Prowlarr"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out, err := query("discover", "{}"); err != nil || !strings.Contains(out, "Test-prowlarr") || strings.Contains(out, "fixture-key") {
		t.Fatal(out, err)
	}
	for _, body := range []string{`{"domain":"https://other.invalid/api/v1/indexer/12"}`, `{"Config":{"Host":"https://other.invalid"}}`} {
		if out, err := query("search", body); err == nil || out != "" {
			t.Fatal("accepted stale endpoint or caller-supplied settings")
		}
	}
	if err := PluginInstallation(t.Context(), cfg, []string{"uninstall", "Prowlarr"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := query("discover", "{}"); err == nil || calls.Load() != 1 {
		t.Fatal("used retained credentials")
	}
}
