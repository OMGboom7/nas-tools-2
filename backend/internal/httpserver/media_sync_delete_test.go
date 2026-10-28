package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediasyncdel"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
)

const mediaDeleteEndpoint = "/api/v1/media-sync-delete"

type mediaDeleteFixture struct {
	f                      *subscriptionRunFixture
	source, target, rootID string
	hooks                  *webhookHTTPFixture
	reads, sends           int
	beforeMedia            func()
	lostNotify             bool
}

func newMediaDeleteFixture(t *testing.T, source bool) *mediaDeleteFixture {
	t.Helper()
	f, src, dst, input := organizationFixture(t)
	x := &mediaDeleteFixture{f: f, source: src, target: dst, rootID: input.SourceID, hooks: &webhookHTTPFixture{subscriptionRunFixture: f}}
	previous := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "sync-delete.test" {
			x.reads++
			if x.beforeMedia != nil {
				x.beforeMedia()
			}
			if r.Method != "GET" || r.Header.Get("X-Emby-Token") != "private-sync-key" {
				t.Fatal("unexpected media operation", r.Method)
			}
			return jsonResponse(r, `{"Id":"sync-server"}`), nil
		}
		if r.URL.Hostname() == "sync-notify.test" {
			x.sends++
			if x.lostNotify {
				return nil, context.DeadlineExceeded
			}
			return jsonResponse(r, `{}`), nil
		}
		return previous.RoundTrip(r)
	})
	if err := config.NewStore(f.path).Update(map[string]any{"media.media_server": "emby", "emby.host": "https://sync-delete.test", "emby.api_key": "private-sync-key", "security.check_apikey": true, "security.media_server_webhook_allow_ip": map[string]any{"ipv4": "192.0.2.0/24", "ipv6": "2001:db8::/32"}}); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, f)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS TRANSFER_HISTORY(ID INTEGER PRIMARY KEY,MODE TEXT,TYPE TEXT,CATEGORY TEXT,TMDBID INTEGER,TITLE TEXT,YEAR TEXT,SEASON_EPISODE TEXT,SOURCE TEXT,SOURCE_PATH TEXT,SOURCE_FILENAME TEXT,DEST TEXT,DEST_PATH TEXT,DEST_FILENAME TEXT,DATE TEXT)`,
		`CREATE TABLE IF NOT EXISTS TRANSFER_BLACKLIST(ID INTEGER PRIMARY KEY,PATH TEXT)`,
		`CREATE TABLE IF NOT EXISTS SYNC_HISTORY(ID INTEGER PRIMARY KEY,PATH TEXT,SRC TEXT,DEST TEXT)`,
	} {
		if _, err := f.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if w := mediaDeleteHTTP(t, x, "POST", "/api/v1/plugins/MediaSyncDel/install", map[string]any{}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	mediaDeleteSave(t, x, map[string]any{"enable": true, "del_source": source, "send_notify": false})
	return x
}
func mediaDeleteHTTP(t *testing.T, x *mediaDeleteFixture, method, uri string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return mediaRefreshHTTP(t, x.f, method, uri, body)
}
func mediaDeleteStatusHTTP(t *testing.T, x *mediaDeleteFixture) mediaSyncDeleteStatus {
	t.Helper()
	return recoveryData[mediaSyncDeleteStatus](t, mediaDeleteHTTP(t, x, "GET", mediaDeleteEndpoint, nil))
}
func mediaDeleteSave(t *testing.T, x *mediaDeleteFixture, values map[string]any) {
	t.Helper()
	d := recoveryData[pluginConfigDetail](t, mediaDeleteHTTP(t, x, "GET", "/api/v1/plugins/MediaSyncDel", nil))
	w := mediaDeleteHTTP(t, x, "PUT", "/api/v1/plugins/MediaSyncDel", map[string]any{"fingerprint": d.Meta.Fingerprint, "values": values})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func mediaDeleteReviewHTTP(t *testing.T, x *mediaDeleteFixture) mediaSyncDeleteReview {
	t.Helper()
	s := mediaDeleteStatusHTTP(t, x)
	roots := []string{}
	if s.Spec.DeleteSource {
		roots = append(roots, x.rootID)
	}
	return recoveryData[mediaSyncDeleteReview](t, mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirmRead": true, "rootIds": roots}))
}
func mediaDeleteGrants() map[string]any {
	return map[string]any{"confirm": true, "history": true, "dedupe": true, "closeRelated": true, "source": true, "onlyCopy": true, "expanded": true, "legacyStopped": true, "notify": true}
}
func mediaDeleteEnable(t *testing.T, x *mediaDeleteFixture) {
	t.Helper()
	v := mediaDeleteReviewHTTP(t, x)
	in := mediaDeleteGrants()
	in["id"], in["fingerprint"] = v.ID, v.Fingerprint
	if w := mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/authorize", in); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func mediaDeleteHistory(t *testing.T, x *mediaDeleteFixture, id int64, kind, episode, filename string, tmdb int) {
	t.Helper()
	if _, err := x.f.db.Exec(`INSERT INTO TRANSFER_HISTORY(ID,MODE,TYPE,CATEGORY,TMDBID,TITLE,YEAR,SEASON_EPISODE,SOURCE,SOURCE_PATH,SOURCE_FILENAME,DEST,DEST_PATH,DEST_FILENAME,DATE)VALUES(?,'复制',?,'Original',?,'Original','2026',?,'manual',?,?,?, ?,?,'2026-10-28')`, id, kind, tmdb, episode, path.Join(x.source, "Movie"), "original-"+strconv.FormatInt(id, 10)+".mkv", x.target, path.Join(x.target, "Movie"), filename); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`INSERT INTO TRANSFER_BLACKLIST VALUES(?,?)`, `INSERT INTO SYNC_HISTORY(ID,PATH)VALUES(?,?)`} {
		if _, err := x.f.db.Exec(q, id, path.Join(x.source, "Movie", "original-"+strconv.FormatInt(id, 10)+".mkv")); err != nil {
			t.Fatal(err)
		}
	}
}
func mediaDeleteBody(kind, filename string, season, episode, tmdb int) string {
	b, _ := json.Marshal(map[string]any{"event_type": "media_del", "item_isvirtual": "False", "media_type": kind, "media_name": "原媒体", "media_path": "/emby/" + filename, "tmdb_id": strconv.Itoa(tmdb), "season_num": strconv.Itoa(season), "episode_num": strconv.Itoa(episode)})
	return string(b)
}
func mediaDeleteReceive(t *testing.T, x *mediaDeleteFixture, key, body string) string {
	t.Helper()
	return webhookIDHTTP(t, webhookReceiveHTTP(x.hooks, "emby", body, key))
}
func mediaDeleteRows(t *testing.T, x *mediaDeleteFixture) []mediasyncdel.Receipt {
	t.Helper()
	return recoveryData[[]mediasyncdel.Receipt](t, mediaDeleteHTTP(t, x, "GET", mediaDeleteEndpoint+"/receipts", nil))
}
func mediaDeleteTick(t *testing.T, x *mediaDeleteFixture) {
	t.Helper()
	if err := x.f.runner.mediaSyncDelete.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func mediaDeleteHistoryCount(t *testing.T, x *mediaDeleteFixture) int {
	t.Helper()
	var count int
	if err := x.f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestMediaSyncDeleteActualOldGetAndPostSelectorsDoNotReprocessPriorCallbacks(t *testing.T) {
	for _, kind := range []string{"Movie", "Series", "Season", "Episode"} {
		t.Run(kind, func(t *testing.T) {
			x := newMediaDeleteFixture(t, false)
			mediaDeleteHistory(t, x, 1, "电影", "", "movie.mkv", 100)
			mediaDeleteHistory(t, x, 2, "电影", "", "other.mkv", 100)
			mediaDeleteHistory(t, x, 3, "电视剧", "S01 E01", "one.mkv", 100)
			mediaDeleteHistory(t, x, 4, "电视剧", "S02 E03", "episode.mkv", 100)
			mediaDeleteHistory(t, x, 5, "电视剧", "S020 E01", "twenty.mkv", 100)
			webhookAuthorizeHTTP(t, x.hooks)
			filename := "movie.mkv"
			if kind == "Episode" {
				filename = "episode.mkv"
			}
			body := mediaDeleteBody(kind, filename, 2, 3, 100)
			prior := mediaDeleteReceive(t, x, "before-grant", body)
			if len(mediaDeleteRows(t, x)) != 0 {
				t.Fatal("old flag enabled automatically")
			}
			mediaDeleteEnable(t, x)
			if repeated := mediaDeleteReceive(t, x, "before-grant", body); repeated != prior {
				t.Fatal("original callback changed")
			}
			if len(mediaDeleteRows(t, x)) != 0 {
				t.Fatal("new grant adopted old callback")
			}
			var callback string
			if kind == "Season" {
				values := url.Values{"apikey": {"native-services-secret"}, "event_type": {"media_del"}, "item_isvirtual": {"False"}, "media_type": {kind}, "media_path": {"/emby/Show/Season2"}, "media_name": {"原媒体"}, "tmdb_id": {"100"}, "season_num": {"2"}, "EventId": {"get-original"}}
				r := httptest.NewRequest("GET", "/emby?"+values.Encode(), nil)
				w := httptest.NewRecorder()
				x.f.handler.ServeHTTP(w, r)
				callback = webhookIDHTTP(t, w)
			} else {
				callback = mediaDeleteReceive(t, x, "future", body)
				if repeat := mediaDeleteReceive(t, x, "future", body); repeat != callback {
					t.Fatal("duplicate identity changed")
				}
			}
			if mediaDeleteHistoryCount(t, x) != 5 {
				t.Fatal("receiver deleted synchronously")
			}
			mediaDeleteTick(t, x)
			rows := mediaDeleteRows(t, x)
			wantCount := 1
			if kind == "Series" {
				wantCount = 3
			}
			if len(rows) != 1 || rows[0].State != "completed" || rows[0].Callback != callback || rows[0].Count != wantCount || mediaDeleteHistoryCount(t, x) != 5-wantCount {
				t.Fatal(rows, mediaDeleteHistoryCount(t, x))
			}
			monitorRebuild(t, x.f)
			mediaDeleteTick(t, x)
			if len(mediaDeleteRows(t, x)) != 1 || mediaDeleteHistoryCount(t, x) != 5-wantCount {
				t.Fatal("restarted old deletion")
			}
		})
	}
}
func TestMediaSyncDeleteWholeSeriesOverHundredAndExactTypedRows(t *testing.T) {
	x := newMediaDeleteFixture(t, false)
	for i := 1; i <= 151; i++ {
		mediaDeleteHistory(t, x, int64(i), "电视剧", fmt.Sprintf("S01 E%02d", i), "episode-"+strconv.Itoa(i)+".mkv", 200)
	}
	mediaDeleteHistory(t, x, 200, "电影", "", "movie.mkv", 200)
	webhookAuthorizeHTTP(t, x.hooks)
	mediaDeleteEnable(t, x)
	mediaDeleteReceive(t, x, "large-series", mediaDeleteBody("Series", "Show", 0, 0, 200))
	mediaDeleteTick(t, x)
	rows := mediaDeleteRows(t, x)
	if len(rows) != 1 || rows[0].Count != 151 || rows[0].State != "completed" || mediaDeleteHistoryCount(t, x) != 1 {
		t.Fatal(rows, mediaDeleteHistoryCount(t, x))
	}
	r := mediaDeleteHTTP(t, x, "GET", "/api/v1/organization/media-removals/"+rows[0].Parent, nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"mediaSyncReceipt"`) {
		t.Fatal("shared recovery projection lost original link", r.Code, r.Body.String())
	}
}
func TestMediaSyncDeleteSourceNfoCleanupOnlyApprovedOriginalSourceAndNeverLibraryTarget(t *testing.T) {
	x := newMediaDeleteFixture(t, true)
	mediaDeleteHistory(t, x, 1, "电影", "", "movie.mkv", 100)
	for _, file := range []string{"Movie/original-1.mkv", "Movie/original-1.nfo", "Movie/cover.jpg"} {
		libraryWriteHTTP(t, x.source, file, "owned temporary source")
	}
	libraryWriteHTTP(t, x.target, "Movie/movie.mkv", "Emby library target remains")
	webhookAuthorizeHTTP(t, x.hooks)
	mediaDeleteEnable(t, x)
	mediaDeleteReceive(t, x, "source", mediaDeleteBody("Movie", "movie.mkv", 0, 0, 100))
	mediaDeleteTick(t, x)
	rows := mediaDeleteRows(t, x)
	if len(rows) != 1 || rows[0].State != "completed" || mediaDeleteHistoryCount(t, x) != 0 {
		t.Fatal(rows)
	}
	if _, err := os.Lstat(path.Join(x.source, "Movie")); !os.IsNotExist(err) {
		t.Fatal("original cleanup scope retained", err)
	}
	if b, err := os.ReadFile(path.Join(x.target, "Movie/movie.mkv")); err != nil || string(b) != "Emby library target remains" {
		t.Fatal("library was deleted", err)
	}
}
func TestMediaSyncDeleteVirtualMissingAndExclusionsNeverOpenSourceOrClearHistory(t *testing.T) {
	for _, mode := range []string{"virtual", "missing", "excluded", "prefix-sibling"} {
		t.Run(mode, func(t *testing.T) {
			x := newMediaDeleteFixture(t, false)
			mediaDeleteHistory(t, x, 1, "电影", "", "movie.mkv", 100)
			if mode == "excluded" || mode == "prefix-sibling" {
				mediaDeleteSave(t, x, map[string]any{"exclude_path": "/emby"})
			}
			webhookAuthorizeHTTP(t, x.hooks)
			mediaDeleteEnable(t, x)
			body := mediaDeleteBody("Movie", "movie.mkv", 0, 0, 100)
			if mode == "virtual" {
				body = `{"event_type":"media_del","item_isvirtual":"True"}`
			}
			if mode == "missing" {
				body = `{"event_type":"media_del","media_type":"Movie"}`
			}
			if mode == "prefix-sibling" {
				body = strings.Replace(body, "/emby/", "/emby2/", 1)
			}
			mediaDeleteReceive(t, x, mode, body)
			mediaDeleteTick(t, x)
			rows := mediaDeleteRows(t, x)
			if mode == "prefix-sibling" {
				if rows[0].State != "completed" || mediaDeleteHistoryCount(t, x) != 0 {
					t.Fatal(rows)
				}
				return
			}
			if mediaDeleteHistoryCount(t, x) != 1 || len(rows) != 1 || rows[0].Parent != "" {
				t.Fatal(rows)
			}
			if mode == "missing" {
				status := mediaDeleteStatusHTTP(t, x)
				if status.Spec.Enabled || status.Active || rows[0].State != "blocked" {
					t.Fatal(status, rows)
				}
			}
		})
	}
}
func TestMediaSyncDeleteMetadataRollbackDoesNotResumeAndExplicitOriginalContinueCommitsNotificationOnce(t *testing.T) {
	x := newMediaDeleteFixture(t, false)
	mediaDeleteHistory(t, x, 1, "电影", "", "movie.mkv", 100)
	mediaDeleteSave(t, x, map[string]any{"send_notify": true})
	if _, err := x.f.db.Exec(`INSERT INTO MESSAGE_CLIENT(ID,NAME,TYPE,CONFIG,SWITCHS,ENABLED,INTERACTIVE) VALUES(88,'custom','webhook','{"url":"https://sync-notify.test","method":"POST"}','["custom_message"]',1,0)`); err != nil {
		t.Fatal(err)
	}
	webhookAuthorizeHTTP(t, x.hooks)
	mediaDeleteEnable(t, x)
	c, _, _, err := x.f.runner.mediaSyncDelete.queue.Scope(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c.Authorized = false
	raw, _ := json.Marshal(c)
	query := `CREATE TRIGGER revoke_at_final AFTER DELETE ON TRANSFER_HISTORY BEGIN UPDATE SYSTEM_DICT SET VALUE='` + strings.ReplaceAll(string(raw), "'", "''") + `' WHERE KEY='GoMediaSyncDel';END`
	if _, err := x.f.db.Exec(query); err != nil {
		t.Fatal(err)
	}
	mediaDeleteReceive(t, x, "atomic", mediaDeleteBody("Movie", "movie.mkv", 0, 0, 100))
	if err := x.f.runner.mediaSyncDelete.tick(t.Context()); err == nil {
		t.Fatal("revoked completion committed")
	}
	rows := mediaDeleteRows(t, x)
	if len(rows) != 1 || rows[0].State != "needs_review" || mediaDeleteHistoryCount(t, x) != 1 {
		t.Fatal(rows, mediaDeleteHistoryCount(t, x))
	}
	events, err := bus.New(x.f.db).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "plugin_message" {
			t.Fatal("failed deletion sent completion")
		}
	}
	monitorRebuild(t, x.f)
	mediaDeleteTick(t, x)
	if mediaDeleteHistoryCount(t, x) != 1 {
		t.Fatal("restart resumed old plan")
	}
	if _, err := x.f.db.Exec(`DROP TRIGGER revoke_at_final`); err != nil {
		t.Fatal(err)
	}
	status := mediaDeleteStatusHTTP(t, x)
	if w := mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/stop", map[string]any{"fingerprint": status.Fingerprint, "confirm": true}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	review := recoveryData[mediaSyncDeleteExecutionReview](t, mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/receipts/"+rows[0].ID+"/review", map[string]any{"confirmRead": true}))
	if !review.Parent.CanContinue {
		t.Fatal("original metadata-only recovery unavailable", review.Parent)
	}
	alternate := mediaRemovalHTTPGrants()
	alternate["fingerprint"] = review.Parent.Fingerprint
	if w := mediaDeleteHTTP(t, x, "POST", "/api/v1/organization/media-removals/"+rows[0].Parent+"/continue", alternate); w.Code != 409 {
		t.Fatal("generic continuation bypassed original notification consent", w.Code, w.Body.String())
	}
	in := mediaDeleteGrants()
	in["confirmRead"], in["fingerprint"] = true, review.Parent.Fingerprint
	current := recoveryData[mediasyncdel.Receipt](t, mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/receipts/"+rows[0].ID+"/continue", in))
	if current.State != "completed" || current.ID != rows[0].ID || mediaDeleteHistoryCount(t, x) != 0 {
		t.Fatal(current)
	}
	events, err = bus.New(x.f.db).List(t.Context())
	if err != nil || len(events) != 1 || events[0].Kind != "plugin_message" || len(events[0].Deliveries) != 1 || events[0].Deliveries[0].ChannelID != 88 {
		t.Fatal(events, err)
	}
	x.lostNotify = true
	if err := x.f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, x.f)
	if err := x.f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if x.sends != 1 {
		t.Fatal("unknown completion notification repeated", x.sends)
	}
}
func TestMediaSyncDeleteIndependentConsentsAndLegacyEditsAlwaysStop(t *testing.T) {
	x := newMediaDeleteFixture(t, true)
	mediaDeleteSave(t, x, map[string]any{"send_notify": true})
	review := mediaDeleteReviewHTTP(t, x)
	for _, field := range []string{"confirm", "history", "dedupe", "closeRelated", "source", "onlyCopy", "expanded", "legacyStopped", "notify"} {
		in := mediaDeleteGrants()
		in[field] = false
		in["id"], in["fingerprint"] = review.ID, review.Fingerprint
		if w := mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/authorize", in); w.Code != 400 {
			t.Fatal("missing consent accepted", field, w.Code, w.Body.String())
		}
	}
	mediaDeleteEnable(t, x)
	old := `{"enable":true,"del_source":false,"send_notify":false,"unknown":{"n":9007199254740993}}`
	w := performFormRequest(x.f.handler, "/api/v1/plugin/config", x.f.token, url.Values{"id": {"MediaSyncDel"}, "config": {old}})
	if w.Code != 200 || mediaDeleteStatusHTTP(t, x).Active {
		t.Fatal("legacy form enabled deletion", w.Code, w.Body.String())
	}
	mediaDeleteSave(t, x, map[string]any{"send_notify": true})
	raw, _ := x.f.runner.download.system.Get(t.Context(), mediasyncdel.ConfigKey)
	if !strings.Contains(raw, "9007199254740993") {
		t.Fatal("unknown integer lost", raw)
	}
	for _, p := range []string{"/api/v1/plugin/status", "/api/v1/plugin/page"} {
		if w := performFormRequest(x.f.handler, p, x.f.token, url.Values{"id": {"MediaSyncDel"}}); w.Code != 200 {
			t.Fatal(p, w.Code, w.Body.String())
		}
	}
	if w := mediaDeleteHTTP(t, x, "DELETE", "/api/v1/plugins/MediaSyncDel", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := mediaDeleteHTTP(t, x, "POST", "/api/v1/plugins/MediaSyncDel/install", map[string]any{}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if mediaDeleteStatusHTTP(t, x).Active {
		t.Fatal("reinstall adopted old grant")
	}
}
func TestMediaSyncDeleteOriginalScopeChangesBeforeDispatchBlockWithoutDeleting(t *testing.T) {
	for _, change := range []string{"stop", "config", "source-outside", "media-server", "root-user", "root-identity"} {
		t.Run(change, func(t *testing.T) {
			x := newMediaDeleteFixture(t, true)
			mediaDeleteHistory(t, x, 1, "电影", "", "movie.mkv", 100)
			libraryWriteHTTP(t, x.source, "Movie/original-1.mkv", "original temporary source")
			webhookAuthorizeHTTP(t, x.hooks)
			mediaDeleteEnable(t, x)
			mediaDeleteReceive(t, x, "scope", mediaDeleteBody("Movie", "movie.mkv", 0, 0, 100))
			originalSource := x.source
			switch change {
			case "stop":
				s := mediaDeleteStatusHTTP(t, x)
				if w := mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/stop", map[string]any{"fingerprint": s.Fingerprint, "confirm": true}); w.Code != 200 {
					t.Fatal(w.Code)
				}
			case "config":
				mediaDeleteSave(t, x, map[string]any{"send_notify": true})
			case "source-outside":
				outside := t.TempDir()
				libraryWriteHTTP(t, outside, "original-1.mkv", "outside approved source")
				if _, err := x.f.db.Exec(`UPDATE TRANSFER_HISTORY SET SOURCE_PATH=? WHERE ID=1`, outside); err != nil {
					t.Fatal(err)
				}
			case "media-server":
				if err := config.NewStore(x.f.path).Update(map[string]any{"media.media_server": "jellyfin"}); err != nil {
					t.Fatal(err)
				}
			case "root-user":
				if err := config.NewStore(x.f.path).Update(map[string]any{"app.login_password": "new-fixture-private"}); err != nil {
					t.Fatal(err)
				}
			case "root-identity":
				old := x.source + "-original"
				if err := os.Rename(x.source, old); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(old) })
				if err := os.Mkdir(x.source, 0700); err != nil {
					t.Fatal(err)
				}
				originalSource = old
			}
			if err := x.f.runner.mediaSyncDelete.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows, err := x.f.runner.mediaSyncDelete.queue.List(t.Context(), 1, false)
			if err != nil || len(rows) != 1 || rows[0].State != "blocked" || mediaDeleteHistoryCount(t, x) != 1 {
				t.Fatal(rows, err)
			}
			if _, err := os.Lstat(path.Join(originalSource, "Movie/original-1.mkv")); err != nil {
				t.Fatal("original source changed", err)
			}
		})
	}
}
func TestMediaSyncDeletePlainHTTPRootSelectionAndCurrentRootOnly(t *testing.T) {
	x := newMediaDeleteFixture(t, true)
	if err := config.NewStore(x.f.path).Update(map[string]any{"emby.host": "http://sync-delete.test"}); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, x.f)
	s := mediaDeleteStatusHTTP(t, x)
	w := mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirmRead": true, "rootIds": []string{x.rootID}})
	if w.Code != 400 || x.reads != 0 {
		t.Fatal("plaintext credentials sent before consent", w.Code, x.reads)
	}
	w = mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirmRead": true, "rootIds": []string{x.rootID}, "http": true})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, roots := range [][]string{nil, {"other"}, {x.rootID, x.rootID}} {
		w := mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirmRead": true, "rootIds": roots, "http": true})
		if w.Code != 400 {
			t.Fatal("invalid root selection", roots, w.Code)
		}
	}
	w = performFormRequest(x.f.handler, "/api/v1/user/manage", x.f.token, url.Values{"oper": {"add"}, "name": {"delete-viewer"}, "password": {"fixture-password"}, "pris": {""}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	viewer := loginForTest(t, x.f.handler, "delete-viewer", "fixture-password")
	for _, uri := range []string{mediaDeleteEndpoint, mediaDeleteEndpoint + "/receipts", "/api/v1/plugins/MediaSyncDel"} {
		if w := nativeJSONRequest(x.f.handler, "GET", uri, "", ""); w.Code != 401 {
			t.Fatal("anonymous", uri, w.Code)
		}
		if w := nativeJSONRequest(x.f.handler, "GET", uri, viewer, ""); w.Code != 403 {
			t.Fatal("non-root", uri, w.Code)
		}
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: x.f.path, LegacyBackendURL: "http://legacy:3000"}, x.f.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{mediaDeleteEndpoint, "/api/v1/plugins/MediaSyncDel", "/api/v1/plugins/MediaSyncDel/unsupported"} {
		if w := nativeJSONRequest(handler, "GET", uri, x.f.token, ""); w.Code != 501 {
			t.Fatal("hybrid fell through", uri, w.Code, w.Body.String())
		}
	}
	if w := mediaDeleteHTTP(t, x, "POST", "/api/v1/plugins/MediaSyncDel/unsupported", map[string]any{}); w.Code != 405 {
		t.Fatal(w.Code)
	}
}
func TestMediaSyncDeleteNativeSearchAndWorkerCancellationRemainIndependent(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	if w := nativeJSONRequest(f.handler, "POST", "/api/v1/plugins/MediaSyncDel/install", f.token, "{}"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if code, body := f.run("MOV", "1"); code != 200 || f.adds != 1 {
		t.Fatal("native subscriber blocked search", code, body)
	}
	ctx, cancel := context.WithCancel(t.Context())
	wait := startMediaSyncDeleteWorker(ctx, f.runner.mediaSyncDelete)
	cancel()
	wait()
	if f.runner.mediaSyncDelete.workers.Load() != 0 {
		t.Fatal("worker retained after cancellation")
	}
}
func TestMediaSyncDeleteStopDuringOriginalIdentityReadBlocksBeforeSourcePlan(t *testing.T) {
	x := newMediaDeleteFixture(t, true)
	mediaDeleteHistory(t, x, 1, "电影", "", "movie.mkv", 100)
	libraryWriteHTTP(t, x.source, "Movie/original-1.mkv", "original")
	webhookAuthorizeHTTP(t, x.hooks)
	mediaDeleteEnable(t, x)
	mediaDeleteReceive(t, x, "stop-during-read", mediaDeleteBody("Movie", "movie.mkv", 0, 0, 100))
	x.beforeMedia = func() {
		x.beforeMedia = nil
		status := mediaDeleteStatusHTTP(t, x)
		if w := mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/stop", map[string]any{"fingerprint": status.Fingerprint, "confirm": true}); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	mediaDeleteTick(t, x)
	rows := mediaDeleteRows(t, x)
	if len(rows) != 1 || rows[0].Parent != "" || rows[0].State != "blocked" || mediaDeleteHistoryCount(t, x) != 1 {
		t.Fatal(rows)
	}
	if _, err := os.Lstat(path.Join(x.source, "Movie/original-1.mkv")); err != nil {
		t.Fatal("source changed", err)
	}
}
func TestMediaSyncDeletePhysicalPartialStopRequiresFreshOriginalContinueWithoutReplayingDoneUnit(t *testing.T) {
	x := newMediaDeleteFixture(t, true)
	mediaDeleteHistory(t, x, 1, "电影", "", "movie.mkv", 100)
	for _, file := range []string{"Movie/original-1.mkv", "Movie/original-1.nfo", "Movie/keep.mkv"} {
		libraryWriteHTTP(t, x.source, file, "owned temporary "+file)
	}
	webhookAuthorizeHTTP(t, x.hooks)
	mediaDeleteEnable(t, x)
	c, _, _, err := x.f.runner.mediaSyncDelete.queue.Scope(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c.Authorized = false
	raw, _ := json.Marshal(c)
	if _, err := x.f.db.Exec(`CREATE TABLE IF NOT EXISTS GO_MEDIA_REMOVAL_UNITS(REMOVAL_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,CHILD_ID TEXT NOT NULL,STATE TEXT NOT NULL,PRIMARY KEY(REMOVAL_ID,ORDINAL))`); err != nil {
		t.Fatal(err)
	}
	trigger := `CREATE TRIGGER stop_after_first_unit AFTER UPDATE ON GO_MEDIA_REMOVAL_UNITS WHEN NEW.STATE='done' BEGIN UPDATE SYSTEM_DICT SET VALUE='` + strings.ReplaceAll(string(raw), "'", "''") + `' WHERE KEY='GoMediaSyncDel';END`
	if _, err := x.f.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	mediaDeleteReceive(t, x, "partial-source", mediaDeleteBody("Movie", "movie.mkv", 0, 0, 100))
	if err := x.f.runner.mediaSyncDelete.tick(t.Context()); err == nil {
		t.Fatal("stopped authority completed remaining scope")
	}
	rows := mediaDeleteRows(t, x)
	if len(rows) != 1 || rows[0].State != "needs_review" || mediaDeleteHistoryCount(t, x) != 1 {
		t.Fatal(rows)
	}
	absent := 0
	for _, name := range []string{"Movie/original-1.mkv", "Movie/original-1.nfo"} {
		if _, err := os.Lstat(path.Join(x.source, name)); os.IsNotExist(err) {
			absent++
		}
	}
	if absent != 1 {
		t.Fatal("stop did not preserve the remaining original unit", absent)
	}
	var done int
	if err := x.f.db.QueryRow(`SELECT COUNT(*) FROM GO_MEDIA_REMOVAL_UNITS WHERE STATE='done'`).Scan(&done); err != nil || done != 1 {
		t.Fatal(done, err)
	}
	monitorRebuild(t, x.f)
	mediaDeleteTick(t, x)
	if mediaDeleteHistoryCount(t, x) != 1 {
		t.Fatal("restart resumed physical deletion")
	}
	v := recoveryData[mediaSyncDeleteExecutionReview](t, mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/receipts/"+rows[0].ID+"/review", map[string]any{"confirmRead": true}))
	if !v.Parent.CanContinue {
		t.Fatal("remaining original units could not be reviewed", v.Parent)
	}
	in := mediaDeleteGrants()
	in["confirmRead"], in["fingerprint"] = true, v.Parent.Fingerprint
	p := recoveryData[mediasyncdel.Receipt](t, mediaDeleteHTTP(t, x, "POST", mediaDeleteEndpoint+"/receipts/"+rows[0].ID+"/continue", in))
	if p.State != "completed" || p.Parent != rows[0].Parent || mediaDeleteHistoryCount(t, x) != 0 {
		t.Fatal(p)
	}
	for _, name := range []string{"Movie/original-1.mkv", "Movie/original-1.nfo"} {
		if _, err := os.Lstat(path.Join(x.source, name)); !os.IsNotExist(err) {
			t.Fatal("original remaining source retained", name, err)
		}
	}
	if _, err := os.Lstat(path.Join(x.source, "Movie/keep.mkv")); err != nil {
		t.Fatal("unselected media changed", err)
	}
}
