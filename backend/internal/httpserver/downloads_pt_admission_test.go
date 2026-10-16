package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
)

func reviewAdmissionHTTP(t *testing.T, f *manualDownloadFixture, enabled bool) ptAdmissionReview {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"downloaderId": 1, "enabled": enabled})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/pt-admission/review", f.token, string(raw))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response struct{ Data ptAdmissionReview }
	if json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal(w.Body.String())
	}
	return response.Data
}
func applyAdmissionHTTP(f *manualDownloadFixture, r ptAdmissionReview, all bool) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(map[string]any{"downloaderId": r.DownloaderID, "enabled": r.Enabled, "fingerprint": r.Fingerprint, "confirm": true, "confirmOldExecutorsStopped": all, "confirmUnknownHold": all, "confirmUnlimitedSeeding": all})
	return nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/pt-admission/apply", f.token, string(raw))
}
func enableAdmission(t *testing.T, f *manualDownloadFixture) {
	t.Helper()
	if w := applyAdmissionHTTP(f, reviewAdmissionHTTP(t, f, true), true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func ptResource() externalindexer.Resource {
	seconds, ratio := int64(900), 1.5
	return externalindexer.Resource{Title: "Movie", Indexer: "Tracker", DownloadURL: testMagnet, MinimumSeedTime: &seconds, MinimumRatio: &ratio}
}
func submitPTResource(t *testing.T, w *profileWire, r externalindexer.Resource) *httptest.ResponseRecorder {
	t.Helper()
	ids, err := w.f.runner.download.service.resources.Put("0:admin", []externalindexer.Resource{r})
	if err != nil {
		t.Fatal(err)
	}
	return nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/resource", w.f.token, `{"resourceId":"`+ids[0]+`","setting":"`+w.settingID+`"}`)
}
func zeroPTStopLimits(t *testing.T, w *profileWire) {
	t.Helper()
	id, _ := strconv.ParseInt(w.settingID, 10, 64)
	s, err := w.f.store.GetSetting(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	s.RatioLimit = 0
	s.SeedingTimeLimit = 0
	if _, err := w.f.store.UpsertSetting(t.Context(), s); err != nil {
		t.Fatal(err)
	}
}
func newPTWire(t *testing.T, modern, paused bool) *profileWire {
	t.Helper()
	w := transmissionProfileFixture(t, modern, paused)
	zeroPTStopLimits(t, w)
	w.f.absent = true
	w.f.afterAdd = func() {
		w.f.absent = false
		p := getProfile(t, w)
		policies, _, err := w.f.store.SeedingPolicies(t.Context(), 1, w.f.hash)
		if err != nil || len(policies) != 1 || p.Definition.SeedingPolicyID != policies[0].ID {
			t.Fatal("source was not durable before add", p, policies, err)
		}
	}
	return w
}

func TestPTAdmissionPositiveSourceCompletesBothProtocolsWithFrozenObligations(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, paused := range []bool{false, true} {
			t.Run(strconv.FormatBool(modern)+"/paused="+strconv.FormatBool(paused), func(t *testing.T) {
				w := newPTWire(t, modern, paused)
				enableAdmission(t, w.f)
				resp := submitPTResource(t, w, ptResource())
				if resp.Code != 200 {
					t.Fatal(resp.Code, resp.Body.String())
				}
				if w.f.adds != 1 || w.sets != 1 || w.starts != map[bool]int{true: 0, false: 1}[paused] || w.state.RatioMode != 2 || w.state.IdleMode != 2 {
					t.Fatal(w.f.adds, w.sets, w.starts, w.state)
				}
				p := getProfile(t, w)
				if p.State != "ready" || p.Definition.SeedingPolicyID == "" || p.Definition.Limits.Ratio != 0 || p.Definition.Limits.IdleMinutes != 0 {
					t.Fatal(p)
				}
				policies, _, err := w.f.store.SeedingPolicies(t.Context(), 1, w.f.hash)
				if err != nil || len(policies) != 1 || policies[0].Requirements.HR != nil || *policies[0].Requirements.Seconds != 900 || *policies[0].Requirements.Ratio != 1.5 {
					t.Fatal(policies, err)
				}
				assertManualCommitted(t, w.f, 1)
				w.f.rebuild(t)
				if resp := manualVerify(w.f, manualReceipts(t, w.f)[0].ID); resp.Code != 200 {
					t.Fatal(resp.Code, resp.Body.String())
				}
				for _, action := range []string{"stop", "remove"} {
					resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/"+w.f.hash+"/"+action, w.f.token, `{}`)
					if resp.Code != 409 {
						t.Fatal(action, resp.Code, resp.Body.String())
					}
				}
				if resp := w.f.magnet(); resp.Code != 409 || w.f.adds != 1 {
					t.Fatal("raw magnet bypassed source scope", resp.Code, w.f.adds)
				}
			})
		}
	}
}

func TestPTAdmissionRefusesMissingConsentConflictingLimitsHRAndExistingTasks(t *testing.T) {
	for _, mode := range []string{"disabled", "conflicting limits", "HR marked", "existing task", "changed config", "revoked", "legacy mode"} {
		t.Run(mode, func(t *testing.T) {
			w := newPTWire(t, false, false)
			if mode != "disabled" {
				enableAdmission(t, w.f)
			}
			r := ptResource()
			switch mode {
			case "conflicting limits":
				id, _ := strconv.ParseInt(w.settingID, 10, 64)
				s, _ := w.f.store.GetSetting(t.Context(), id)
				s.RatioLimit = 150
				_, _ = w.f.store.UpsertSetting(t.Context(), s)
			case "HR marked":
				yes := true
				r.HitAndRun = &yes
			case "existing task":
				w.f.absent = false
			case "changed config":
				item, _ := w.f.store.Get(t.Context(), 1)
				item.Name = "changed"
				_, _ = w.f.store.Upsert(t.Context(), item)
			case "revoked":
				if resp := applyAdmissionHTTP(w.f, reviewAdmissionHTTP(t, w.f, false), false); resp.Code != 200 {
					t.Fatal(resp.Code, resp.Body.String())
				}
			case "legacy mode":
				w.f.runner.download.service.pureGo = false
			}
			if mode == "legacy mode" {
				s := w.f.runner.download.service
				item, _ := s.downloaders.Get(t.Context(), 1)
				if _, err := s.preparePTSource(t.Context(), item, r, downloadAddOptions{}); !errors.Is(err, errPTAdmissionBlocked) {
					t.Fatal(err)
				}
				return
			}
			resp := submitPTResource(t, w, r)
			if resp.Code == 200 || w.f.adds != 0 || w.sets != 0 || w.starts != 0 {
				t.Fatal(mode, resp.Code, resp.Body.String(), w.f.adds, w.sets, w.starts)
			}
			policies, _, err := w.f.store.SeedingPolicies(t.Context(), 1, w.f.hash)
			if err != nil || len(policies) != 0 {
				t.Fatal("rejected before admission but recorded obligations", policies, err)
			}
		})
	}
}

func TestPTAdmissionLostAddNeverReplaysAndSourceRequiredForRecovery(t *testing.T) {
	w := newPTWire(t, true, false)
	enableAdmission(t, w.f)
	w.f.lost = true
	resp := submitPTResource(t, w, ptResource())
	if resp.Code != 409 || w.f.adds != 1 || w.sets != 0 || w.starts != 0 {
		t.Fatal(resp.Code, resp.Body.String(), w.f.adds, w.sets, w.starts)
	}
	w.f.lost = false
	w.f.rebuild(t)
	if resp := w.f.magnet(); resp.Code != 409 || w.f.adds != 1 {
		t.Fatal(resp.Code, w.f.adds)
	}
	p := getProfile(t, w)
	status, review, body := reviewProfileHTTP(t, w, p.ID)
	if status != 200 || review.Next != "configure" {
		t.Fatal(status, body)
	}
	if resp := recoverProfileHTTP(w, review, true, false); resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	status, review, body = reviewProfileHTTP(t, w, p.ID)
	if status != 200 || review.Next != "start" {
		t.Fatal(status, body)
	}
	if resp := recoverProfileHTTP(w, review, false, true); resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	if w.f.adds != 1 || w.sets != 1 || w.starts != 1 {
		t.Fatal(w.f.adds, w.sets, w.starts)
	}
	if resp := manualVerify(w.f, manualReceipts(t, w.f)[0].ID); resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	if _, err := w.f.db.Exec(`DELETE FROM GO_SEEDING_POLICIES`); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := reviewProfileHTTP(t, w, p.ID); status != 409 {
		t.Fatal("source-less profile recoverable", status)
	}
	if err := w.f.runner.download.service.checkTransmissionProfileReady(t.Context(), mustDownloader(t, w.f), w.f.hash); !errors.Is(err, downloaderconfig.ErrProfilePending) {
		t.Fatal("source-less profile accepted", err)
	}
}
func mustDownloader(t *testing.T, f *manualDownloadFixture) downloaderconfig.Downloader {
	t.Helper()
	item, err := f.store.Get(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestPTAdmissionGrantReviewRequiresSeparateConsentsAndRejectsStaleScope(t *testing.T) {
	w := newPTWire(t, false, false)
	r := reviewAdmissionHTTP(t, w.f, true)
	if resp := applyAdmissionHTTP(w.f, r, false); resp.Code != 409 {
		t.Fatal(resp.Code)
	}
	item := mustDownloader(t, w.f)
	item.Name = "changed"
	if _, err := w.f.store.Upsert(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if resp := applyAdmissionHTTP(w.f, r, true); resp.Code != 409 {
		t.Fatal("stale config accepted", resp.Code)
	}
	enableAdmission(t, w.f)
	r = reviewAdmissionHTTP(t, w.f, false)
	if resp := applyAdmissionHTTP(w.f, r, false); resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	if resp := applyAdmissionHTTP(w.f, r, false); resp.Code != 409 {
		t.Fatal("stale grant replay accepted", resp.Code)
	}
	created := performFormRequest(w.f.handler, "/api/v1/user/manage", w.f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"下载管理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, w.f.handler, "viewer", "strong-password")
	for _, path := range []string{"/api/v1/downloads/pt-admission/review", "/api/v1/downloads/pt-admission/apply"} {
		resp := nativeJSONRequest(w.f.handler, "POST", path, viewer, `{"downloaderId":1}`)
		if resp.Code != 403 {
			t.Fatal(resp.Code, resp.Body.String())
		}
	}
	resp := nativeJSONRequest(w.f.handler, "GET", "/api/v1/downloads/pt-admission?downloaderId=1", w.f.token, "")
	if resp.Code != 200 || strings.Contains(resp.Body.String(), "private-password") || strings.Contains(resp.Body.String(), bus.Hash(item)) {
		t.Fatal(resp.Code, resp.Body.String())
	}
}

func subscriptionPTWire(t *testing.T, feed, modern bool) *profileWire {
	t.Helper()
	var x *subscriptionRunFixture
	if feed {
		x = newSubscriptionFeedFixture(t, false, `{"parse":"Y"}`, func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/rss" {
				return jsonResponse(r, `<rss xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>`+subscriptionFeedItem("Movie.2026.1080p.WEB-DL", testMagnet, "https://tracker.test/details", `<torznab:attr name="minimumseedtime" value="900"/><torznab:attr name="minimumratio" value="1.5"/>`)+`</channel></rss>`), nil
			}
			return jsonResponse(r, `<html><h1>Movie.2026.1080p.WEB-DL</h1><span class="free"/></html>`), nil
		})
	} else {
		x = newSubscriptionRunFixture(t)
	}
	catalog := x.runner.search.planner.search.catalogPath
	dbPath := filepath.Join(filepath.Dir(x.path), "user.db")
	store, err := downloaderconfig.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	item, err := store.Get(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	item.Type = "transmission"
	item.Config = `{"host":"client.local","port":9091}`
	if _, err := store.Upsert(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	f := &manualDownloadFixture{token: x.token, path: dbPath, hash: magnetInfoHash(testMagnet), kind: "transmission", db: x.db, store: store}
	base := profileSourceTransport(t, modern, f.hash, x.transport)
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "client.local" && r.Header.Get("X-Transmission-Session-Id") != "" && f.adds == 0 {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			var rpc struct{ Method string }
			_ = json.Unmarshal(body, &rpc)
			if rpc.Method == "torrent-get" || rpc.Method == "torrent_get" {
				env := map[string]any{"result": "success", "arguments": map[string]any{"torrents": []any{}}}
				if modern {
					env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"torrents": []any{}}}
				}
				raw, _ := json.Marshal(env)
				return jsonResponse(r, string(raw)), nil
			}
		}
		return base.RoundTrip(r)
	})
	w := transmissionProfileForFixture(t, f, modern, false)
	zeroPTStopLimits(t, w)
	f.runner.search.planner.search.catalogPath = catalog
	// Rebuilding the handler must preserve the RSS detail-verification catalog,
	// which belongs to recognition, not merely the interactive search adapter.
	f.runner.recognition.service.catalogPath = catalog
	if _, err := f.db.Exec(`UPDATE RSS_MOVIES SET DOWNLOAD_SETTING=? WHERE ID=1`, w.settingID); err != nil {
		t.Fatal(err)
	}
	f.afterAdd = func() {
		p := getProfile(t, w)
		policies, _, err := f.store.SeedingPolicies(t.Context(), 1, f.hash)
		if err != nil || len(policies) != 1 || p.Definition.SeedingPolicyID != policies[0].ID {
			t.Fatal("subscription source not durable before add", p, policies, err)
		}
	}
	enableAdmission(t, f)
	return w
}

func TestPTAdmissionSubscriptionAndRSSCommitSourceBoundProfiles(t *testing.T) {
	for _, feed := range []bool{false, true} {
		for _, lost := range []bool{false, true} {
			t.Run("feed="+strconv.FormatBool(feed)+"/lost="+strconv.FormatBool(lost), func(t *testing.T) {
				w := subscriptionPTWire(t, feed, !feed)
				w.loseSet = lost
				var status int
				var body string
				if feed {
					resp := performFormRequest(w.f.handler, "/api/v1/subscriptions/rss/run", w.f.token, nil)
					status, body = resp.Code, resp.Body.String()
				} else {
					candidate := candidateFixture(t, "Movie.2026.1080p.WEB-DL", 1, nil)
					candidate.resource = ptResource()
					candidate.resource.Title = "Movie.2026.1080p.WEB-DL"
					provider := func(context.Context, map[string]any, string, string) ([]identifiedSubscriptionResource, *recognitionFailure) {
						return []identifiedSubscriptionResource{candidate}, nil
					}
					result, failure := w.f.runner.search.executeResources(t.Context(), "MOV", 1, "", provider)
					status = 200
					body = string(mustJSON(t, result))
					if failure != nil {
						status = failure.status
						body = failure.message
					}
				}
				if lost {
					if w.f.adds != 1 || w.starts != 0 {
						t.Fatal(status, body, w.f.adds, w.starts)
					}
					p := getProfile(t, w)
					code, review, text := reviewProfileHTTP(t, w, p.ID)
					if code != 200 || review.Next != "start" {
						t.Fatal(code, text)
					}
					if resp := recoverProfileHTTP(w, review, false, true); resp.Code != 200 {
						t.Fatal(resp.Code, resp.Body.String())
					}
					resp := performFormRequest(w.f.handler, "/api/v1/subscriptions/MOV/1/reconcile", w.f.token, nil)
					status, body = resp.Code, resp.Body.String()
				}
				if status != 200 || w.f.adds != 1 || w.sets != 1 || w.starts != 1 {
					t.Fatal(status, body, w.f.adds, w.sets, w.starts)
				}
				var count int
				if err := w.f.db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&count); err != nil || count != 1 {
					t.Fatal("source history not committed", count, err, body)
				}
				policies, _, err := w.f.store.SeedingPolicies(t.Context(), 1, w.f.hash)
				if err != nil || len(policies) != 1 || *policies[0].Requirements.Seconds != 900 || *policies[0].Requirements.Ratio != 1.5 {
					t.Fatal(policies, err)
				}
				if policies[0].Requirements.HR != nil {
					t.Fatal("page observation became tracker account clearance", policies)
				}
			})
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
