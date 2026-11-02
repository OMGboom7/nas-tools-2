package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

func newSigninHTTPFixture(t *testing.T) (*loginFixture, *siteSigninAPI) {
	t.Helper()
	f := newLoginFixture(t, nil)
	api := f.signin
	site := f.site
	site.SignURL = "https://carpt.net/"
	var e error
	f.site, e = f.api.sites.Upsert(t.Context(), site)
	if e != nil {
		t.Fatal(e)
	}
	if e = api.system.Set(t.Context(), "UserInstalledPlugins", `["AutoSignIn"]`); e != nil {
		t.Fatal(e)
	}
	raw := `{"enabled":false,"queue_cnt":2,"sign_sites":["` + strconv.FormatInt(site.ID, 10) + `"],"future":{"big":9007199254740993,"null":null}}`
	if e = api.system.Set(t.Context(), "plugin.AutoSignIn", raw); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait := startSiteSigninWorker(ctx, api)
	t.Cleanup(func() { cancel(); wait(); api.store.Close() })
	return f, api
}
func signinTestReview(t *testing.T, f *loginFixture, mode string) siteSigninReviewView {
	t.Helper()
	response := f.request(t, "POST", "/api/v1/site-signin/review", map[string]any{"mode": mode, "siteIds": []string{strconv.FormatInt(f.site.ID, 10)}, "options": map[string]bool{}})
	var out struct{ Data siteSigninReviewView }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &out) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "fixture-private") {
		t.Fatal("review leaked Cookie")
	}
	return out.Data
}
func signinTestBatch(t *testing.T, f *loginFixture, id string) siteSigninBatchView {
	t.Helper()
	response := f.request(t, "GET", "/api/v1/site-signin/batches/"+id, nil)
	var out struct{ Data siteSigninBatchView }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &out) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	return out.Data
}
func signinTestFinished(t *testing.T, f *loginFixture, id string) siteSigninBatchView {
	t.Helper()
	end := time.Now().Add(8 * time.Second)
	for {
		out := signinTestBatch(t, f, id)
		done := true
		for _, j := range out.Items {
			if sitesignin.Active(j.State) {
				done = false
			}
		}
		if done && out.Notified {
			return out
		}
		if time.Now().After(end) {
			t.Fatal("original batch did not finish")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
func TestSiteSigninNativeHTTPReusesReceiptAfterHandlerRestart(t *testing.T) {
	f, api := newSigninHTTPFixture(t)
	var calls atomic.Int32
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "carpt.net" || r.URL.Path != "/attendance.php" {
			t.Errorf("unexpected upstream %s", r.URL)
			return nil, io.EOF
		}
		calls.Add(1)
		if !strings.Contains(r.Header.Get("Cookie"), "original=fixture-private") {
			t.Error("original Cookie lost")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("签到成功"))}, nil
	})
	p := signinTestReview(t, f, "manual")
	response := f.request(t, "POST", "/api/v1/site-signin/start", map[string]any{"id": p.ID, "fingerprint": p.Fingerprint, "confirm": true})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	before := signinTestFinished(t, f, p.ID)
	if len(before.Items) != 1 || before.Items[0].Outcome.State != "signed" || calls.Load() != 1 {
		t.Fatalf("%+v calls=%d", before, calls.Load())
	}
	response = f.request(t, "POST", "/api/v1/site-signin/start", map[string]any{"id": p.ID, "fingerprint": p.Fingerprint, "confirm": true})
	if response.Code != 200 || calls.Load() != 1 {
		t.Fatal("confirmation repeated upstream action")
	}
	var runner *rssRunAPI
	handler, e := buildHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: f.configPath, SiteCatalogPath: filepath.Join(filepath.Dir(f.configPath), "catalog.bin")}, f.api.transport, &runner)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { runner.siteSignin.store.Close(); runner.siteLogin.store.Close() })
	response = nativeJSONRequest(handler, "GET", "/api/v1/site-signin/batches/"+p.ID, f.token, "")
	var reopened struct{ Data siteSigninBatchView }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &reopened) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if reopened.Data.Items[0].Updated != before.Items[0].Updated || reopened.Data.Items[0].ID != before.Items[0].ID || calls.Load() != 1 {
		t.Fatal("new handler changed or replayed original receipt")
	}
	site, e := api.login.sites.Get(t.Context(), f.site.ID)
	if e != nil || site.Cookie != f.site.Cookie || site.Note != f.site.Note {
		t.Fatal("sign-in changed original site credentials/unknown note")
	}
}
func TestSiteSigninOldFlagsAndEmptySelectionDoNotStartWork(t *testing.T) {
	f, api := newSigninHTTPFixture(t)
	var calls atomic.Int32
	f.api.transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, io.EOF })
	if e := api.system.Set(t.Context(), "plugin.AutoSignIn", `{"enabled":true,"onlyonce":true,"cron":"0.0001","sign_sites":[]}`); e != nil {
		t.Fatal(e)
	}
	if e := api.tick(t.Context(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 0 {
		t.Fatal("old flags caused an upstream request")
	}
	for _, mode := range []string{"manual", "activate"} {
		response := f.request(t, "POST", "/api/v1/site-signin/review", map[string]any{"mode": mode, "siteIds": []string{}, "options": map[string]bool{}})
		if response.Code != 409 {
			t.Fatal("empty selection expanded", response.Code, response.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("review contacted a provider")
	}
}
func TestSiteSigninUnknownResponseIsNotRetriedByOnceSchedule(t *testing.T) {
	f, api := newSigninHTTPFixture(t)
	var calls atomic.Int32
	f.api.transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, io.ErrUnexpectedEOF })
	p := signinTestReview(t, f, "manual")
	response := f.request(t, "POST", "/api/v1/site-signin/start", map[string]any{"id": p.ID, "fingerprint": p.Fingerprint, "confirm": true})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	out := signinTestFinished(t, f, p.ID)
	if len(out.Items) != 1 || out.Items[0].Outcome.State != "unknown" {
		t.Fatalf("%+v", out)
	}
	raw := `{"enabled":false,"onlyonce":true,"queue_cnt":2,"sign_sites":["` + strconv.FormatInt(f.site.ID, 10) + `"]}`
	if e := api.system.Set(t.Context(), "plugin.AutoSignIn", raw); e != nil {
		t.Fatal(e)
	}
	p = signinTestReview(t, f, "activate")
	response = f.request(t, "POST", "/api/v1/site-signin/activate", map[string]any{"id": p.ID, "fingerprint": p.Fingerprint, "confirm": true})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	end := time.Now().Add(8 * time.Second)
	for {
		control, e := api.store.Control(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		if control.NextRun == "" {
			break
		}
		if time.Now().After(end) {
			t.Fatal("one-shot schedule not consumed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("unknown original request replayed", calls.Load())
	}
	raw, e := api.system.Get(t.Context(), "plugin.AutoSignIn")
	if e != nil || !strings.Contains(raw, `"onlyonce":false`) {
		t.Fatal("one-shot source flag not consumed")
	}
}
func TestSiteSigninNativePluginRoutesRetainUnknownSettings(t *testing.T) {
	f, api := newSigninHTTPFixture(t)
	response := f.request(t, "GET", "/api/v1/site-signin", nil)
	var status struct{ Data siteSigninStatus }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	response = f.request(t, "PUT", "/api/v1/plugins/AutoSignIn", map[string]any{"fingerprint": status.Data.Fingerprint, "values": map[string]any{"cron": "23.5", "queue_cnt": "3"}})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	raw, e := api.system.Get(t.Context(), "plugin.AutoSignIn")
	if e != nil || !strings.Contains(raw, `9007199254740993`) || !strings.Contains(raw, `"null":null`) {
		t.Fatal("unknown plugin fields lost", raw, e)
	}
	for _, path := range []string{"/api/v1/plugins", "/api/v1/plugins/AutoSignIn", "/api/v1/plugins/AutoSignIn/page"} {
		response = f.request(t, "GET", path, nil)
		if response.Code != 200 {
			t.Fatal(path, response.Code, response.Body.String())
		}
	}
}
func TestSiteSigninOriginalScheduleUnitsAndCalendarBoundaries(t *testing.T) {
	zone, e := time.LoadLocation("Asia/Shanghai")
	if e != nil {
		t.Fatal(e)
	}
	start := time.Date(2026, 12, 31, 23, 58, 0, 0, zone)
	s, e := parseSigninSchedule("23.5", zone)
	if e != nil {
		t.Fatal(e)
	}
	next, e := s.next(start)
	if e != nil || next.Sub(start) != 23*time.Hour+30*time.Minute {
		t.Fatal("plugin hours changed to RSS minutes", next, e)
	}
	for _, date := range []time.Time{start, time.Date(2026, 1, 31, 23, 58, 0, 0, zone), time.Date(2028, 2, 29, 23, 58, 0, 0, zone)} {
		next, e := signinMissedNext("09:30-23:59", zone, date)
		tomorrow := date.AddDate(0, 0, 1)
		if e != nil || next.Year() != tomorrow.Year() || next.Month() != tomorrow.Month() || next.Day() != tomorrow.Day() || next.Hour() != 9 || next.Minute() < 30 || next.Minute() > 59 {
			t.Fatalf("calendar boundary %s -> %s %v", date, next, e)
		}
	}
	s, e = parseSigninSchedule("0 8 * * mon", zone)
	if e != nil {
		t.Fatal(e)
	}
	next, e = s.next(time.Date(2026, 11, 1, 9, 0, 0, 0, zone))
	if e != nil || next.Weekday() != time.Monday || next.Day() != 2 || next.Hour() != 8 {
		t.Fatal("original Monday weekday semantics changed", next, e)
	}
}
func TestSiteSigninCFDependencyHonorsOriginalPluginPrerequisites(t *testing.T) {
	for _, test := range []struct {
		name  string
		hosts bool
		ip    string
		want  int
	}{{"hosts-disabled", false, "1.2.3.4", 0}, {"cf-unconfigured", true, "", 0}, {"both-configured", true, "1.2.3.4", 1}} {
		t.Run(test.name, func(t *testing.T) {
			f, api := newSigninHTTPFixture(t)
			site := f.site
			site.SignURL = "https://totheglory.im/"
			var e error
			f.site, e = f.api.sites.Upsert(t.Context(), site)
			if e != nil {
				t.Fatal(e)
			}
			for key, value := range map[string]any{"plugin.AutoSignIn": map[string]any{"queue_cnt": 2, "auto_cf": 1}, "plugin.CustomHosts": map[string]any{"enable": test.hosts}, "plugin.CloudflareSpeedTest": map[string]any{"cf_ip": test.ip}} {
				raw, _ := json.Marshal(value)
				if e = api.system.Set(t.Context(), key, string(raw)); e != nil {
					t.Fatal(e)
				}
			}
			f.api.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`<a href="logout.php">fixture login</a>`))}, nil
			})
			p := signinTestReview(t, f, "manual")
			response := f.request(t, "POST", "/api/v1/site-signin/start", map[string]any{"id": p.ID, "fingerprint": p.Fingerprint, "confirm": true})
			if response.Code != 200 {
				t.Fatal(response.Body.String())
			}
			out := signinTestFinished(t, f, p.ID)
			if len(out.Items) != 1 || out.Items[0].Outcome.State != "failed" || !out.Items[0].Outcome.Retryable {
				t.Fatalf("%+v", out)
			}
			dependencies, e := api.store.Dependencies(t.Context())
			if e != nil || len(dependencies) != test.want {
				t.Fatalf("dependencies=%+v want=%d error=%v", dependencies, test.want, e)
			}
		})
	}
}
