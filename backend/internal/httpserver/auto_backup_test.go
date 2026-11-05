package httpserver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/autobackup"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestAutoBackupPluginArchiveRetentionAndScheduledTakeover(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	for name, contents := range map[string]string{"config.yaml": "app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: fixture-key\n", "default-category.yaml": "movie: {}\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var runner *rssRunAPI
	handler, err := buildHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: configPath}, http.DefaultTransport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	api := runner.autoBackup
	ctx, cancel := context.WithCancel(context.Background())
	wait := startAutoBackupWorker(ctx, api)
	defer func() { cancel(); wait(); api.store.Close() }()
	token := loginForTest(t, handler, "admin", "password")
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return nativeJSONRequest(handler, method, path, token, string(b))
	}
	install := call("POST", "/api/v1/plugins/AutoBackup/install", nil)
	if install.Code != 200 {
		t.Fatal(install.Code, install.Body.String())
	}
	system, err := systemconfig.Open(filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	if err := system.Set(t.Context(), autobackup.ConfigKey, `{"cnt":1,"onlyonce":true,"notify":true,"future":9007199254740993}`); err != nil {
		t.Fatal(err)
	}
	detail := call("GET", "/api/v1/plugins/AutoBackup", nil)
	if detail.Code != 200 || strings.Contains(detail.Body.String(), "future") {
		t.Fatal(detail.Code, detail.Body.String())
	}
	saved := performFormRequest(handler, "/api/v1/plugin/config", token, url.Values{"id": {"AutoBackup"}, "config": {`{"full":true}`}})
	if saved.Code != 200 {
		t.Fatal(saved.Code, saved.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "backup_file")); !os.IsNotExist(err) {
		t.Fatal("saving created target", err)
	}
	status := func() string {
		res := call("GET", "/api/v1/backup", nil)
		var o struct{ Data struct{ Fingerprint string } }
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &o) != nil || o.Data.Fingerprint == "" {
			t.Fatal(res.Code, res.Body.String())
		}
		return o.Data.Fingerprint
	}
	waitRun := func(id string, pruned int) autobackup.Run {
		for end := time.Now().Add(8 * time.Second); time.Now().Before(end); {
			r, e := api.store.Run(t.Context(), id)
			if e == nil && r.State != "running" && r.State != "staged" && r.Pruned >= pruned {
				if guard, err := api.acquire(t.Context()); err == nil {
					guard.Close()
					return r
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("record %s did not finish", id)
		return autobackup.Run{}
	}
	start := func(nonce string, approved bool) (string, int) {
		res := call("POST", "/api/v1/backup/run", map[string]any{"fingerprint": status(), "requestId": nonce, "confirm": true, "confirmRetention": approved})
		var o struct{ Data struct{ ID string } }
		_ = json.Unmarshal(res.Body.Bytes(), &o)
		return o.Data.ID, res.Code
	}
	if _, code := start(strings.Repeat("a", 64), false); code != 409 {
		t.Fatal(code)
	}
	id, code := start(strings.Repeat("a", 64), true)
	if code != 200 {
		t.Fatal(code)
	}
	first := waitRun(id, 0)
	if first.State != "complete" {
		t.Fatal(first)
	}
	original := filepath.Join(dir, "backup_file", "bk_20200101000000.zip")
	if err := os.WriteFile(original, []byte("legacy retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if again, code := start(strings.Repeat("a", 64), true); code != 200 || again != id {
		t.Fatal(again, code)
	}
	res := call("GET", "/api/v1/backup/runs/"+id+"/file", nil)
	if res.Code != 200 || res.Header().Get("Content-Type") != "application/zip" {
		t.Fatal(res.Code)
	}
	if _, err := zip.NewReader(bytes.NewReader(res.Body.Bytes()), int64(res.Body.Len())); err != nil {
		t.Fatal(err)
	}
	res = call("POST", "/api/v1/backup/activate", map[string]any{"fingerprint": status(), "confirm": true, "confirmRetention": true})
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body.String())
	}
	api.tick(t.Context(), time.Now().Add(10*time.Second))
	var scheduled autobackup.Run
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); {
		runs, err := api.store.Runs(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.Kind == "once" {
				scheduled = r
			}
		}
		if scheduled.ID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if scheduled.ID == "" {
		t.Fatal("one-shot not claimed")
	}
	if r := waitRun(scheduled.ID, 1); r.State != "complete" {
		t.Fatal(r)
	}
	if _, err := os.Stat(filepath.Join(first.Target.Path, first.Archive.Name)); !os.IsNotExist(err) {
		t.Fatal("managed retention did not remove old archive", err)
	}
	if b, err := os.ReadFile(original); err != nil || string(b) != "legacy retained" {
		t.Fatal("legacy archive changed", err)
	}
	api.tick(t.Context(), time.Now().Add(10*time.Minute))
	state, err := api.store.State(t.Context())
	if err != nil || state.Control.Once != "" || !strings.Contains(state.Raw, `"future":9007199254740993`) || !strings.Contains(state.Raw, `"onlyonce":false`) {
		t.Fatal(state, err)
	}
	saved = performFormRequest(handler, "/api/v1/plugin/config", token, url.Values{"id": {"AutoBackup"}, "config": {`{"cnt":"2"}`}})
	if saved.Code != 200 {
		t.Fatal(saved.Code, saved.Body.String())
	}
	b, code := start(strings.Repeat("b", 64), true)
	if code != 200 {
		t.Fatal(code)
	}
	newer := waitRun(b, 0)
	if err := os.Rename(filepath.Join(newer.Target.Path, newer.Archive.Name), filepath.Join(dir, "moved-test-archive.zip")); err != nil {
		t.Fatal(err)
	}
	c, code := start(strings.Repeat("c", 64), true)
	if code != 200 {
		t.Fatal(code)
	}
	if r := waitRun(c, 0); r.State != "complete" {
		t.Fatal(r)
	}
	if _, err := os.Stat(filepath.Join(scheduled.Target.Path, scheduled.Archive.Name)); err != nil {
		t.Fatal("pruned an older valid archive despite a missing retained backup", err)
	}
	res = call("POST", "/backup", nil)
	if res.Code != 200 || res.Header().Get("Content-Type") != "application/zip" {
		t.Fatal(res.Code)
	}
	res = call("DELETE", "/api/v1/plugins/AutoBackup", nil)
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body.String())
	}
	res = call("POST", "/api/v1/backup/run", map[string]any{"requestId": strings.Repeat("c", 64)})
	if res.Code != 200 || !strings.Contains(res.Body.String(), c) {
		t.Fatal("retry lost receipt after uninstall", res.Code, res.Body.String())
	}
}
