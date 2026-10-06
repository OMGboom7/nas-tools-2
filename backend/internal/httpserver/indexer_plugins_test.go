package httpserver

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestPluginIndexersServeOptionsWithoutPython(t *testing.T) {
	_, _, databasePath := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := map[string]string{
		"UserInstalledPlugins": `["Jackett","Prowlarr","CustomHosts","Customization","Jackett"]`,
		"UserIndexerSites":     `["private-jackett","Selected-prowlarr"]`,
		"plugin.Jackett":       `{"host":"http://jackett.local/base","api_key":"jackett-secret","cron":"0 0 * * *"}`,
		"plugin.Prowlarr":      `{"host":"http://prowlarr.local","api_key":"prowlarr-secret"}`,
	}
	for key, value := range settings {
		if err := store.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	badResponse := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if badResponse {
			return jsonResponse(r, `{"broken":true}`), nil
		}
		switch r.URL.Hostname() {
		case "jackett.local":
			if r.URL.Path != "/base/api/v2.0/indexers" || r.Header.Get("X-Api-Key") != "jackett-secret" {
				t.Fatalf("bad Jackett request: %s", r.URL)
			}
			return jsonResponse(r, `[{"id":"private","name":"Private","type":"private"},{"id":"unselected","name":"Not selected","type":"public"}]`), nil
		case "prowlarr.local":
			if r.URL.Path != "/api/v1/indexerstats" || r.Header.Get("X-Api-Key") != "prowlarr-secret" {
				t.Fatalf("bad Prowlarr request: %s", r.URL)
			}
			return jsonResponse(r, `{"indexers":[{"indexerId":42,"indexerName":"Selected"}]}`), nil
		default:
			t.Fatalf("unexpected upstream (Python prohibited): %s", r.URL)
			return nil, nil
		}
	})
	handler, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(databasePath), "config.yaml"), SiteCatalogPath: "../../../web/backend/user.sites.bin", DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, handler, "admin", "password")
	w := performFormRequest(handler, "/api/v1/site/indexers", token, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"private-jackett"`) || !strings.Contains(w.Body.String(), `"id":"Selected-prowlarr"`) || strings.Contains(w.Body.String(), "unselected") || strings.Contains(w.Body.String(), "secret") || calls != 2 {
		t.Fatalf("calls=%d status=%d %s", calls, w.Code, w.Body.String())
	}
	w = nativeJSONRequest(handler, http.MethodGet, "/api/v1/subscriptions/options", token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"value":"private-jackett"`) || strings.Contains(w.Body.String(), "searchSites unavailable") || calls != 4 {
		t.Fatalf("options=%d %s calls=%d", w.Code, w.Body.String(), calls)
	}
	badResponse = true
	w = performFormRequest(handler, "/api/v1/site/indexers", token, nil)
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("bad response=%d %s", w.Code, w.Body.String())
	}
	// Missing plugin configuration is an unconfigured provider, not a legacy call.
	badResponse = false
	if err := store.Set(t.Context(), "plugin.Jackett", `{}`); err != nil {
		t.Fatal(err)
	}
	w = performFormRequest(handler, "/api/v1/site/indexers", token, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-jackett") {
		t.Fatalf("unconfigured=%d %s", w.Code, w.Body.String())
	}
}
