package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/torrentremover"
)

type removalFixture struct {
	f                   *loginFixture
	api                 *torrentRemovalAPI
	db                  *sql.DB
	hash, kind          string
	directory           string
	added               int64
	removed, lost       bool
	mutations, requests int
}

func newRemovalFixture(t *testing.T, kind string, modern bool) *removalFixture {
	f := newLoginFixture(t, nil)
	t.Cleanup(func() { f.removal.store.Close() })
	db, err := sql.Open("sqlite", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := &removalFixture{f: f, api: f.removal, db: db, hash: strings.Repeat("a", 40), kind: kind, directory: "/downloads", added: 1}
	if kind == "aria2" {
		m.hash = strings.Repeat("a", 16)
	}
	d, err := f.removal.organizer.service.downloaders.Upsert(t.Context(), downloaderconfig.Downloader{Name: "Fixture downloader", Enabled: 1, Type: kind, Config: `{"host":"http://remove.fixture.test:8080","port":"8080","secret":"fixture-secret","username":"fixture-user","password":"fixture-password"}`, DownloadDir: "[]"})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`CREATE TABLE IF NOT EXISTS DOWNLOAD_HISTORY(ID INTEGER PRIMARY KEY,TITLE TEXT,DOWNLOADER TEXT,DOWNLOAD_ID TEXT,SAVE_PATH TEXT,DATE TEXT)`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO DOWNLOAD_HISTORY(ID,TITLE,DOWNLOADER,DOWNLOAD_ID,SAVE_PATH,DATE)VALUES(9007199254740993,'Fixture movie',?,?,'/downloads','2026-11-06 00:00:00')`, strconv.FormatInt(d.ID, 10), m.hash); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		method, path string
		body         any
	}{{"POST", "/api/v1/plugins/TorrentRemover/install", nil}, {"PUT", "/api/v1/plugins/TorrentRemover", nil}} {
		if call.method == "PUT" {
			detail := f.request(t, "GET", "/api/v1/plugins/TorrentRemover", nil)
			var e struct{ Data pluginConfigDetail }
			if json.Unmarshal(detail.Body.Bytes(), &e) != nil {
				t.Fatal(detail.Body.String())
			}
			call.body = map[string]any{"fingerprint": e.Data.Meta.Fingerprint, "values": map[string]any{"enable": true}}
		}
		r := f.request(t, call.method, call.path, call.body)
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	scope, err := m.api.scope(t.Context(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	response := f.request(t, "POST", "/api/v1/torrent-remover/activate", map[string]any{"confirm": true, "oldExecutorsStopped": true, "writersStopped": true, "preserveFiles": true, "fingerprint": scope.fingerprint()})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		m.requests++
		if r.URL.Host != "remove.fixture.test:8080" {
			t.Fatal("unexpected receiver", r.URL)
		}
		empty := func(s string) *http.Response {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(s))}
		}
		if kind == "qbittorrent" {
			switch r.URL.Path {
			case "/api/v2/auth/login":
				return empty("Ok."), nil
			case "/api/v2/torrents/info":
				if m.removed {
					return jsonResponse(r, `[]`), nil
				}
				return jsonResponse(r, `[{"hash":"`+m.hash+`","name":"Fixture movie","save_path":"`+m.directory+`","added_on":`+strconv.FormatInt(m.added, 10)+`,"state":"uploading","progress":1,"amount_left":0,"completion_on":1,"tags":"keep"}]`), nil
			case "/api/v2/torrents/files":
				return jsonResponse(r, `[{"name":"Fixture/movie.mkv","size":8,"progress":1,"priority":1}]`), nil
			case "/api/v2/torrents/delete":
				_ = r.ParseForm()
				if r.Form.Get("hashes") != m.hash || r.Form.Get("deleteFiles") != "false" {
					t.Fatal(r.Form)
				}
				m.mutations++
				m.removed = true
				if m.lost {
					return nil, errors.New("lost after removal")
				}
				return empty(""), nil
			}
		}
		var call struct {
			Method    string
			ID        json.RawMessage
			Arguments map[string]json.RawMessage
			Params    json.RawMessage
		}
		if json.NewDecoder(r.Body).Decode(&call) != nil {
			t.Fatal("bad request")
		}
		if kind == "transmission" {
			if r.Header.Get("X-Transmission-Session-Id") == "" {
				reply := empty("")
				reply.StatusCode = 409
				reply.Header.Set("X-Transmission-Session-Id", "fixture")
				if modern {
					reply.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
				}
				return reply, nil
			}
			key := func(a, b string) string {
				if modern {
					return b
				}
				return a
			}
			args := call.Arguments
			if modern {
				if json.Unmarshal(call.Params, &args) != nil {
					t.Fatal("bad params")
				}
			}
			result := map[string]any{}
			if call.Method == key("torrent-remove", "torrent_remove") {
				var ids []string
				_ = json.Unmarshal(args["ids"], &ids)
				if len(ids) != 1 || ids[0] != m.hash || string(args[key("delete-local-data", "delete_local_data")]) != "false" {
					t.Fatal(args)
				}
				m.mutations++
				m.removed = true
				if m.lost {
					return nil, errors.New("lost after removal")
				}
			} else if call.Method == key("torrent-get", "torrent_get") {
				rows := []any{}
				if !m.removed {
					rows = append(rows, map[string]any{key("hashString", "hash_string"): m.hash, key("addedDate", "added_date"): m.added, "name": "Fixture movie", key("downloadDir", "download_dir"): "/downloads", key("percentDone", "percent_done"): 1, key("leftUntilDone", "left_until_done"): 0, "status": 6, "error": 0, key("metadataPercentComplete", "metadata_percent_complete"): 1, "labels": []string{"keep"}, "files": []any{map[string]any{"name": "Fixture/movie.mkv", "length": 8, key("bytesCompleted", "bytes_completed"): 8}}, key("fileStats", "file_stats"): []any{map[string]any{"wanted": true, key("bytesCompleted", "bytes_completed"): 8}}})
				}
				result["torrents"] = rows
			} else {
				t.Fatal(call.Method)
			}
			out := map[string]any{"result": "success", "arguments": result}
			if modern {
				out = map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}
			}
			raw, _ := json.Marshal(out)
			return jsonResponse(r, string(raw)), nil
		}
		var result any
		switch call.Method {
		case "aria2.tellStatus":
			result = map[string]any{"gid": m.hash, "status": "complete", "dir": "/downloads", "totalLength": "8", "completedLength": "8", "errorCode": "0", "files": []any{map[string]string{"path": "/downloads/Fixture/movie.mkv", "length": "8", "completedLength": "8", "selected": "true"}}}
		case "aria2.removeDownloadResult":
			m.mutations++
			m.removed = true
			if m.lost {
				return nil, errors.New("lost after removal")
			}
			result = "OK"
		case "aria2.tellActive", "aria2.tellWaiting", "aria2.tellStopped":
			rows := []any{}
			if !m.removed && call.Method == "aria2.tellStopped" {
				rows = append(rows, map[string]string{"gid": m.hash})
			}
			result = rows
		default:
			t.Fatal(call.Method)
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
		return jsonResponse(r, string(raw)), nil
	})
	return m
}
func (m *removalFixture) capture(t *testing.T) torrentremover.Receipt {
	t.Helper()
	tx, err := m.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	event := torrentremover.Event{ID: strings.Repeat("b", 64), Removal: strings.Repeat("c", 64), History: "9007199254740993", Title: "Fixture movie", Path: "/source/Fixture/movie.mkv"}
	if err := torrentremover.Capture(t.Context(), tx, event); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	records, err := m.api.store.List(t.Context(), "")
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	return records[0]
}
func TestTorrentRemoverAllNativeProtocolsOnlyRemoveTasksKeepEveryFile(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		modern bool
	}{{"qbittorrent", false}, {"transmission", false}, {"transmission", true}, {"aria2", false}} {
		t.Run(tc.kind+strconv.FormatBool(tc.modern), func(t *testing.T) {
			m := newRemovalFixture(t, tc.kind, tc.modern)
			record := m.capture(t)
			m.api.tick(t.Context())
			done, err := m.api.store.Get(t.Context(), record.ID)
			if err != nil || done.State != "completed" || m.mutations != 1 {
				t.Fatal(done, err, m.mutations)
			}
			m.api.tick(t.Context())
			if m.mutations != 1 {
				t.Fatal("removal repeated")
			}
		})
	}
}
func TestTorrentRemoverLostReplyOnlyReconcilesWithoutSendingAgain(t *testing.T) {
	m := newRemovalFixture(t, "qbittorrent", false)
	m.lost = true
	record := m.capture(t)
	m.api.tick(t.Context())
	receipt, err := m.api.store.Get(t.Context(), record.ID)
	if err != nil || receipt.State != "unknown" || m.mutations != 1 {
		t.Fatal(receipt, err, m.mutations)
	}
	m.api.tick(t.Context())
	if m.mutations != 1 {
		t.Fatal("unknown action repeated")
	}
	scope, err := m.api.scope(t.Context(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	r := m.f.request(t, "POST", "/api/v1/torrent-remover/reconcile", map[string]any{"confirm": true, "fingerprint": scope.fingerprint(), "id": record.ID})
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	receipt, err = m.api.store.Get(t.Context(), record.ID)
	if err != nil || receipt.State != "completed" || m.mutations != 1 {
		t.Fatal(receipt, err, m.mutations)
	}
}
func TestTorrentRemoverStopAndChangedHistoryPreventQueuedRemoval(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(strconv.FormatBool(changed), func(t *testing.T) {
			m := newRemovalFixture(t, "qbittorrent", false)
			record := m.capture(t)
			if changed {
				if _, err := m.db.Exec(`UPDATE DOWNLOAD_HISTORY SET SAVE_PATH='/changed'`); err != nil {
					t.Fatal(err)
				}
			} else {
				settings, _ := m.api.store.Settings(t.Context())
				r := m.f.request(t, "POST", "/api/v1/torrent-remover/stop", map[string]any{"confirm": true, "epoch": settings.Control.Epoch})
				if r.Code != 200 {
					t.Fatal(r.Body.String())
				}
			}
			m.api.tick(t.Context())
			r, err := m.api.store.Get(t.Context(), record.ID)
			if err != nil || m.mutations != 0 || m.requests != 0 || r.State == "completed" {
				t.Fatal(r, err, m.mutations, m.requests)
			}
		})
	}
}
func TestTorrentRemoverRealSourceRemovalTransactionCapturesFutureEvent(t *testing.T) {
	x := recoveryHTTPFixture(t, "")
	mediaRemovalHTTPHistory(t, x, 1)
	libraryWriteHTTP(t, x.f.source, "original.mkv", "source")
	x.put(t, "Movie/movie.mkv", "library")
	libraryWriteHTTP(t, x.f.source, "keep.txt", "preserve")
	api := x.f.f.runner.torrentRemover
	for _, key := range []struct{ k, v string }{{"UserInstalledPlugins", `["TorrentRemover"]`}, {torrentremover.ConfigKey, `{"enable":true}`}} {
		if err := api.organizer.service.systemConfig.Set(t.Context(), key.k, key.v); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := api.scope(t.Context(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	control := torrentremover.Control{Exclusive: true, Epoch: strings.Repeat("a", 64), Actor: "admin", Scope: scope.fingerprint(), Config: torrentremover.Hash(scope.settings.Raw), Targets: []torrentremover.Target{}}
	for _, item := range scope.targets {
		control.Targets = append(control.Targets, torrentremover.Target{ID: strconv.FormatInt(item.ID, 10), Name: item.Name, Type: item.Type, Digest: removalDownloaderDigest(item)})
	}
	if len(control.Targets) == 0 {
		t.Fatal("fixture downloader missing")
	}
	if err := api.store.Change(t.Context(), scope.settings, nil, nil, control); err != nil {
		t.Fatal(err)
	}
	in := mediaRemovalHTTPInput()
	in["flag"] = "del_source"
	in["includeNfo"] = false
	v := mediaRemovalHTTPCreate(t, x, in)
	v = mediaRemovalHTTPReview(t, x, v.ID)
	done := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/"+v.ID+"/continue", mergeRecovery(mediaRemovalHTTPGrants(), map[string]any{"fingerprint": v.Fingerprint})))
	if done.State != "completed" {
		t.Fatal(done)
	}
	records, err := api.store.List(t.Context(), "")
	if err != nil || len(records) != 1 || records[0].Definition.Event.Removal != v.ID || records[0].Definition.Event.Title != "Movie" {
		t.Fatal(records, err)
	}
	if _, err := os.Stat(filepath.Join(x.f.source, "keep.txt")); err != nil {
		t.Fatal(err)
	}
}

var _ = context.Canceled

func TestTorrentRemoverDuplicateSourceFactsCannotResendAnUnknownPhysicalTask(t *testing.T) {
	m := newRemovalFixture(t, "qbittorrent", false)
	m.lost = true
	m.capture(t)
	m.api.tick(t.Context())
	if m.mutations != 1 {
		t.Fatal(m.mutations)
	}
	// The first request may still be in progress remotely; it is not known absent.
	m.removed = false
	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := torrentremover.Capture(t.Context(), tx, torrentremover.Event{ID: strings.Repeat("d", 64), Removal: strings.Repeat("c", 64), History: "9007199254740993", Title: "Fixture movie", Path: "/source/Fixture/movie.mkv"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	m.api.tick(t.Context())
	if m.mutations != 1 {
		t.Fatal("unknown physical task was dispatched through another receipt", m.mutations)
	}
}
func TestTorrentRemoverRequiresTheOriginalDirectoryAndTaskIncarnation(t *testing.T) {
	for _, future := range []bool{false, true} {
		t.Run(strconv.FormatBool(future), func(t *testing.T) {
			m := newRemovalFixture(t, "qbittorrent", false)
			m.capture(t)
			if future {
				m.added = 2000000000
			} else {
				m.directory = "/new-task-directory"
			}
			m.api.tick(t.Context())
			if m.mutations != 0 {
				t.Fatal("stale history authorized another task incarnation", m.mutations)
			}
		})
	}
}
func TestTorrentRemoverDisabledPluginCanReadOnlyReconcileItsUnknownTask(t *testing.T) {
	m := newRemovalFixture(t, "qbittorrent", false)
	m.lost = true
	receipt := m.capture(t)
	m.api.tick(t.Context())
	st, err := m.api.store.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := torrentremover.Merge(st.Raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.api.store.Change(t.Context(), st, &raw, nil, torrentremover.Control{}); err != nil {
		t.Fatal(err)
	}
	scope, err := m.api.scope(t.Context(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	response := m.f.request(t, "POST", "/api/v1/torrent-remover/reconcile", map[string]any{"confirm": true, "fingerprint": scope.fingerprint(), "id": receipt.ID})
	if response.Code != 200 || m.mutations != 1 {
		t.Fatal(response.Code, response.Body.String(), m.mutations)
	}
}
