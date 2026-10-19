package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type indexerPluginFixture struct {
	h           http.Handler
	token, path string
	store       *systemconfig.Store
	manager     *nativeIndexerPlugins
	calls       int
	fail        bool
	afterRead   func()
}

func nativeIndexerPluginFixture(t *testing.T) *indexerPluginFixture {
	t.Helper()
	f := &indexerPluginFixture{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "prowlarr.local" && r.URL.Hostname() != "jackett.local" {
			t.Fatal("unexpected legacy request", r.URL)
		}
		f.calls++
		if r.Header.Get("X-Api-Key") != "private-key" {
			t.Fatal("missing saved key")
		}
		if f.afterRead != nil {
			fn := f.afterRead
			f.afterRead = nil
			fn()
		}
		if f.fail {
			return nil, errors.New("private-password/upstream-secret")
		}
		if r.URL.Hostname() == "prowlarr.local" {
			if r.URL.Path != "/api/v1/indexerstats" {
				t.Fatal(r.URL)
			}
			return jsonResponse(r, `{"indexers":[{"indexerId":42,"indexerName":"Selected"}]}`), nil
		}
		if r.URL.Path == "/UI/Dashboard" {
			if err := r.ParseForm(); err != nil || r.PostForm.Get("password") != "private-password" || r.URL.RawQuery != "" {
				t.Fatal("password escaped into URL")
			}
			resp := jsonResponse(r, "")
			resp.Header.Set("Set-Cookie", "SID=private-session; Path=/")
			return resp, nil
		}
		if r.URL.Path != "/api/v2.0/indexers" || r.URL.Query().Get("configured") != "true" {
			t.Fatal(r.URL)
		}
		return jsonResponse(r, `[{"id":"demo","name":"Demo","type":"private"}]`), nil
	})
	_, f.token, f.path = nativeServicesFixture(t, "", nil, transport)
	var runner *rssRunAPI
	var err error
	f.h, err = buildHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: filepath.Join(filepath.Dir(f.path), "config.yaml")}, transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	f.manager = runner.indexerPlugins
	if f.manager == nil {
		t.Fatal("production runner missing plugin manager")
	}
	f.store, err = systemconfig.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	return f
}

