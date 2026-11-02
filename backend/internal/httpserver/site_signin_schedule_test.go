package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

func signinActivateSchedule(t *testing.T, f *loginFixture, api *siteSigninAPI, cron string, once bool) sitesignin.Control {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"enabled": true, "cron": cron, "onlyonce": once, "queue_cnt": 2, "sign_sites": []string{strconv.FormatInt(f.site.ID, 10)}})
	if e := api.system.Set(t.Context(), "plugin.AutoSignIn", string(raw)); e != nil {
		t.Fatal(e)
	}
	p := signinTestReview(t, f, "activate")
	r := f.request(t, "POST", "/api/v1/site-signin/activate", map[string]any{"id": p.ID, "fingerprint": p.Fingerprint, "confirm": true})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	c, e := api.store.Control(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	return c
}

func signinScheduleSuccess(f *loginFixture) {
	f.api.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("签到成功"))}, nil
	})
}

func TestSiteSigninOnceDoesNotReplaceIndependentPeriodicSchedule(t *testing.T) {
	f, api := newSigninQuietFixture(t)
	signinScheduleSuccess(f)
	before := time.Now()
	c := signinActivateSchedule(t, f, api, "1", true)
	run, e := time.Parse(time.RFC3339Nano, c.NextRun)
	if e != nil || run.Before(before.Add(59*time.Minute)) || run.After(time.Now().Add(time.Hour)) {
		t.Fatalf("one-shot replaced the independent hourly occurrence: %s %v", c.NextRun, e)
	}
	r := f.request(t, "GET", "/api/v1/site-signin", nil)
	var status struct {
		Data struct {
			Control struct {
				NextOnce string `json:"nextOnce"`
			}
		}
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &status) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	once, e := time.Parse(time.RFC3339Nano, status.Data.Control.NextOnce)
	if e != nil || once.Before(before.Add(3*time.Second)) || once.After(time.Now().Add(3*time.Second)) {
		t.Fatal("independent one-shot receipt missing", status.Data.Control.NextOnce, e)
	}
	if e = api.tick(t.Context(), once.Add(time.Second)); e != nil {
		t.Fatal(e)
	}
	after, e := api.store.Control(t.Context())
	if e != nil || after.NextRun != c.NextRun || !after.Enabled {
		t.Fatal("one-shot moved the periodic phase", after, e)
	}
}

func TestSiteSigninSlowOrMissedIntervalKeepsOriginalPhase(t *testing.T) {
	f, api := newSigninQuietFixture(t)
	signinScheduleSuccess(f)
	c := signinActivateSchedule(t, f, api, "1", false)
	now := time.Now()
	due := now.Add(-150 * time.Minute)
	pending := c
	pending.NextRun = due.UTC().Format(time.RFC3339Nano)
	if e := api.store.Advance(t.Context(), c, pending); e != nil {
		t.Fatal(e)
	}
	if e := api.tick(t.Context(), now); e != nil {
		t.Fatal(e)
	}
	after, e := api.store.Control(t.Context())
	want := due.Add(3 * time.Hour).UTC().Format(time.RFC3339Nano)
	if e != nil || after.NextRun != want {
		t.Fatalf("interval drifted from original phase: got %s, want %s (%v)", after.NextRun, want, e)
	}
}

func TestSiteSigninRandomWindowNeverSchedulesTwiceForConsumedDay(t *testing.T) {
	f, api := newSigninQuietFixture(t)
	signinScheduleSuccess(f)
	c := signinActivateSchedule(t, f, api, "00:00-23:59", false)
	zone, e := time.LoadLocation(c.Scope.Location)
	if e != nil {
		t.Fatal(e)
	}
	local := time.Now().In(zone).AddDate(0, 0, 1)
	due := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, zone)
	pending := c
	pending.NextRun = due.UTC().Format(time.RFC3339Nano)
	if e = api.store.Advance(t.Context(), c, pending); e != nil {
		t.Fatal(e)
	}
	// A backwards wall clock after executing the due occurrence must not
	// permit another random occurrence on the consumed local date.
	if e = api.tick(t.Context(), due.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	after, e := api.store.Control(t.Context())
	next, parse := time.Parse(time.RFC3339Nano, after.NextRun)
	wantDay := due.AddDate(0, 0, 1).Format("2006-01-02")
	if e != nil || parse != nil || next.In(zone).Format("2006-01-02") != wantDay {
		t.Fatalf("random window reused a consumed date: %s want %s (%v %v)", after.NextRun, wantDay, e, parse)
	}
}
