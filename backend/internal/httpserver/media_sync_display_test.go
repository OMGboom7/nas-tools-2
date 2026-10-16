package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediasync"
)

func readMediaActivities(t *testing.T, f *mediaSyncFixture) []mediasync.Activity {
	t.Helper()
	calls := f.calls
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/library/sync/progress", f.token, "")
	var envelope struct {
		Data struct{ Activities []mediasync.Activity }
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || f.calls != calls {
		t.Fatal(w.Code, w.Body.String())
	}
	return envelope.Data.Activities
}
func TestMediaSyncProgressIsObservableBeforeRemoteReadCompletes(t *testing.T) {
	f := newMediaSyncFixture(t)
	observed := false
	f.afterRead = func() {
		activities := readMediaActivities(t, f)
		if len(activities) != 1 || activities[0].Kind != "manual" || activities[0].State != "collecting" || activities[0].SnapshotGeneration != "" || activities[0].Progress.Stage != "items" {
			t.Fatal(activities)
		}
		view, _ := f.view(t)
		if view.Generation != "" {
			t.Fatal("incomplete fetch published")
		}
		w := performFormRequest(f.handler, "/api/v1/system/progress", f.token, url.Values{"type": {"mediasync"}})
		var progress struct {
			Value  int
			Enable bool
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &progress) != nil || progress.Value != 0 || !progress.Enable {
			t.Fatal(w.Code, w.Body.String())
		}
		observed = true
	}
	if w := f.sync(); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !observed {
		t.Fatal("no in-flight observation")
	}
	f.afterRead = nil
	activities := readMediaActivities(t, f)
	view, _ := f.view(t)
	if len(activities) != 1 || activities[0].State != "completed" || activities[0].SnapshotGeneration != view.Generation || activities[0].Progress.ItemsDone != 1 || activities[0].Progress.PagesRead != 1 {
		t.Fatal(activities)
	}
	w := performFormRequest(f.handler, "/api/v1/system/progress", f.token, url.Values{"type": {"mediasync"}})
	var progress struct {
		Value  int
		Enable bool
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &progress) != nil || progress.Value != 100 || progress.Enable {
		t.Fatal(w.Code, w.Body.String())
	}
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/status", f.token, `{}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "电影：1") || strings.Contains(w.Body.String(), "private-media-key") {
		t.Fatal(w.Code, w.Body.String())
	}
	f.mode = "missing"
	if w := f.sync(); w.Code != 502 {
		t.Fatal(w.Code, w.Body.String())
	}
	activities = readMediaActivities(t, f)
	if len(activities) != 2 || activities[0].State != "failed" || activities[0].SnapshotGeneration != "" {
		t.Fatal(activities)
	}
	w = performFormRequest(f.handler, "/api/v1/system/progress", f.token, url.Values{"type": {"mediasync"}})
	if json.Unmarshal(w.Body.Bytes(), &progress) != nil || progress.Value == 100 || progress.Enable {
		t.Fatal(w.Body.String())
	}
	viewAfter, _ := f.view(t)
	if viewAfter.Generation != view.Generation {
		t.Fatal("failed collection cleared old cache")
	}
}
func TestMediaDisplayLookupHasNoMediaServerCallsAndCannotChangeTasks(t *testing.T) {
	f := newMediaSyncFixture(t)
	if w := f.sync(); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	calls := f.calls
	body := `{"items":[{"tmdbid":"100","type":"movie"},{"tmdbid":"100","type":"show"},{"tmdbid":"101","title":"电影","year":"2026","type":"movie"}]}`
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/lookup", f.token, body)
	var envelope struct{ Data mediasync.CacheBatch }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Data.Cache.State != "ready" || !envelope.Data.Cache.DisplayOnly || envelope.Data.Results[0].State != "present" || envelope.Data.Results[1].State != "not-recorded" || envelope.Data.Results[2].State != "not-recorded" || f.calls != calls {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := config.NewStore(filepath.Join(filepath.Dir(f.path), "config.yaml")).Update(map[string]any{"emby.username": "changed"}); err != nil {
		t.Fatal(err)
	}
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/lookup", f.token, body)
	if json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Data.Cache.State != "scope-changed" || envelope.Data.Results[0].State != "unavailable" || f.calls != calls {
		t.Fatal(w.Body.String())
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/lookup", viewer, body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, route := range []string{"GET /api/v1/library/sync/progress", "POST /api/v1/library/sync/status", "POST /api/v1/system/progress"} {
		parts := strings.SplitN(route, " ", 2)
		w := nativeJSONRequest(f.handler, parts[0], parts[1], viewer, `{}`)
		if w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/lookup", "", body)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, bad := range []string{`{"items":[]}`, `{"items":[{"type":"movie"}]}`, `{"items":[{"tmdbid":"1","type":"movie","season":1}]}`, `{"items":[{"tmdbid":"1","type":"movie"}],"execute":true}`} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/lookup", f.token, bad)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w = performFormRequest(f.handler, "/api/v1/system/progress", f.token, url.Values{"type": {"search"}})
	if w.Code != 501 || f.calls != calls {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestDiscoveryDisplaysCachedPresenceWithoutChangingSubscriptionControls(t *testing.T) {
	f := newMediaSyncFixture(t)
	if w := f.sync(); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	configPath := filepath.Join(filepath.Dir(f.path), "config.yaml")
	if err := config.NewStore(configPath).Update(map[string]any{"app.rmt_tmdbkey": "tmdb-private-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "tmdb.test" || r.Method != "GET" {
			t.Fatal("discovery contacted media/legacy/downloader", r.URL)
		}
		return jsonResponse(r, `{"results":[{"id":100,"media_type":"movie","title":"Movie","release_date":"2026-01-01"},{"id":100,"media_type":"tv","name":"Show","first_air_date":"2026-01-01"},{"id":101,"media_type":"movie","title":"Unknown","release_date":"2026-01-01"}]}`), nil
	})
	var err error
	f.handler, err = newHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: configPath}, transport)
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/discovery", f.token, `{"category":"trending","page":1}`)
	var envelope struct{ Data discoveryData }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Data.Cache == nil || envelope.Data.Cache.State != "ready" || calls != 1 || len(envelope.Data.Items) != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	if envelope.Data.Items[0].CachedPresence.State != "present" || envelope.Data.Items[1].CachedPresence.State != "not-recorded" || envelope.Data.Items[2].CachedPresence.State != "not-recorded" {
		t.Fatal(envelope.Data)
	}
	for _, item := range envelope.Data.Items {
		if item.Subscribed {
			t.Fatal("cache implicitly subscribed", item)
		}
	}
	if strings.Contains(w.Body.String(), "private-media-key") || strings.Contains(w.Body.String(), "tmdb-private-key") {
		t.Fatal("secret leaked")
	}
}
