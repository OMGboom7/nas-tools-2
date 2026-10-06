package httpserver

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestLegacyMetadataPluginRoutesAreNativeAndReplaceFullConfiguration(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	post := func(action, config string) int {
		t.Helper()
		response := performFormRequest(handler, "/api/v1/plugin/"+action, token, url.Values{"id": {"CustomReleaseGroups"}, "config": {config}})
		return response.Code
	}
	if status := post("config", `{"release_groups":"NativeGroup","separator":"+","unknown":true}`); status != 200 {
		t.Fatal(status)
	}
	if status := post("install", ""); status != 200 {
		t.Fatal(status)
	}
	options, err := mediameta.ReadLabelOptions(t.Context(), store)
	if err != nil || options.ReleaseGroups != "NativeGroup" {
		t.Fatalf("legacy activation=%+v %v", options, err)
	}
	if status := post("config", `{"release_groups":""}`); status != 200 {
		t.Fatal(status)
	}
	raw, _ := store.Get(t.Context(), "plugin.CustomReleaseGroups")
	if raw != `{"release_groups":""}` {
		t.Fatalf("legacy config did not replace full object=%s", raw)
	}
	for _, invalid := range []string{`null`, `[]`, `not-json`, `{"release_groups":"("}`} {
		if status := post("config", invalid); status != 400 {
			t.Fatalf("bad legacy config=%d", status)
		}
		current, _ := store.Get(t.Context(), "plugin.CustomReleaseGroups")
		if current != raw {
			t.Fatal("invalid config overwrote saved values")
		}
	}
	for index := 0; index < 2; index++ {
		if status := post("uninstall", ""); status != 200 {
			t.Fatalf("idempotent legacy uninstall=%d", status)
		}
	}
	if response := performFormRequest(handler, "/api/v1/plugin/install", "", url.Values{"id": {"CustomReleaseGroups"}}); response.Code != 401 {
		t.Fatal(response.Code)
	}
}

func TestUnmigratedLegacyPluginRequestRetainsOriginalBody(t *testing.T) {
	called := false
	body := url.Values{"id": {"CookieCloud"}, "config": {`{"server":"https://example.test","notes":"中文+特殊&字符"}`}, "extra": {"one", "two"}}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		if request.URL.Path != "/api/v1/plugin/config" {
			t.Fatalf("unexpected request=%s", request.URL.Path)
		}
		contents, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != body.Encode() {
			t.Fatalf("fallback body changed: %q", contents)
		}
		if !strings.Contains(request.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			t.Fatal("content type lost")
		}
		return jsonResponse(request, `{"code":0,"msg":"saved by remaining legacy handler"}`), nil
	})
	handler, token, _ := nativeServicesFixture(t, "", nil, transport)
	response := performFormRequest(handler, "/api/v1/plugin/config", token, body)
	if response.Code != 200 || !called {
		t.Fatalf("remaining plugin fallback=%d called=%v %s", response.Code, called, response.Body.String())
	}
}
