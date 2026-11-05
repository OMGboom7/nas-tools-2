package httpserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/cookiecloud"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

const cookieCloudPath = "/api/v1/cookie-cloud"

func newCookieCloudFixture(t *testing.T) (*loginFixture, *cookieCloudAPI) {
	t.Helper()
	f := newLoginFixture(t, nil)
	api := f.cookieCloud
	ctx, cancel := context.WithCancel(context.Background())
	wait := startCookieCloudWorker(ctx, api)
	t.Cleanup(func() { cancel(); wait(); api.store.Close() })
	response := f.request(t, "POST", "/api/v1/plugins/CookieCloud/install", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	return f, api
}

func ccStatus(t *testing.T, f *loginFixture) (string, cookiecloud.Control) {
	t.Helper()
	response := f.request(t, "GET", cookieCloudPath, nil)
	var out struct {
		Data struct {
			Fingerprint string
			Control     cookiecloud.Control
		}
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &out) != nil || out.Data.Fingerprint == "" ||
		strings.Contains(response.Body.String(), "fixture-cloud-password") || strings.Contains(response.Body.String(), "fixture-cloud-key") {
		t.Fatal(response.Code, response.Body.String())
	}
	return out.Data.Fingerprint, out.Data.Control
}

func ccStart(t *testing.T, f *loginFixture, kind, id string) cookiecloud.Run {
	t.Helper()
	fingerprint, _ := ccStatus(t, f)
	r := f.request(t, "POST", cookieCloudPath+"/"+kind, map[string]any{"confirm": true, "fingerprint": fingerprint, "requestId": id})
	var out struct{ Data cookiecloud.Run }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil || out.Data.ID == "" {
		t.Fatal(r.Code, r.Body.String())
	}
	return out.Data
}

func ccWait(t *testing.T, f *loginFixture, id string) cookiecloud.Run {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		response := f.request(t, "GET", cookieCloudPath+"/runs/"+id, nil)
		var out struct{ Data cookiecloud.Run }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &out) != nil {
			t.Fatal(response.Code, response.Body.String())
		}
		if out.Data.State != "running" {
			return out.Data
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("原同步仍未结束")
	return cookiecloud.Run{}
}

func TestCookieCloudNativeLifecycleRetainsConfigAndOnlyPublishesAllowedCookies(t *testing.T) {
	f, api := newCookieCloudFixture(t)
	t.Setenv("TZ", "Asia/Shanghai")
	catalog := `{"indexer":[{"id":"fresh","name":"New tracker","domain":"https://new.indexer.test/"}],"conf":{}}`
	if err := os.WriteFile(f.api.catalogPath, []byte(base64.StdEncoding.EncodeToString([]byte(catalog))), 0600); err != nil {
		t.Fatal(err)
	}
	const raw = `{"server":"https://receiver.fixture.test/base","key":"fixture-cloud-key","password":"fixture-cloud-password","onlyonce":true,"notify":true,"future":{"big":9007199254740993,"null":null}}`
	if err := f.signin.system.Set(t.Context(), cookiecloud.ConfigKey, raw); err != nil {
		t.Fatal(err)
	}
	healthy, err := f.api.sites.Upsert(t.Context(), siteconfig.Site{Name: "Healthy", SignURL: "https://healthy.other.test/", Cookie: "original=healthy", Priority: "7", Note: `{"ua":"original"}`})
	if err != nil {
		t.Fatal(err)
	}
	var cloud, probes atomic.Int32
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := func(body string) *http.Response {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}
		}
		switch r.URL.Host {
		case "receiver.fixture.test":
			cloud.Add(1)
			var body map[string]string
			if r.Method != "POST" || r.URL.Path != "/base/get/fixture-cloud-key" || json.NewDecoder(r.Body).Decode(&body) != nil ||
				body["password"] != "fixture-cloud-password" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
				t.Error("unexpected Cloud receiver request")
			}
			return response(`{"cookie_data":{"site.fixture.test":[{"domain":"site.fixture.test","name":"SID","value":"fixture-fresh"}],
				"healthy.other.test":[{"domain":"healthy.other.test","name":"SID","value":"fixture-ignored"}],
				"new.indexer.test":[{"domain":"new.indexer.test","name":"SID","value":"fixture-new"}],
				"unknown.example":[{"domain":"unknown.example","name":"SID","value":"fixture-unknown"}]}}`), nil
		case "site.fixture.test", "healthy.other.test":
			probes.Add(1)
			if r.Method != "GET" || r.URL.Path != "/" || !strings.HasPrefix(r.Header.Get("Cookie"), "original=") {
				t.Error("probe sent an incorrect credential")
			}
			if r.URL.Host == "healthy.other.test" {
				return response(`<a href="logout.php">退出</a>`), nil
			}
			return response(`<input type="password" name="password">`), nil
		default:
			t.Error("unexpected site or legacy request", r.URL.Host)
			return nil, errors.New("fixture-only")
		}
	})
	detail := f.request(t, "GET", "/api/v1/plugins/CookieCloud", nil)
	if detail.Code != 200 || strings.Contains(detail.Body.String(), "fixture-cloud-password") || !strings.Contains(detail.Body.String(), `"writeOnly":true`) {
		t.Fatal(detail.Code, detail.Body.String())
	}
	postForm := func(path string, values url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(values.Encode()))
		r.Header.Set("Authorization", f.token)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		return w
	}
	list := postForm("/api/v1/plugin/list", nil)
	if list.Code != 200 || strings.Contains(list.Body.String(), "fixture-cloud-password") || !strings.Contains(list.Body.String(), `"native":true`) {
		t.Fatal(list.Code, list.Body.String())
	}
	saved := postForm("/api/v1/plugin/config", url.Values{"id": {"CookieCloud"}, "config": {`{"black_list":"blocked.invalid"}`}})
	if saved.Code != 200 || cloud.Load() != 0 {
		t.Fatal(saved.Code, saved.Body.String(), cloud.Load())
	}

	test := ccStart(t, f, "test", strings.Repeat("1", 64))
	if run := ccWait(t, f, test.ID); run.State != "complete" || run.Added != 0 || run.UpdatedSites != 0 || cloud.Load() != 1 || probes.Load() != 0 {
		t.Fatal(run, cloud.Load(), probes.Load())
	}
	id := strings.Repeat("2", 64)
	manual := ccStart(t, f, "run", id)
	if run := ccWait(t, f, manual.ID); run.State != "complete" || run.Added != 1 || run.UpdatedSites != 1 || run.Kept != 1 || cloud.Load() != 2 || probes.Load() != 2 {
		t.Fatal(run, cloud.Load(), probes.Load())
	}
	if again := ccStart(t, f, "run", id); again.ID != manual.ID || again.State != "complete" || cloud.Load() != 2 {
		t.Fatal(again, cloud.Load())
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var plugin string
	if err := db.QueryRowContext(t.Context(), `SELECT json_extract(DATA,'$.plugin') FROM GO_BUSINESS_EVENT_DATA D
		JOIN GO_BUSINESS_EVENTS E ON E.ID=D.EVENT_ID WHERE E.REFERENCE=?`, manual.ID).Scan(&plugin); err != nil || plugin != cookiecloud.Plugin {
		t.Fatal("completion event lacks its plugin", err)
	}
	site, err := f.api.sites.Get(t.Context(), f.site.ID)
	want := f.site
	want.Cookie = "SID=fixture-fresh"
	if err != nil || site != want {
		t.Fatal("original site data changed", err)
	}
	other, err := f.api.sites.Get(t.Context(), healthy.ID)
	if err != nil || other != healthy {
		t.Fatal("working Cookie overwritten", err)
	}
	sites, err := f.api.sites.List(t.Context())
	if err != nil || len(sites) != 3 || sites[2].Name != "New tracker" || sites[2].Priority != "8" || sites[2].Include != "T" || sites[2].Cookie != "SID=fixture-new" {
		t.Fatal(sites, err)
	}

	fp, control := ccStatus(t, f)
	response := f.request(t, "POST", cookieCloudPath+"/activate", map[string]any{"fingerprint": fp, "epoch": control.Epoch, "confirm": true})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	_, control = ccStatus(t, f)
	if control.Epoch == "" || control.NextOnce == "" {
		t.Fatal(control)
	}
	onceID := sitelogin.Hash([]string{"cookie-cloud", control.Epoch, "once", control.NextOnce})
	api.tick(t.Context(), time.Now().Add(5*time.Second))
	if once := ccWait(t, f, onceID); once.State != "complete" || once.Added != 0 || once.UpdatedSites != 0 {
		t.Fatal(once)
	}
	api.tick(t.Context(), time.Now().Add(20*time.Second))
	if cloud.Load() != 3 {
		t.Fatal("one-time slot was replayed", cloud.Load())
	}
	stored, err := f.signin.system.Get(t.Context(), cookiecloud.ConfigKey)
	if err != nil || !strings.Contains(stored, `"onlyonce":false`) || !strings.Contains(stored, `"big":9007199254740993`) ||
		!strings.Contains(stored, `"null":null`) || !strings.Contains(stored, `"black_list":"blocked.invalid"`) {
		t.Fatal(err)
	}
	page := postForm("/api/v1/plugin/page", url.Values{"id": {"CookieCloud"}})
	if page.Code != 200 || !strings.Contains(page.Body.String(), manual.ID) || strings.Contains(page.Body.String(), "fixture-fresh") {
		t.Fatal(page.Code, page.Body.String())
	}
}

