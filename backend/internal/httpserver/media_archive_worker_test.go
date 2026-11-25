//go:build linux || darwin

package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestMediaArchiveRunsSavedOnceAndCronAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: archive-test-key\nmedia:\n  media_server: emby\nemby:\n  host: http://archive.test\n  api_key: fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var h http.Handler
	var s *pluginService
	start := func() {
		t.Helper()
		var runner *rssRunAPI
		var err error
		h, err = buildHandler(config.Config{ApplicationConfigPath: path, DisableLegacy: true}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host != "archive.test" || r.Method != "GET" {
				t.Fatal(r.URL)
			}
			switch r.URL.Path {
			case "/emby/Users":
				return jsonResponse(r, `[{"Id":"u","Name":"viewer","Policy":{"IsAdministrator":true}}]`), nil
			case "/emby/Users/u/Views":
				return jsonResponse(r, `{"Items":[{"Id":"films","Name":"影片","CollectionType":"movies"},{"Id":"tv","Name":"剧集","CollectionType":"tvshows"}],"TotalRecordCount":2}`), nil
			case "/emby/Users/u/Items":
				if r.URL.Query().Get("ParentId") == "tv" {
					return jsonResponse(r, `{"Items":[],"TotalRecordCount":0}`), nil
				}
				return jsonResponse(r, `{"Items":[{"Id":"2","Name":"Later","Type":"Movie"},{"Id":"1","Name":"First","ProductionYear":1990,"ProviderIds":{"Tmdb":"12","Imdb":"tt3"},"Type":"Movie"}],"TotalRecordCount":2}`), nil
			}
			t.Fatal(r.URL)
			return nil, nil
		}), &runner)
		if err != nil {
			t.Fatal(err)
		}
		s = runner.archivePlugin
	}
	start()
	token := loginForTest(t, h, "admin", "password")
	const api = "/api/v1/plugins/MediaLibraryArchive"
	store := s.system
	if err := store.Set(t.Context(), archiveSettingsKey, `{"future":{"v":7},"enable":true,"cron":"* * * * *","run_once":true}`); err != nil {
		t.Fatal(err)
	}
	if w := nativeJSONRequest(h, "POST", api+"/install", token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := s.archiveTick(t.Context(), time.Now().Add(time.Hour)); err != nil || calls != 0 {
		t.Fatal(err, calls)
	}
	detail := nativeJSONRequest(h, "GET", api, token, "")
	var out struct{ Data pluginConfigDetail }
	if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &out) != nil || out.Data.Meta.Fingerprint == "" {
		t.Fatal(detail.Code, detail.Body.String())
	}
	body := `{"values":{"run_once":true,"enable":true,"cron":"* * * * *","max_count":"1","enable_notify":true},"fingerprint":"` + out.Data.Meta.Fingerprint + `"}`
	if w := nativeJSONRequest(h, "PUT", api, token, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := nativeJSONRequest(h, "PUT", api, token, body); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	read := func() archiveControl {
		t.Helper()
		raw, err := store.Get(t.Context(), archiveControlKey)
		if err != nil {
			t.Fatal(err)
		}
		c, err := decodeArchiveControl(raw)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	raw, _ := store.Get(t.Context(), archiveSettingsKey)
	if !strings.Contains(raw, `"future":{"v":7}`) || !strings.Contains(raw, `"run_once":false`) {
		t.Fatal(raw)
	}
	controls := read()
	root := filepath.Join(dir, "archive_file", "media_library")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"归档_20000101000000.md", "keep.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	start()
	if err := s.archiveTick(t.Context(), controls.Once); err != nil {
		t.Fatal(err)
	}
	first := read()
	if first.Running || !first.Once.IsZero() || !first.Next.Equal(controls.Next) || !strings.Contains(first.Last, "清理 1") {
		t.Fatal(first)
	}
	files, err := filepath.Glob(filepath.Join(root, "归档_*.md"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil || string(b) != "- 影片\n  - First (1990) [TMDB:12] [IMDB:tt3]\n  - Later\n- 剧集\n" {
		t.Fatal(string(b), err)
	}
	before := calls
	start()
	if err := s.archiveTick(t.Context(), time.Time{}); err != nil || calls != before {
		t.Fatal(calls, before, err)
	}
	cronAt := controls.Next
	if !cronAt.After(controls.Once) {
		cronAt = controls.Once.Add(time.Minute)
	}
	if err := s.archiveTick(t.Context(), cronAt); err != nil {
		t.Fatal(err)
	}
	second := read()
	if !second.Next.After(cronAt) || calls == before || !strings.Contains(second.Last, "清理 1") {
		t.Fatal(calls, second)
	}
	events, err := s.archiveMedia.guards.BusinessEvents().List(t.Context())
	if err != nil || len(events) != 2 {
		t.Fatal(events, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "keep.txt")); err != nil || string(b) != "fixture" {
		t.Fatal(err)
	}
	page := nativeJSONRequest(h, "GET", api+"/page", token, "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), second.Last) {
		t.Fatal(page.Code, page.Body.String())
	}
	legacy := performFormRequest(h, "/api/v1/plugin/list", token, url.Values{})
	if legacy.Code != 200 || !strings.Contains(legacy.Body.String(), "max_count") || strings.Contains(legacy.Body.String(), "future") {
		t.Fatal(legacy.Code, legacy.Body.String())
	}
	if w := nativeJSONRequest(h, "DELETE", api, token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	before = calls
	if err := s.archiveTick(t.Context(), second.Next); err != nil || calls != before {
		t.Fatal(err, calls)
	}
	// A process lost after claiming its slot leaves a receipt. Restart must
	// pause the schedule without collecting, deleting files or sending again.
	if w := nativeJSONRequest(h, "POST", api+"/install", token, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	second.Running = true
	crash, _ := json.Marshal(second)
	if err := store.Set(t.Context(), archiveControlKey, string(crash)); err != nil {
		t.Fatal(err)
	}
	start()
	if err := s.archiveTick(t.Context(), second.Next); err != nil || calls != before || !strings.Contains(read().Last, "中断") {
		t.Fatal(err, calls, read())
	}
	// A changed server cannot receive a previously authorized scheduled read.
	second.Running = false
	scheduled, _ := json.Marshal(second)
	if err := store.Set(t.Context(), archiveControlKey, string(scheduled)); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(strings.ReplaceAll(string(cfg), "archive.test", "changed.test")), 0600); err != nil {
		t.Fatal(err)
	}
	start()
	if err := s.archiveTick(t.Context(), second.Next); err != nil || calls != before || !strings.Contains(read().Last, "变更") {
		t.Fatal(err, calls, read())
	}

}