func (f *indexerPluginFixture) call(method, path, body string) int {
	return nativeJSONRequest(f.h, method, path, f.token, body).Code
}
func installIndexer(t *testing.T, f *indexerPluginFixture, id string) {
	t.Helper()
	if code := f.call("POST", "/api/v1/plugins/"+id+"/install", ""); code != 200 {
		t.Fatal("install", id, code)
	}
}
func saveIndexer(t *testing.T, f *indexerPluginFixture, id string, once bool) {
	t.Helper()
	password := ""
	if id == "Jackett" {
		password = `,"password":"private-password"`
	}
	host := strings.ToLower(id) + ".local"
	flag := "false"
	if once {
		flag = "true"
	}
	body := `{"values":{"host":"http://` + host + `","api_key":"private-key","cron":"* * * * *","onlyonce":` + flag + password + `}}`
	w := nativeJSONRequest(f.h, "PUT", "/api/v1/plugins/"+id, f.token, body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestNativeIndexerPluginLifecycleConfigurationWorkerAndPages(t *testing.T) {
	f := nativeIndexerPluginFixture(t)
	for _, id := range []string{"Jackett", "Prowlarr"} {
		installIndexer(t, f, id)
		saveIndexer(t, f, id, true)
	}
	// Preserve unknown state and never return it, host, key or password.
	if err := f.store.Update(t.Context(), "plugin.Prowlarr", func(raw string) (string, error) {
		var v map[string]any
		_ = json.Unmarshal([]byte(raw), &v)
		v["unknown_cache"] = "private-cache"
		b, _ := json.Marshal(v)
		return string(b), nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"Jackett", "Prowlarr"} {
		w := nativeJSONRequest(f.h, "GET", "/api/v1/plugins/"+id, f.token, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"configured":true`) {
			t.Fatal(w.Code, w.Body.String())
		}
		for _, secret := range []string{"private-key", "private-password", "private-cache", "prowlarr.local", "jackett.local"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("config leaked", secret)
			}
		}
	}
	if f.calls != 0 {
		t.Fatal("save/get contacted server", f.calls)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	if err := f.manager.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if f.calls != 3 {
		t.Fatal("missing probe", f.calls)
	}
	for _, id := range []string{"Jackett", "Prowlarr"} {
		v, s, installed, err := f.manager.read(t.Context(), id)
		if err != nil || !installed || v["onlyonce"] != false || v["refresh_enabled"] != true || s.Status != "ready" || len(s.Items) != 1 || s.Next.IsZero() {
			t.Fatal(v, s, installed, err)
		}
		w := nativeJSONRequest(f.h, "GET", "/api/v1/plugins/"+id+"/page", f.token, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "private-key") || strings.Contains(w.Body.String(), "private-cache") || !strings.Contains(w.Body.String(), `"readOnly":true`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := performFormRequest(f.h, "/api/v1/plugin/list", f.token, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-key") || strings.Contains(w.Body.String(), "private-password") || strings.Contains(w.Body.String(), "private-cache") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = performFormRequest(f.h, "/api/v1/plugin/page", f.token, url.Values{"id": {"Prowlarr"}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := f.manager.tick(t.Context(), now.Add(2*time.Second)); err != nil || f.calls != 3 {
		t.Fatal("not due probe", err, f.calls)
	}
	// A reconstructed manager uses the persisted next occurrence, not an eager replay.
	restarted := &nativeIndexerPlugins{system: f.store, transport: f.manager.transport, pureGo: true}
	if err := restarted.tick(t.Context(), now.Add(2*time.Second)); err != nil || f.calls != 3 {
		t.Fatal(err, f.calls)
	}
	if err := restarted.tick(t.Context(), now.Add(2*time.Minute)); err != nil || f.calls != 6 {
		t.Fatal("due probe missing", err, f.calls)
	}
	if code := f.call("DELETE", "/api/v1/plugins/Prowlarr", ""); code != 200 {
		t.Fatal(code)
	}
	if raw, _ := f.store.Get(t.Context(), "plugin.Prowlarr"); !strings.Contains(raw, "private-cache") {
		t.Fatal("uninstall removed config")
	}
	if err := restarted.tick(t.Context(), now.Add(4*time.Minute)); err != nil || f.calls != 8 {
		t.Fatal("uninstalled plugin probed", err, f.calls)
	}
}

func TestNativeIndexerPluginRejectsInvalidInputsAndPreservesWriteOnlyConfig(t *testing.T) {
	f := nativeIndexerPluginFixture(t)
	installIndexer(t, f, "Prowlarr")
	saveIndexer(t, f, "Prowlarr", false)
	before, _ := f.store.Get(t.Context(), "plugin.Prowlarr")
	for _, body := range []string{`{"values":{"cron":"nonsense"}}`, `{"values":{"cron":"30"}}`, `{"values":{"host":"http://user:secret@other.local"}}`, `{"values":{"api_key":"bad\r\nkey"}}`, `{"values":{"onlyonce":"yes"}}`, `{"values":{"unknown":"x"}}`, `{"clearConfig":["api_key"]}`} {
		w := nativeJSONRequest(f.h, "PUT", "/api/v1/plugins/Prowlarr", f.token, body)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
		if raw, _ := f.store.Get(t.Context(), "plugin.Prowlarr"); raw != before {
			t.Fatal("invalid request modified config")
		}
	}
	if code := f.call("PUT", "/api/v1/plugins/Prowlarr", `{"values":{"host":"","api_key":""}}`); code != 200 {
		t.Fatal(code)
	}
	after, _ := f.store.Get(t.Context(), "plugin.Prowlarr")
	if after != before {
		t.Fatal("blank cleared credential", after)
	}
	if f.calls != 0 {
		t.Fatal("validation used network")
	}
	if w := performFormRequest(f.h, "/api/v1/plugin/config", f.token, url.Values{"id": {"Prowlarr"}, "config": {`{"host":"http://prowlarr.local","api_key":"private-key","cron":"* * * * *","onlyonce":true,"legacy_unknown":"keep"}`}}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := f.manager.tick(t.Context(), time.Now()); err != nil || f.calls != 1 {
		t.Fatal(err, f.calls)
	}
}

func TestNativeIndexerPluginPublicationRejectsConfigChangeAndUninstallReinstallABA(t *testing.T) {
	for _, aba := range []bool{false, true} {
		f := nativeIndexerPluginFixture(t)
		installIndexer(t, f, "Prowlarr")
		saveIndexer(t, f, "Prowlarr", false)
		f.afterRead = func() {
			if aba {
				if code := f.call("DELETE", "/api/v1/plugins/Prowlarr", ""); code != 200 {
					t.Fatal(code)
				}
				installIndexer(t, f, "Prowlarr")
			} else {
				saveIndexer(t, f, "Prowlarr", false)
			}
		}
		if err := f.manager.refresh(t.Context(), "Prowlarr", time.Now()); !errors.Is(err, errIndexerScope) {
			t.Fatal("stale probe published", err)
		}
		_, s, _, err := f.manager.read(t.Context(), "Prowlarr")
		if err != nil || len(s.Items) != 0 || s.Status == "ready" {
			t.Fatal(s, err)
		}
	}
}

func TestNativeIndexerPluginConcurrentClaimAndWorkerCancellation(t *testing.T) {
	f := nativeIndexerPluginFixture(t)
	installIndexer(t, f, "Prowlarr")
	saveIndexer(t, f, "Prowlarr", true)
	entered, release := make(chan struct{}), make(chan struct{})
	f.manager.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return jsonResponse(r, `{"indexers":[]}`), nil
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = f.manager.refresh(context.Background(), "Prowlarr", time.Now()) }()
	<-entered
	other := &nativeIndexerPlugins{system: f.store, transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("concurrent claim issued network request")
		return nil, errors.New("unexpected")
	}), pureGo: true}
	if err := other.tick(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	// A blocked HTTP call is cancelled and the worker is joined by its real handle.
	if code := f.call("PUT", "/api/v1/plugins/Prowlarr", `{"values":{"onlyonce":true}}`); code != 200 {
		t.Fatal(code)
	}
	started := make(chan struct{})
	f.manager.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	wait := startIndexerPluginWorker(ctx, f.manager)
	<-started
	cancel()
	wait()
	_, s, _, err := f.manager.read(t.Context(), "Prowlarr")
	if err != nil || s.Status != "failed" {
		t.Fatal(s, err)
	}
}

func TestNativeIndexerPluginFailureNoSecretLeakAndUncertainLeaseRecovery(t *testing.T) {
	f := nativeIndexerPluginFixture(t)
	installIndexer(t, f, "Prowlarr")
	saveIndexer(t, f, "Prowlarr", true)
	now := time.Now().UTC().Truncate(time.Minute)
	if err := f.manager.tick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	f.fail = true
	if err := f.manager.tick(t.Context(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, s, _, err := f.manager.read(t.Context(), "Prowlarr")
	if err != nil || s.Status != "failed" || len(s.Items) != 1 {
		t.Fatal(s, err)
	}
	w := nativeJSONRequest(f.h, "GET", "/api/v1/plugins/Prowlarr/page", f.token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "最近读取失败") || strings.Contains(w.Body.String(), "upstream-secret") || strings.Contains(w.Body.String(), "private-key") {
		t.Fatal(w.Code, w.Body.String())
	}
	if raw, _ := f.store.Get(t.Context(), "GoIndexerPlugin.Prowlarr"); strings.Contains(raw, "private-password") {
		t.Fatal("checkpoint leaked upstream diagnostics")
	}
	// Simulate a process lost after the durable read claim, before publication.
	s.Status = "checking"
	s.Started = now.Add(2 * time.Minute)
	s.Next = now.Add(3 * time.Minute)
	raw, _ := encodeIndexerState(s)
	if err := f.store.Set(t.Context(), "GoIndexerPlugin.Prowlarr", raw); err != nil {
		t.Fatal(err)
	}
	f.fail = false
	before := f.calls
	if err := f.manager.tick(t.Context(), now.Add(2*time.Minute+30*time.Second)); err != nil || f.calls != before {
		t.Fatal(err, f.calls)
	}
	if err := f.manager.tick(t.Context(), now.Add(3*time.Minute)); err != nil || f.calls != before+1 {
		t.Fatal("readonly lease not recovered", err, f.calls)
	}
}

func TestNativeIndexerPluginPermissionAndMixedModeBoundary(t *testing.T) {
	f := nativeIndexerPluginFixture(t)
	installIndexer(t, f, "Prowlarr")
	saveIndexer(t, f, "Prowlarr", false)
	w := performFormRequest(f.h, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	viewer := loginForTest(t, f.h, "viewer", "strong-password")
	for _, spec := range []struct{ method, path, body string }{{"PUT", "/api/v1/plugins/Prowlarr", `{"values":{"onlyonce":true}}`}, {"DELETE", "/api/v1/plugins/Prowlarr", ""}, {"POST", "/api/v1/plugins/Jackett/install", ""}} {
		if w := nativeJSONRequest(f.h, spec.method, spec.path, viewer, spec.body); w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := performFormRequest(f.h, "/api/v1/plugin/config", viewer, url.Values{"id": {"Prowlarr"}, "config": {`{}`}}); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	h, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(f.path), "config.yaml"), LegacyBackendURL: "http://legacy:3000"}, f.manager.transport)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := f.store.Get(t.Context(), "plugin.Prowlarr")
	if w := nativeJSONRequest(h, "PUT", "/api/v1/plugins/Prowlarr", f.token, `{"values":{"onlyonce":true}}`); w.Code != 501 {
		t.Fatal(w.Code, w.Body.String())
	}
	if raw, _ := f.store.Get(t.Context(), "plugin.Prowlarr"); raw != before || f.calls != 0 {
		t.Fatal("mixed mode modified state or contacted upstream")
	}
	mixed := &nativeIndexerPlugins{system: f.store, transport: f.manager.transport, pureGo: false}
	if err := mixed.tick(t.Context(), time.Now()); err != nil || f.calls != 0 {
		t.Fatal(err, f.calls)
	}
}

func TestNativeIndexerPluginUninstallDoesNotRequireValidConfigOrDeleteSecrets(t *testing.T) {
	f := nativeIndexerPluginFixture(t)
	installIndexer(t, f, "Prowlarr")
	if err := f.store.Set(t.Context(), "plugin.Prowlarr", `invalid-private-config`); err != nil {
		t.Fatal(err)
	}
	if code := f.call("DELETE", "/api/v1/plugins/Prowlarr", ""); code != 200 {
		t.Fatal(code)
	}
	if raw, _ := f.store.Get(t.Context(), "plugin.Prowlarr"); raw != `invalid-private-config` {
		t.Fatal("uninstall rewrote stored credentials")
	}
	w := performFormRequest(f.h, "/api/v1/plugin/uninstall", f.token, url.Values{"id": {"Prowlarr"}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := f.manager.tick(t.Context(), time.Now()); err != nil || f.calls != 0 {
		t.Fatal("uninstalled corrupt config probed", err, f.calls)
	}
}
