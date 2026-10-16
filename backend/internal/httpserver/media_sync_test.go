package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediasync"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type mediaSyncFixture struct {
	handler                  http.Handler
	token, path, fingerprint string
	calls                    int
	mode                     string
	afterRead                func()
}

func newMediaSyncFixture(t *testing.T) *mediaSyncFixture {
	t.Helper()
	f := &mediaSyncFixture{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.calls++
		if r.URL.Host != "media.local" || r.Method != "GET" || r.Header.Get("X-Emby-Token") != "private-media-key" {
			t.Fatal("unexpected external request", r.URL)
		}
		switch r.URL.Path {
		case "/emby/Users":
			return jsonResponse(r, `[{"Id":"u","Name":"viewer","Policy":{"IsAdministrator":true}}]`), nil
		case "/emby/Users/u/Views":
			return jsonResponse(r, `{"Items":[{"Id":"films","Name":"电影","CollectionType":"movies"}],"TotalRecordCount":1}`), nil
		case "/emby/Users/u/Items":
			if f.afterRead != nil {
				f.afterRead()
			}
			if f.mode == "missing" {
				return jsonResponse(r, `{"Items":[],"TotalRecordCount":5}`), nil
			}
			if f.mode == "empty" {
				return jsonResponse(r, `{"Items":[],"TotalRecordCount":0}`), nil
			}
			return jsonResponse(r, `{"Items":[{"Id":"m","Name":"电影","Type":"Movie","ProductionYear":2026,"ProviderIds":{"Tmdb":"100"}}],"TotalRecordCount":1}`), nil
		default:
			t.Fatal("unexpected", r.URL)
			return nil, nil
		}
	})
	_, f.token, f.path = nativeServicesFixture(t, "media:\n  media_server: emby\nemby:\n  host: http://media.local\n  api_key: private-media-key\n  username: viewer\n", nil, transport)
	var err error
	f.handler, err = newHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: filepath.Join(filepath.Dir(f.path), "config.yaml")}, transport)
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/library/sync/libraries", f.token, "")
	var catalog struct{ Data struct{ Fingerprint string } }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalog) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	f.fingerprint = catalog.Data.Fingerprint
	return f
}
func (f *mediaSyncFixture) sync() *httptest.ResponseRecorder {
	return nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync", f.token, `{"confirm":true,"legacyStopped":true,"libraries":["films"],"fingerprint":"`+f.fingerprint+`"}`)
}
func (f *mediaSyncFixture) view(t *testing.T) (mediasync.Snapshot, bool) {
	t.Helper()
	calls := f.calls
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/library/sync", f.token, "")
	var envelope struct {
		Data struct {
			Snapshot    mediasync.Snapshot
			Current     bool
			DisplayOnly bool
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || !envelope.Data.DisplayOnly || calls != f.calls {
		t.Fatal(w.Code, w.Body.String(), "status made network calls")
	}
	if strings.Contains(w.Body.String(), "private-media-key") {
		t.Fatal("credential leak")
	}
	return envelope.Data.Snapshot, envelope.Data.Current
}
func TestMediaSyncHTTPAtomicPublicationAndFailedRefresh(t *testing.T) {
	f := newMediaSyncFixture(t)
	view, current := f.view(t)
	if view.Generation != "" || current {
		t.Fatal(view, current)
	}
	w := f.sync()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	view, current = f.view(t)
	if !current || view.Total != 1 || len(view.Items) != 1 || view.Items[0].TMDBID != "100" || view.Actor != "admin" {
		t.Fatal(view, current)
	}
	previous := view.Generation
	f.mode = "missing"
	w = f.sync()
	if w.Code != 502 {
		t.Fatal(w.Code, w.Body.String())
	}
	view, _ = f.view(t)
	if view.Generation != previous {
		t.Fatal("partial refresh replaced cache")
	}
	f.mode = "empty"
	w = f.sync()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	view, _ = f.view(t)
	if view.Total != 0 || len(view.Items) != 0 || view.Generation == previous {
		t.Fatal(view)
	}
}
func TestMediaSyncConfigurationChangesRetainPriorGeneration(t *testing.T) {
	f := newMediaSyncFixture(t)
	if w := f.sync(); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	before, _ := f.view(t)
	f.afterRead = func() {
		if err := config.NewStore(filepath.Join(filepath.Dir(f.path), "config.yaml")).Update(map[string]any{"emby.username": "changed"}); err != nil {
			t.Fatal(err)
		}
	}
	w := f.sync()
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	after, current := f.view(t)
	if after.Generation != before.Generation || current {
		t.Fatal(after, current)
	}
	f.afterRead = nil
	calls := f.calls
	w = f.sync()
	if w.Code != 409 || calls != f.calls {
		t.Fatal("stale scope reached remote", w.Code)
	}
}
func TestMediaSyncRequiresAdministratorConfirmationAndExclusiveGuard(t *testing.T) {
	f := newMediaSyncFixture(t)
	calls := f.calls
	for _, spec := range []struct {
		method, path, token, body string
		status                    int
	}{
		{"GET", "/api/v1/library/sync", "", "", 401},
		{"GET", "/api/v1/library/sync?page=0", f.token, "", 400},
		{"POST", "/api/v1/library/sync", f.token, `{"libraries":["films"]}`, 400},
		{"POST", "/api/v1/library/sync", f.token, `{"confirm":true,"legacyStopped":true,"libraries":[]}`, 400},
	} {
		w := nativeJSONRequest(f.handler, spec.method, spec.path, spec.token, spec.body)
		if w.Code != spec.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if f.calls != calls {
		t.Fatal("unauthorized requests reached media server")
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"other"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	token := loginForTest(t, f.handler, "other", "strong-password")
	for _, route := range []string{"GET /api/v1/library/sync", "GET /api/v1/library/sync/libraries", "POST /api/v1/library/sync", "GET /api/v1/library/sync/schedule", "POST /api/v1/library/sync/schedule/review", "POST /api/v1/library/sync/schedule/activate", "POST /api/v1/library/sync/schedule/stop"} {
		parts := strings.SplitN(route, " ", 2)
		w := nativeJSONRequest(f.handler, parts[0], parts[1], token, `{}`)
		if w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	guards, err := organization.OpenStore(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer guards.Close()
	guard, err := guards.AcquireJob(context.Background(), "media-library-sync")
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	w := f.sync()
	if w.Code != 409 || f.calls != calls {
		t.Fatal("guard failed", w.Code, w.Body.String())
	}
}
func TestMediaSyncMixedModeCannotWrite(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("legacy or media request", r.URL)
		return nil, nil
	})
	h, token, _ := nativeServicesFixture(t, "", nil, transport)
	w := nativeJSONRequest(h, "POST", "/api/v1/library/sync", token, `{"confirm":true}`)
	if w.Code != 501 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, route := range []string{"GET /api/v1/library/sync/schedule", "POST /api/v1/library/sync/schedule/review", "POST /api/v1/library/sync/schedule/activate", "POST /api/v1/library/sync/schedule/stop"} {
		parts := strings.SplitN(route, " ", 2)
		w := nativeJSONRequest(h, parts[0], parts[1], token, `{}`)
		if w.Code != 501 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
