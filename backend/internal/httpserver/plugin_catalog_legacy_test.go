package httpserver

import (
	"encoding/json"
	"net/url"
	"testing"

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