func TestCookieCloudSettingsRevokeGrantEvenIfRestored(t *testing.T) {
	f, _ := newCookieCloudFixture(t)
	if err := f.signin.system.Set(t.Context(), cookiecloud.ConfigKey, `{"server":"https://receiver.fixture.test","key":"fixture","password":"fixture","cron":"0 8 * * 0"}`); err != nil {
		t.Fatal(err)
	}
	fp, c := ccStatus(t, f)
	response := f.request(t, "POST", cookieCloudPath+"/activate", map[string]any{"confirm": true, "fingerprint": fp, "epoch": c.Epoch})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	fp, c = ccStatus(t, f)
	if c.Epoch == "" || c.NextRun == "" {
		t.Fatal(c)
	}
	for _, value := range []string{"https://different.fixture.test", "https://receiver.fixture.test"} {
		response = f.request(t, "POST", cookieCloudPath+"/settings", map[string]any{"fingerprint": fp, "values": map[string]string{"server": value}})
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		fp, c = ccStatus(t, f)
		if c != (cookiecloud.Control{}) {
			t.Fatal("settings retained old grant", c)
		}
	}
}

func TestCookieCloudPublicationRequiresOriginalReceipt(t *testing.T) {
	f, api := newCookieCloudFixture(t)
	if err := f.signin.system.Set(t.Context(), cookiecloud.ConfigKey, `{"server":"https://receiver.fixture.test","key":"fixture","password":"fixture","notify":true}`); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `CREATE TRIGGER fixture_drop_receipt BEFORE UPDATE OF STATE ON GO_COOKIE_CLOUD_RUNS
		WHEN NEW.STATE='complete' BEGIN SELECT RAISE(IGNORE); END`)
	if err != nil {
		t.Fatal(err)
	}
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "receiver.fixture.test":
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
				`{"site.fixture.test":[{"domain":"site.fixture.test","name":"SID","value":"new"}]}`))}, nil
		case "site.fixture.test":
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(""))}, nil
		default:
			t.Error("unexpected receiver", r.URL.Host)
			return nil, errors.New("fixture only")
		}
	})
	run := ccStart(t, f, "run", strings.Repeat("4", 64))
	if run = ccWait(t, f, run.ID); run.State != "failed" || run.UpdatedSites != 0 {
		t.Fatal(run)
	}
	site, err := api.login.sites.Get(t.Context(), f.site.ID)
	if err != nil || site != f.site {
		t.Fatal("publication outlived its receipt", err)
	}
	if events, err := bus.New(db).List(t.Context()); err != nil || len(events) != 0 {
		t.Fatal("notification outlived its receipt", events, err)
	}
}

