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

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

type qbWire struct {
	f                                                             *manualDownloadFixture
	version, setting                                              string
	state                                                         map[string]any
	sets, starts                                                  int
	existing, ignoreAdd, ignoreSet, loseSet, loseStart, duplicate bool
}

func qbWireForFixture(t *testing.T, f *manualDownloadFixture, version string, paused bool) *qbWire {
	t.Helper()
	w := &qbWire{f: f, version: version}
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "client.local" {
			return base.RoundTrip(r)
		}
		if strings.HasSuffix(r.URL.Path, "auth/login") {
			return jsonResponse(r, "Ok."), nil
		}
		if strings.HasSuffix(r.URL.Path, "app/version") {
			return jsonResponse(r, w.version), nil
		}
		if strings.HasSuffix(r.URL.Path, "torrents/info") {
			if r.Method != "GET" || r.URL.Query().Get("hashes") != f.hash || r.URL.Query().Get("filter") != "all" {
				t.Fatal("widened qB scope", r.URL)
			}
			if f.adds == 0 && !w.existing {
				return jsonResponse(r, `[]`), nil
			}
			if w.state == nil {
				w.state = map[string]any{"hash": f.hash, "state": "downloading"}
			}
			raw, _ := json.Marshal([]any{w.state})
			return jsonResponse(r, string(raw)), nil
		}
		if strings.HasSuffix(r.URL.Path, "torrents/add") {
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
				if err := r.ParseMultipartForm(8 << 20); err != nil {
					t.Fatal(err)
				}
				file, _, err := r.FormFile("torrents")
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(file)
				file.Close()
				if string(body) != string(testTorrent) {
					t.Fatal("wrong torrent payload")
				}
			} else if r.ParseForm() != nil || !validMagnet(r.Form.Get("urls")) {
				t.Fatal("wrong magnet payload")
			}
			pause, other := "stopped", "paused"
			if strings.HasPrefix(w.version, "v4") {
				pause, other = other, pause
			}
			if r.Form.Get(pause) != "true" || r.Form.Get(other) != "" || r.Form.Get("autoTMM") != "false" || r.Form.Get("ratioLimit") != "-1" || r.Form.Get("seedingTimeLimit") != "-1" || r.Form.Get("inactiveSeedingTimeLimit") != "-1" {
				t.Fatal(r.Form)
			}
			p := getQbProfile(t, w)
			policies, _, err := f.store.SeedingPolicies(t.Context(), 1, f.hash)
			if err != nil || len(policies) != 1 || p.Definition.SeedingPolicyID != policies[0].ID || p.State != "add_pending" {
				t.Fatal("missing durable intent before add", p, policies, err)
			}
			state := "stoppedDL"
			if strings.HasPrefix(w.version, "v4") {
				state = "pausedDL"
			}
			tags := r.Form.Get("tags")
			if w.duplicate {
				tags = "external"
			}
			w.state = map[string]any{"hash": f.hash, "state": state, "save_path": "/downloads", "category": r.Form.Get("category"), "tags": tags, "auto_tmm": false, "force_start": false, "up_limit": 10240, "dl_limit": 20480, "ratio_limit": -1, "seeding_time_limit": -1, "inactive_seeding_time_limit": -1, "max_ratio": -1, "max_seeding_time": -1, "max_inactive_seeding_time": -1, "share_limit_action": "Default", "progress": 0, "amount_left": 100}
			if strings.HasPrefix(w.version, "v4") || strings.HasPrefix(w.version, "v5.0.") || strings.HasPrefix(w.version, "v5.1.") {
				delete(w.state, "share_limit_action")
			}
			if w.ignoreAdd {
				for _, key := range []string{"ratio_limit", "seeding_time_limit", "inactive_seeding_time_limit", "max_ratio", "max_seeding_time", "max_inactive_seeding_time"} {
					w.state[key] = -2
				}
			}
			f.adds++
			if f.afterAdd != nil {
				f.afterAdd()
			}
			if f.lost {
				return nil, errors.New("add accepted then response lost")
			}
			return jsonResponse(r, "Ok."), nil
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("hashes") != f.hash {
			t.Fatal("widened qB mutation", r.URL, r.Form)
		}
		p := getQbProfile(t, w)
		if strings.HasSuffix(r.URL.Path, "setShareLimits") {
			expectedAction := ""
			if v, ok := w.state["share_limit_action"].(string); ok {
				expectedAction = v
			}
			if p.State != "configuring" || r.Form.Get("ratioLimit") != "-1" || r.Form.Get("seedingTimeLimit") != "-1" || r.Form.Get("inactiveSeedingTimeLimit") != "-1" || r.Form.Get("shareLimitAction") != expectedAction {
				t.Fatal("missing settings intent", p, r.Form)
			}
			w.sets++
			if !w.ignoreSet {
				for _, key := range []string{"ratio_limit", "seeding_time_limit", "inactive_seeding_time_limit", "max_ratio", "max_seeding_time", "max_inactive_seeding_time"} {
					w.state[key] = -1
				}
			}
			if w.loseSet {
				return nil, errors.New("lost setting acknowledgment")
			}
			return jsonResponse(r, ""), nil
		}
		start := "/api/v2/torrents/start"
		if strings.HasPrefix(w.version, "v4") {
			start = "/api/v2/torrents/resume"
		}
		if r.URL.Path != start || p.State != "starting" && p.State != "ready" {
			t.Fatal("wrong startup scope", r.URL, p.State)
		}
		w.starts++
		w.state["state"] = "downloading"
		if w.loseStart {
			return nil, errors.New("lost start acknowledgment")
		}
		return jsonResponse(r, ""), nil
	})
	f.rebuild(t)
	s, err := f.store.UpsertSetting(t.Context(), downloaderconfig.DownloadSetting{Name: "PT QB", DownloaderID: "1", Category: "pt", Tags: "user", Paused: map[bool]int{true: 1}[paused], UploadLimit: 10, DownloadLimit: 20})
	if err != nil {
		t.Fatal(err)
	}
	w.setting = strconv.FormatInt(s.ID, 10)
	return w
}
func newQbWire(t *testing.T, version string, paused bool) *qbWire {
	t.Helper()
	return qbWireForFixture(t, manualHTTPFixture(t, "qbittorrent", false), version, paused)
}
func getQbProfile(t *testing.T, w *qbWire) downloaderconfig.QbProfile {
	t.Helper()
	rows, err := w.f.store.QbProfiles(t.Context(), 1)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	return rows[0]
}
func (w *qbWire) submit(t *testing.T, torrent bool) *httptest.ResponseRecorder {
	t.Helper()
	r := ptResource()
	if torrent {
		meta, err := torrentmeta.Parse(testTorrent)
		if err != nil {
			t.Fatal(err)
		}
		w.f.hash = meta.InfoHash
		r.DownloadURL = "https://tracker.test/torrent?passkey=private"
		if _, err := w.f.runner.download.service.sites.Upsert(t.Context(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.test", Cookie: "private-cookie"}); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := w.f.runner.download.service.resources.Put("0:admin", []externalindexer.Resource{r})
	if err != nil {
		t.Fatal(err)
	}
	return nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/resource", w.f.token, `{"resourceId":"`+ids[0]+`","setting":"`+w.setting+`"}`)
}
func qbReviewHTTP(t *testing.T, w *qbWire) qbProfileReview {
	t.Helper()
	p := getQbProfile(t, w)
	resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/qbittorrent-profiles/"+p.ID+"/review", w.f.token, `{}`)
	if resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	var envelope struct{ Data qbProfileReview }
	if json.Unmarshal(resp.Body.Bytes(), &envelope) != nil {
		t.Fatal(resp.Body.String())
	}
	return envelope.Data
}
func qbRecoverHTTP(w *qbWire, r qbProfileReview, settings, start bool) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(map[string]any{"fingerprint": r.Fingerprint, "confirm": true, "confirmSettings": settings, "confirmStart": start})
	return nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/qbittorrent-profiles/"+r.Profile.ID+"/recover", w.f.token, string(raw))
}

func TestQbPTProfilesActualSourceAddBothProtocolsAndTorrentPayload(t *testing.T) {
	for _, version := range []string{"v4.6.7", "v5.0.2", "v5.1.0", "v5.2.3"} {
		for _, paused := range []bool{false, true} {
			t.Run(version+"/paused="+strconv.FormatBool(paused), func(t *testing.T) {
				w := newQbWire(t, version, paused)
				enableAdmission(t, w.f)
				w.f.afterAdd = func() {
					resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/"+w.f.hash+"/start", w.f.token, `{}`)
					if resp.Code != 409 {
						t.Fatal("startup crossed source guard", resp.Code)
					}
				}
				resp := w.submit(t, version == "v5.2.3")
				if resp.Code != 200 || w.f.adds != 1 || w.sets != 1 || w.starts != map[bool]int{true: 0, false: 1}[paused] {
					t.Fatal(resp.Code, resp.Body.String(), w.f.adds, w.sets, w.starts)
				}
				p := getQbProfile(t, w)
				if p.State != "ready" || !strings.HasPrefix(p.Fingerprint, "qb:") {
					t.Fatal(p)
				}
				assertManualCommitted(t, w.f, 1)
				w.f.rebuild(t)
				if resp := manualVerify(w.f, manualReceipts(t, w.f)[0].ID); resp.Code != 200 {
					t.Fatal(resp.Code, resp.Body.String())
				}
				for _, action := range []string{"stop", "remove"} {
					resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/"+w.f.hash+"/"+action, w.f.token, `{}`)
					if resp.Code != 409 {
						t.Fatal(resp.Code, resp.Body.String())
					}
				}
			})
		}
	}
}

func TestQbPTProfilesLostResponsesRecoverOnlyObservedNextStep(t *testing.T) {
	for _, lost := range []string{"add", "set", "start"} {
		t.Run(lost, func(t *testing.T) {
			w := newQbWire(t, "v5.2.3", false)
			enableAdmission(t, w.f)
			w.f.lost = lost == "add"
			w.loseSet = lost == "set"
			w.loseStart = lost == "start"
			if resp := w.submit(t, false); resp.Code != 409 {
				t.Fatal(resp.Code, resp.Body.String())
			}
			w.f.lost = false
			w.loseSet = false
			w.loseStart = false
			w.f.rebuild(t)
			if resp := w.f.magnet(); resp.Code != 409 || w.f.adds != 1 {
				t.Fatal("raw request repeated add", resp.Code, w.f.adds)
			}
			r := qbReviewHTTP(t, w)
			expected := "start"
			if lost == "start" {
				expected = "acknowledge"
			}
			if r.Next != expected {
				t.Fatal(r.Next)
			}
			if resp := qbRecoverHTTP(w, r, false, r.Next == "start"); resp.Code != 200 {
				t.Fatal(resp.Code, resp.Body.String())
			}
			if resp := manualVerify(w.f, manualReceipts(t, w.f)[0].ID); resp.Code != 200 {
				t.Fatal(resp.Code, resp.Body.String())
			}
			if w.f.adds != 1 || w.starts != 1 || w.sets != map[string]int{"add": 0, "set": 1, "start": 1}[lost] {
				t.Fatal(w.f.adds, w.sets, w.starts)
			}
		})
	}
}

func TestQbPTProfilesRefuseUnownedUnknownScopeAndRepeatedUncertainWrites(t *testing.T) {
	for _, mode := range []string{"existing", "duplicate", "unknown version", "lost ineffective set", "missing field", "changed config"} {
		t.Run(mode, func(t *testing.T) {
			w := newQbWire(t, "v5.2.3", false)
			enableAdmission(t, w.f)
			switch mode {
			case "existing":
				w.existing = true
			case "duplicate":
				w.duplicate = true
			case "unknown version":
				w.version = "v6.0.0"
			case "lost ineffective set":
				w.ignoreAdd = true
				w.ignoreSet = true
				w.loseSet = true
			case "missing field":
				w.f.afterAdd = func() { delete(w.state, "max_inactive_seeding_time") }
			case "changed config":
				w.f.afterAdd = func() {
					item := mustDownloader(t, w.f)
					item.Name = "changed"
					if _, err := w.f.store.Upsert(t.Context(), item); err != nil {
						t.Fatal(err)
					}
				}
			}
			resp := w.submit(t, false)
			if resp.Code == 200 || w.starts != 0 {
				t.Fatal(mode, resp.Code, resp.Body.String(), w.starts)
			}
			if mode == "existing" || mode == "unknown version" {
				if w.f.adds != 0 {
					t.Fatal(w.f.adds)
				}
				return
			}
			p := getQbProfile(t, w)
			resp = nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/qbittorrent-profiles/"+p.ID+"/review", w.f.token, `{}`)
			if resp.Code != 409 {
				t.Fatal("uncertain write or unowned task became recoverable", resp.Code, resp.Body.String())
			}
		})
	}
}

func TestQbPTProfilesSourceReferenceStaleReviewAndSeparateConsent(t *testing.T) {
	w := newQbWire(t, "v4.6.7", false)
	enableAdmission(t, w.f)
	w.f.lost = true
	w.ignoreAdd = true
	if resp := w.submit(t, false); resp.Code != 409 {
		t.Fatal(resp.Code)
	}
	w.f.lost = false
	r := qbReviewHTTP(t, w)
	if r.Next != "configure" {
		t.Fatal(r.Next)
	}
	if resp := qbRecoverHTTP(w, r, false, false); resp.Code != 409 || w.sets != 0 {
		t.Fatal(resp.Code, w.sets)
	}
	w.state["tags"] = w.state["tags"].(string) + ",user-edit"
	if resp := qbRecoverHTTP(w, r, true, false); resp.Code != 409 || w.sets != 0 {
		t.Fatal("stale review accepted", resp.Code, w.sets)
	}
	r = qbReviewHTTP(t, w)
	if resp := qbRecoverHTTP(w, r, true, false); resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	r = qbReviewHTTP(t, w)
	if r.Next != "start" {
		t.Fatal(r.Next)
	}
	if resp := qbRecoverHTTP(w, r, false, false); resp.Code != 409 || w.starts != 0 {
		t.Fatal(resp.Code, w.starts)
	}
	if _, err := w.f.db.Exec(`DELETE FROM GO_SEEDING_POLICIES`); err != nil {
		t.Fatal(err)
	}
	if resp := qbRecoverHTTP(w, r, false, true); resp.Code != 409 || w.starts != 0 {
		t.Fatal("source-less startup accepted", resp.Code, w.starts)
	}
	if resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/"+w.f.hash+"/start", w.f.token, `{}`); resp.Code != 409 || w.starts != 0 {
		t.Fatal("generic startup bypassed missing source", resp.Code, w.starts)
	}
}

func TestQbPTProfilesSubscriptionAndRSSCommitAndReconcileTypedSourceProof(t *testing.T) {
	for _, feed := range []bool{false, true} {
		for _, lost := range []bool{false, true} {
			t.Run("feed="+strconv.FormatBool(feed)+"/lost="+strconv.FormatBool(lost), func(t *testing.T) {
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
				catalog := x.runner.recognition.service.catalogPath
				dbPath := filepath.Join(filepath.Dir(x.path), "user.db")
				store, err := downloaderconfig.Open(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				item, err := store.Get(t.Context(), 1)
				if err != nil {
					t.Fatal(err)
				}
				item.Type = "qbittorrent"
				item.Config = `{"host":"client.local","port":8080}`
				if _, err := store.Upsert(t.Context(), item); err != nil {
					t.Fatal(err)
				}
				f := &manualDownloadFixture{token: x.token, path: dbPath, hash: magnetInfoHash(testMagnet), kind: "qbittorrent", db: x.db, store: store, transport: x.transport}
				w := qbWireForFixture(t, f, "v5.2.3", false)
				f.runner.recognition.service.catalogPath = catalog
				w.loseSet = lost
				if _, err := f.db.Exec(`UPDATE RSS_MOVIES SET DOWNLOAD_SETTING=? WHERE ID=1`, w.setting); err != nil {
					t.Fatal(err)
				}
				enableAdmission(t, f)
				var status int
				var body string
				if feed {
					resp := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, nil)
					status, body = resp.Code, resp.Body.String()
				} else {
					candidate := candidateFixture(t, "Movie.2026.1080p.WEB-DL", 1, nil)
					candidate.resource = ptResource()
					candidate.resource.Title = "Movie.2026.1080p.WEB-DL"
					provider := func(context.Context, map[string]any, string, string) ([]identifiedSubscriptionResource, *recognitionFailure) {
						return []identifiedSubscriptionResource{candidate}, nil
					}
					result, failure := f.runner.search.executeResources(t.Context(), "MOV", 1, "", provider)
					status = 200
					body = string(mustJSON(t, result))
					if failure != nil {
						status = failure.status
						body = failure.message
					}
				}
				if lost {
					if f.adds != 1 || w.starts != 0 {
						t.Fatal(status, body, f.adds, w.starts)
					}
					resp := performFormRequest(f.handler, "/api/v1/subscriptions/MOV/1/reconcile", f.token, nil)
					if resp.Code != 409 {
						t.Fatal("unready source committed", resp.Code, resp.Body.String())
					}
					r := qbReviewHTTP(t, w)
					if r.Next != "start" {
						t.Fatal(r.Next)
					}
					if resp := qbRecoverHTTP(w, r, false, true); resp.Code != 200 {
						t.Fatal(resp.Code, resp.Body.String())
					}
					resp = performFormRequest(f.handler, "/api/v1/subscriptions/MOV/1/reconcile", f.token, nil)
					status, body = resp.Code, resp.Body.String()
				}
				if status != 200 || f.adds != 1 || w.starts != 1 || w.sets != 1 {
					t.Fatal(status, body, f.adds, w.starts, w.sets)
				}
				var count int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&count); err != nil || count != 1 {
					t.Fatal(count, err, body)
				}
				p := getQbProfile(t, w)
				if p.State != "ready" || !strings.HasPrefix(p.Fingerprint, "qb:") {
					t.Fatal(p)
				}
			})
		}
	}
}

