package httpserver

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestExternalCatalogStoredSettingsAndAccessAfterUpgrade(t *testing.T) {
	_, _, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dir := filepath.Join(filepath.Dir(path), "plugins")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "fixture.py")
	if err := os.WriteFile(source, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("fixture")))
	const raw = `{"enabled":false,"token":"secret-value","private-dynamic-key":{"hidden":"data"}}`
	const visible = "公开插件"
	metadata := map[string]any{}
	for id, level := range map[string]int{visible: 1, "AdminOnly": 2, "Neither": 3, "CustomReleaseGroups": 0} {
		metadata[id] = map[string]any{"source": "fixture.py", "digest": digest, "level": level, "fields": map[string]any{"id": id, "name": "declared-" + id, "author_url": "javascript:bad", "icon": "fixture.png", "color": "#aaaaaa"}}
	}
	b, _ := json.Marshal(metadata)
	for key, value := range map[string]string{"GoExternalPluginCatalog": string(b), "UserInstalledPlugins": `["公开插件","Orphan","Neither"]`, "plugin." + visible: raw, "GoExternalPluginFields." + visible: fmt.Sprintf(`{"digest":"%s","revision":"%x","keys":["enabled","token"]}`, digest, sha256.Sum256([]byte(raw)))} {
		if err := store.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	h, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(path), "config.yaml"), DisableLegacy: true}, roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected upstream"); return nil, nil }))
	if err != nil {
		t.Fatal(err)
	}
	admin := loginForTest(t, h, "admin", "password")
	performFormRequest(h, "/api/v1/user/manage", admin, url.Values{"oper": {"add"}, "name": {"reader"}, "password": {"fixture-pass"}, "pris": {""}})
	reader := loginForTest(t, h, "reader", "fixture-pass")
	for _, token := range []string{admin, reader} {
		for _, route := range []string{"/api/v1/plugins", "/api/v1/plugin/apps"} {
			method := "GET"
			if strings.HasSuffix(route, "/apps") {
				method = "POST"
			}
			w := nativeJSONRequest(h, method, route, token, "")
			body := w.Body.String()
			if w.Code != 200 || !strings.Contains(body, "declared-"+visible) || strings.Contains(body, "declared-AdminOnly") != (token == admin) || strings.Contains(body, "declared-Neither") || strings.Contains(body, "declared-CustomReleaseGroups") || strings.Contains(body, "javascript:") {
				t.Fatal(route, w.Code, body)
			}
			if (token == reader && strings.Contains(body, "fixture.py")) || strings.Contains(body, digest) {
				t.Fatal("source details leaked")
			}
		}
	}
	if w := performFormRequest(h, "/api/v1/plugin/list", admin, nil); w.Code != 200 || strings.Contains(w.Body.String(), "Neither") {
		t.Fatal("restricted class in configuration list", w.Body.String())
	}
	if w := nativeJSONRequest(h, "DELETE", "/api/v1/plugins/Neither", admin, ""); w.Code != 404 {
		t.Fatal("restricted class visible", w.Code)
	}
	check := func(token string, status int, want []string, exclude ...string) {
		t.Helper()
		w := nativeJSONRequest(h, "GET", "/api/v1/plugins/"+visible, token, "")
		body := w.Body.String()
		if w.Code != status {
			t.Fatal(w.Code, body)
		}
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Fatal("missing", s, body)
			}
		}
		for _, s := range append(exclude, "secret-value", "private-dynamic-key") {
			if strings.Contains(body, s) {
				t.Fatal("unexpected", s, body)
			}
		}
	}
	check(admin, 200, []string{`"switch":false`, `"key":"token"`, `"other":1`})
	check(reader, 404, nil, `"stored"`)
	if w := nativeJSONRequest(h, "PUT", "/api/v1/plugins/"+visible, admin, `{"values":{"enabled":true}}`); w.Code < 400 {
		t.Fatal("wrote unported settings")
	}
	if err := store.Set(t.Context(), "plugin."+visible, `{"enabled":true}`); err != nil {
		t.Fatal(err)
	}
	check(admin, 200, []string{`"other":1`}, `"key":"enabled"`)
	if err := os.WriteFile(source, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := nativeJSONRequest(h, "GET", "/api/v1/plugins", reader, ""); w.Code != 200 || strings.Contains(w.Body.String(), "declared-") {
		t.Fatal("stale declaration visible")
	}
	check(admin, 200, []string{`"other":1`}, "declared-")
}

func TestPluginConfigListHonorsSingleSelection(t *testing.T) {
	calls := 0
	h, admin, path := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1/plugin/list" || r.ParseForm() != nil || r.PostForm.Get("id") != "ExternalClass" {
			t.Fatal("unexpected legacy request", r.URL)
		}
		calls++
		return jsonResponse(r, `{"code":0,"result":{"ExternalClass":{"name":"selected"},"OtherClass":{"name":"private-other"}}}`), nil
	}))
	s, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Set(t.Context(), "UserInstalledPlugins", `["CustomReleaseGroups","ExternalClass","OtherClass"]`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ExternalClass", "CustomReleaseGroups"} {
		for _, route := range []string{"/api/v1/plugin/list", "/do"} {
			var body string
			if route == "/do" {
				body = nativeJSONRequest(h, "POST", route, admin, `{"cmd":"get_plugins_conf","data":{"id":"`+id+`"}}`).Body.String()
			} else {
				body = performFormRequest(h, route, admin, url.Values{"id": {id}}).Body.String()
			}
			var out struct{ Result map[string]json.RawMessage }
			if json.Unmarshal([]byte(body), &out) != nil || len(out.Result) != 1 || out.Result[id] == nil || strings.Contains(body, "private-other") {
				t.Fatal(route, body)
			}
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
