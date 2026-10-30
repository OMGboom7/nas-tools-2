package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

const mediaRefreshPluginEndpoint = "/api/v1/plugins/LibraryRefresh"
const mediaRefreshEndpoint = "/api/v1/library/refresh"

type mediaRefreshWireState struct {
	writes, reads atomic.Int32
	lost          bool
	identity      string
	sections      string
	beforeWrite   func(*http.Request)
}

func mediaRefreshWire(t *testing.T, f *subscriptionRunFixture, kind string) *mediaRefreshWireState {
	t.Helper()
	w := &mediaRefreshWireState{identity: "server-1", sections: `{"MediaContainer":{"size":1,"Directory":[{"key":"1","title":"Movies","Location":[{"path":"/"}]}]}}`}
	previous := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "refresh.test" {
			return previous.RoundTrip(r)
		}
		if r.Header.Get("X-Plex-Token")+r.Header.Get("X-Emby-Token") != "private-refresh-key" || r.URL.Query().Get("api_key") != "" {
			t.Fatal("private scope changed", r.URL)
		}
		if r.Method == "POST" {
			w.writes.Add(1)
			// Each network side effect must already have its exact durable intent.
			rows, err := f.db.Query(`SELECT STATE FROM GO_MEDIA_REFRESH_ACTIONS WHERE STATE='intent'`)
			if err != nil {
				t.Fatal(err)
			}
			seen := rows.Next()
			rows.Close()
			if !seen {
				t.Fatal("scan before durable intent")
			}
			if w.beforeWrite != nil {
				w.beforeWrite(r)
			}
			if w.lost {
				return nil, errors.New("private upstream lost acknowledgement")
			}
			return &http.Response{StatusCode: 204, Header: http.Header{}, Body: http.NoBody}, nil
		}
		if r.Method != "GET" {
			t.Fatal("unexpected verb", r.Method)
		}
		w.reads.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/System/Info"):
			return jsonResponse(r, `{"Id":"`+w.identity+`"}`), nil
		case r.URL.Path == "/prefix/" || r.URL.Path == "/prefix":
			return jsonResponse(r, `{"MediaContainer":{"machineIdentifier":"`+w.identity+`"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			return jsonResponse(r, w.sections), nil
		case strings.HasSuffix(r.URL.Path, "/Items"):
			return jsonResponse(r, `{"Items":[],"TotalRecordCount":0}`), nil
		case strings.HasSuffix(r.URL.Path, "/Library/SelectableMediaFolders"):
			return jsonResponse(r, `[]`), nil
		default:
			t.Fatal("unexpected media server API", r.URL)
			return nil, context.Canceled
		}
	})
	key := "api_key"
	if kind == "plex" {
		key = "token"
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.media_server": kind, kind + ".host": "https://refresh.test/prefix", kind + "." + key: "private-refresh-key"}); err != nil {
		t.Fatal(err)
	}
	var err error
	f.handler, err = buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport, &f.runner)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func mediaRefreshHTTP(t *testing.T, f *subscriptionRunFixture, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw := ""
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		raw = string(b)
	}
	return nativeJSONRequest(f.handler, method, path, f.token, raw)
}
func mediaRefreshDetail(t *testing.T, f *subscriptionRunFixture) pluginConfigDetail {
	t.Helper()
	return recoveryData[pluginConfigDetail](t, mediaRefreshHTTP(t, f, "GET", mediaRefreshPluginEndpoint, nil))
}
func mediaRefreshGrant(t *testing.T, f *subscriptionRunFixture, delay string) {
	t.Helper()
	if r := mediaRefreshHTTP(t, f, "POST", mediaRefreshPluginEndpoint+"/install", nil); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	detail := mediaRefreshDetail(t, f)
	r := mediaRefreshHTTP(t, f, "PUT", mediaRefreshPluginEndpoint, map[string]any{"fingerprint": detail.Meta.Fingerprint, "values": map[string]any{"enable": true, "delay": delay, "go_confirm": true, "go_old_stopped": true, "go_scan": true}})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
}
func mediaRefreshPreview(t *testing.T, f *subscriptionRunFixture, job string, whole bool) mediaRefreshReview {
	t.Helper()
	return recoveryData[mediaRefreshReview](t, mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/preview", map[string]any{"jobId": job, "whole": whole, "confirmRead": true}))
}
func mediaRefreshSubmitInput(v mediaRefreshReview) map[string]any {
	return map[string]any{"jobId": v.JobID, "whole": v.Whole, "requestId": v.RequestID, "fingerprint": v.Fingerprint, "confirmRead": true, "confirm": true, "confirmScan": true, "confirmOldExecutorsStopped": true}
}
func TestMediaRefreshAllLocalIntakeModesAndDiscQueueOnlyAfterWholeSuccess(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move", "disc"} {
		t.Run(mode, func(t *testing.T) {
			var f *subscriptionRunFixture
			var jobID string
			if mode == "disc" {
				var input organizationPlanInput
				f, _, _, input = organizationDiscFixture(t, "copy")
				w := mediaRefreshWire(t, f, "emby")
				mediaRefreshGrant(t, f, "2.9")
				code, plan, body := organizationPreview(t, f, input)
				if code != 200 {
					t.Fatal(code, body)
				}
				job := organizationCreateJob(t, f, input, plan)
				jobID = job.ID
				if rows, err := f.runner.mediaRefresh.queue.List(t.Context(), false, time.Now()); err != nil || len(rows) != 0 {
					t.Fatal(rows, err)
				}
				code, _, body = organizationMoveRequest(t, f, jobID, "execute")
				if code != 200 {
					t.Fatal(code, body)
				}
				rows, err := f.runner.mediaRefresh.queue.List(t.Context(), false, time.Now())
				if err != nil || len(rows) != 1 || !rows[0].Definition.Items[0].Directory || w.writes.Load() != 0 {
					t.Fatal(rows, err, w.writes.Load())
				}
			} else {
				var input organizationPlanInput
				var plan organizationPlan
				f, _, _, input, plan = organizationLinkFixture(t, mode)
				w := mediaRefreshWire(t, f, "emby")
				mediaRefreshGrant(t, f, "2.9")
				code, currentPlan, body := organizationPreview(t, f, input)
				if code != 200 {
					t.Fatal(code, body)
				}
				plan = currentPlan
				job := organizationCreateJob(t, f, input, plan)
				jobID = job.ID
				code, _, body = organizationMoveRequest(t, f, jobID, "execute")
				if code != 200 {
					t.Fatal(code, body)
				}
				if w.writes.Load() != 0 {
					t.Fatal("intake sent inline refresh")
				}
			}
			rows, err := f.runner.mediaRefresh.queue.List(t.Context(), false, time.Now())
			if err != nil || len(rows) != 1 || rows[0].Reference != jobID || len(rows[0].Definition.Items) != 1 {
				t.Fatal(rows, err)
			}
			due, err := time.Parse(time.RFC3339Nano, rows[0].Due)
			if err != nil {
				t.Fatal(err)
			}
			if pending, err := f.runner.mediaRefresh.queue.List(t.Context(), true, due.Add(-time.Nanosecond)); err != nil || len(pending) != 0 {
				t.Fatal("early dispatch", pending, err)
			}
			if err := f.runner.mediaRefresh.tick(t.Context(), due); err != nil {
				t.Fatal(err)
			}
			j, err := f.runner.mediaRefresh.queue.Get(t.Context(), rows[0].ID)
			if err != nil || j.State != "accepted" {
				t.Fatal(j, err)
			}
			if err := f.runner.mediaRefresh.tick(t.Context(), due.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			code, _, body := organizationMoveRequest(t, f, jobID, "execute")
			if code != 200 {
				t.Fatal(code, body)
			}
			rows, err = f.runner.mediaRefresh.queue.List(t.Context(), false, time.Now())
			if err != nil || len(rows) != 1 {
				t.Fatal("duplicate original event", rows, err)
			}
		})
	}
}
func TestMediaRefreshManualPreviewStableReceiptAndNoRetryAcrossProviders(t *testing.T) {
	for _, kind := range []string{"emby", "jellyfin", "plex"} {
		t.Run(kind, func(t *testing.T) {
			f := newSubscriptionRunFixture(t)
			w := mediaRefreshWire(t, f, kind)
			mediaRefreshGrant(t, f, "0")
			reads := w.reads.Load()
			status := mediaRefreshHTTP(t, f, "GET", mediaRefreshEndpoint, nil)
			if status.Code != 200 || w.reads.Load() != reads {
				t.Fatal("status contacted server", status.Code)
			}
			v := mediaRefreshPreview(t, f, "", true)
			if len(v.Plan.Targets) != 1 || w.writes.Load() != 0 {
				t.Fatal(v)
			}
			for _, grant := range []string{"confirmRead", "confirm", "confirmScan", "confirmOldExecutorsStopped"} {
				in := mediaRefreshSubmitInput(v)
				in[grant] = false
				if r := mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", in); r.Code != 400 {
					t.Fatal(grant, r.Code, r.Body.String())
				}
			}
			j := recoveryData[mediaserver.RefreshJob](t, mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", mediaRefreshSubmitInput(v)))
			if j.ID != v.ID || j.State != "pending" || w.writes.Load() != 0 {
				t.Fatal(j)
			}
			if err := f.runner.mediaRefresh.tick(t.Context(), time.Now()); err != nil {
				t.Fatal(err)
			}
			j, err := f.runner.mediaRefresh.queue.Get(t.Context(), j.ID)
			if err != nil || j.State != "accepted" || w.writes.Load() != 1 {
				t.Fatal(j, err, w.writes.Load())
			}
			before := w.reads.Load()
			repeated := mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", mediaRefreshSubmitInput(v))
			if repeated.Code != 200 || w.reads.Load() != before || w.writes.Load() != 1 {
				t.Fatal("saved stable request rescanned", repeated.Code, w.reads.Load(), before)
			}
			lookup := mediaRefreshHTTP(t, f, "GET", mediaRefreshEndpoint+"/jobs/"+j.ID, nil)
			if lookup.Code != 200 || strings.Contains(lookup.Body.String(), "private-refresh-key") || strings.Contains(lookup.Body.String(), "ServerDigest") {
				t.Fatal(lookup.Code, lookup.Body.String())
			}
		})
	}
}
func TestMediaRefreshOldSuccessfulEventsNeedNewExplicitReviewAndAreNotAutomaticallyBackfilled(t *testing.T) {
	f, _, _, input, _ := organizationCopyFixture(t)
	w := mediaRefreshWire(t, f, "emby")
	code, plan, body := organizationPreview(t, f, input)
	if code != 200 {
		t.Fatal(code, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	code, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if code != 200 {
		t.Fatal(code, body)
	}
	mediaRefreshGrant(t, f, "0")
	if err := f.runner.mediaRefresh.tick(t.Context(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := f.runner.mediaRefresh.queue.List(t.Context(), false, time.Now())
	if err != nil || len(rows) != 0 || w.writes.Load() != 0 {
		t.Fatal("old event backfilled", rows, err, w.writes.Load())
	}
	v := mediaRefreshPreview(t, f, job.ID, false)
	if v.ItemCount != 1 || v.Whole || w.writes.Load() != 0 {
		t.Fatal(v)
	}
	j := recoveryData[mediaserver.RefreshJob](t, mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", mediaRefreshSubmitInput(v)))
	if j.Source != "refresh_media_server" || j.Reference != job.ID {
		t.Fatal(j)
	}
	if r := mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/jobs/"+j.ID+"/stop", map[string]any{"confirm": true}); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if err := f.runner.mediaRefresh.tick(t.Context(), time.Now()); err != nil || w.writes.Load() != 0 {
		t.Fatal(err, w.writes.Load())
	}
}
func TestMediaRefreshWaitsForBoundEnrichmentWithoutStarvingOtherExplicitRequests(t *testing.T) {
	f, _, _, input, _ := organizationCopyFixture(t)
	if err := config.NewStore(f.path).Update(map[string]any{"media.nfo_poster": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,'{}')`, scraper.ConfigKey); err != nil {
		t.Fatal(err)
	}
	w := mediaRefreshWire(t, f, "emby")
	mediaRefreshGrant(t, f, "0")
	code, plan, body := organizationPreview(t, f, input)
	if code != 200 {
		t.Fatal(code, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	code, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if code != 200 {
		t.Fatal(code, body)
	}
	rows, err := f.runner.mediaRefresh.queue.List(t.Context(), false, time.Now())
	if err != nil || len(rows) != 1 || len(rows[0].Definition.Enrichment) != 1 {
		t.Fatal(rows, err)
	}
	now := time.Now()
	if err := f.runner.mediaRefresh.tick(t.Context(), now); err != nil || w.writes.Load() != 0 {
		t.Fatal(err, w.writes.Load())
	}
	out, err := f.runner.mediaRefresh.queue.Get(t.Context(), rows[0].ID)
	if err != nil || out.State != "pending" || !strings.Contains(out.Reason, "waiting") {
		t.Fatal(out, err)
	}
	v := mediaRefreshPreview(t, f, "", true)
	manual := recoveryData[mediaserver.RefreshJob](t, mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", mediaRefreshSubmitInput(v)))
	if err := f.runner.mediaRefresh.tick(t.Context(), time.Now()); err != nil || w.writes.Load() != 1 {
		t.Fatal("waiting source starved explicit work", err, w.writes.Load())
	}
	out, err = f.runner.mediaRefresh.queue.Get(t.Context(), manual.ID)
	if err != nil || out.State != "accepted" {
		t.Fatal(out, err)
	}
	if _, err := f.db.Exec(`UPDATE GO_SCRAPE_JOBS SET STATE='completed' WHERE JOB_ID=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.mediaRefresh.tick(t.Context(), now.Add(11*time.Second)); err != nil || w.writes.Load() != 2 {
		t.Fatal(err, w.writes.Load())
	}
}
func TestMediaRefreshClosedIntakeOrUncertainEnrichmentCannotDispatchAnOldAutomaticEvent(t *testing.T) {
	for _, changed := range []string{"intake-closed", "definition-changed", "enrichment-uncertain"} {
		t.Run(changed, func(t *testing.T) {
			f, _, _, input, _ := organizationCopyFixture(t)
			if changed == "enrichment-uncertain" {
				if err := config.NewStore(f.path).Update(map[string]any{"media.nfo_poster": true}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,'{}')`, scraper.ConfigKey); err != nil {
					t.Fatal(err)
				}
			}
			w := mediaRefreshWire(t, f, "emby")
			mediaRefreshGrant(t, f, "0")
			code, plan, body := organizationPreview(t, f, input)
			if code != 200 {
				t.Fatal(code, body)
			}
			job := organizationCreateJob(t, f, input, plan)
			code, _, body = organizationMoveRequest(t, f, job.ID, "execute")
			if code != 200 {
				t.Fatal(code, body)
			}
			switch changed {
			case "intake-closed":
				_, err := f.db.Exec(`UPDATE GO_ORGANIZATION_JOBS SET STATE='recovery_closed' WHERE ID=?`, job.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "definition-changed":
				_, err := f.db.Exec(`UPDATE GO_ORGANIZATION_JOBS SET DEFINITION=json_set(DEFINITION,'$.changed',true) WHERE ID=?`, job.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "enrichment-uncertain":
				_, err := f.db.Exec(`UPDATE GO_SCRAPE_JOBS SET STATE='partial',OUTPUTS='[{"State":"uncertain"}]' WHERE JOB_ID=?`, job.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := f.runner.mediaRefresh.tick(t.Context(), time.Now()); err != nil {
				t.Fatal(err)
			}
			rows, err := f.runner.mediaRefresh.queue.List(t.Context(), false, time.Now())
			if err != nil || len(rows) != 1 || rows[0].State != "blocked" || w.writes.Load() != 0 {
				t.Fatal(rows, err, w.writes.Load())
			}
		})
	}
}
func TestMediaRefreshLostAcknowledgementInterruptedIntentAndFailedIntentNeverReplay(t *testing.T) {
	for _, mode := range []string{"lost-response", "lost-local-receipt", "ignored-intent"} {
		t.Run(mode, func(t *testing.T) {
			f := newSubscriptionRunFixture(t)
			w := mediaRefreshWire(t, f, "emby")
			mediaRefreshGrant(t, f, "0")
			v := mediaRefreshPreview(t, f, "", true)
			j := recoveryData[mediaserver.RefreshJob](t, mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", mediaRefreshSubmitInput(v)))
			switch mode {
			case "lost-response":
				w.lost = true
			case "lost-local-receipt":
				if _, err := f.db.Exec(`CREATE TRIGGER reject_refresh_result BEFORE UPDATE ON GO_MEDIA_REFRESH_ACTIONS WHEN NEW.STATE='accepted' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
			case "ignored-intent":
				if _, err := f.db.Exec(`CREATE TRIGGER ignore_refresh_intent BEFORE UPDATE ON GO_MEDIA_REFRESH_ACTIONS WHEN NEW.STATE='intent' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
					t.Fatal(err)
				}
			}
			err := f.runner.mediaRefresh.tick(t.Context(), time.Now())
			if mode == "lost-local-receipt" && err == nil {
				t.Fatal("lost local receipt reported success")
			}
			j, err = f.runner.mediaRefresh.queue.Get(t.Context(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := map[string]string{"lost-response": "uncertain", "lost-local-receipt": "running", "ignored-intent": "blocked"}[mode]
			if j.State != expected {
				t.Fatal(j)
			}
			count := w.writes.Load()
			if mode == "ignored-intent" && count != 0 || mode != "ignored-intent" && count != 1 {
				t.Fatal("network boundary", count)
			}
			if err := f.runner.mediaRefresh.tick(t.Context(), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if w.writes.Load() != count {
				t.Fatal("replayed uncertain refresh")
			}
		})
	}
}
func TestMediaRefreshLifecycleLegacyReadPrivacyRevocationAndCorruptStateUninstall(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	w := mediaRefreshWire(t, f, "emby")
	if r := mediaRefreshHTTP(t, f, "POST", mediaRefreshPluginEndpoint+"/install", nil); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if err := f.runner.download.system.Set(t.Context(), mediaserver.RefreshPluginKey, `{"enable":true,"delay":"1.9","private_cache":"never-expose-this-cache"}`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/plugins", mediaRefreshPluginEndpoint, mediaRefreshPluginEndpoint + "/page", mediaRefreshEndpoint} {
		r := mediaRefreshHTTP(t, f, "GET", path, nil)
		if r.Code != 200 || strings.Contains(r.Body.String(), "never-expose") || w.reads.Load() != 0 {
			t.Fatal(path, r.Code, r.Body.String(), w.reads.Load())
		}
	}
	d := mediaRefreshDetail(t, f)
	for _, key := range []string{"go_confirm", "go_old_stopped", "go_scan"} {
		if d.Values[key] != false {
			t.Fatal("approval inherited", d)
		}
	}
	mediaRefreshGrant(t, f, "0")
	oldPreview := mediaRefreshPreview(t, f, "", true)
	job := recoveryData[mediaserver.RefreshJob](t, mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", mediaRefreshSubmitInput(oldPreview)))
	if _, err := f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE='["LibraryRefresh"]' WHERE KEY='UserInstalledPlugins' AND TYPE='SystemConfig'`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/plugin/list", "/api/v1/plugin/status", "/api/v1/plugin/page"} {
		r := performFormRequest(f.handler, path, f.token, url.Values{"id": {"LibraryRefresh"}})
		if r.Code != 200 || strings.Contains(r.Body.String(), "never-expose") || strings.Contains(r.Body.String(), "private-refresh") {
			t.Fatal(path, r.Code, r.Body.String())
		}
	}
	if err := config.NewStore(f.path).Update(map[string]any{"emby.api_key": "changed-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.mediaRefresh.tick(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	out, err := f.runner.mediaRefresh.queue.Get(t.Context(), job.ID)
	if err != nil || out.State != "blocked" || w.writes.Load() != 0 {
		t.Fatal(out, err, w.writes.Load())
	}
	if err := f.runner.download.system.Set(t.Context(), mediaserver.RefreshControlKey, "corrupt"); err != nil {
		t.Fatal(err)
	}
	r := mediaRefreshHTTP(t, f, "DELETE", mediaRefreshPluginEndpoint, nil)
	if r.Code != 200 {
		t.Fatal("corrupt state could not be uninstalled", r.Code, r.Body.String())
	}
}
func TestMediaRefreshFreshPlanServerIdentityAuthorityLocksAndStrictInputs(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	w := mediaRefreshWire(t, f, "plex")
	mediaRefreshGrant(t, f, "0")
	v := mediaRefreshPreview(t, f, "", true)
	w.sections = `{"MediaContainer":{"size":1,"Directory":[{"key":"2","title":"Other"}]}}`
	if r := mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", mediaRefreshSubmitInput(v)); r.Code != 409 || w.writes.Load() != 0 {
		t.Fatal(r.Code, r.Body.String())
	}
	w.identity = "different-server"
	if r := mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/preview", map[string]any{"whole": true, "confirmRead": true}); r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
	w.identity = "server-1"
	for _, raw := range []string{`{"whole":true,"confirmRead":true,"host":"https://escape.test"}`, `{"whole":true,"confirmRead":true} {}`} {
		r := nativeJSONRequest(f.handler, "POST", mediaRefreshEndpoint+"/preview", f.token, raw)
		if r.Code != 400 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if r := nativeJSONRequest(f.handler, "GET", mediaRefreshEndpoint, "", ""); r.Code != 401 {
		t.Fatal(r.Code)
	}
	viewer, err := f.runner.organizer.auth.service.IssueToken("viewer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if r := nativeJSONRequest(f.handler, "GET", mediaRefreshEndpoint, viewer, ""); r.Code != 403 {
		t.Fatal(r.Code)
	}
	guard, err := f.runner.organizer.jobs.AcquireJob(t.Context(), mediaRefreshGuard)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.mediaRefresh.tick(t.Context(), time.Now()); err == nil {
		t.Fatal("cross-process guard bypass")
	}
	guard.Close()
	if r := mediaRefreshHTTP(t, f, "GET", mediaRefreshEndpoint+"/jobs/"+strings.Repeat("f", 64), nil); r.Code != 404 {
		t.Fatal(r.Code)
	}
}
func TestMediaRefreshStopCancelsFurtherRequestsWithoutClaimingRemoteUndo(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	w := mediaRefreshWire(t, f, "plex")
	mediaRefreshGrant(t, f, "0")
	w.sections = `{"MediaContainer":{"size":2,"Directory":[{"key":"1","title":"First"},{"key":"2","title":"Second"}]}}`
	v := mediaRefreshPreview(t, f, "", true)
	j := recoveryData[mediaserver.RefreshJob](t, mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", mediaRefreshSubmitInput(v)))
	w.beforeWrite = func(*http.Request) {
		r := mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/jobs/"+j.ID+"/stop", map[string]any{"confirm": true})
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if err := f.runner.mediaRefresh.tick(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	out, err := f.runner.mediaRefresh.queue.Get(t.Context(), j.ID)
	if err != nil || out.State != "stopped" || out.Actions[0].State != "accepted" || out.Actions[1].State != "pending" || w.writes.Load() != 1 {
		t.Fatal(out, err, w.writes.Load())
	}
}
func TestMediaRefreshProductionWorkerDispatchAndShutdown(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	w := mediaRefreshWire(t, f, "emby")
	mediaRefreshGrant(t, f, "0")
	v := mediaRefreshPreview(t, f, "", true)
	j := recoveryData[mediaserver.RefreshJob](t, mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", mediaRefreshSubmitInput(v)))
	ctx, cancel := context.WithCancel(t.Context())
	wait := startMediaRefreshWorker(ctx, f.runner.mediaRefresh)
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		out, err := f.runner.mediaRefresh.queue.Get(t.Context(), j.ID)
		if err != nil {
			cancel()
			wait()
			t.Fatal(err)
		}
		if out.State == "accepted" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	out, err := f.runner.mediaRefresh.queue.Get(t.Context(), j.ID)
	if err != nil || out.State != "accepted" || w.writes.Load() != 1 || f.runner.mediaRefresh.workers.Load() != 0 {
		t.Fatal(out, err, w.writes.Load())
	}
}
func TestMediaRefreshMixedModeNamespaceCannotFallBackToPython(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	var legacy atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { legacy.Add(1); http.Error(w, "Python must not run", 500) }))
	defer fallback.Close()
	handler, err := newHandler(config.Config{ApplicationConfigPath: f.path, LegacyBackendURL: fallback.URL}, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{mediaRefreshEndpoint, mediaRefreshEndpoint + "/preview", mediaRefreshEndpoint + "/unknown", mediaRefreshPluginEndpoint, mediaRefreshPluginEndpoint + "/page", mediaRefreshPluginEndpoint + "/unknown"} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			// Modern plugin verbs not owned by this plugin have their normal mux
			// response; the dedicated refresh namespace is always reserved.
			if strings.HasPrefix(path, mediaRefreshPluginEndpoint) && method != "GET" && !strings.HasSuffix(path, "/unknown") {
				continue
			}
			r := nativeJSONRequest(handler, method, path, f.token, `{}`)
			if r.Code != 501 || legacy.Load() != 0 {
				t.Fatal(path, method, r.Code, r.Body.String(), legacy.Load())
			}
		}
	}
}
func TestMediaRefreshPlexAccountModeRequiresIndependentConsentAndCachesLoginOnlyInMemory(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	w := mediaRefreshWire(t, f, "plex")
	previous := f.transport
	var signins, discoveries atomic.Int32
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "plex.tv" {
			return previous.RoundTrip(r)
		}
		if r.URL.Scheme != "https" {
			t.Fatal("account transmission not HTTPS")
		}
		if r.URL.Path == "/users/sign_in.json" {
			signins.Add(1)
			if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("user[password]") != "private-account-password" {
				t.Fatal("legacy account form")
			}
			return jsonResponse(r, `{"user":{"authToken":"private-account-token"}}`), nil
		}
		discoveries.Add(1)
		if r.Header.Get("X-Plex-Token") != "private-account-token" {
			t.Fatal("account discovery token")
		}
		return jsonResponse(r, `<MediaContainer size="1"><Device name="Selected" provides="server" clientIdentifier="server-1" accessToken="private-refresh-key"><Connection protocol="https" uri="https://refresh.test/prefix"/></Device></MediaContainer>`), nil
	})
	if err := config.NewStore(f.path).Update(map[string]any{"plex.host": "", "plex.token": "", "plex.username": "fixture-user", "plex.password": "private-account-password", "plex.servername": "Selected"}); err != nil {
		t.Fatal(err)
	}
	var err error
	f.handler, err = buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport, &f.runner)
	if err != nil {
		t.Fatal(err)
	}
	if r := mediaRefreshHTTP(t, f, "POST", mediaRefreshPluginEndpoint+"/install", nil); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	d := mediaRefreshDetail(t, f)
	if d.Values["go_plex_login"] != false || d.Values["go_plex_http"] != false || signins.Load() != 0 {
		t.Fatal("account mode auto-adopted", d)
	}
	values := map[string]any{"enable": true, "delay": "0", "go_confirm": true, "go_old_stopped": true, "go_scan": true}
	in := map[string]any{"fingerprint": d.Meta.Fingerprint, "values": values}
	if r := mediaRefreshHTTP(t, f, "PUT", mediaRefreshPluginEndpoint, in); r.Code != 400 || signins.Load() != 0 {
		t.Fatal("account login lacked consent", r.Code, signins.Load())
	}
	values["go_plex_login"] = true
	if r := mediaRefreshHTTP(t, f, "PUT", mediaRefreshPluginEndpoint, in); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if signins.Load() != 1 || discoveries.Load() != 1 {
		t.Fatal(signins.Load(), discoveries.Load())
	}
	v := mediaRefreshPreview(t, f, "", true)
	j := recoveryData[mediaserver.RefreshJob](t, mediaRefreshHTTP(t, f, "POST", mediaRefreshEndpoint+"/submit", mediaRefreshSubmitInput(v)))
	if err := f.runner.mediaRefresh.tick(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	out, err := f.runner.mediaRefresh.queue.Get(t.Context(), j.ID)
	if err != nil || out.State != "accepted" || w.writes.Load() != 1 || signins.Load() != 1 {
		t.Fatal(out, err, w.writes.Load(), signins.Load())
	}
	r := mediaRefreshHTTP(t, f, "GET", mediaRefreshEndpoint+"/jobs/"+j.ID, nil)
	for _, private := range []string{"private-account-token", "private-account-password", "private-refresh-key", "PlexAccount"} {
		if strings.Contains(r.Body.String(), private) {
			t.Fatal("account credential in receipt", private)
		}
	}
}
