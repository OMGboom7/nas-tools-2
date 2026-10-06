package httpserver

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestNativeMetadataPluginConfigurationIsLiveAndNeverCallsPython(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["CustomReleaseGroups","Customization"]`); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(t.Context(), "plugin.CustomReleaseGroups", `{"release_groups":"OldGroup","separator":"@","internal_cache":{"unknown":true}}`); err != nil {
		t.Fatal(err)
	}
	call := func(method, id, authorization, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "/api/v1/plugins/"+id, strings.NewReader(body))
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	response := call(http.MethodGet, "CustomReleaseGroups", token, "")
	if response.Code != 200 || strings.Contains(response.Body.String(), "OldGroup") || strings.Contains(response.Body.String(), "internal_cache") {
		t.Fatalf("config detail=%d %s", response.Code, response.Body.String())
	}
	for _, test := range []struct{ id, body string }{
		{"CustomReleaseGroups", `{"values":{"release_groups":"NativeGroup","separator":"+"}}`},
		{"Customization", `{"values":{"customization":"RAW;EXT","separator":"-"}}`},
	} {
		if response := call(http.MethodPut, test.id, token, test.body); response.Code != 200 {
			t.Fatalf("save %s=%d %s", test.id, response.Code, response.Body.String())
		}
	}
	options, err := mediameta.ReadLabelOptions(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := mediameta.ParseWithOptions(t.Context(), "[NativeGroup] Movie.2025.RAW.EXT", "", options)
	if err != nil || meta.Team != "NativeGroup" || meta.Customization != "RAW-EXT" {
		t.Fatalf("live native metadata=%+v err=%v", meta, err)
	}
	raw, err := store.Get(t.Context(), "plugin.CustomReleaseGroups")
	if err != nil || !strings.Contains(raw, "internal_cache") {
		t.Fatalf("unknown fields lost=%s %v", raw, err)
	}
	for _, body := range []string{`{"values":{"release_groups":"("}}`, `{"values":{"separator":42}}`, `{"values":{"unknown":"x"}}`, `{"clearConfig":["unknown"]}`, `{"values":{"release_groups":"` + strings.Repeat("x", 16385) + `"}}`} {
		if response := call(http.MethodPut, "CustomReleaseGroups", token, body); response.Code != 400 {
			t.Fatalf("invalid configuration=%d %s", response.Code, response.Body.String())
		}
		current, _ := store.Get(t.Context(), "plugin.CustomReleaseGroups")
		if current != raw {
			t.Fatal("invalid configuration modified stored value")
		}
	}
	if response := call(http.MethodPut, "CustomReleaseGroups", token, `{"values":{"release_groups":""}}`); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	options, _ = mediameta.ReadLabelOptions(t.Context(), store)
	if options.ReleaseGroups != "NativeGroup" {
		t.Fatal("blank write-only value erased configuration")
	}
	if response := call(http.MethodPut, "CustomReleaseGroups", token, `{"clearConfig":["release_groups"]}`); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	options, _ = mediameta.ReadLabelOptions(t.Context(), store)
	if options.ReleaseGroups != "" || options.ReleaseSeparator != "+" {
		t.Fatalf("explicit clear=%+v", options)
	}
	if response := call(http.MethodGet, "Customization", "", ""); response.Code != 401 {
		t.Fatal(response.Code)
	}
	response = performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	if response := call(http.MethodGet, "Customization", viewer, ""); response.Code != 200 {
		t.Fatal(response.Code)
	}
	if response := call(http.MethodPut, "Customization", viewer, `{"clearConfig":["customization"]}`); response.Code != 403 {
		t.Fatal(response.Code)
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `[]`); err != nil {
		t.Fatal(err)
	}
	if response := call(http.MethodGet, "Customization", token, ""); response.Code != 404 {
		t.Fatal(response.Code)
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["Customization"]`); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(t.Context(), "plugin.Customization", `not-json`); err != nil {
		t.Fatal(err)
	}
	if response := call(http.MethodGet, "Customization", token, ""); response.Code != 502 {
		t.Fatal(response.Code)
	}
}
