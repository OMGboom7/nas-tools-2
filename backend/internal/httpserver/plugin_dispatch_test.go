package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestPluginManagementCommandsShareGoStateWithReactAcrossRestart(t *testing.T) {
	_, _, db := nativeServicesFixture(t, "", nil, nil)
	cfg := config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(db), "config.yaml"), DisableLegacy: true}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected outgoing plugin request: %s", r.URL.Path)
		return nil, nil
	})
	start := func() (http.Handler, string) {
		t.Helper()
		h, err := newHandler(cfg, transport)
		if err != nil {
			t.Fatal(err)
		}
		return h, loginForTest(t, h, "admin", "password")
	}
	h, admin := start()
	request := func(token, command, data string, want int) map[string]any {
		t.Helper()
		w := nativeJSONRequest(h, "POST", "/do", token, `{"cmd":"`+command+`","data":`+data+`}`)
		var out map[string]any
		if w.Code != want || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s = %d %s", command, w.Code, w.Body.String())
		}
		return out
	}
	const id = `{"id":"CustomReleaseGroups"}`
	const settings = `{"plugin":"CustomReleaseGroups","config":{"release_groups":"GroupA","separator":"+","extension":"& 空格 + 原样"}}`
	request("", "get_plugin_state", id, 401)
	request("", "install_plugin", id, 401)
	request(admin, "update_plugin_config", settings, 200)
	request(admin, "install_plugin", id, 200)
	if request(admin, "get_plugin_state", id, 200)["state"] != true {
		t.Fatal("original command did not activate saved settings")
	}
	if w := nativeJSONRequest(h, "GET", "/api/v1/plugins/CustomReleaseGroups", admin, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := performFormRequest(h, "/api/v1/user/manage", admin, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	viewer := loginForTest(t, h, "viewer", "strong-password")
	request(viewer, "uninstall_plugin", id, 403)
	request(viewer, "update_plugin_config", settings, 403)
	for _, data := range []string{`{"id":"CustomReleaseGroups","config":{}}`, `{"plugin":"CustomReleaseGroups","config":null}`, `{"plugin":"CustomReleaseGroups","config":[]}`, `{"plugin":"CustomReleaseGroups","config":"{}"}`} {
		request(admin, "update_plugin_config", data, 400)
	}
	h, admin = start()
	if request(admin, "get_plugin_state", id, 200)["state"] != true {
		t.Fatal("lost settings on reopening database")
	}
	request(admin, "uninstall_plugin", id, 200)
	request(admin, "uninstall_plugin", id, 200)
	if request(admin, "get_plugin_state", id, 200)["state"] != nil {
		t.Fatal("uninstalled plugin still active")
	}
	dbStore, err := systemconfig.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer dbStore.Close()
	raw, err := dbStore.Get(t.Context(), "plugin.CustomReleaseGroups")
	var saved map[string]string
	if err != nil || json.Unmarshal([]byte(raw), &saved) != nil || saved["extension"] != "& 空格 + 原样" {
		t.Fatal(raw, err)
	}
	// A scheduled plugin has a distinct executor and requires its own config.
	const timer = `{"id":"SyncTimer"}`
	request(admin, "install_plugin", timer, 200)
	request(admin, "get_plugin_state", timer, 200)
	request(admin, "uninstall_plugin", timer, 200)
}

func TestUnavailableGoPluginRetainsExternalStateAndHonorsCatalogVisibility(t *testing.T) {
	_, _, db := nativeServicesFixture(t, "", nil, nil)
	h, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(db), "config.yaml"), DisableLegacy: true}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected external call for %s", r.URL.Path)
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	store, err := systemconfig.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for key, value := range map[string]string{"UserInstalledPlugins": `["ExternalPlugin"]`, "plugin.ExternalPlugin": `{"token":"private-fixture"}`} {
		if err := store.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	admin := loginForTest(t, h, "admin", "password")
	if w := performFormRequest(h, "/api/v1/user/manage", admin, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, actor := range []struct {
		token  string
		status int
	}{{admin, 409}, {loginForTest(t, h, "viewer", "strong-password"), 404}, {"invalid", 401}} {
		for _, route := range []struct{ method, path, body string }{
			{"GET", "/api/v1/plugins/ExternalPlugin", ""},
			{"PUT", "/api/v1/plugins/ExternalPlugin", `{"values":{}}`},
			{"DELETE", "/api/v1/plugins/ExternalPlugin", ""},
			{"POST", "/api/v1/plugins/ExternalPlugin/install", ""},
			{"GET", "/api/v1/plugins/ExternalPlugin/page", ""},
			{"POST", "/do", `{"cmd":"uninstall_plugin","data":{"id":"ExternalPlugin"}}`},
		} {
			w := nativeJSONRequest(h, route.method, route.path, actor.token, route.body)
			want := actor.status
			if actor.token == admin && route.method == "GET" && route.path == "/api/v1/plugins/ExternalPlugin" {
				want = http.StatusOK
				if !strings.Contains(w.Body.String(), `"other":1`) || strings.Contains(w.Body.String(), `"key":"token"`) {
					t.Fatal("undeclared stored configuration was not kept private", w.Body.String())
				}
			}
			if w.Code != want || strings.Contains(w.Body.String(), "private-fixture") {
				t.Fatal(route, w.Code, w.Body.String())
			}
		}
		for _, action := range []string{"status", "install", "uninstall", "config", "page", "page/action"} {
			w := performFormRequest(h, "/api/v1/plugin/"+action, actor.token, url.Values{"id": {"ExternalPlugin"}})
			if w.Code != actor.status {
				t.Fatal(action, w.Code, w.Body.String())
			}
		}
	}
	for key, value := range map[string]string{"UserInstalledPlugins": `["ExternalPlugin"]`, "plugin.ExternalPlugin": `{"token":"private-fixture"}`} {
		if raw, err := store.Get(t.Context(), key); err != nil || raw != value {
			t.Fatal(key, raw, err)
		}
	}
}
