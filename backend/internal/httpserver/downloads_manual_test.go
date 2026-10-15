package httpserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

type manualDownloadFixture struct {
	handler                            http.Handler
	runner                             *rssRunAPI
	transport                          http.RoundTripper
	token, path, hash, kind            string
	db                                 *sql.DB
	store                              *downloaderconfig.Store
	adds, reads                        int
	lost, absent, authFailure, badHash bool
	afterAdd                           func()
}

func manualHTTPFixture(t *testing.T, kind string, modern bool) *manualDownloadFixture {
	t.Helper()
	f := &manualDownloadFixture{kind: kind, hash: magnetInfoHash(testMagnet)}
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "tracker.test" {
			return jsonResponse(r, string(testTorrent)), nil
		}
		added := false
		read := false
		var rpc struct {
			ID        json.RawMessage            `json:"id"`
			Method    string                     `json:"method"`
			Arguments map[string]json.RawMessage `json:"arguments"`
			Params    json.RawMessage            `json:"params"`
		}
		if kind == "qbittorrent" {
			if strings.HasSuffix(r.URL.Path, "/auth/login") {
				if f.authFailure {
					return jsonResponse(r, "Fails."), nil
				}
				return jsonResponse(r, "Ok."), nil
			}
			added = strings.HasSuffix(r.URL.Path, "/torrents/add")
			read = strings.HasSuffix(r.URL.Path, "/torrents/info")
			if read && (r.URL.Query().Get("hashes") != f.hash || r.URL.Query().Get("filter") != "all") {
				t.Fatal("verification widened", r.URL)
			}
		} else if kind == "pan115" {
			if r.URL.Host == "webapi.115.com" {
				return jsonResponse(r, `{"state":true,"id":"123"}`), nil
			}
			added = r.URL.Host == "115.com" && r.URL.Query().Get("ac") == "add_task_urls"
		} else {
			if kind == "transmission" && r.Header.Get("X-Transmission-Session-Id") == "" {
				resp := jsonResponse(r, "")
				resp.StatusCode = 409
				resp.Header.Set("X-Transmission-Session-Id", "session")
				if modern {
					resp.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
				}
				return resp, nil
			}
			if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
				t.Fatal(err)
			}
			added = rpc.Method == "torrent-add" || rpc.Method == "torrent_add" || rpc.Method == "aria2.addUri" || rpc.Method == "aria2.addTorrent"
			read = rpc.Method == "torrent-get" || rpc.Method == "torrent_get"
			if read {
				args := rpc.Arguments
				if modern {
					_ = json.Unmarshal(rpc.Params, &args)
				}
				var ids []string
				_ = json.Unmarshal(args["ids"], &ids)
				if len(ids) != 1 || ids[0] != f.hash {
					t.Fatal("verification scope", ids)
				}
			}
		}
		if added {
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_MANUAL_DOWNLOAD_SUBMISSIONS WHERE STATE='pending' AND json_extract(DOCUMENT,'$.InfoHash')=?`, f.hash).Scan(&count); err != nil || count != 1 {
				t.Fatal("durable intent missing before add", count, err)
			}
			f.adds++
			if f.afterAdd != nil {
				f.afterAdd()
			}
			if f.lost {
				return nil, errors.New("private-token: accepted response lost")
			}
		}
		if read {
			f.reads++
		}
		hash := f.hash
		if f.badHash {
			hash = strings.Repeat("f", 40)
		}
		switch kind {
		case "qbittorrent":
			if added {
				return jsonResponse(r, "Ok."), nil
			}
			if read {
				if f.absent {
					return jsonResponse(r, `[]`), nil
				}
				return jsonResponse(r, `[{"hash":"`+hash+`"}]`), nil
			}
		case "transmission":
			args := map[string]any{}
			hashKey, addedKey := "hashString", "torrent-added"
			if modern {
				hashKey, addedKey = "hash_string", "torrent_added"
			}
			if added {
				args[addedKey] = map[string]any{hashKey: hash}
			} else if read {
				items := []any{}
				if !f.absent {
					items = append(items, map[string]any{hashKey: hash})
				}
				args["torrents"] = items
			}
			env := map[string]any{"result": "success", "arguments": args}
			if modern {
				env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": args}
			}
			body, _ := json.Marshal(env)
			return jsonResponse(r, string(body)), nil
		case "aria2":
			if added {
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": "0123456789abcdef"})
				return jsonResponse(r, string(body)), nil
			}
		case "pan115":
			if added {
				return jsonResponse(r, `{"state":true,"result":[{"info_hash":"`+hash+`"}]}`), nil
			}
		}
		t.Fatal("unexpected external request", r.URL, rpc.Method)
		return nil, nil
	})
	c := downloaderconfig.Downloader{Name: "Manual", Type: kind, Enabled: 1, Config: `{"host":"client.local","port":8080,"username":"admin","password":"private-password","secret":"private-token","cookie":"UID=private-cookie"}`}
	_, f.token, f.path = nativeServicesFixture(t, "", &c, f.transport)
	var err error
	f.db, err = sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	f.store, err = downloaderconfig.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	f.rebuild(t)
	return f
}
func (f *manualDownloadFixture) rebuild(t *testing.T) {
	t.Helper()
	var err error
	f.handler, err = buildHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: filepath.Join(filepath.Dir(f.path), "config.yaml")}, f.transport, &f.runner)
	if err != nil {
		t.Fatal(err)
	}
}
func (f *manualDownloadFixture) magnet() *httptest.ResponseRecorder {
	return nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/magnet", f.token, `{"magnet":"`+testMagnet+`"}`)
}
func manualReceipts(t *testing.T, f *manualDownloadFixture) []manualReceipt {
	t.Helper()
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/downloads/submissions", f.token, "")
	var result struct {
		Data []manualReceipt `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, secret := range []string{"private-password", "private-token", "private-cookie", "ConfigDigest", "OptionsDigest", "magnet:?"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("receipt leaked", secret, w.Body.String())
		}
	}
	return result.Data
}
func manualVerify(f *manualDownloadFixture, id string) *httptest.ResponseRecorder {
	return nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/submissions/"+id+"/reconcile", f.token, `{"confirm":true}`)
}
func assertManualCommitted(t *testing.T, f *manualDownloadFixture, expected int) {
	t.Helper()
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM DOWNLOAD_HISTORY`).Scan(&count); err != nil || count != expected {
		t.Fatal("history", count, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='download_start'`).Scan(&count); err != nil || count != expected {
		t.Fatal("events", count, err)
	}
}
func TestManualDownloadAllProtocolsPersistAndDoNotResubmitAfterRestart(t *testing.T) {
	for _, spec := range []struct {
		kind   string
		modern bool
	}{{"qbittorrent", false}, {"transmission", false}, {"transmission", true}, {"aria2", false}, {"pan115", false}} {
		for _, source := range []string{"magnet", "torrent"} {
			if spec.kind == "pan115" && source == "torrent" {
				continue
			}
			t.Run(spec.kind+source+map[bool]string{true: "modern"}[spec.modern], func(t *testing.T) {
				f := manualHTTPFixture(t, spec.kind, spec.modern)
				if len(manualReceipts(t, f)) != 0 {
					t.Fatal("unexpected baseline")
				}
				var count int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_MANUAL_DOWNLOAD_SUBMISSIONS'`).Scan(&count); err != nil || count != 0 {
					t.Fatal("read created tables", count, err)
				}
				add := f.magnet
				if source == "torrent" {
					metadata, _ := torrentmeta.Parse(testTorrent)
					f.hash = metadata.InfoHash
					add = func() *httptest.ResponseRecorder {
						return torrentUpload(f.handler, f.token, "movie.torrent", testTorrent)
					}
				}
				w := add()
				if w.Code != 200 || f.adds != 1 {
					t.Fatal(w.Code, w.Body.String(), f.adds)
				}
				items := manualReceipts(t, f)
				if len(items) != 1 || items[0].State != "submitted" || items[0].TaskID == "" {
					t.Fatal(items)
				}
				assertManualCommitted(t, f, 1)
				f.rebuild(t)
				w = add()
				if w.Code != 200 || f.adds != 1 || len(manualReceipts(t, f)) != 1 {
					t.Fatal("restart replay", w.Code, w.Body.String(), f.adds)
				}
				assertManualCommitted(t, f, 1)
			})
		}
	}
}
func TestManualDownloadLostAckBlockedUntilPositiveExactVerification(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, kind := range []string{"qbittorrent", "transmission"} {
			t.Run(kind+map[bool]string{true: "modern"}[modern], func(t *testing.T) {
				f := manualHTTPFixture(t, kind, modern)
				f.lost = true
				w := f.magnet()
				if w.Code != 409 || f.adds != 1 {
					t.Fatal(w.Code, w.Body.String())
				}
				items := manualReceipts(t, f)
				if len(items) != 1 || items[0].State != "pending" {
					t.Fatal(items)
				}
				id := items[0].ID
				f.rebuild(t)
				if w := f.magnet(); w.Code != 409 || f.adds != 1 {
					t.Fatal(w.Code, f.adds)
				}
				f.absent = true
				if w := manualVerify(f, id); w.Code != 409 || f.adds != 1 {
					t.Fatal(w.Code, w.Body.String(), f.adds)
				}
				f.absent = false
				f.badHash = true
				if w := manualVerify(f, id); w.Code != 409 {
					t.Fatal(w.Code, w.Body.String())
				}
				f.badHash = false
				w = manualVerify(f, id)
				if w.Code != 200 || f.adds != 1 {
					t.Fatal(w.Code, w.Body.String(), f.adds)
				}
				assertManualCommitted(t, f, 1)
				if w := manualVerify(f, id); w.Code != 200 || f.adds != 1 {
					t.Fatal(w.Code, w.Body.String())
				}
				assertManualCommitted(t, f, 1)
			})
		}
	}
}
func TestManualDownloadAriaAnd115UncertainCannotBeReleasedByGuessing(t *testing.T) {
	for _, kind := range []string{"aria2", "pan115"} {
		t.Run(kind, func(t *testing.T) {
			f := manualHTTPFixture(t, kind, false)
			f.lost = true
			if w := f.magnet(); w.Code != 409 {
				t.Fatal(w.Code, w.Body.String())
			}
			id := manualReceipts(t, f)[0].ID
			if w := manualVerify(f, id); w.Code != 501 || f.adds != 1 {
				t.Fatal(w.Code, w.Body.String(), f.adds)
			}
			f.rebuild(t)
			if w := f.magnet(); w.Code != 409 || f.adds != 1 {
				t.Fatal(w.Code, f.adds)
			}
		})
	}
}
func TestManualDownloadConfigSwitchCannotRetargetOrCommitOldTask(t *testing.T) {
	f := manualHTTPFixture(t, "qbittorrent", false)
	f.afterAdd = func() {
		if _, err := f.db.Exec(`UPDATE DOWNLOADER SET CONFIG='{"host":"changed.test","port":8080}' WHERE ID=1`); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.magnet(); w.Code != 409 || f.adds != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	id := manualReceipts(t, f)[0].ID
	if w := manualVerify(f, id); w.Code != 409 || f.reads != 0 {
		t.Fatal(w.Code, w.Body.String(), f.reads)
	}
	other, err := f.store.Upsert(t.Context(), downloaderconfig.Downloader{Name: "Other", Type: "qbittorrent", Enabled: 1, Config: `{"host":"other.test","port":8080}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY='DefaultDownloader'`, other.ID); err != nil {
		t.Fatal(err)
	}
	if w := f.magnet(); w.Code != 409 || f.adds != 1 {
		t.Fatal("changed default bypassed pending", w.Code, w.Body.String(), f.adds)
	}
}
func TestManualDownloadKnownAuthRejectionKeepsAuditAndAllowsExplicitNewAttempt(t *testing.T) {
	f := manualHTTPFixture(t, "qbittorrent", false)
	f.authFailure = true
	if w := f.magnet(); w.Code == 200 || f.adds != 0 {
		t.Fatal(w.Code, f.adds)
	}
	if items := manualReceipts(t, f); len(items) != 1 || items[0].State != "rejected" {
		t.Fatal(items)
	}
	f.authFailure = false
	if w := f.magnet(); w.Code != 200 || f.adds != 1 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
	assertManualCommitted(t, f, 1)
}
func TestManualDownloadReadReconcileAndSubmitShareCrossProcessGuard(t *testing.T) {
	f := manualHTTPFixture(t, "qbittorrent", false)
	f.afterAdd = func() {
		id := manualReceipts(t, f)[0].ID
		if w := manualVerify(f, id); w.Code != 409 {
			t.Fatal("active submission reconciled", w.Code, w.Body.String())
		}
		if w := f.magnet(); w.Code != 409 {
			t.Fatal("active submission duplicated", w.Code, w.Body.String())
		}
	}
	if w := f.magnet(); w.Code != 200 || f.adds != 1 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
}
func TestManualDownloadCacheAndSiteLinkShareActualInfoHashScope(t *testing.T) {
	f := manualHTTPFixture(t, "qbittorrent", false)
	metadata, _ := torrentmeta.Parse(testTorrent)
	f.hash = metadata.InfoHash
	siteStore, err := siteconfig.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer siteStore.Close()
	site, err := siteStore.Upsert(t.Context(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.test/signin"})
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/link", f.token, `{"siteId":"`+text(site.ID)+`","url":"https://tracker.test/torrent?passkey=private"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = torrentUpload(f.handler, f.token, "same.torrent", testTorrent)
	if w.Code != 200 || f.adds != 1 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
	assertManualCommitted(t, f, 1)
	ids, err := f.runner.download.service.resources.Put("0:admin", []externalindexer.Resource{{Title: "Same search result", DownloadURL: "https://tracker.test/torrent?passkey=private"}})
	if err != nil {
		t.Fatal(err)
	}
	// Search's preset NASTOOL tag changes the immutable option scope. It must
	// not silently reuse or mutate a manually submitted task with no tag.
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/resource", f.token, `{"resourceId":"`+ids[0]+`"}`)
	if w.Code != 409 || f.adds != 1 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
}
func TestManualDownloadPermissionAndExplicitReconciliation(t *testing.T) {
	f := manualHTTPFixture(t, "qbittorrent", false)
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"下载管理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	for _, path := range []string{"/api/v1/downloads/submissions", "/api/v1/downloads/submissions/" + strings.Repeat("a", 32) + "/reconcile"} {
		method := "GET"
		if strings.HasSuffix(path, "reconcile") {
			method = "POST"
		}
		for _, token := range []string{"", viewer} {
			w := nativeJSONRequest(f.handler, method, path, token, `{}`)
			want := 403
			if token == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
		}
	}
	f.lost = true
	if w := f.magnet(); w.Code != 409 {
		t.Fatal(w.Code)
	}
	id := manualReceipts(t, f)[0].ID
	for _, body := range []string{`{"confirm":false}`, `{"confirm":true,"infoHash":"injected"}`} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/submissions/"+id+"/reconcile", f.token, body)
		if w.Code != 400 || f.reads != 0 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestManualDownloadAcceptedButLocalCommitLostOnlyAllowsReadonlyRecovery(t *testing.T) {
	f := manualHTTPFixture(t, "qbittorrent", false)
	if _, err := f.db.Exec(`CREATE TABLE DOWNLOAD_HISTORY(ID INTEGER PRIMARY KEY,TITLE TEXT,YEAR TEXT,TYPE TEXT,TMDBID TEXT,SE TEXT,VOTE TEXT,POSTER TEXT,OVERVIEW TEXT,TORRENT TEXT,ENCLOSURE TEXT,SITE TEXT,DESC TEXT,DOWNLOADER TEXT,DOWNLOAD_ID TEXT,SAVE_PATH TEXT,DATE TEXT);CREATE TRIGGER lose_history BEFORE INSERT ON DOWNLOAD_HISTORY BEGIN SELECT RAISE(ABORT,'local write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if w := f.magnet(); w.Code != 409 || f.adds != 1 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM DOWNLOAD_HISTORY`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	id := manualReceipts(t, f)[0].ID
	if _, err := f.db.Exec(`DROP TRIGGER lose_history`); err != nil {
		t.Fatal(err)
	}
	f.rebuild(t)
	if w := f.magnet(); w.Code != 409 || f.adds != 1 {
		t.Fatal(w.Code, f.adds)
	}
	if w := manualVerify(f, id); w.Code != 200 || f.adds != 1 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
	assertManualCommitted(t, f, 1)
	var actor, proof string
	if err := f.db.QueryRow(`SELECT ACTOR,PROOF FROM GO_MANUAL_DOWNLOAD_CONFIRMATIONS WHERE SUBMISSION_ID=?`, id).Scan(&actor, &proof); err != nil || actor != "0:admin" || proof != "verified_present" {
		t.Fatal(actor, proof, err)
	}
}

func TestManualSearchLostResponseRemainsBlockedAfterNewCacheIDAndTrackerPolicyRefusal(t *testing.T) {
	f := manualHTTPFixture(t, "qbittorrent", false)
	resource := externalindexer.Resource{Title: "Movie", DownloadURL: testMagnet}
	f.lost = true
	for i := 0; i < 2; i++ {
		ids, err := f.runner.download.service.resources.Put("0:admin", []externalindexer.Resource{resource})
		if err != nil {
			t.Fatal(err)
		}
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/resource", f.token, `{"resourceId":"`+ids[0]+`"}`)
		if w.Code != 409 || f.adds != 1 {
			t.Fatal(w.Code, w.Body.String(), f.adds)
		}
		f.rebuild(t)
	}
	minimum := float64(1)
	resource.MinimumRatio = &minimum
	ids, err := f.runner.download.service.resources.Put("0:admin", []externalindexer.Resource{resource})
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/resource", f.token, `{"resourceId":"`+ids[0]+`"}`)
	if w.Code != 501 || f.adds != 1 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
}

func TestManualDownloadInvalidConfigDoesNotReserveAnUnsentTask(t *testing.T) {
	f := manualHTTPFixture(t, "qbittorrent", false)
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET CONFIG='invalid-json' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	w := f.magnet()
	if w.Code != 400 || f.adds != 0 || len(manualReceipts(t, f)) != 0 {
		t.Fatal(w.Code, w.Body.String(), f.adds)
	}
}
