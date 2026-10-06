package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestNativeMetadataPluginLifecycleActivatesAndStopsRealMatcher(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config := `{"release_groups":"NativeGroup","separator":"+","unknown":{"preserve":true}}`
	if err := store.Set(t.Context(), "plugin.CustomReleaseGroups", config); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["CookieCloud"]`); err != nil {
		t.Fatal(err)
	}
	call := func(method, id, authorization string) *httptest.ResponseRecorder {
		t.Helper()
		route := "/api/v1/plugins/" + id
		if method == http.MethodPost {
			route += "/install"
		}
		request := httptest.NewRequest(method, route, nil)
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	checkMatch := func(expected string) {
		t.Helper()
		options, err := mediameta.ReadLabelOptions(t.Context(), store)
		if err != nil {
			t.Fatal(err)
		}
		team, _, err := mediameta.MatchLabels(t.Context(), "[NativeGroup] Movie.2025", options)
		if err != nil || team != expected {
			t.Fatalf("actual native plugin behavior=%q expected=%q err=%v", team, expected, err)
		}
	}
	checkMatch("")
	for index := 0; index < 2; index++ {
		if response := call(http.MethodPost, "CustomReleaseGroups", token); response.Code != 200 {
			t.Fatalf("install=%d %s", response.Code, response.Body.String())
		}
	}
	checkMatch("NativeGroup")
	raw, _ := store.Get(t.Context(), "UserInstalledPlugins")
	if raw != `["CookieCloud","CustomReleaseGroups"]` {
		t.Fatalf("install list=%s", raw)
	}
	if response := call(http.MethodDelete, "CustomReleaseGroups", token); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	checkMatch("")
	raw, _ = store.Get(t.Context(), "plugin.CustomReleaseGroups")
	if raw != config {
		t.Fatalf("uninstall erased saved settings: %s", raw)
	}
	if response := call(http.MethodDelete, "CustomReleaseGroups", token); response.Code != 409 {
		t.Fatal(response.Code)
	}
	if response := call(http.MethodPost, "CustomReleaseGroups", token); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	checkMatch("NativeGroup")
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["CookieCloud","CustomReleaseGroups","CustomReleaseGroups"]`); err != nil {
		t.Fatal(err)
	}
	if response := call(http.MethodDelete, "CustomReleaseGroups", token); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	raw, _ = store.Get(t.Context(), "UserInstalledPlugins")
	if raw != `["CookieCloud"]` {
		t.Fatalf("duplicate removal/unrelated state=%s", raw)
	}
	if response := call(http.MethodPost, "Customization", ""); response.Code != 401 {
		t.Fatal(response.Code)
	}
	if response := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}}); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if response := call(method, "Customization", viewer); response.Code != 403 {
			t.Fatal(response.Code)
		}
	}
	if err := store.Set(t.Context(), "plugin.Customization", `{"customization":"("}`); err != nil {
		t.Fatal(err)
	}
	if response := call(http.MethodPost, "Customization", token); response.Code != 400 {
		t.Fatalf("invalid activation=%d %s", response.Code, response.Body.String())
	}
	raw, _ = store.Get(t.Context(), "UserInstalledPlugins")
	if raw != `["CookieCloud"]` {
		t.Fatalf("failed activation changed state=%s", raw)
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `not-json`); err != nil {
		t.Fatal(err)
	}
	if response := call(http.MethodPost, "CustomReleaseGroups", token); response.Code != 502 {
		t.Fatal(response.Code)
	}
	raw, _ = store.Get(t.Context(), "UserInstalledPlugins")
	if raw != "not-json" {
		t.Fatal("corrupt registry overwritten")
	}
}

func TestConcurrentNativePluginInstallsPreserveBothEntries(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	var wait sync.WaitGroup
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			id := "Customization"
			if index%2 == 0 {
				id = "CustomReleaseGroups"
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/"+id+"/install", nil)
			request.Header.Set("Authorization", token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Errorf("concurrent install=%d %s", response.Code, response.Body.String())
			}
		}(index)
	}
	wait.Wait()
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, err := store.Get(t.Context(), "UserInstalledPlugins")
	if err != nil {
		t.Fatal(err)
	}
	var installed []string
	if err := json.Unmarshal([]byte(raw), &installed); err != nil || len(installed) != 2 || !strings.Contains(raw, "CustomReleaseGroups") || !strings.Contains(raw, "Customization") {
		t.Fatalf("concurrent installed list=%s %v", raw, err)
	}
}
