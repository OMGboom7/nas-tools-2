package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

const statsHome = `<html><head><title>Fixture</title><link rel="search"></head><body><a href="userdetails.php?id=42">fixture</a><a href="logout.php">Logout</a></body></html>`
const statsProfile = `<table><tr><td>Uploaded</td><td>2 GiB</td></tr><tr><td>Downloaded</td><td>1 GiB</td></tr><tr><td>Class</td><td>VIP</td></tr></table>`
const statsSeeds = `<table class="torrents"><tr><td>Name</td><td>Other</td><td>Size</td><td>Seeders</td></tr><tr><td>Fixture</td><td>Other</td><td>1.5 GiB</td><td>3</td></tr></table>`

func newStatsFixture(t *testing.T) (*loginFixture, *siteStatisticsAPI) {
	t.Helper()
	f := newLoginFixture(t, nil)
	api := f.signin.statistics
	if api == nil {
		t.Fatal("statistics not attached")
	}
	site := f.site
	site.Include = "T"
	var e error
	f.site, e = f.api.sites.Upsert(t.Context(), site)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait := startSiteStatisticsWorker(ctx, api)
	t.Cleanup(func() { cancel(); wait(); api.store.Close() })
	return f, api
}
func statsState(t *testing.T, f *loginFixture) statsStatus {
	t.Helper()
	r := f.request(t, "GET", "/api/v1/site-statistics", nil)
	var out struct{ Data statsStatus }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	if strings.Contains(r.Body.String(), "fixture-private") || strings.Contains(r.Body.String(), "privateAuthority") {
		t.Fatal("private statistics authority leaked")
	}
	return out.Data
}
func statsRefresh(t *testing.T, f *loginFixture, nonce string, read bool) sitestats.Job {
	t.Helper()
	s := statsState(t, f)
	r := f.request(t, "POST", "/api/v1/site-statistics/refresh", map[string]any{"fingerprint": s.Fingerprint, "requestId": sitestats.Hash(nonce), "siteIds": []string{strconv.FormatInt(f.site.ID, 10)}, "readMessages": read, "confirm": true})
	var out struct{ Data sitestats.Job }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return out.Data
}
func statsWait(t *testing.T, api *siteStatisticsAPI, id string) sitestats.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, e := api.store.Get(t.Context(), id)
		if e != nil {
			t.Fatal(e)
		}
		if j.State != "queued" && j.State != "running" {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatal("statistics did not finish", j)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func statsTransport(t *testing.T, calls *atomic.Int32, fail *atomic.Bool) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host != "site.fixture.test" || r.Header.Get("Cookie") != "original=fixture-private" || r.Header.Get("X-API-KEY") != "" {
			t.Errorf("statistics request escaped original site: %s", r.URL)
			return nil, io.EOF
		}
		body := ""
		switch r.URL.Path {
		case "/":
			body = statsHome
		case "/userdetails.php":
			if fail != nil && fail.Load() {
				return nil, io.EOF
			}
			body = statsProfile
		case "/getusertorrentlistajax.php":
			body = statsSeeds
		case "/favicon.ico":
			return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		default:
			t.Errorf("unexpected original path %s", r.URL)
			return nil, io.EOF
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
}

func TestSiteStatisticsNativeHTTPCompleteFailureDedupAndLegacyReads(t *testing.T) {
	f, api := newStatsFixture(t)
	var calls atomic.Int32
	var fail atomic.Bool
	f.api.transport = statsTransport(t, &calls, &fail)
	// No original schedule or browser opening may run without Go ownership.
	api.tick(t.Context(), time.Now().Add(time.Hour))
	if calls.Load() != 0 {
		t.Fatal("statistics ran before takeover")
	}
	j := statsRefresh(t, f, "fixture-original", false)
	j = statsWait(t, api, j.ID)
	if j.State != "complete" || len(j.Results) != 1 || j.Requests != 4 {
		t.Fatal("native refresh", j)
	}
	s := statsState(t, f)
	if len(s.Items) != 1 || s.Items[0].Upload != 2<<30 || s.Items[0].SeedingSize != 1610612736 {
		t.Fatal(s.Items)
	}
	before := calls.Load()
	same := statsRefresh(t, f, "fixture-original", false)
	if same.ID != j.ID || calls.Load() != before {
		t.Fatal("duplicate refreshed site")
	}
	fail.Store(true)
	failed := statsRefresh(t, f, "fixture-failed", false)
	if statsWait(t, api, failed.ID).State != "failed" {
		t.Fatal("upstream error reported success")
	}
	s = statsState(t, f)
	if len(s.Items) != 1 || s.Items[0].Upload != 2<<30 {
		t.Fatal("failure overwrote previous data")
	}
	r := f.request(t, "GET", "/api/v1/site-statistics/detail/"+strconv.FormatInt(f.site.ID, 10), nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), "1610612736") {
		t.Fatal(r.Code, r.Body.String())
	}
	raw := httptest.NewRequest("GET", "/api/v1/site/statistics?apikey=native-site-login-fixture", nil)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, raw)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"user_statistics"`) {
		t.Fatal("legacy key endpoint", w.Code, w.Body.String())
	}
	for _, path := range []string{"activity", "seedinfo", "history"} {
		values := url.Values{"name": {"Fixture site"}, "days": {"7"}}
		raw = httptest.NewRequest("POST", "/api/v1/site/statistics/"+path, strings.NewReader(values.Encode()))
		raw.Header.Set("Authorization", f.token)
		raw.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		f.handler.ServeHTTP(w, raw)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"dataset"`) {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	store, e := sitestats.Open(f.path)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	restored, e := store.Get(t.Context(), j.ID)
	if e != nil || restored.State != "complete" || restored.Requests != j.Requests {
		t.Fatal("receipt restart", restored, e)
	}
}

