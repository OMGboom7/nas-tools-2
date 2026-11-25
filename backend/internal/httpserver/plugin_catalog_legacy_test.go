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

func TestLegacyPluginMarketUsesEntireNativeCatalogWithoutPluginExecution(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["CookieCloud","Customization","UnknownInstalled"]`); err != nil {
		t.Fatal(err)
	}
	// An invalid runtime config must not prevent browsing catalog metadata.
	if err := store.Set(t.Context(), "plugin.Customization", `{"customization":"("}`); err != nil {
		t.Fatal(err)
	}
	read := func(authorization string) map[string]any {
		t.Helper()
		response := performFormRequest(handler, "/api/v1/plugin/apps", authorization, nil)
		if response.Code != 200 {
			t.Fatalf("old native market=%d %s", response.Code, response.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(objectValue(payload["statistic"])) != 0 {
			t.Fatal("invented install statistics")
		}
		return objectValue(payload["result"])
	}
	result := read(token)
	if len(result) != 31 || objectValue(result["CookieCloud"])["installed"] != true || objectValue(result["UnknownInstalled"])["installed"] != true {
		t.Fatalf("old market lost plugins: %d", len(result))
	}
	var entries []pluginCatalogEntry
	if err := json.Unmarshal([]byte(nativePluginCatalogJSON), &entries); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		item := objectValue(result[entry.ID])
		if entry.ID == "OpenSubtitles" {
			entry.Description = "使用原网页或明确配置的官方 REST 提供方，下载中文字幕并保留已有文件"
		}
		for key, want := range map[string]any{"id": entry.ID, "name": entry.Name, "desc": entry.Description, "version": entry.Version, "author": entry.Author, "author_url": entry.AuthorURL, "icon": entry.Icon, "color": entry.Color} {
			if item[key] != want {
				t.Errorf("%s.%s=%v want=%v", entry.ID, key, item[key], want)
			}
		}
		if entry.Icon == "" || entry.Color == "" {
			t.Errorf("bundled visual metadata missing: %s", entry.ID)
		}
	}
	if response := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}}); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	viewer := read(loginForTest(t, handler, "viewer", "strong-password"))
	for _, entry := range entries {
		if entry.AuthLevel > 1 {
			if _, found := viewer[entry.ID]; found {
				t.Errorf("restricted old market entry visible: %s", entry.ID)
			}
		}
	}
	if _, found := viewer["UnknownInstalled"]; found {
		t.Fatal("unknown privilege level exposed to viewer")
	}
	if response := performFormRequest(handler, "/api/v1/plugin/apps", "", nil); response.Code != 401 {
		t.Fatal(response.Code)
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `not-json`); err != nil {
		t.Fatal(err)
	}
	if response := performFormRequest(handler, "/api/v1/plugin/apps", token, nil); response.Code != 502 {
		t.Fatal(response.Code)
	}
}

func TestPluginCommandReadsUseNativeCatalogAndInstalledState(t *testing.T) {
	_, _, path := nativeServicesFixture(t, "", nil, nil)
	h, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(path), "config.yaml"), DisableLegacy: true}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected network access %s", r.URL)
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	admin := loginForTest(t, h, "admin", "password")
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for key, value := range map[string]string{
		"UserInstalledPlugins":       `["CustomReleaseGroups","AutoSub","UnknownInstalled","UnknownInstalled"]`,
		"plugin.CustomReleaseGroups": `{"release_groups":"Local","cache":"private-cache"}`,
		"plugin.AutoSub":             `{"password":"private-legacy"}`,
		"plugin.UnknownInstalled":    `{"key":"private-extension"}`,
	} {
		if err := store.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	if w := performFormRequest(h, "/api/v1/user/manage", admin, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	viewer := loginForTest(t, h, "viewer", "strong-password")
	for cmd, route := range map[string]string{"get_plugin_apps": "apps", "get_plugins_conf": "list"} {
		for _, token := range []string{admin, viewer, ""} {
			w := nativeJSONRequest(h, "POST", "/do", token, `{"cmd":"`+cmd+`","data":{}}`)
			rest := performFormRequest(h, "/api/v1/plugin/"+route, token, nil)
			if token == "" {
				if w.Code != 401 || rest.Code != 401 {
					t.Fatal(cmd, w.Code, rest.Code)
				}
				continue
			}
			if w.Code != 200 || rest.Code != 200 || w.Body.String() != rest.Body.String() || strings.Contains(w.Body.String(), "private-") {
				t.Fatal(cmd, w.Code, w.Body.String())
			}
			var out struct{ Result map[string]map[string]any }
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"UnknownInstalled", "AutoSub"} {
				row, found := out.Result[id]
				if found != (token == admin) {
					t.Fatal(cmd, id, found)
				}
				if found && route == "list" && (row["state"] != nil || row["stateKnown"] != false || row["native"] != false || row["installed"] != true || row["actionsAvailable"] != false || row["config"] != nil) {
					t.Fatal(row)
				}
			}
			if route == "list" && out.Result["CustomReleaseGroups"]["state"] != true {
				t.Fatal(out)
			}
		}
	}
}

func TestBundledLegacyPluginMutationReadsInstallationFromGo(t *testing.T) {
	var paths []string
	h, token, path := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if err := r.ParseForm(); err != nil || r.PostForm.Get("id") != "AutoSub" {
			t.Fatal(r.PostForm, err)
		}
		return jsonResponse(r, `{"code":0,"msg":"ok"}`), nil
	}))
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, tc := range []struct {
		installed, method, suffix, wantPath string
		status                              int
	}{
		{`[]`, "POST", "/install", "/api/v1/plugin/install", 200},
		{`["AutoSub"]`, "POST", "/install", "", 200},
		{`["AutoSub"]`, "DELETE", "", "/api/v1/plugin/uninstall", 200},
		{`[]`, "DELETE", "", "", 409},
		{`invalid`, "POST", "/install", "", 502},
	} {
		if err := store.Set(t.Context(), "UserInstalledPlugins", tc.installed); err != nil {
			t.Fatal(err)
		}
		paths = nil
		w := nativeJSONRequest(h, tc.method, "/api/v1/plugins/AutoSub"+tc.suffix, token, "")
		if w.Code != tc.status || strings.Join(paths, ",") != tc.wantPath {
			t.Fatal(tc, w.Code, paths, w.Body.String())
		}
	}
}
