package httpserver

import (
	"bytes"
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
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

type profileWire struct {
	f                                                            *manualDownloadFixture
	modern                                                       bool
	state                                                        transmission.ProfileState
	sets, starts                                                 int
	loseSet, loseStart, ignoreSet, failRead, duplicate, omitFlag bool
	afterSet                                                     func()
	settingID                                                    string
}

func transmissionProfileFixture(t *testing.T, modern, paused bool) *profileWire {
	t.Helper()
	f := manualHTTPFixture(t, "transmission", modern)
	return transmissionProfileForFixture(t, f, modern, paused)
}
func transmissionProfileForFixture(t *testing.T, f *manualDownloadFixture, modern, paused bool) *profileWire {
	t.Helper()
	w := &profileWire{f: f, modern: modern, state: transmission.ProfileState{Hash: f.hash, Directory: "/downloads", Labels: []string{}, Status: 0}}
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "client.local" {
			return base.RoundTrip(r)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if r.Header.Get("X-Transmission-Session-Id") == "" {
			return base.RoundTrip(r)
		}
		var req struct {
			Method    string                     `json:"method"`
			Arguments map[string]json.RawMessage `json:"arguments"`
			Params    map[string]json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil {
			t.Fatal("request")
		}
		args := req.Arguments
		if modern {
			args = req.Params
		}
		reply := func(data any) (*http.Response, error) {
			envelope := map[string]any{"result": "success", "arguments": data}
			if modern {
				envelope = map[string]any{"jsonrpc": "2.0", "id": 1, "result": data}
			}
			raw, _ := json.Marshal(envelope)
			return jsonResponse(r, string(raw)), nil
		}
		if req.Method == "torrent-add" || req.Method == "torrent_add" {
			var pause bool
			if json.Unmarshal(args["paused"], &pause) != nil || !pause {
				t.Fatal("profile task must initially pause", args)
			}
			if !w.duplicate {
				if json.Unmarshal(args["labels"], &w.state.Labels) != nil {
					t.Fatal(args)
				}
			}
			key := "download-dir"
			if modern {
				key = "download_dir"
			}
			if raw := args[key]; len(raw) > 0 {
				_ = json.Unmarshal(raw, &w.state.Directory)
			}
			var state string
			if err := f.db.QueryRow(`SELECT STATE FROM GO_TRANSMISSION_PROFILES ORDER BY CREATED DESC LIMIT 1`).Scan(&state); err != nil || state != "add_pending" {
				t.Fatal("missing add intent", state, err)
			}
			f.adds++
			if f.afterAdd != nil {
				f.afterAdd()
			}
			if f.lost {
				return nil, errors.New("add accepted then response lost")
			}
			key = "hashString"
			addedKey := "torrent-added"
			if modern {
				key, addedKey = "hash_string", "torrent_added"
			}
			return reply(map[string]any{addedKey: map[string]any{key: f.hash}})
		}
		if req.Method == "torrent-set" || req.Method == "torrent_set" || req.Method == "torrent-start" || req.Method == "torrent_start" {
			var ids []string
			_ = json.Unmarshal(args["ids"], &ids)
			if len(ids) != 1 || ids[0] != f.hash {
				t.Fatal("widened mutation", args)
			}
			var state string
			if err := f.db.QueryRow(`SELECT STATE FROM GO_TRANSMISSION_PROFILES ORDER BY CREATED DESC LIMIT 1`).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(req.Method, "set") {
				if state != "configuring" || len(args) != 9 {
					t.Fatal("missing settings intent or unwanted mutation", state, args)
				}
				w.sets++
				if !w.ignoreSet {
					keys := []string{"uploadLimit", "uploadLimited", "downloadLimit", "downloadLimited", "seedRatioLimit", "seedRatioMode", "seedIdleLimit", "seedIdleMode"}
					if modern {
						keys = []string{"upload_limit", "upload_limited", "download_limit", "download_limited", "seed_ratio_limit", "seed_ratio_mode", "seed_idle_limit", "seed_idle_mode"}
					}
					outputs := []any{&w.state.UploadKB, &w.state.UploadLimited, &w.state.DownloadKB, &w.state.DownloadLimited, &w.state.Ratio, &w.state.RatioMode, &w.state.IdleMinutes, &w.state.IdleMode}
					for i, key := range keys {
						if json.Unmarshal(args[key], outputs[i]) != nil {
							t.Fatal(args)
						}
					}
				}
				if w.afterSet != nil {
					w.afterSet()
				}
				if w.loseSet {
					return nil, errors.New("private-token: lost set acknowledgment")
				}
			} else {
				if state != "starting" {
					t.Fatal("missing start intent", state)
				}
				w.starts++
				w.state.Status = 4
				if w.loseStart {
					return nil, errors.New("lost startup acknowledgment")
				}
			}
			return reply(map[string]any{})
		}
		if req.Method == "torrent-get" || req.Method == "torrent_get" {
			var fields []string
			_ = json.Unmarshal(args["fields"], &fields)
			if len(fields) > 1 && (strings.Contains(string(args["fields"]), "seedRatioMode") || strings.Contains(string(args["fields"]), "seed_ratio_mode")) {
				if w.failRead {
					return nil, errors.New("profile unavailable")
				}
				var ids []string
				_ = json.Unmarshal(args["ids"], &ids)
				if len(ids) != 1 || ids[0] != f.hash {
					t.Fatal(ids)
				}
				keys := []string{"hashString", "downloadDir", "labels", "status", "uploadLimit", "uploadLimited", "downloadLimit", "downloadLimited", "seedRatioLimit", "seedRatioMode", "seedIdleLimit", "seedIdleMode"}
				if modern {
					keys = []string{"hash_string", "download_dir", "labels", "status", "upload_limit", "upload_limited", "download_limit", "download_limited", "seed_ratio_limit", "seed_ratio_mode", "seed_idle_limit", "seed_idle_mode"}
				}
				s := w.state
				values := []any{s.Hash, s.Directory, s.Labels, s.Status, s.UploadKB, s.UploadLimited, s.DownloadKB, s.DownloadLimited, s.Ratio, s.RatioMode, s.IdleMinutes, s.IdleMode}
				row := map[string]any{}
				for i, key := range keys {
					if w.omitFlag && i == 5 {
						continue
					}
					row[key] = values[i]
				}
				return reply(map[string]any{"torrents": []any{row}})
			}
		}
		return base.RoundTrip(r)
	})
	f.rebuild(t)
	setting, err := f.store.UpsertSetting(t.Context(), downloaderconfig.DownloadSetting{Name: "TR profile", DownloaderID: "1", Tags: "NASTOOL;user", Paused: map[bool]int{true: 1}[paused], UploadLimit: 10, DownloadLimit: 20, RatioLimit: 150, SeedingTimeLimit: 60})
	if err != nil {
		t.Fatal(err)
	}
	w.settingID = strconv.FormatInt(setting.ID, 10)
	return w
}
func (w *profileWire) submit(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	ids, err := w.f.runner.download.service.resources.Put("0:admin", []externalindexer.Resource{{Title: "Movie", DownloadURL: testMagnet}})
	if err != nil {
		t.Fatal(err)
	}
	return nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/resource", w.f.token, `{"resourceId":"`+ids[0]+`","setting":"`+w.settingID+`"}`)
}
func getProfile(t *testing.T, w *profileWire) downloaderconfig.TransmissionProfile {
	t.Helper()
	profiles, err := w.f.store.TransmissionProfiles(t.Context())
	if err != nil || len(profiles) != 1 {
		t.Fatal(profiles, err)
	}
	return profiles[0]
}
func reviewProfileHTTP(t *testing.T, w *profileWire, id string) (int, transmissionProfileReview, string) {
	t.Helper()
	resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/transmission-profiles/"+id+"/review", w.f.token, `{}`)
	var result struct {
		Data transmissionProfileReview `json:"data"`
	}
	_ = json.Unmarshal(resp.Body.Bytes(), &result)
	return resp.Code, result.Data, resp.Body.String()
}
func recoverProfileHTTP(w *profileWire, r transmissionProfileReview, settings, start bool) *httptest.ResponseRecorder {
	data, _ := json.Marshal(map[string]any{"confirm": true, "fingerprint": r.Fingerprint, "confirmSettings": settings, "confirmStart": start})
	return nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/transmission-profiles/"+r.Profile.ID+"/recover", w.f.token, string(data))
}
func TestTransmissionProfileActualSearchAppliesSettingsBeforeOptionalStartup(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, paused := range []bool{false, true} {
			t.Run(strconv.FormatBool(modern)+"/"+strconv.FormatBool(paused), func(t *testing.T) {
				w := transmissionProfileFixture(t, modern, paused)
				resp := w.submit(t)
				if resp.Code != 200 || w.f.adds != 1 || w.sets != 1 || w.starts != map[bool]int{false: 1}[paused] {
					t.Fatal(resp.Code, resp.Body.String(), w.f.adds, w.sets, w.starts)
				}
				p := getProfile(t, w)
				if p.State != "ready" || !p.Ready(w.state) {
					t.Fatal(p, w.state)
				}
				assertManualCommitted(t, w.f, 1)
				resp = w.submit(t)
				if resp.Code != 200 || w.f.adds != 1 || w.sets != 1 {
					t.Fatal(resp.Code, resp.Body.String())
				}
			})
		}
	}
}
func TestTransmissionProfileSettingsLostAckRequiresReadonlyProofAndSeparateStart(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(strconv.FormatBool(modern), func(t *testing.T) {
			w := transmissionProfileFixture(t, modern, false)
			w.loseSet = true
			resp := w.submit(t)
			if resp.Code != 409 || w.f.adds != 1 || w.sets != 1 || w.starts != 0 {
				t.Fatal(resp.Code, resp.Body.String(), w.f.adds, w.sets, w.starts)
			}
			p := getProfile(t, w)
			if p.State != "configuring" {
				t.Fatal(p)
			}
			manualID := manualReceipts(t, w.f)[0].ID
			if r := manualVerify(w.f, manualID); r.Code != 409 {
				t.Fatal("bare existence accepted unapplied profile", r.Code, r.Body.String())
			}
			w.f.rebuild(t)
			status, review, body := reviewProfileHTTP(t, w, p.ID)
			if status != 200 || review.Next != "start" {
				t.Fatal(status, body)
			}
			if r := recoverProfileHTTP(w, review, true, false); r.Code != 409 {
				t.Fatal(r.Code, r.Body.String())
			}
			if r := recoverProfileHTTP(w, review, false, true); r.Code != 200 || w.starts != 1 || w.sets != 1 || w.f.adds != 1 {
				t.Fatal(r.Code, r.Body.String(), w.starts, w.sets, w.f.adds)
			}
			if r := manualVerify(w.f, manualID); r.Code != 200 {
				t.Fatal(r.Code, r.Body.String())
			}
			assertManualCommitted(t, w.f, 1)
		})
	}
}
func TestTransmissionProfileRecoveryConfiguresPausedTaskWithoutImplicitStartup(t *testing.T) {
	w := transmissionProfileFixture(t, true, false)
	w.failRead = true
	if resp := w.submit(t); resp.Code != 409 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	p := getProfile(t, w)
	if p.State != "added" || w.sets != 0 {
		t.Fatal(p, w.sets)
	}
	w.failRead = false
	status, r, body := reviewProfileHTTP(t, w, p.ID)
	if status != 200 || r.Next != "configure" {
		t.Fatal(status, body)
	}
	if resp := recoverProfileHTTP(w, r, false, true); resp.Code != 409 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	if resp := recoverProfileHTTP(w, r, true, false); resp.Code != 200 || w.sets != 1 || w.starts != 0 || getProfile(t, w).State != "configured" {
		t.Fatal(resp.Code, resp.Body.String(), w.sets, w.starts)
	}
	status, r, body = reviewProfileHTTP(t, w, p.ID)
	if status != 200 || r.Next != "start" {
		t.Fatal(status, body)
	}
	if resp := recoverProfileHTTP(w, r, false, true); resp.Code != 200 || w.starts != 1 || w.f.adds != 1 {
		t.Fatal(resp.Code, resp.Body.String())
	}
}
func TestTransmissionProfileLostStartOnlyAcknowledgesAndNeverStartsTwice(t *testing.T) {
	w := transmissionProfileFixture(t, false, false)
	w.loseStart = true
	if resp := w.submit(t); resp.Code != 409 || w.starts != 1 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	p := getProfile(t, w)
	if p.State != "starting" {
		t.Fatal(p)
	}
	status, r, body := reviewProfileHTTP(t, w, p.ID)
	if status != 200 || r.Next != "acknowledge" {
		t.Fatal(status, body)
	}
	if resp := recoverProfileHTTP(w, r, false, false); resp.Code != 200 || w.starts != 1 || w.f.adds != 1 {
		t.Fatal(resp.Code, resp.Body.String())
	}
}
func TestTransmissionProfileUnprovedIntentsMissingFieldsOrMarkerNeverReplay(t *testing.T) {
	for _, reason := range []string{"ignored-set", "missing-marker", "missing-field", "starting-still-stopped"} {
		t.Run(reason, func(t *testing.T) {
			w := transmissionProfileFixture(t, true, false)
			switch reason {
			case "ignored-set":
				w.ignoreSet = true
				w.loseSet = true
			case "missing-marker":
				w.duplicate = true
			case "missing-field":
				w.omitFlag = true
			case "starting-still-stopped":
				w.loseStart = true
			}
			if resp := w.submit(t); resp.Code != 409 {
				t.Fatal(resp.Code, resp.Body.String())
			}
			p := getProfile(t, w)
			if reason == "starting-still-stopped" {
				w.state.Status = 0
			}
			status, _, body := reviewProfileHTTP(t, w, p.ID)
			if status != 409 {
				t.Fatal(status, body)
			}
			sets, starts := w.sets, w.starts
			if resp := w.submit(t); resp.Code != 409 || w.f.adds != 1 || w.sets != sets || w.starts != starts {
				t.Fatal(resp.Code, resp.Body.String())
			}
		})
	}
}
func TestTransmissionProfileConfigChangeStaleReviewAndGenericStartupCannotBypass(t *testing.T) {
	w := transmissionProfileFixture(t, true, false)
	w.failRead = true
	if r := w.submit(t); r.Code != 409 {
		t.Fatal(r.Code)
	}
	p := getProfile(t, w)
	w.failRead = false
	status, r, body := reviewProfileHTTP(t, w, p.ID)
	if status != 200 {
		t.Fatal(status, body)
	}
	resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/"+w.f.hash+"/start", w.f.token, `{}`)
	if resp.Code != 409 || w.starts != 0 {
		t.Fatal(resp.Code, resp.Body.String(), w.starts)
	}
	w.state.Labels = append(w.state.Labels, "new-user-label")
	if resp := recoverProfileHTTP(w, r, true, false); resp.Code != 409 || w.sets != 0 {
		t.Fatal(resp.Code, resp.Body.String(), w.sets)
	}
	if _, err := w.f.db.Exec(`UPDATE DOWNLOADER SET CONFIG='{"host":"changed.test"}' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	status, _, body = reviewProfileHTTP(t, w, p.ID)
	if status != 409 {
		t.Fatal(status, body)
	}
}
func TestTransmissionProfilePermissionsAndSourceTransactionGate(t *testing.T) {
	w := transmissionProfileFixture(t, false, false)
	w.failRead = true
	if r := w.submit(t); r.Code != 409 {
		t.Fatal(r.Code)
	}
	p := getProfile(t, w)
	created := performFormRequest(w.f.handler, "/api/v1/user/manage", w.f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"下载管理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, w.f.handler, "viewer", "strong-password")
	for _, path := range []string{"/api/v1/downloads/transmission-profiles", "/api/v1/downloads/transmission-profiles/" + p.ID + "/review", "/api/v1/downloads/transmission-profiles/" + p.ID + "/recover"} {
		method := "POST"
		if strings.HasSuffix(path, "profiles") {
			method = "GET"
		}
		for _, token := range []string{"", viewer} {
			r := nativeJSONRequest(w.f.handler, method, path, token, `{}`)
			want := 403
			if token == "" {
				want = 401
			}
			if r.Code != want {
				t.Fatal(r.Code, r.Body.String())
			}
		}
	}
	items, err := w.f.store.ManualSubmissions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.f.store.CompleteManual(t.Context(), items[0], w.f.hash, "verified_present", "0:admin"); err == nil {
		t.Fatal("pending profile bypassed source transaction")
	}
}

func profileSourceTransport(t *testing.T, modern bool, hash string, fallback http.RoundTripper) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "client.local" {
			return fallback.RoundTrip(r)
		}
		if r.Header.Get("X-Transmission-Session-Id") == "" {
			response := jsonResponse(r, "")
			response.StatusCode = 409
			response.Header.Set("X-Transmission-Session-Id", "session")
			if modern {
				response.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
			}
			return response, nil
		}
		var rpc struct {
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&rpc) != nil || rpc.Method != "torrent-get" && rpc.Method != "torrent_get" {
			t.Fatal("unexpected source task RPC", rpc.Method)
		}
		key := "hashString"
		if modern {
			key = "hash_string"
		}
		data := map[string]any{"torrents": []any{map[string]any{key: hash}}}
		envelope := map[string]any{"result": "success", "arguments": data}
		if modern {
			envelope = map[string]any{"jsonrpc": "2.0", "id": 1, "result": data}
		}
		body, _ := json.Marshal(envelope)
		return jsonResponse(r, string(body)), nil
	})
}
func TestTransmissionProfileSubscriptionActualSubmitAndLostSettingsRecovery(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(strconv.FormatBool(lost), func(t *testing.T) {
			x := newSubscriptionRunFixture(t)
			path := filepath.Join(filepath.Dir(x.path), "user.db")
			store, err := downloaderconfig.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			item, err := store.Get(t.Context(), 1)
			if err != nil {
				t.Fatal(err)
			}
			item.Type = "transmission"
			item.Config = `{"host":"client.local","port":9091}`
			if _, err := store.Upsert(t.Context(), item); err != nil {
				t.Fatal(err)
			}
			f := &manualDownloadFixture{token: x.token, path: path, hash: magnetInfoHash(testMagnet), kind: "transmission", db: x.db, store: store}
			f.transport = profileSourceTransport(t, true, f.hash, x.transport)
			w := transmissionProfileForFixture(t, f, true, false)
			w.loseSet = lost
			if _, err := f.db.Exec(`UPDATE RSS_MOVIES SET DOWNLOAD_SETTING=? WHERE ID=1`, w.settingID); err != nil {
				t.Fatal(err)
			}
			resp := performFormRequest(f.handler, "/api/v1/subscriptions/search/run", f.token, url.Values{"type": {"MOV"}, "id": {"1"}})
			if lost {
				if resp.Code != 502 || f.adds != 1 || w.starts != 0 {
					t.Fatal(resp.Code, resp.Body.String(), f.adds, w.starts)
				}
				r := performFormRequest(f.handler, "/api/v1/subscriptions/MOV/1/reconcile", f.token, nil)
				if r.Code != 409 {
					t.Fatal("unready profile accepted", r.Code, r.Body.String())
				}
				p := getProfile(t, w)
				status, review, body := reviewProfileHTTP(t, w, p.ID)
				if status != 200 || review.Next != "start" {
					t.Fatal(status, body)
				}
				if r := recoverProfileHTTP(w, review, false, true); r.Code != 200 {
					t.Fatal(r.Code, r.Body.String())
				}
				resp = performFormRequest(f.handler, "/api/v1/subscriptions/MOV/1/reconcile", f.token, nil)
			}
			if resp.Code != 200 || f.adds != 1 || w.sets != 1 || w.starts != 1 || x.adds != 0 {
				t.Fatal(resp.Code, resp.Body.String(), f.adds, w.sets, w.starts, x.adds)
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_HISTORY`).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestTransmissionProfileRSSActualTaskAppliesLimitsAndCommitsOnlyReady(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(strconv.FormatBool(manual), func(t *testing.T) {
			feed := `{"items":[{"title":"Movie.2026","enclosure":"` + testMagnet + `","size":1024}]}`
			fallback := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() != "feed.local" {
					t.Fatal(r.URL)
				}
				return jsonResponse(r, feed), nil
			})
			_, token, db, configPath := rssRunFixture(t, fallback)
			path := filepath.Join(filepath.Dir(configPath), "user.db")
			store, err := downloaderconfig.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			item, err := store.Get(t.Context(), 1)
			if err != nil {
				t.Fatal(err)
			}
			item.Type = "transmission"
			item.Config = `{"host":"client.local","port":9091}`
			if _, err := store.Upsert(t.Context(), item); err != nil {
				t.Fatal(err)
			}
			f := &manualDownloadFixture{token: token, path: path, hash: magnetInfoHash(testMagnet), kind: "transmission", db: db, store: store}
			f.transport = profileSourceTransport(t, false, f.hash, fallback)
			w := transmissionProfileForFixture(t, f, false, false)
			if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET DOWNLOAD_SETTING=? WHERE ID=7`, w.settingID); err != nil {
				t.Fatal(err)
			}
			var r *httptest.ResponseRecorder
			if manual {
				r = performFormRequest(f.handler, "/api/v1/rss/item/download", token, url.Values{"taskid": {"7"}, "articles": {`[{"title":"Movie.2026","enclosure":"` + testMagnet + `","year":"2026"}]`}})
			} else {
				r = performFormRequest(f.handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
			}
			if r.Code != 200 || f.adds != 1 || w.sets != 1 || w.starts != 1 {
				t.Fatal(r.Code, r.Body.String(), f.adds, w.sets, w.starts)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM USERRSS_TASK_HISTORY`).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestTransmissionProfileConfigRevocationAfterLimitsDoesNotStartOrCommitSource(t *testing.T) {
	w := transmissionProfileFixture(t, true, false)
	w.afterSet = func() {
		if _, err := w.f.db.Exec(`UPDATE DOWNLOADER SET ENABLED=0 WHERE ID=1`); err != nil {
			t.Fatal(err)
		}
	}
	r := w.submit(t)
	if r.Code != 409 || w.starts != 0 || w.f.adds != 1 || w.sets != 1 {
		t.Fatal(r.Code, r.Body.String(), w.starts, w.f.adds, w.sets)
	}
	if getProfile(t, w).State != "configuring" || manualReceipts(t, w.f)[0].State != "pending" {
		t.Fatal("stale receipt committed")
	}
}

func TestTransmissionProfileLostAddCanRecoverOnlyItsUniqueMarkedPausedTask(t *testing.T) {
	w := transmissionProfileFixture(t, false, true)
	w.f.lost = true
	r := w.submit(t)
	if r.Code != 409 || w.f.adds != 1 || w.sets != 0 {
		t.Fatal(r.Code, r.Body.String())
	}
	p := getProfile(t, w)
	if p.State != "add_pending" {
		t.Fatal(p)
	}
	status, review, body := reviewProfileHTTP(t, w, p.ID)
	if status != 200 || review.Next != "configure" {
		t.Fatal(status, body)
	}
	if r := recoverProfileHTTP(w, review, true, false); r.Code != 200 || w.sets != 1 || w.starts != 0 || w.f.adds != 1 {
		t.Fatal(r.Code, r.Body.String())
	}
	id := manualReceipts(t, w.f)[0].ID
	if r := manualVerify(w.f, id); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	assertManualCommitted(t, w.f, 1)
}

func TestTransmissionProfileExplicitZeroPresetDisablesPerTaskStopLimitsNotInheritsThem(t *testing.T) {
	w := transmissionProfileFixture(t, true, true)
	setting, err := w.f.store.GetSetting(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	setting.UploadLimit = 0
	setting.DownloadLimit = 0
	setting.RatioLimit = 0
	setting.SeedingTimeLimit = 0
	if _, err := w.f.store.UpsertSetting(t.Context(), setting); err != nil {
		t.Fatal(err)
	}
	resp := w.submit(t)
	if resp.Code != 200 || w.sets != 1 || w.starts != 0 || w.state.RatioMode != 2 || w.state.IdleMode != 2 || w.state.UploadLimited || w.state.DownloadLimited {
		t.Fatal(resp.Code, resp.Body.String(), w.state)
	}
}
