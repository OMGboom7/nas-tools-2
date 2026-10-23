package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

const mediaRemovalEndpoint = "/api/v1/organization/media-removals"

func mediaRemovalHTTPHistory(t *testing.T, x *recoveryHTTP, id int64) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS TRANSFER_HISTORY(ID INTEGER PRIMARY KEY,MODE TEXT,TYPE TEXT,CATEGORY TEXT,TMDBID INTEGER,TITLE TEXT,YEAR TEXT,SEASON_EPISODE TEXT,SOURCE TEXT,SOURCE_PATH TEXT,SOURCE_FILENAME TEXT,DEST TEXT,DEST_PATH TEXT,DEST_FILENAME TEXT,DATE TEXT)`,
		`CREATE TABLE IF NOT EXISTS TRANSFER_BLACKLIST(ID INTEGER PRIMARY KEY,PATH TEXT)`,
		`CREATE TABLE IF NOT EXISTS SYNC_HISTORY(ID INTEGER PRIMARY KEY,PATH TEXT,SRC TEXT,DEST TEXT)`,
	} {
		if _, err := x.f.f.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	mode := "复制"
	if x.driver == "minio" {
		mode = "Minio复制"
	} else if x.driver == "rclone" {
		mode = "Rclone复制"
	}
	if _, err := x.f.f.db.Exec(`INSERT INTO TRANSFER_HISTORY(ID,MODE,TYPE,TMDBID,TITLE,YEAR,SOURCE_PATH,SOURCE_FILENAME,DEST,DEST_PATH,DEST_FILENAME,DATE)VALUES(?,?,'电影',100,'Movie','2026',?,'original.mkv',?,?,'movie.mkv','2026-10-23')`, id, mode, x.f.source, x.root, path.Join(x.root, "Movie")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`INSERT INTO TRANSFER_BLACKLIST(ID,PATH)VALUES(?,?)`, `INSERT INTO SYNC_HISTORY(ID,PATH)VALUES(?,?)`} {
		if _, err := x.f.f.db.Exec(q, id, path.Join(x.f.source, "original.mkv")); err != nil {
			t.Fatal(err)
		}
	}
}
func mediaRemovalHTTPRequest(t *testing.T, x *recoveryHTTP, method, suffix string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	return nativeJSONRequest(x.f.f.handler, method, mediaRemovalEndpoint+suffix, x.f.f.token, raw)
}
func mediaRemovalHTTPInput() map[string]any {
	return map[string]any{"historyIds": []string{"1"}, "flag": "del_all", "paths": []any{}, "includeNfo": true, "cleanupDirectories": false, "requestId": strings.Repeat("a", 32), "decision": "complete independently reviewed source and library removal", "confirmRead": true}
}
func mediaRemovalHTTPGrants() map[string]any {
	return map[string]any{"confirm": true, "confirmRead": true, "confirmHistoryDeletion": true, "confirmDedupeReset": true, "confirmPermanentDeletion": true, "confirmOnlyCopyRisk": true, "confirmExpandedScope": true, "confirmOldExecutorsStopped": true, "confirmCloseRelated": true, "confirmRemoteExclusive": true}
}
func mediaRemovalHTTPCreate(t *testing.T, x *recoveryHTTP, in map[string]any) organization.MediaRemovalReview {
	t.Helper()
	p := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/preview", in))
	body := mergeRecovery(in, mediaRemovalHTTPGrants())
	body["requestId"], body["fingerprint"] = p.RequestID, p.Fingerprint
	return recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "", body))
}
func mediaRemovalHTTPReview(t *testing.T, x *recoveryHTTP, id string) organization.MediaRemovalReview {
	t.Helper()
	return recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "GET", "/"+id, nil))
}
func mediaRemovalHTTPExists(t *testing.T, x *recoveryHTTP, name string) bool {
	t.Helper()
	if x.remote != nil {
		_, yes := x.remote.read(t, name)
		return yes
	}
	_, err := os.Lstat(path.Join(x.root, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestMediaRemovalHTTPAllProvidersWholeBatchReviewSaveRestartAndSingleExecution(t *testing.T) {
	for _, driver := range []string{"", "minio", "rclone"} {
		t.Run(driver, func(t *testing.T) {
			x := recoveryHTTPFixture(t, driver)
			mediaRemovalHTTPHistory(t, x, 1)
			libraryWriteHTTP(t, x.f.source, "original.mkv", "source")
			libraryWriteHTTP(t, x.f.source, "original.nfo", "source metadata")
			x.put(t, "Movie/movie.mkv", "library")
			x.put(t, "Movie/movie.nfo", "library metadata")
			x.put(t, "Movie/keep.jpg", "not implicitly removed")
			in := mediaRemovalHTTPInput()
			p := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/preview", in))
			if len(p.Units) != 4 || p.DedupeRows != 2 {
				t.Fatal(p)
			}
			for _, key := range []string{"confirmHistoryDeletion", "confirmDedupeReset", "confirmExpandedScope", "confirmOnlyCopyRisk", "confirmCloseRelated", "confirmOldExecutorsStopped"} {
				body := mergeRecovery(in, mediaRemovalHTTPGrants())
				body["requestId"], body["fingerprint"] = p.RequestID, p.Fingerprint
				body[key] = false
				if r := mediaRemovalHTTPRequest(t, x, "POST", "", body); r.Code < 400 {
					t.Fatal("missing grant accepted", key, r.Code)
				}
			}
			v := mediaRemovalHTTPCreate(t, x, in)
			if v.State != "ready" || !mediaRemovalHTTPExists(t, x, "Movie/movie.mkv") {
				t.Fatal(v)
			}
			if x.remote != nil {
				x.remote.restart(t)
			} else {
				monitorRebuild(t, x.f.f)
			}
			if !mediaRemovalHTTPExists(t, x, "Movie/movie.mkv") {
				t.Fatal("restart performed deletion")
			}
			v = mediaRemovalHTTPReview(t, x, v.ID)
			if !v.CanContinue {
				t.Fatal(v.Reason)
			}
			body := mergeRecovery(mediaRemovalHTTPGrants(), map[string]any{"fingerprint": v.Fingerprint})
			done := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/"+v.ID+"/continue", body))
			if done.State != "completed" {
				t.Fatal(done)
			}
			if mediaRemovalHTTPExists(t, x, "Movie/movie.mkv") || mediaRemovalHTTPExists(t, x, "Movie/movie.nfo") || !mediaRemovalHTTPExists(t, x, "Movie/keep.jpg") {
				t.Fatal("wrong library scope")
			}
			if _, err := os.Lstat(path.Join(x.f.source, "original.mkv")); !os.IsNotExist(err) {
				t.Fatal("source retained", err)
			}
			var count int
			if err := x.f.f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
			events, err := x.f.f.runner.organizer.jobs.BusinessEvents().List(t.Context())
			if err != nil || len(events) != 2 {
				t.Fatal(events, err)
			}
			for _, event := range events {
				if event.Kind != "source_file_deleted" && event.Kind != "library_file_deleted" {
					t.Fatal(event.Kind)
				}
			}
			// Same explicit key is receipt-only, including when the original source
			// and remote account no longer exist; it cannot send another delete.
			before := 0
			if x.remote != nil && x.remote.cloud != nil {
				before = len(x.remote.cloud.Requests())
			}
			if x.remote != nil {
				if err := os.WriteFile(x.remote.account, []byte("revoked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			repeated := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/"+v.ID+"/continue", body))
			if repeated.State != "completed" {
				t.Fatal(repeated)
			}
			if x.remote != nil && x.remote.cloud != nil && len(x.remote.cloud.Requests()) != before {
				t.Fatal("receipt retry touched revoked account")
			}
			terminal := mediaRemovalHTTPReview(t, x, v.ID)
			if terminal.State != "completed" {
				t.Fatal(terminal)
			}
		})
	}
}
func TestMediaRemovalHTTPLegacyRecordOnlyKeepsFilesAndExactRowsAndRejectsBlindFileDeletion(t *testing.T) {
	x := recoveryHTTPFixture(t, "")
	mediaRemovalHTTPHistory(t, x, 1)
	x.put(t, "Movie/movie.mkv", "keep")
	r := httptest.NewRequest("POST", "/api/v1/organization/history/delete", strings.NewReader(url.Values{"logids": {"1"}}.Encode()))
	r.Header.Set("Authorization", x.f.f.token)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	x.f.f.handler.ServeHTTP(w, r)
	v := recoveryData[organization.MediaRemovalReview](t, w)
	if v.State != "completed" || !mediaRemovalHTTPExists(t, x, "Movie/movie.mkv") {
		t.Fatal(v)
	}
	mediaRemovalHTTPHistory(t, x, 2)
	r = nativeLegacyRequest("/api/v1/organization/history/delete", x.f.f.token, `{"logids":[2],"flag":"del_dest"}`)
	w = httptest.NewRecorder()
	x.f.f.handler.ServeHTTP(w, r)
	if w.Code < 400 {
		t.Fatal("blind legacy file deletion accepted", w.Body.String())
	}
	var count int
	_ = x.f.f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE ID=2`).Scan(&count)
	if count != 1 || !mediaRemovalHTTPExists(t, x, "Movie/movie.mkv") {
		t.Fatal("failed legacy request mutated data")
	}
}
func nativeLegacyRequest(url, token, body string) *http.Request {
	r := httptest.NewRequest("POST", url, strings.NewReader(body))
	r.Header.Set("Authorization", token)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestMediaRemovalHTTPExplicitCleanupIncludesEveryNonMediaMemberButKeepsOtherEpisodes(t *testing.T) {
	x := recoveryHTTPFixture(t, "")
	mediaRemovalHTTPHistory(t, x, 1)
	x.put(t, "Show/Season 01/Show.S01E01.mkv", "episode one")
	x.put(t, "Show/Season 01/Show.S01E01.nfo", "one metadata")
	x.put(t, "Show/Season 01/poster.jpg", "season art")
	x.put(t, "Show/Season 02/Show.S02E01.mkv", "episode two")
	x.put(t, "Show/tvshow.nfo", "show metadata")
	in := map[string]any{"historyIds": []string{}, "flag": "", "paths": []mediaRemovalPath{{RootID: x.f.input.RootID, Path: "Show/Season 01/Show.S01E01.mkv"}}, "includeNfo": true, "cleanupDirectories": true, "decision": "explicit first-season cleanup including complete remaining season contents", "confirmRead": true}
	v := mediaRemovalHTTPCreate(t, x, in)
	if len(v.Units) != 1 || v.Units[0].Path != "Show/Season 01" || v.Units[0].Files != 3 {
		t.Fatal(v)
	}
	v = mediaRemovalHTTPReview(t, x, v.ID)
	done := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/"+v.ID+"/continue", mergeRecovery(mediaRemovalHTTPGrants(), map[string]any{"fingerprint": v.Fingerprint})))
	if done.State != "completed" || mediaRemovalHTTPExists(t, x, "Show/Season 01") || !mediaRemovalHTTPExists(t, x, "Show/Season 02/Show.S02E01.mkv") || !mediaRemovalHTTPExists(t, x, "Show/tvshow.nfo") {
		t.Fatal(done)
	}
	var count int
	_ = x.f.f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE ID=1`).Scan(&count)
	if count != 1 {
		t.Fatal("file-only request deleted history")
	}
}
func TestMediaRemovalHTTPLegacyMissingDestinationUsesActualNamingAndEpisodeRange(t *testing.T) {
	x := recoveryHTTPFixture(t, "")
	mediaRemovalHTTPHistory(t, x, 1)
	if _, err := x.f.f.db.Exec(`UPDATE TRANSFER_HISTORY SET TYPE='电视剧',TITLE='Show',SEASON_EPISODE='S01 E01',DEST_PATH=NULL,DEST_FILENAME=NULL WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	x.put(t, "Show (2026)/Season 01/Show.S01E01.mkv", "first")
	x.put(t, "Show (2026)/Season 01/Show.S01E02.mkv", "second")
	in := mediaRemovalHTTPInput()
	in["flag"] = "del_dest"
	in["includeNfo"] = false
	v := mediaRemovalHTTPCreate(t, x, in)
	if len(v.Units) != 1 || v.Units[0].Path != "Show (2026)/Season 01/Show.S01E01.mkv" {
		t.Fatal(v)
	}
	v = mediaRemovalHTTPReview(t, x, v.ID)
	done := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/"+v.ID+"/continue", mergeRecovery(mediaRemovalHTTPGrants(), map[string]any{"fingerprint": v.Fingerprint})))
	if done.State != "completed" || mediaRemovalHTTPExists(t, x, "Show (2026)/Season 01/Show.S01E01.mkv") || !mediaRemovalHTTPExists(t, x, "Show (2026)/Season 01/Show.S01E02.mkv") {
		t.Fatal(done)
	}
}
func TestMediaRemovalHTTPRejectsStaleScopePathsAuthAndPrivateChildBypass(t *testing.T) {
	x := recoveryHTTPFixture(t, "")
	mediaRemovalHTTPHistory(t, x, 1)
	x.put(t, "Movie/movie.mkv", "old")
	in := mediaRemovalHTTPInput()
	in["flag"] = "del_dest"
	in["includeNfo"] = false
	p := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/preview", in))
	x.put(t, "Movie/movie.mkv", "new data")
	body := mergeRecovery(in, mediaRemovalHTTPGrants())
	body["requestId"], body["fingerprint"] = p.RequestID, p.Fingerprint
	if r := mediaRemovalHTTPRequest(t, x, "POST", "", body); r.Code < 400 {
		t.Fatal("stale content accepted")
	}
	for _, bad := range []string{".", "..", "../outside", "/absolute", "Movie/../movie.mkv"} {
		b := map[string]any{"historyIds": []string{}, "paths": []mediaRemovalPath{{RootID: x.f.input.RootID, Path: bad}}, "confirmRead": true, "decision": "invalid scope"}
		if r := mediaRemovalHTTPRequest(t, x, "POST", "/preview", b); r.Code < 400 {
			t.Fatal("invalid path accepted", bad)
		}
	}
	if r := nativeJSONRequest(x.f.f.handler, "GET", mediaRemovalEndpoint, "", ""); r.Code != 401 {
		t.Fatal("missing admin token accepted", r.Code)
	}
	v := mediaRemovalHTTPCreate(t, x, in)
	if v.Units[0].Disposal == nil {
		t.Fatal("missing internal child")
	}
	child := v.Units[0].Disposal.ID
	for _, action := range []string{"", "/continue", "/close", "/return-capture"} {
		method := "GET"
		var b any
		if action != "" {
			method = "POST"
			b = recoveryGrant()
		}
		if r := x.request(t, method, "/disposals/"+child+action, b); r.Code != 404 {
			t.Fatal("private recovery endpoint exposed media child", action, r.Code)
		}
	}
	x.put(t, "Movie/other.txt", "not selected")
	v = mediaRemovalHTTPReview(t, x, v.ID)
	if !v.CanContinue {
		t.Fatal("unselected sibling invalidated file-only scope", v.Reason)
	}
	if err := config.NewStore(x.f.f.path).Update(map[string]any{"media.min_filesize": 1}); err != nil {
		t.Fatal(err)
	}
	if r := mediaRemovalHTTPRequest(t, x, "POST", "/"+v.ID+"/continue", mergeRecovery(mediaRemovalHTTPGrants(), map[string]any{"fingerprint": v.Fingerprint})); r.Code < 400 {
		t.Fatal("revoked config continued")
	}
	if x.read(t, "Movie/movie.mkv") != "new data" {
		t.Fatal("stale or revoked scope changed")
	}
}
func TestMediaRemovalHTTPStopCancelsInFlightReadWithoutWaitingForTheWorkingGuard(t *testing.T) {
	x := recoveryHTTPFixture(t, "minio")
	mediaRemovalHTTPHistory(t, x, 1)
	x.put(t, "Movie/movie.mkv", "movie")
	in := mediaRemovalHTTPInput()
	in["flag"] = "del_dest"
	in["includeNfo"] = false
	v := mediaRemovalHTTPCreate(t, x, in)
	v = mediaRemovalHTTPReview(t, x, v.ID)
	entered := make(chan struct{})
	x.remote.cloud.BeforeNextRead(func(ctx context.Context) { close(entered); <-ctx.Done() })
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- mediaRemovalHTTPRequest(t, x, "POST", "/"+v.ID+"/continue", mergeRecovery(mediaRemovalHTTPGrants(), map[string]any{"fingerprint": v.Fingerprint}))
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("long read not reached")
	}
	stopped := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/"+v.ID+"/close", map[string]any{"confirm": true, "confirmClose": true}))
	if stopped.State != "closed" {
		t.Fatal(stopped)
	}
	select {
	case r := <-done:
		if r.Code < 400 {
			t.Fatal("cancelled read succeeded", r.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop waited for long-read work lock")
	}
	if x.read(t, "Movie/movie.mkv") != "movie" {
		t.Fatal("stop deleted data")
	}
	var count int
	_ = x.f.f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE ID=1`).Scan(&count)
	if count != 1 {
		t.Fatal("stop removed history")
	}
}

func TestMediaRemovalHTTPLegacyFileListBecomesTheSameNativeExplicitCoordinator(t *testing.T) {
	x := recoveryHTTPFixture(t, "")
	x.put(t, "Movie/movie.mkv", "library")
	body := map[string]any{"files": []string{path.Join(x.root, "Movie/movie.mkv")}, "driver": "", "decision": "exact legacy file selection with new independent review", "confirmRead": true}
	raw, _ := json.Marshal(body)
	r := nativeJSONRequest(x.f.f.handler, "POST", "/api/v1/organization/files/delete/plan", x.f.f.token, string(raw))
	p := recoveryData[organization.MediaRemovalReview](t, r)
	if len(p.Units) != 1 || p.Units[0].Path != "Movie/movie.mkv" || len(p.HistoryIDs) != 0 {
		t.Fatal(p)
	}
	for k, v := range mediaRemovalHTTPGrants() {
		body[k] = v
	}
	body["requestId"], body["fingerprint"] = p.RequestID, p.Fingerprint
	raw, _ = json.Marshal(body)
	r = nativeJSONRequest(x.f.f.handler, "POST", "/api/v1/organization/files/delete", x.f.f.token, string(raw))
	saved := recoveryData[organization.MediaRemovalReview](t, r)
	if saved.State != "ready" || !mediaRemovalHTTPExists(t, x, "Movie/movie.mkv") {
		t.Fatal("legacy save performed deletion", saved)
	}
	v := mediaRemovalHTTPReview(t, x, saved.ID)
	done := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/"+v.ID+"/continue", mergeRecovery(mediaRemovalHTTPGrants(), map[string]any{"fingerprint": v.Fingerprint})))
	if done.State != "completed" || mediaRemovalHTTPExists(t, x, "Movie/movie.mkv") {
		t.Fatal(done)
	}
	events, err := x.f.f.runner.organizer.jobs.BusinessEvents().List(t.Context())
	if err != nil || len(events) != 0 {
		t.Fatal("arbitrary file selection forged historical media events", events, err)
	}
}

func TestMediaRemovalHTTPAggregateBoundsAndMergedMissingHistoryNeverInventsAnEvent(t *testing.T) {
	units := []mediaRemovalReadUnit{{unit: organization.MediaRemovalUnit{Definition: organization.RecoveryDefinition{Nodes: make([]organization.RecoveryNode, 10001)}}}, {unit: organization.MediaRemovalUnit{Definition: organization.RecoveryDefinition{Nodes: make([]organization.RecoveryNode, 10000)}}}}
	if err := mediaRemovalReadBudget(units); err == nil {
		t.Fatal("per-unit-only object limit")
	}
	units = []mediaRemovalReadUnit{{unit: organization.MediaRemovalUnit{Definition: organization.RecoveryDefinition{Nodes: []organization.RecoveryNode{{Kind: "file", Node: organization.LibraryNode{Size: organization.RecoveryByteLimit}}}}}}, {unit: organization.MediaRemovalUnit{Definition: organization.RecoveryDefinition{Nodes: []organization.RecoveryNode{{Kind: "file", Node: organization.LibraryNode{Size: 1}}}}}}}
	if err := mediaRemovalReadBudget(units); err == nil {
		t.Fatal("aggregate byte overflow accepted")
	}
	x := recoveryHTTPFixture(t, "")
	mediaRemovalHTTPHistory(t, x, 1)
	x.put(t, "Movie/unknown.txt", "full explicit folder")
	in := mediaRemovalHTTPInput()
	in["flag"] = "del_dest"
	in["paths"] = []mediaRemovalPath{{RootID: x.f.input.RootID, Path: "Movie"}}
	in["includeNfo"] = false
	v := mediaRemovalHTTPCreate(t, x, in)
	if len(v.Units) != 1 || len(v.Units[0].Events) != 0 {
		t.Fatal("missing history file adopted after folder merge", v)
	}
	v = mediaRemovalHTTPReview(t, x, v.ID)
	done := recoveryData[organization.MediaRemovalReview](t, mediaRemovalHTTPRequest(t, x, "POST", "/"+v.ID+"/continue", mergeRecovery(mediaRemovalHTTPGrants(), map[string]any{"fingerprint": v.Fingerprint})))
	if done.State != "completed" {
		t.Fatal(done)
	}
	events, err := x.f.f.runner.organizer.jobs.BusinessEvents().List(t.Context())
	if err != nil || len(events) != 0 {
		t.Fatal("already missing file emitted successful deletion", events, err)
	}
}
