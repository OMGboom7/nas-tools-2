package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestNativeMetadataPluginLegacyListAndState(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	list := func() map[string]any {
		t.Helper()
		response := performFormRequest(handler, "/api/v1/plugin/list", token, nil)
		if response.Code != 200 {
			t.Fatalf("native installed list=%d %s", response.Code, response.Body.String())
		}
		var payload struct{ Result map[string]any }
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Result
	}
	if result := list(); len(result) != 0 {
		t.Fatal(result)
	}
	status := func(id string) any {
		t.Helper()
		response := performFormRequest(handler, "/api/v1/plugin/status", token, url.Values{"id": {id}})
		if response.Code != 200 {
			t.Fatalf("status=%d %s", response.Code, response.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload["state"]
	}
	if status("CustomReleaseGroups") != nil {
		t.Fatal("uninstalled plugin fabricated active state")
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["CustomReleaseGroups","Customization"]`); err != nil {
		t.Fatal(err)
	}
	if status("CustomReleaseGroups") != false || status("Customization") != false {
		t.Fatal("empty plugin config should be inactive")
	}
	if err := store.Set(t.Context(), "plugin.CustomReleaseGroups", `{"release_groups":"NativeGroup","separator":"+","cache":{"password":"private-secret"}}`); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(t.Context(), "plugin.Customization", `{"separator":"-"}`); err != nil {
		t.Fatal(err)
	}
	if status("CustomReleaseGroups") != true || status("Customization") != false {
		t.Fatal("metadata plugin states do not match configured behavior")
	}
	result := list()
	if len(result) != 2 {
		t.Fatal(result)
	}
	raw := objectValue(result["CustomReleaseGroups"])
	if raw["state"] != true || len(slice(raw["fields"])) == 0 || objectValue(raw["config"])["release_groups"] != "NativeGroup" {
		t.Fatalf("legacy editor fields=%v", raw)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "private-secret") || strings.Contains(string(encoded), "password") {
		t.Fatal("private persisted metadata leaked through old editor")
	}
	if err := store.Set(t.Context(), "plugin.Customization", `{"customization":"("}`); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"list", "status"} {
		response := performFormRequest(handler, "/api/v1/plugin/"+route, token, url.Values{"id": {"Customization"}})
		if response.Code != 502 {
			t.Fatalf("invalid stored config %s=%d", route, response.Code)
		}
	}
	if response := performFormRequest(handler, "/api/v1/plugin/list", "", nil); response.Code != 401 {
		t.Fatal(response.Code)
	}
}

func TestInstalledPluginListDoesNotHideRemainingPlugins(t *testing.T) {
	body := url.Values{"extra": {"one", "two"}}
	called := false
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		if request.URL.Path != "/api/v1/plugin/list" {
			t.Fatal(request.URL.Path)
		}
		contents, err := io.ReadAll(request.Body)
		if err != nil || string(contents) != body.Encode() {
			t.Fatalf("remaining list request changed=%q %v", contents, err)
		}
		return jsonResponse(request, `{"code":0,"result":{"CookieCloud":{"name":"CookieCloud"},"CustomReleaseGroups":{"name":"Native groups"}}}`), nil
	})
	handler, token, path := nativeServicesFixture(t, "", nil, transport)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["CookieCloud","CustomReleaseGroups"]`); err != nil {
		t.Fatal(err)
	}
	response := performFormRequest(handler, "/api/v1/plugin/list", token, body)
	if response.Code != 200 || !called || !strings.Contains(response.Body.String(), "CookieCloud") {
		t.Fatalf("remaining installed plugin was hidden: %d %s", response.Code, response.Body.String())
	}
}