func TestSiteStatisticsStopCancelsOriginalRequestAndKeepsPriorData(t *testing.T) {
	f, api := newStatsFixture(t)
	started := make(chan struct{})
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	j := statsRefresh(t, f, "fixture-stop", false)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request not started")
	}
	state := statsState(t, f)
	response := f.request(t, "POST", "/api/v1/site-statistics/stop", map[string]any{"expectedEpoch": state.Control.Epoch, "confirm": true})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if statsWait(t, api, j.ID).State != "stopped" {
		t.Fatal("not stopped")
	}
	if len(statsState(t, f).Items) != 0 {
		t.Fatal("stopped request published")
	}
}

func TestSiteStatisticsAuthenticationAndStaleGrant(t *testing.T) {
	f, _ := newStatsFixture(t)
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/site-statistics", "", "")
	if w.Code != 401 {
		t.Fatal(w.Code, w.Body.String())
	}
	state := statsState(t, f)
	r := f.request(t, "POST", "/api/v1/site-statistics/activate", map[string]any{"fingerprint": state.Fingerprint, "expectedEpoch": "stale", "readMessages": false, "remote": false, "confirm": true})
	if r.Code != 409 {
		t.Fatal("stale activate", r.Code)
	}
	r = f.request(t, "POST", "/api/v1/site-statistics/refresh", map[string]any{"fingerprint": "stale", "requestId": sitestats.Hash("x"), "siteIds": []string{}, "readMessages": false, "confirm": true})
	if r.Code != 400 {
		t.Fatal("stale refresh", r.Code)
	}
	raw := httptest.NewRequest("GET", "/api/v1/site/statistics?apikey=wrong", nil)
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, raw)
	if w.Code != 401 {
		t.Fatal("statistics ignored API key")
	}
}

func TestSiteStatisticsRemoteGrantWorksWithoutSigninPlugin(t *testing.T) {
	f, api := newStatsFixture(t)
	var calls atomic.Int32
	f.api.transport = statsTransport(t, &calls, nil)
	s := statsState(t, f)
	r := f.request(t, "POST", "/api/v1/site-statistics/activate", map[string]any{"fingerprint": s.Fingerprint, "expectedEpoch": "", "readMessages": false, "remote": true, "confirm": true})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	c, _, e := api.signin.remoteControl(t.Context())
	if e != nil || c.Scope.PluginDigest != "native-site-statistics" {
		t.Fatal("statistics depends on signer grant", c, e)
	}
	if e = api.signin.remoteCurrent(t.Context(), c); e != nil {
		t.Fatal(e)
	}
	if e = api.signin.remoteCurrent(t.Context(), c); e != nil {
		t.Fatal(e)
	}
	state := statsState(t, f)
	if !state.Current || !state.Control.Remote {
		t.Fatal(state)
	}
	r = f.request(t, "POST", "/api/v1/site-statistics/stop", map[string]any{"expectedEpoch": state.Control.Epoch, "confirm": true})
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	if e = api.signin.remoteCurrent(t.Context(), c); e == nil {
		t.Fatal("revoked receiver still active")
	}
	if calls.Load() != 0 {
		t.Fatal("grant itself dispatched refresh")
	}
}
