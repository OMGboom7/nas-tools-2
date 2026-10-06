package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestHostsPluginLifecycleWithoutPython(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: hosts-native-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hostsPath := filepath.Join(dir, "hosts")
	if err := os.WriteFile(hostsPath, []byte("127.0.0.1 localhost\n"), 0600); err != nil {
		t.Fatal(err)
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: configPath, DisableLegacy: true, HostsPath: hostsPath}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected upstream request: %s", r.URL)
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, handler, "admin", "password")
	store, err := systemconfig.Open(filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", token)
		if strings.HasPrefix(body, "id=") {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	expect := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := call(method, path, body)
		if w.Code != status {
			t.Fatalf("%s %s=%d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	expect("POST", "/api/v1/plugins/CustomHosts/install", "", 200)
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest("GET", "/api/v1/plugins/CustomHosts", nil))
	if unauthorized.Code != 401 {
		t.Fatalf("unauthorized=%d", unauthorized.Code)
	}
	created := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	denied := httptest.NewRecorder()
	viewerRequest := httptest.NewRequest("GET", "/api/v1/plugins/CustomHosts", nil)
	viewerRequest.Header.Set("Authorization", viewer)
	handler.ServeHTTP(denied, viewerRequest)
	if denied.Code != 403 {
		t.Fatalf("viewer=%d %s", denied.Code, denied.Body.String())
	}
	viewerList := performFormRequest(handler, "/api/v1/plugin/list", viewer, url.Values{})
	if viewerList.Code != 200 || strings.Contains(viewerList.Body.String(), "CustomHosts") {
		t.Fatalf("viewer list=%d %s", viewerList.Code, viewerList.Body.String())
	}
	expect("PUT", "/api/v1/plugins/CustomHosts", `{"values":{"hosts":"127.0.0.2 private.local\ninvalid row","enable":true}}`, 200)
	after, err := os.ReadFile(hostsPath)
	if err != nil || !strings.Contains(string(after), "private.local") || strings.Contains(string(after), "invalid row") {
		t.Fatalf("hosts=%s err=%v", after, err)
	}
	w := expect("GET", "/api/v1/plugins/CustomHosts", "", 200)
	if strings.Contains(w.Body.String(), "private.local") || strings.Contains(w.Body.String(), "invalid row") {
		t.Fatalf("config=%s", w.Body.String())
	}
	w = expect("POST", "/api/v1/plugin/status", "id=CustomHosts", 200)
	if !strings.Contains(w.Body.String(), `"state":true`) {
		t.Fatal(w.Body.String())
	}
	w = expect("GET", "/api/v1/plugins", "", 200)
	if !strings.Contains(w.Body.String(), `"runningCount":1`) {
		t.Fatal(w.Body.String())
	}
	w = expect("POST", "/api/v1/plugin/list", "", 200)
	if !strings.Contains(w.Body.String(), "private.local") || !strings.Contains(w.Body.String(), "invalid row") {
		t.Fatal(w.Body.String())
	}
	expect("POST", "/api/v1/plugin/config", "id=CustomHosts&config=%7B%22enable%22%3Atrue%2C%22hosts%22%3A%22127.0.0.2+private.local%22%7D", 200)
	for _, body := range []string{`{"values":{"enable":"true"}}`, `{"values":{"err_hosts":"fake"}}`, `{"values":{"enable":true},"clearConfig":["hosts"]}`} {
		expect("PUT", "/api/v1/plugins/CustomHosts", body, 400)
	}
	expect("PUT", "/api/v1/plugins/CustomHosts", `{"values":{"enable":false}}`, 200)
	unchanged, _ := os.ReadFile(hostsPath)
	if string(unchanged) != string(after) {
		t.Fatal("disable unexpectedly removed mappings")
	}
	expect("PUT", "/api/v1/plugins/CustomHosts", `{"values":{"enable":true}}`, 200)
	expect("DELETE", "/api/v1/plugins/CustomHosts", "", 200)
	expect("GET", "/api/v1/plugins/CustomHosts", "", 404)
	w = expect("POST", "/api/v1/plugin/status", "id=CustomHosts", 200)
	if !strings.Contains(w.Body.String(), `"state":null`) {
		t.Fatal(w.Body.String())
	}
	expect("POST", "/api/v1/plugin/install", "id=CustomHosts", 200)
	// Shared lifecycle state must serialize copies of the service held by routes.
	var workers sync.WaitGroup
	statuses := make(chan int, 12)
	for i := 0; i < 12; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			body := fmt.Sprintf(`{"values":{"hosts":"127.0.0.%d worker.local","enable":%t}}`, index+10, index%2 == 0)
			statuses <- call("PUT", "/api/v1/plugins/CustomHosts", body).Code
		}(i)
	}
	workers.Wait()
	close(statuses)
	for status := range statuses {
		if status != 200 {
			t.Fatalf("concurrent save=%d", status)
		}
	}
	concurrentRaw, err := store.Get(t.Context(), "plugin.CustomHosts")
	if err != nil {
		t.Fatal(err)
	}
	var concurrentConfig map[string]any
	if err := json.Unmarshal([]byte(concurrentRaw), &concurrentConfig); err != nil {
		t.Fatal(err)
	}
	concurrentStatus := expect("POST", "/api/v1/plugin/status", "id=CustomHosts", 200)
	if !strings.Contains(concurrentStatus.Body.String(), fmt.Sprintf(`"state":%t`, concurrentConfig["enable"] == true)) {
		t.Fatal(concurrentStatus.Body.String())
	}
	concurrentFile, err := os.ReadFile(hostsPath)
	if err != nil || strings.Count(string(concurrentFile), "# CustomHostsPlugin") != 1 {
		t.Fatalf("concurrent file=%s err=%v", concurrentFile, err)
	}
	// A replacement target with a symlink must fail without touching its target.
	renamed := filepath.Join(dir, "original-hosts")
	if err := os.Rename(hostsPath, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(renamed, hostsPath); err != nil {
		t.Fatal(err)
	}
	expect("PUT", "/api/v1/plugins/CustomHosts", `{"values":{"hosts":"127.0.0.3 next.local","enable":true}}`, 502)
	raw, err := store.Get(t.Context(), "plugin.CustomHosts")
	if err != nil || !strings.Contains(raw, `"enable":false`) {
		t.Fatalf("saved=%s err=%v", raw, err)
	}
	w = expect("POST", "/api/v1/plugin/status", "id=CustomHosts", 200)
	if !strings.Contains(w.Body.String(), `"state":false`) {
		t.Fatal(w.Body.String())
	}
	untouched, _ := os.ReadFile(renamed)
	if strings.Contains(string(untouched), "next.local") {
		t.Fatal("symlink target changed")
	}
}
