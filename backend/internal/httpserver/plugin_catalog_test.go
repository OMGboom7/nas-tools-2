package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestNativePluginCatalogRetainsEntireBundledAndUnknownInstalledDirectory(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	list := func(authorization string) pluginsData {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("native catalog=%d %s", response.Code, response.Body.String())
		}
		var payload struct{ Data pluginsData }
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Data
	}
	data := list(token)
	if len(data.Items) != 30 || data.InstalledCount != 0 {
		t.Fatalf("bundled catalog shrank: items=%d installed=%d", len(data.Items), data.InstalledCount)
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["CookieCloud","CustomReleaseGroups","GhostPlugin","GhostPlugin"]`); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(t.Context(), "plugin.CustomReleaseGroups", `{"release_groups":"NativeGroup","cache":{"secret":"do-not-return"}}`); err != nil {
		t.Fatal(err)
	}
	data = list(token)
	if len(data.Items) != 31 || data.InstalledCount != 3 || data.RunningCount != 1 || data.UnknownStateCount != 2 {
		t.Fatalf("installed catalog=%+v", data)
	}
	for _, item := range data.Items {
		switch item.ID {
		case "CustomReleaseGroups":
			if !item.Native || !item.StateKnown || !item.Running || !item.ActionsAvailable {
				t.Fatal(item)
			}
		case "CookieCloud":
			if item.Native || item.StateKnown || !item.Installed || !item.HasPage || !item.ActionsAvailable {
				t.Fatal(item)
			}
		case "GhostPlugin":
			if item.StateKnown || !item.Installed || item.ActionsAvailable {
				t.Fatal(item)
			}
		}
	}
	encoded, _ := json.Marshal(data)
	if strings.Contains(string(encoded), "do-not-return") || strings.Contains(string(encoded), "NativeGroup") {
		t.Fatal("catalog leaked plugin settings")
	}
	response := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	visible := list(viewer)
	var entries []pluginCatalogEntry
	if err := json.Unmarshal([]byte(nativePluginCatalogJSON), &entries); err != nil {
		t.Fatal(err)
	}
	restricted := map[string]bool{"GhostPlugin": true}
	for _, entry := range entries {
		if entry.AuthLevel > 1 {
			restricted[entry.ID] = true
		}
	}
	for _, item := range visible.Items {
		if restricted[item.ID] {
			t.Fatalf("administrator-only plugin exposed: %s", item.ID)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal(response.Code)
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `not-json`); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
	request.Header.Set("Authorization", token)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 502 {
		t.Fatal(response.Code)
	}
}

func TestGoOnlyCatalogDoesNotAdvertiseUnmigratedActionsAsAvailable(t *testing.T) {
	_, _, path := nativeServicesFixture(t, "", nil, nil)
	cfg := config.Config{ApplicationConfigPath: strings.TrimSuffix(path, "user.db") + "config.yaml", DisableLegacy: true}
	handler, err := newHandler(cfg, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("Go-only catalog accessed Python: %s", request.URL.Path)
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, handler, "admin", "password")
	request := httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var payload struct{ Data pluginsData }
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != 200 || len(payload.Data.Items) != 30 {
		t.Fatalf("Go-only catalog=%d %s", response.Code, response.Body.String())
	}
	for _, item := range payload.Data.Items {
		if item.ActionsAvailable != nativeMetadataPlugin(item.ID) {
			t.Fatalf("unsupported Go-only action advertised: %+v", item)
		}
	}
}