func TestQbPTProfilesRequireCurrentAdministratorAndFreshScope(t *testing.T) {
	w := newQbWire(t, "v5.2.3", true)
	enableAdmission(t, w.f)
	if resp := w.submit(t, false); resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	created := performFormRequest(w.f.handler, "/api/v1/user/manage", w.f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"下载管理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, w.f.handler, "viewer", "strong-password")
	p := getQbProfile(t, w)
	for _, path := range []string{"/api/v1/downloads/qbittorrent-profiles", "/api/v1/downloads/qbittorrent-profiles/" + p.ID + "/review", "/api/v1/downloads/qbittorrent-profiles/" + p.ID + "/recover"} {
		method := "POST"
		if strings.HasSuffix(path, "profiles") {
			method = "GET"
		}
		resp := nativeJSONRequest(w.f.handler, method, path, viewer, `{}`)
		if resp.Code != 403 {
			t.Fatal(resp.Code, resp.Body.String())
		}
	}
	r := qbReviewHTTP(t, w)
	item := mustDownloader(t, w.f)
	item.Name = "changed"
	if _, err := w.f.store.Upsert(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if resp := qbRecoverHTTP(w, r, false, false); resp.Code != 409 {
		t.Fatal("changed config accepted", resp.Code, resp.Body.String())
	}
}