func TestCookieCloudHybridWritesDoNotFallThrough(t *testing.T) {
	handler, token, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["CookieCloud","TorrentMark"]`); err != nil {
		t.Fatal(err)
	}
	res := nativeJSONRequest(handler, http.MethodPut, "/api/v1/plugins/%20CookieCloud", token, `{"values":{}}`)
	if res.Code != http.StatusNotImplemented {
		t.Fatal(res.Code, res.Body.String())
	}
	res = performFormRequest(handler, "/api/v1/plugin/list", token, nil)
	if res.Code != http.StatusNotImplemented {
		t.Fatal(res.Code, res.Body.String())
	}
	for _, key := range []string{cookiecloud.ConfigKey, "UserInstalledPlugins"} {
		res = performFormRequest(handler, "/api/v1/config/set", token, url.Values{"key": {key}, "value": {"{}"}})
		if res.Code != http.StatusBadRequest {
			t.Fatal(res.Code, res.Body.String())
		}
	}
}

func TestCookieCloudPeerStopPreventsPublicationAfterCloudResponse(t *testing.T) {
	f, api := newCookieCloudFixture(t)
	if err := f.signin.system.Set(t.Context(), cookiecloud.ConfigKey, `{"server":"https://receiver.fixture.test","key":"fixture","password":"fixture"}`); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "receiver.fixture.test" {
			t.Error("stop allowed a tracker probe")
			return nil, errors.New("denied")
		}
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"site.fixture.test":[{"domain":"site.fixture.test","name":"SID","value":"would-write"}]}`))}, nil
	})
	run := ccStart(t, f, "run", strings.Repeat("3", 64))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cloud request did not start")
	}
	peer, err := cookiecloud.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.Stop(t.Context(), "", strings.Repeat("0", 64)); !errors.Is(err, cookiecloud.ErrScope) {
		t.Fatal("stale stop accepted", err)
	}
	if err := peer.Stop(t.Context(), "", run.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if out := ccWait(t, f, run.ID); out.State != "stopped" || out.UpdatedSites != 0 {
		t.Fatal(out)
	}
	site, err := api.login.sites.Get(t.Context(), f.site.ID)
	if err != nil || site != f.site {
		t.Fatal("peer stop lost the original site", err)
	}
}
