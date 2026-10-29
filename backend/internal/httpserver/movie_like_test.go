package httpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/movielike"
)

const favoriteEndpoint = "/api/v1/movie-like"

type favoriteHTTPFixture struct {
	x          *mediaDeleteFixture
	root       string
	categories string
}

func newFavoriteHTTPFixture(t *testing.T) *favoriteHTTPFixture {
	t.Helper()
	x := newMediaDeleteFixture(t, false)
	categories := filepath.Join(filepath.Dir(x.f.path), "favorite-categories.yaml")
	if err := os.WriteFile(categories, []byte("movie:\n  Films:\n  Animation:\ntv:\n  Drama:\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(x.f.path).Update(map[string]any{"media.category": "favorite-categories"}); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, x.f)
	f := &favoriteHTTPFixture{x: x, categories: categories}
	if w := favoriteHTTP(t, f, "POST", "/api/v1/plugins/MovieLike/install", map[string]any{}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	favoriteSaveHTTP(t, f, map[string]any{"enable": true, "dir_name": "Favorites", "local_path": x.target, "remote_path": "/emby"})
	status := favoriteStatusHTTP(t, f)
	for _, root := range status.Roots {
		if root.Path == x.target {
			f.root = root.ID
		}
	}
	if f.root == "" || !status.Ready {
		t.Fatal("original movie root/category missing", status)
	}
	return f
}
func favoriteHTTP(t *testing.T, f *favoriteHTTPFixture, method, uri string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return mediaRefreshHTTP(t, f.x.f, method, uri, body)
}
func favoriteStatusHTTP(t *testing.T, f *favoriteHTTPFixture) movieLikeStatus {
	t.Helper()
	return recoveryData[movieLikeStatus](t, favoriteHTTP(t, f, "GET", favoriteEndpoint, nil))
}
func favoriteSaveHTTP(t *testing.T, f *favoriteHTTPFixture, values map[string]any) {
	t.Helper()
	d := recoveryData[pluginConfigDetail](t, favoriteHTTP(t, f, "GET", "/api/v1/plugins/MovieLike", nil))
	if w := favoriteHTTP(t, f, "PUT", "/api/v1/plugins/MovieLike", map[string]any{"fingerprint": d.Meta.Fingerprint, "values": values}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func favoriteReviewHTTP(t *testing.T, f *favoriteHTTPFixture) movieLikeReview {
	t.Helper()
	s := favoriteStatusHTTP(t, f)
	return recoveryData[movieLikeReview](t, favoriteHTTP(t, f, "POST", favoriteEndpoint+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirmRead": true, "rootIds": []string{f.root}}))
}
func favoriteGrants() map[string]any {
	return map[string]any{"confirm": true, "move": true, "whole": true, "sourceRemoval": true, "closeRelated": true, "legacyStopped": true, "refresh": true}
}
func favoriteEnableHTTP(t *testing.T, f *favoriteHTTPFixture) {
	t.Helper()
	v := favoriteReviewHTTP(t, f)
	in := favoriteGrants()
	in["id"], in["fingerprint"] = v.ID, v.Fingerprint
	if w := favoriteHTTP(t, f, "POST", favoriteEndpoint+"/authorize", in); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func favoriteBody(t *testing.T, file string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"Event": "item.rate", "Item": map[string]any{"Type": "Movie", "Id": "original-movie", "Name": "Original Movie", "Path": file}})
	return string(raw)
}
func favoriteReceiveHTTP(t *testing.T, f *favoriteHTTPFixture, key, file string) string {
	t.Helper()
	return webhookIDHTTP(t, webhookReceiveHTTP(f.x.hooks, "emby", favoriteBody(t, file), key))
}
func favoriteRowsHTTP(t *testing.T, f *favoriteHTTPFixture) []movielike.Receipt {
	t.Helper()
	return recoveryData[[]movielike.Receipt](t, favoriteHTTP(t, f, "GET", favoriteEndpoint+"/receipts", nil))
}
func favoritePutMovie(t *testing.T, f *favoriteHTTPFixture) {
	t.Helper()
	for file, body := range map[string]string{"Films/Movie/movie.mkv": "original movie", "Films/Movie/movie.nfo": "original metadata", "Films/Movie/poster.jpg": "original image", "Films/Movie/extras/readme.txt": "all attachments", "keep.txt": "must remain"} {
		libraryWriteHTTP(t, f.x.target, file, body)
	}
	if err := os.MkdirAll(path.Join(f.x.target, "Films/Movie/empty"), 0755); err != nil {
		t.Fatal(err)
	}
}
func favoriteTickHTTP(t *testing.T, f *favoriteHTTPFixture) {
	t.Helper()
	if err := f.x.f.runner.movieLike.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func TestMovieLikeActualCallbackWholeDirectoryAndOriginalHistoryRemain(t *testing.T) {
	f := newFavoriteHTTPFixture(t)
	favoritePutMovie(t, f)
	mediaDeleteHistory(t, f.x, 9007199254740993, "电影", "", "movie.mkv", 100)
	if _, err := f.x.f.db.Exec(`UPDATE TRANSFER_HISTORY SET DEST_PATH=?,CATEGORY='Films'`, path.Join(f.x.target, "Films/Movie")); err != nil {
		t.Fatal(err)
	}
	webhookAuthorizeHTTP(t, f.x.hooks)
	prior := favoriteReceiveHTTP(t, f, "before-native-grant", "/emby/Films/Movie/movie.mkv")
	if rows := favoriteRowsHTTP(t, f); len(rows) != 0 {
		t.Fatal("old enable adopted prior event", rows)
	}
	favoriteEnableHTTP(t, f)
	if got := favoriteReceiveHTTP(t, f, "before-native-grant", "/emby/Films/Movie/movie.mkv"); got != prior {
		t.Fatal("old receiver identity adopted by new grant")
	}
	if rows := favoriteRowsHTTP(t, f); len(rows) != 0 {
		t.Fatal(rows)
	}
	receipt := favoriteReceiveHTTP(t, f, "future-native-grant", "/emby/Films/Movie/movie.mkv")
	favoriteTickHTTP(t, f)
	rows := favoriteRowsHTTP(t, f)
	if len(rows) != 1 || rows[0].State != "completed" || rows[0].Callback != receipt {
		t.Fatal(rows)
	}
	for _, rel := range []string{"Favorites/Movie/movie.mkv", "Favorites/Movie/movie.nfo", "Favorites/Movie/poster.jpg", "Favorites/Movie/extras/readme.txt", "Favorites/Movie/empty", "keep.txt"} {
		if _, err := os.Lstat(path.Join(f.x.target, rel)); err != nil {
			t.Fatal(rel, err)
		}
	}
	if _, err := os.Lstat(path.Join(f.x.target, "Films/Movie")); !os.IsNotExist(err) {
		t.Fatal("old complete directory remained", err)
	}
	h, err := f.x.f.runner.organizer.jobs.GetHistory(t.Context(), 9007199254740993)
	if err != nil || h.DestPath != path.Join(f.x.target, "Films/Movie") {
		t.Fatal("reclassification rewrote original history", h, err)
	}
	if got := favoriteReceiveHTTP(t, f, "future-native-grant", "/emby/Films/Movie/movie.mkv"); got != receipt {
		t.Fatal(got)
	}
	monitorRebuild(t, f.x.f)
	favoriteTickHTTP(t, f)
	if rows := favoriteRowsHTTP(t, f); len(rows) != 1 || rows[0].State != "completed" {
		t.Fatal(rows)
	}
	if w := favoriteHTTP(t, f, "POST", favoriteEndpoint+"/receipts/"+rows[0].ID+"/continue", favoriteGrants()); w.Code == 200 {
		t.Fatal("completed work replayed")
	}
}
func TestMovieLikeOriginalEventGateAndWholeComponentMapping(t *testing.T) {
	for _, test := range []struct {
		name, body string
		handled    bool
	}{
		{"episode", `{"Event":"item.rate","Item":{"Type":"Episode","Name":"Episode","Path":"/emby/Films/Movie/movie.mkv","ParentIndexNumber":1,"IndexNumber":1}}`, false},
		{"playback", `{"Event":"playback.pause","Item":{"Type":"Movie","Path":"/emby/Films/Movie/movie.mkv"}}`, false},
		{"rating-compatibility", `{"Event":"item.rate","Item":{"Type":"Movie","Path":"/emby/Films/Movie/movie.mkv","UserData":{"IsFavorite":false}}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFavoriteHTTPFixture(t)
			favoritePutMovie(t, f)
			webhookAuthorizeHTTP(t, f.x.hooks)
			favoriteEnableHTTP(t, f)
			webhookIDHTTP(t, webhookReceiveHTTP(f.x.hooks, "emby", test.body, test.name))
			favoriteTickHTTP(t, f)
			rows := favoriteRowsHTTP(t, f)
			if test.handled {
				if len(rows) != 1 || rows[0].State != "completed" {
					t.Fatal(rows)
				}
			} else if len(rows) != 0 {
				t.Fatal("unrelated event consumed", rows)
			}
		})
	}
	for _, test := range []struct{ name, original, state string }{
		{"directory", "/emby/Films/Movie", "completed"},
		{"not-category", "/emby/Other/Movie/movie.mkv", "ignored"},
		{"outside-approved", "/unapproved/Films/Movie/movie.mkv", "blocked"},
		{"already-favorite", "/emby/Favorites/Movie/movie.mkv", "ignored"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFavoriteHTTPFixture(t)
			favoritePutMovie(t, f)
			if test.name == "not-category" {
				libraryWriteHTTP(t, f.x.target, "Other/Movie/movie.mkv", "outside classification")
			}
			if test.name == "already-favorite" {
				libraryWriteHTTP(t, f.x.target, "Favorites/Movie/movie.mkv", "already favorite")
			}
			webhookAuthorizeHTTP(t, f.x.hooks)
			favoriteEnableHTTP(t, f)
			favoriteReceiveHTTP(t, f, test.name, test.original)
			favoriteTickHTTP(t, f)
			rows := favoriteRowsHTTP(t, f)
			if len(rows) != 1 || rows[0].State != test.state {
				t.Fatal(rows)
			}
		})
	}
}
func TestMovieLikeThreeMappedPrefixesAndExistingTargetNeverMerge(t *testing.T) {
	f := newFavoriteHTTPFixture(t)
	favoritePutMovie(t, f)
	favoriteSaveHTTP(t, f, map[string]any{"local_path": "/intermediate", "remote_path": "Z:/Media", "local_path2": "/stage", "remote_path2": "/intermediate", "local_path3": f.x.target, "remote_path3": "/stage"})
	webhookAuthorizeHTTP(t, f.x.hooks)
	favoriteEnableHTTP(t, f)
	favoriteReceiveHTTP(t, f, "windows-prefix", `Z:\Media\Films\Movie\movie.mkv`)
	favoriteTickHTTP(t, f)
	if rows := favoriteRowsHTTP(t, f); len(rows) != 1 || rows[0].State != "completed" {
		t.Fatal(rows)
	}
	x := newFavoriteHTTPFixture(t)
	favoritePutMovie(t, x)
	libraryWriteHTTP(t, x.x.target, "Favorites/Movie/foreign.txt", "do not overwrite")
	webhookAuthorizeHTTP(t, x.x.hooks)
	favoriteEnableHTTP(t, x)
	favoriteReceiveHTTP(t, x, "existing-target", "/emby/Films/Movie/movie.mkv")
	favoriteTickHTTP(t, x)
	if rows := favoriteRowsHTTP(t, x); len(rows) != 1 || rows[0].State != "blocked" {
		t.Fatal(rows)
	}
	if _, err := os.Lstat(path.Join(x.x.target, "Films/Movie/movie.mkv")); err != nil {
		t.Fatal("source moved into existing target", err)
	}
	bytes, err := os.ReadFile(path.Join(x.x.target, "Favorites/Movie/foreign.txt"))
	if err != nil || string(bytes) != "do not overwrite" {
		t.Fatal(string(bytes), err)
	}
}
func TestMovieLikeEveryFutureGrantIsIndependentAndPlainHTTPNeedsEarlierConsent(t *testing.T) {
	f := newFavoriteHTTPFixture(t)
	v := favoriteReviewHTTP(t, f)
	for field := range favoriteGrants() {
		in := favoriteGrants()
		in["id"], in["fingerprint"], in[field] = v.ID, v.Fingerprint, false
		if w := favoriteHTTP(t, f, "POST", favoriteEndpoint+"/authorize", in); w.Code != 400 {
			t.Fatal(field, w.Code, w.Body.String())
		}
	}
	if favoriteStatusHTTP(t, f).Active {
		t.Fatal("partial grants activated")
	}
	if err := config.NewStore(f.x.f.path).Update(map[string]any{"emby.host": "http://sync-delete.test"}); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, f.x.f)
	s := favoriteStatusHTTP(t, f)
	before := f.x.reads
	for _, roots := range [][]string{nil, {"other"}, {f.root, f.root}} {
		if w := favoriteHTTP(t, f, "POST", favoriteEndpoint+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirmRead": true, "rootIds": roots, "http": true}); w.Code != 400 && w.Code != 409 {
			t.Fatal(roots, w.Code)
		}
	}
	if w := favoriteHTTP(t, f, "POST", favoriteEndpoint+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirmRead": true, "rootIds": []string{f.root}}); w.Code != 400 || f.x.reads != before {
		t.Fatal("plaintext identity read before independent consent", w.Code, f.x.reads, before)
	}
	if w := favoriteHTTP(t, f, "POST", favoriteEndpoint+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirmRead": true, "rootIds": []string{f.root}, "http": true}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestMovieLikeOriginalScopeRevocationBlocksBeforeMoving(t *testing.T) {
	for _, change := range []string{"stop", "save", "category", "server", "root"} {
		t.Run(change, func(t *testing.T) {
			f := newFavoriteHTTPFixture(t)
			favoritePutMovie(t, f)
			webhookAuthorizeHTTP(t, f.x.hooks)
			favoriteEnableHTTP(t, f)
			favoriteReceiveHTTP(t, f, change, "/emby/Films/Movie/movie.mkv")
			original := f.x.target
			switch change {
			case "stop":
				s := favoriteStatusHTTP(t, f)
				if w := favoriteHTTP(t, f, "POST", favoriteEndpoint+"/stop", map[string]any{"fingerprint": s.Fingerprint, "confirm": true}); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			case "save":
				favoriteSaveHTTP(t, f, map[string]any{"dir_name": "New Favorites"})
			case "category":
				if err := os.WriteFile(f.categories, []byte("movie:\n  Other:\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "server":
				if err := config.NewStore(f.x.f.path).Update(map[string]any{"emby.api_key": "changed-key"}); err != nil {
					t.Fatal(err)
				}
			case "root":
				if err := os.Rename(original, original+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(original, 0755); err != nil {
					t.Fatal(err)
				}
				original += ".retained"
			}
			favoriteTickHTTP(t, f)
			rows := favoriteRowsHTTP(t, f)
			if len(rows) != 1 || rows[0].State != "blocked" || rows[0].Phase != "" {
				t.Fatal(rows)
			}
			if _, err := os.Lstat(path.Join(original, "Films/Movie/movie.mkv")); err != nil {
				t.Fatal("revocation changed original data", err)
			}
		})
	}
}
func TestMovieLikeStopDuringIdentityReadNeverCreatesMovePlan(t *testing.T) {
	f := newFavoriteHTTPFixture(t)
	favoritePutMovie(t, f)
	webhookAuthorizeHTTP(t, f.x.hooks)
	favoriteEnableHTTP(t, f)
	favoriteReceiveHTTP(t, f, "stop-read", "/emby/Films/Movie/movie.mkv")
	f.x.beforeMedia = func() {
		f.x.beforeMedia = nil
		s := favoriteStatusHTTP(t, f)
		if w := favoriteHTTP(t, f, "POST", favoriteEndpoint+"/stop", map[string]any{"fingerprint": s.Fingerprint, "confirm": true}); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	favoriteTickHTTP(t, f)
	rows := favoriteRowsHTTP(t, f)
	if len(rows) != 1 || rows[0].State != "blocked" {
		t.Fatal(rows)
	}
	if _, err := os.Lstat(path.Join(f.x.target, "Films/Movie/movie.mkv")); err != nil {
		t.Fatal(err)
	}
}
func TestMovieLikeMetadataFailureRequiresFreshOriginalContinueAndOneRefresh(t *testing.T) {
	f := newFavoriteHTTPFixture(t)
	favoritePutMovie(t, f)
	wire := mediaRefreshWire(t, f.x.f, "emby")
	wire.lost = true
	mediaRefreshGrant(t, f.x.f, "0")
	webhookAuthorizeHTTP(t, f.x.hooks)
	favoriteEnableHTTP(t, f)
	favoriteReceiveHTTP(t, f, "lost-local-receipt", "/emby/Films/Movie/movie.mkv")
	if _, err := f.x.f.db.Exec(`CREATE TRIGGER fail_favorite_completed BEFORE UPDATE ON GO_MOVIE_LIKE_EVENTS WHEN NEW.STATE='completed' BEGIN SELECT RAISE(ABORT,'fixture final transaction');END`); err != nil {
		t.Fatal(err)
	}
	if err := f.x.f.runner.movieLike.tick(t.Context()); err == nil {
		t.Fatal("failed metadata became completion")
	}
	rows := favoriteRowsHTTP(t, f)
	if len(rows) != 1 || rows[0].State != "needs_review" || rows[0].Phase != "moved" {
		t.Fatal(rows)
	}
	if wire.writes.Load() != 0 {
		t.Fatal("refresh before complete move receipt")
	}
	monitorRebuild(t, f.x.f)
	favoriteTickHTTP(t, f)
	if wire.writes.Load() != 0 {
		t.Fatal("restart retried")
	}
	if _, err := f.x.f.db.Exec("DROP TRIGGER fail_favorite_completed"); err != nil {
		t.Fatal(err)
	}
	s := favoriteStatusHTTP(t, f)
	if w := favoriteHTTP(t, f, "POST", favoriteEndpoint+"/stop", map[string]any{"fingerprint": s.Fingerprint, "confirm": true}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	review := recoveryData[movieLikeExecution](t, favoriteHTTP(t, f, "POST", favoriteEndpoint+"/receipts/"+rows[0].ID+"/review", map[string]any{"confirmRead": true}))
	if !review.Move.CanContinue {
		t.Fatal(review.Move.Evidence)
	}
	for field := range favoriteGrants() {
		in := favoriteGrants()
		in["confirmRead"], in["fingerprint"], in[field] = true, review.Move.Fingerprint, false
		if w := favoriteHTTP(t, f, "POST", favoriteEndpoint+"/receipts/"+rows[0].ID+"/continue", in); w.Code != 400 {
			t.Fatal(field, w.Code, w.Body.String())
		}
	}
	in := favoriteGrants()
	in["confirmRead"], in["fingerprint"] = true, review.Move.Fingerprint
	done := recoveryData[movielike.Receipt](t, favoriteHTTP(t, f, "POST", favoriteEndpoint+"/receipts/"+rows[0].ID+"/continue", in))
	if done.State != "completed" {
		t.Fatal(done)
	}
	jobs, err := f.x.f.runner.mediaRefresh.queue.List(t.Context(), false, time.Now())
	if err != nil || len(jobs) != 1 || jobs[0].Source != "movie_like" {
		t.Fatal(jobs, err)
	}
	if err := f.x.f.runner.mediaRefresh.tick(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	jobs, err = f.x.f.runner.mediaRefresh.queue.List(t.Context(), false, time.Now())
	if err != nil || len(jobs) != 1 || wire.writes.Load() != 1 {
		t.Fatal(jobs, wire.writes.Load(), err)
	}
	monitorRebuild(t, f.x.f)
	if err := f.x.f.runner.mediaRefresh.tick(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if wire.writes.Load() != 1 {
		t.Fatal("uncertain refresh repeated")
	}
}
func TestMovieLikeLifecycleKeepsUnknownValuesAndStopsPreviousAuthority(t *testing.T) {
	f := newFavoriteHTTPFixture(t)
	favoritePutMovie(t, f)
	webhookAuthorizeHTTP(t, f.x.hooks)
	favoriteEnableHTTP(t, f)
	raw := `{"enable":true,"dir_name":"Favorites","local_path":` + strconvQuote(f.x.target) + `,"remote_path":"/emby","unknown":{"n":9007199254740993}}`
	w := performFormRequest(f.x.f.handler, "/api/v1/plugin/config", f.x.f.token, url.Values{"id": {"MovieLike"}, "config": {raw}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if favoriteStatusHTTP(t, f).Active {
		t.Fatal("legacy save kept automatic authority")
	}
	var got string
	if err := f.x.f.db.QueryRow(`SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.MovieLike' ORDER BY ID DESC LIMIT 1`).Scan(&got); err != nil || !strings.Contains(got, "9007199254740993") {
		t.Fatal(got, err)
	}
	favoriteEnableHTTP(t, f)
	if w := favoriteHTTP(t, f, "DELETE", "/api/v1/plugins/MovieLike", map[string]any{}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := favoriteHTTP(t, f, "POST", "/api/v1/plugins/MovieLike/install", map[string]any{}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if favoriteStatusHTTP(t, f).Active {
		t.Fatal("reinstall restarted")
	}
}
func strconvQuote(s string) string { raw, _ := json.Marshal(s); return string(raw) }
func TestMovieLikeRootOnlyHybridRefusalAndNativeSearchRemainSeparate(t *testing.T) {
	f := newFavoriteHTTPFixture(t)
	if w := performFormRequest(f.x.f.handler, "/api/v1/user/manage", f.x.f.token, url.Values{"oper": {"add"}, "name": {"favorite-viewer"}, "password": {"fixture-password"}, "pris": {""}}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	viewer := loginForTest(t, f.x.f.handler, "favorite-viewer", "fixture-password")
	for _, uri := range []string{favoriteEndpoint, favoriteEndpoint + "/receipts", "/api/v1/plugins/MovieLike"} {
		if w := nativeJSONRequest(f.x.f.handler, "GET", uri, "", ""); w.Code != 401 {
			t.Fatal(uri, w.Code)
		}
		if w := nativeJSONRequest(f.x.f.handler, "GET", uri, viewer, ""); w.Code != 403 {
			t.Fatal(uri, w.Code)
		}
	}
	hybrid, err := newHandler(config.Config{ApplicationConfigPath: f.x.f.path, LegacyBackendURL: "http://legacy:3000"}, f.x.f.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{favoriteEndpoint, "/api/v1/plugins/MovieLike", "/api/v1/plugins/MovieLike/unsupported"} {
		if w := nativeJSONRequest(hybrid, "GET", uri, f.x.f.token, ""); w.Code != 501 {
			t.Fatal(uri, w.Code, w.Body.String())
		}
	}
	if w := favoriteHTTP(t, f, "POST", "/api/v1/plugins/MovieLike/unsupported", map[string]any{}); w.Code != 405 {
		t.Fatal(w.Code, w.Body.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	wait := startMovieLikeWorker(ctx, f.x.f.runner.movieLike)
	cancel()
	wait()
	if f.x.f.runner.movieLike.workers.Load() != 0 {
		t.Fatal("worker did not stop")
	}
}
func TestMovieLikeCustomFavoriteLayoutFeedsInventoryAndSubsequentIntakeWhileStopped(t *testing.T) {
	f := newFavoriteHTTPFixture(t)
	favoriteSaveHTTP(t, f, map[string]any{"enable": false, "dir_name": "My Picks"})
	libraryWriteHTTP(t, f.x.target, "My Picks/Movie (2026)/movie.mkv", "existing favorite")
	service := f.x.f.runner.organizer.service
	name, err := service.favoriteDirectory(t.Context())
	if err != nil || name != "My Picks" {
		t.Fatal(name, err)
	}
	snapshot, err := config.NewStore(f.x.f.path).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := mediameta.Parse("Movie.2026.1080p", "")
	var detail tmdbMediaDetails
	if err := json.Unmarshal([]byte(`{"id":100,"title":"Movie","release_date":"2026-01-01"}`), &detail); err != nil {
		t.Fatal(err)
	}
	inventory, err := service.nativeLocalMediaInventory(t.Context(), objectValue(snapshot["media"]), meta, detail, mediaserver.Identity{Title: "Movie", Year: "2026", TMDBID: "100"}, []string{"Films"})
	if err != nil || len(inventory.ItemIDs) == 0 {
		t.Fatal("custom favorite disappeared from inventory", inventory, err)
	}
	status := favoriteStatusHTTP(t, f)
	var root organizationRoot
	for _, r := range status.Roots {
		if r.ID == f.root {
			root = r
		}
	}
	selected, err := f.x.f.runner.organizer.favoriteTarget(t.Context(), root, "Films/Movie (2026)/new-version.mkv", &nativeRecognition{kind: "movie", libraryCategory: "Films"}, false, nil)
	if err != nil || selected != "My Picks/Movie (2026)/new-version.mkv" {
		t.Fatal("new intake ignored existing favorite layout", selected, err)
	}
	if w := favoriteHTTP(t, f, "DELETE", "/api/v1/plugins/MovieLike", map[string]any{}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if after, err := service.favoriteDirectory(t.Context()); err != nil || after != name {
		t.Fatal("uninstall hid retained layout", after, err)
	}
}
func TestMovieLikeCategoryAliasesAndInvalidNamesDoNotTruncateOriginalScope(t *testing.T) {
	f := newFavoriteHTTPFixture(t)
	if err := os.WriteFile(f.categories, []byte("base: &movies\n  Films:\n  Animation:\nmovie: *movies\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := favoriteStatusHTTP(t, f)
	if !s.Ready || strings.Join(s.Categories, ",") != "Films,Animation" {
		t.Fatal(s)
	}
	if err := os.WriteFile(f.categories, []byte("movie:\n  ../escape:\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s = favoriteStatusHTTP(t, f)
	if s.Ready || s.Current {
		t.Fatal("unsafe category enabled a partial scope", s)
	}
}
