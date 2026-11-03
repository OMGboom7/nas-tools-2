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

	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

func TestSiteStatisticsOriginalScheduleFormatsAndHours(t *testing.T) {
	for _, raw := range []string{"6", "23.5", "08:00", "08:00-09:00", "0 1 * * *"} {
		t.Run(raw, func(t *testing.T) {
			f, api := newStatsFixture(t)
			var calls atomic.Int32
			f.api.transport = statsTransport(t, &calls, nil)
			if e := f.api.config.Update(map[string]any{"pt.ptrefresh_date_cron": raw}); e != nil {
				t.Fatal(e)
			}
			s := statsState(t, f)
			before := time.Now()
			r := f.request(t, "POST", "/api/v1/site-statistics/activate", map[string]any{"fingerprint": s.Fingerprint, "expectedEpoch": "", "readMessages": false, "remote": false, "confirm": true})
			if r.Code != 200 {
				t.Fatal(raw, r.Code, r.Body.String())
			}
			c, e := api.store.Control(t.Context())
			if e != nil {
				t.Fatal(e)
			}
			first, e := time.Parse(time.RFC3339Nano, c.NextRun)
			if e != nil || first.Sub(before) < time.Minute || first.Sub(before) > time.Minute+time.Second {
				t.Fatal("startup delay", c.NextRun, e)
			}
			if raw == "6" {
				api.tick(t.Context(), first.Add(time.Second))
				jobs, e := api.store.Jobs(t.Context())
				if e != nil || len(jobs) != 1 {
					t.Fatal(jobs, e)
				}
				if statsWait(t, api, jobs[0].ID).State != "complete" {
					t.Fatal("timer refresh failed")
				}
				c, _ = api.store.Control(t.Context())
				next, _ := time.Parse(time.RFC3339Nano, c.NextRun)
				if next.Sub(first) != 6*time.Hour {
					t.Fatal("hour schedule changed units", next.Sub(first))
				}
			}
		})
	}
}

func TestSiteStatisticsOrphanReceiptRecoversWithoutNewRefresh(t *testing.T) {
	f, api := newStatsFixture(t)
	j, _, e := api.store.Create(t.Context(), sitestats.Job{ID: sitestats.Hash("orphan"), Origin: "fixture-orphan", Actor: "admin"})
	if e != nil {
		t.Fatal(e)
	}
	if e = api.store.Change(t.Context(), j.ID, func(v *sitestats.Job) error { v.State = "running"; return nil }); e != nil {
		t.Fatal(e)
	}
	s := statsState(t, f)
	if len(s.Jobs) != 1 || s.Jobs[0].State != "unknown" {
		t.Fatal("orphan keeps page locked", s.Jobs)
	}
}

func TestSiteStatisticsOtherProcessStopRevokesManualRun(t *testing.T) {
	f, api := newStatsFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-release
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(statsHome))}, nil
	})
	j := statsRefresh(t, f, "cross-process-stop", false)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request not started")
	}
	s := statsState(t, f)
	peerStore, e := sitestats.Open(f.path)
	if e != nil {
		t.Fatal(e)
	}
	defer peerStore.Close()
	peer := &siteStatisticsAPI{store: peerStore, login: f.api, signin: f.signin}
	request := httptest.NewRequest("POST", "/api/v1/site-statistics/stop", strings.NewReader(`{"expectedEpoch":"`+s.Control.Epoch+`","confirm":true}`))
	request.Header.Set("Authorization", f.token)
	w := httptest.NewRecorder()
	peer.manage(w, request)
	close(release)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	row := statsWait(t, api, j.ID)
	if row.State != "stopped" || !row.StopRequested || calls.Load() != 1 || len(statsState(t, f).Items) != 0 {
		t.Fatal("peer stop failed", row, calls.Load())
	}
}

func TestSiteStatisticsLegacyReadsRetainOrdinaryUserAccess(t *testing.T) {
	f, _ := newStatsFixture(t)
	if _, e := f.api.auth.service.CreateUser(t.Context(), "fixture-viewer", "fixture-viewer-password", []string{}); e != nil {
		t.Fatal(e)
	}
	token := loginForTest(t, f.handler, "fixture-viewer", "fixture-viewer-password")
	for _, path := range []string{"activity", "history", "seedinfo"} {
		values := url.Values{"name": {"Fixture site"}, "days": {"7"}}
		r := httptest.NewRequest("POST", "/api/v1/site/statistics/"+path, strings.NewReader(values.Encode()))
		r.Header.Set("Authorization", token)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/site-statistics/refresh", token, `{}`)
	if w.Code != 403 {
		t.Fatal("viewer could start refresh", w.Code)
	}
}

func TestSiteStatisticsRemoteTransportAndIncomingSTAIndependentOfSigner(t *testing.T) {
	f, api := newStatsFixture(t)
	var calls atomic.Int32
	f.api.transport = statsTransport(t, &calls, nil)
	if _, e := f.signin.channels.Save(t.Context(), notificationconfig.Channel{Name: "Fixture Synology", Type: "synologychat", Config: `{"token":"fixture-token","future":9007199254740993}`, Switches: `[]`, Enabled: 1, Interactive: 1}); e != nil {
		t.Fatal(e)
	}
	s := statsState(t, f)
	r := f.request(t, "POST", "/api/v1/site-statistics/activate", map[string]any{"fingerprint": s.Fingerprint, "expectedEpoch": "", "readMessages": false, "remote": true, "confirm": true})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	scope, sources, e := f.signin.remoteControl(t.Context())
	if e != nil || len(sources) != 1 {
		t.Fatal(scope, sources, e)
	}
	probe, _ := http.NewRequestWithContext(t.Context(), "GET", "http://message.fixture.test/probe", nil)
	raw, status, e := f.signin.remoteCall(t.Context(), scope, sources[0], "fixture-transport", probe, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	}))
	if e != nil || status != 200 || !json.Valid(raw) {
		t.Fatal("external receiver journal", string(raw), status, e)
	}
	values := url.Values{"post_id": {"fixture-event"}, "token": {"fixture-token"}, "user_id": {"fixture-user"}, "username": {"Fixture User"}, "text": {"/sta"}, "timestamp": {strconv.FormatInt(time.Now().Unix(), 10)}}
	for repeat := 0; repeat < 2; repeat++ {
		request := httptest.NewRequest("POST", "/synology?apikey=native-site-login-fixture", strings.NewReader(values.Encode()))
		request.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, request)
		if w.Code != 200 {
			t.Fatal("original /sta callback", w.Code, w.Body.String())
		}
		jobs, e := api.store.Jobs(context.Background())
		if e != nil || len(jobs) != 1 {
			t.Fatal(jobs, e)
		}
		if statsWait(t, api, jobs[0].ID).State != "complete" {
			t.Fatal("remote job failed")
		}
	}
	if calls.Load() != 4 {
		t.Fatal("duplicate remote effect", calls.Load())
	}
}
