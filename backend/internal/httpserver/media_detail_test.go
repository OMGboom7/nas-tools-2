package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

const displayMovieJSON = `{"id":100,"title":"Movie","original_title":"Original Movie","release_date":"2026-01-01","overview":"Overview","tagline":"Tagline","vote_average":8.2,"runtime":120,"poster_path":"/movie.jpg","backdrop_path":"/back.jpg","genres":[{"id":1,"name":"剧情"}],"status":"Released","original_language":"en","budget":100,"revenue":200,"production_countries":[{"name":"Country"}],"production_companies":[{"name":"Studio"}]}`
const displayTVJSON = `{"id":200,"name":"Series","original_name":"Original Anime","first_air_date":"2025-04-01","overview":"TV overview","vote_average":8,"episode_run_time":[45],"poster_path":"/tv.jpg","networks":[{"name":"Network"}],"number_of_seasons":2,"seasons":[{"season_number":0,"id":20,"name":"Specials","episode_count":1},{"season_number":1,"id":21,"name":"Season 1","episode_count":101},{"season_number":2,"id":22,"name":"Season 2","episode_count":0}]}`

type mediaDetailFixture struct {
	handler           http.Handler
	token, path, mode string
	runner            *rssRunAPI
	db                *sql.DB
	calls             map[string]int
	onInfo            func()
}

func newMediaDetailFixture(t *testing.T) *mediaDetailFixture {
	t.Helper()
	f := &mediaDetailFixture{calls: map[string]int{}}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Fatal("display performed an upstream mutation", r.URL)
		}
		f.calls[r.URL.Host+r.URL.Path]++
		if r.URL.Host == "api.bgm.tv" && r.URL.Path == "/v0/subjects/300" {
			return jsonResponse(r, `{"id":300,"name":"Original Anime","name_cn":"动画","date":"2025-04-01"}`), nil
		}
		if r.URL.Host == "frodo.douban.com" && r.URL.Path == "/api/v2/movie/300" {
			return jsonResponse(r, `{"id":"300","title":"电影","original_title":"Movie","year":"2026"}`), nil
		}
		if r.URL.Host == "media.local" {
			if r.Header.Get("X-Emby-Token") != "private-media-token" || strings.Contains(r.URL.String(), "private-media-token") {
				t.Fatal("unsafe media credential", r.URL)
			}
			switch r.URL.Path {
			case "/emby/Items":
				if f.mode == "live-unknown" {
					response := jsonResponse(r, `{}`)
					response.StatusCode = 503
					return response, nil
				}
				if f.mode == "live-absent" {
					return jsonResponse(r, `{"Items":[],"TotalRecordCount":0}`), nil
				}
				if r.URL.Query().Get("IncludeItemTypes") == "Series" {
					return jsonResponse(r, `{"Items":[{"Id":"show","Name":"Series","ProductionYear":2025,"ProviderIds":{"Tmdb":"200"}}],"TotalRecordCount":1}`), nil
				}
				return jsonResponse(r, `{"Items":[{"Id":"m","Name":"Movie","ProductionYear":2026,"ProviderIds":{"Tmdb":"100"}}],"TotalRecordCount":1}`), nil
			case "/emby/Shows/show/Episodes":
				return jsonResponse(r, `{"Items":[{"Id":"ep","ParentIndexNumber":1,"IndexNumber":1}],"TotalRecordCount":1}`), nil
			case "/emby/System/Info":
				if f.onInfo != nil {
					f.onInfo()
				}
				return jsonResponse(r, `{"Id":"server-id"}`), nil
			default:
				t.Fatal("unexpected media request", r.URL)
			}
		}
		if r.URL.Host != "tmdb.test" || r.URL.Query().Get("api_key") != "private-tmdb-key" || r.URL.Query().Get("language") != "zh" {
			t.Fatal("legacy or unexpected metadata host", r.URL)
		}
		if f.mode == "redirect" {
			response := jsonResponse(r, `{}`)
			response.StatusCode = 302
			response.Header.Set("Location", "http://legacy:3000/private")
			return response, nil
		}
		switch r.URL.Path {
		case "/3/movie/100":
			body := displayMovieJSON
			if f.mode == "wrong-id" {
				body = strings.Replace(body, `"id":100`, `"id":101`, 1)
			}
			return jsonResponse(r, body), nil
		case "/3/tv/200":
			return jsonResponse(r, displayTVJSON), nil
		case "/3/search/tv":
			return jsonResponse(r, `{"results":[{"id":200,"name":"Original Anime","first_air_date":"2025-04-01"}]}`), nil
		case "/3/search/movie":
			return jsonResponse(r, `{"results":[{"id":100,"title":"Movie","release_date":"2026-01-01"}]}`), nil
		case "/3/movie/100/credits", "/3/tv/200/credits":
			if f.mode == "credits-fail" {
				response := jsonResponse(r, `{}`)
				response.StatusCode = 503
				return response, nil
			}
			id := 100
			if strings.Contains(r.URL.Path, "/tv/") {
				id = 200
			}
			body, _ := json.Marshal(map[string]any{"id": id, "cast": []map[string]any{{"id": 9, "name": "Actor", "character": "Role", "profile_path": "/actor.jpg"}}, "crew": []map[string]any{{"id": 10, "name": "Director", "job": "Director"}}})
			return jsonResponse(r, string(body)), nil
		case "/3/tv/200/season/0", "/3/tv/200/season/1", "/3/tv/200/season/2":
			season, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/3/tv/200/season/"))
			episodes := []map[string]any{}
			count := 101
			if season == 0 {
				count = 1
			}
			if season == 2 {
				count = 0
			}
			for i := 0; i < count; i++ {
				n := i + 1
				if season == 0 {
					n = 0
				}
				episodes = append(episodes, map[string]any{"id": 1000 + n, "show_id": 200, "season_number": season, "episode_number": n, "name": "Episode " + strconv.Itoa(n), "overview": "Episode overview", "runtime": nil, "still_path": "/episode.jpg", "vote_average": 7.5})
			}
			if f.mode == "wrong-show" && len(episodes) > 0 {
				episodes[0]["show_id"] = 999
			}
			if f.mode == "duplicate-episode" && len(episodes) > 1 {
				episodes[1]["episode_number"] = episodes[0]["episode_number"]
			}
			body, _ := json.Marshal(map[string]any{"id": 2000 + season, "season_number": season, "name": "Season", "episodes": episodes})
			return jsonResponse(r, string(body)), nil
		case "/3/tv/200/similar", "/3/tv/200/recommendations":
			return jsonResponse(r, `{"results":[{"id":201,"name":"Related Series","first_air_date":"2026-01-01"}]}`), nil
		case "/3/movie/100/similar", "/3/movie/100/recommendations":
			return jsonResponse(r, `{"results":[{"id":101,"title":"Related Movie","release_date":"2026-01-01"}]}`), nil
		default:
			t.Fatal("unexpected display request", r.URL)
		}
		return nil, nil
	})
	_, f.token, f.path = nativeServicesFixture(t, "media:\n  media_server: emby\nemby:\n  host: http://media.local\n  api_key: private-media-token\n  play_host: https://play.example\n", nil, transport)
	configPath := filepath.Join(filepath.Dir(f.path), "config.yaml")
	if err := config.NewStore(configPath).Update(map[string]any{"app.rmt_tmdbkey": "private-tmdb-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	var err error
	f.handler, err = buildHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: configPath}, transport, &f.runner)
	if err != nil {
		t.Fatal(err)
	}
	f.db, err = sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	for _, kind := range []string{"MOV", "TV"} {
		if _, err := f.db.Exec(nativeSubscriptionSchema(kind)); err != nil {
			t.Fatal(err)
		}
	}
	_, kind, _, digest, err := f.runner.mediaSync.connection()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.runner.mediaSync.store.Publish(context.Background(), kind, digest, "admin", []string{"films", "tv"}, []mediaserver.SyncItem{{ID: "m", Library: "films", Type: "movie", Title: "Movie", TMDBID: "100", Year: "2026"}, {ID: "show", Library: "tv", Type: "show", Title: "Series", TMDBID: "200", Year: "2025", Episodes: []mediaserver.SyncEpisode{{Season: 0, Episode: 0}, {Season: 1, Episode: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *mediaDetailFixture) get(path string) *httptest.ResponseRecorder {
	return nativeJSONRequest(f.handler, "GET", path, f.token, "")
}
func readDisplayDetail(t *testing.T, w *httptest.ResponseRecorder) mediaDisplayDetail {
	t.Helper()
	var result struct{ Data mediaDisplayDetail }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, secret := range []string{"private-tmdb-key", "private-media-token", "native-services-secret", "private-password"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("detail leaked", secret)
		}
	}
	return result.Data
}
func TestMediaDetailNativeFieldsCreditsCacheAndNoImplicitLiveServerLookup(t *testing.T) {
	f := newMediaDetailFixture(t)
	movie := readDisplayDetail(t, f.get("/api/v1/media/details/movie/100"))
	if movie.ID != "100" || movie.Title != "Movie" || movie.Runtime == nil || *movie.Runtime != 120 || len(movie.Cast) != 1 || movie.Cast[0].Role != "Role" || len(movie.Crew) != 1 || movie.Cache.State != "ready" || movie.CachedPresence.State != "present" || !movie.SubscriptionsKnown || movie.Subscribed || len(movie.Facts) < 6 || movie.Poster == "" {
		t.Fatal(movie)
	}
	show := readDisplayDetail(t, f.get("/api/v1/media/details/tv/200"))
	if len(show.Seasons) != 3 || show.Seasons[0].Number != 2 || show.Seasons[2].Number != 0 || show.Seasons[1].CachedPresence.State != "present" || show.Seasons[0].CachedPresence.State != "not-recorded" || show.Seasons[2].CachedPresence.State != "present" {
		t.Fatal(show)
	}
	for key := range f.calls {
		if strings.HasPrefix(key, "media.local") {
			t.Fatal("read-only detail implicitly probed media server", key)
		}
	}
	f.mode = "credits-fail"
	partial := readDisplayDetail(t, f.get("/api/v1/media/details/movie/100"))
	if partial.Title != "Movie" || len(partial.Warnings) != 1 || len(partial.Cast) != 0 {
		t.Fatal(partial)
	}
}
func TestMediaDetailSeasonSpecialsPaginationAndIdentityFailures(t *testing.T) {
	f := newMediaDetailFixture(t)
	var envelope struct{ Data mediaSeasonDetail }
	w := f.get("/api/v1/media/details/tv/200/seasons/0?page=1")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || len(envelope.Data.Episodes) != 1 || envelope.Data.Episodes[0].Number != 0 || envelope.Data.Episodes[0].CachedPresence.State != "present" || envelope.Data.Episodes[0].Runtime != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	w = f.get("/api/v1/media/details/tv/200/seasons/1?page=2")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Data.Total != 101 || len(envelope.Data.Episodes) != 1 || envelope.Data.Episodes[0].Number != 101 || envelope.Data.Episodes[0].CachedPresence.State != "not-recorded" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = f.get("/api/v1/media/details/tv/200/seasons/2")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Data.Total != 0 || len(envelope.Data.Episodes) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, mode := range []string{"wrong-show", "duplicate-episode"} {
		f.mode = mode
		w = f.get("/api/v1/media/details/tv/200/seasons/1")
		if w.Code != 502 {
			t.Fatal(mode, w.Code, w.Body.String())
		}
	}
}
func TestMediaDetailRelatedTelevisionTypeAndLegacyFormParity(t *testing.T) {
	f := newMediaDetailFixture(t)
	for _, relation := range []string{"similar", "recommendations"} {
		w := f.get("/api/v1/media/details/tv/200/related/" + relation + "?page=2")
		var envelope struct{ Data discoveryData }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Data.Page != 2 || len(envelope.Data.Items) != 1 || envelope.Data.Items[0].Type != "TV" || envelope.Data.Items[0].Title != "Related Series" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if _, err := f.db.Exec(`INSERT INTO RSS_MOVIES(ID,NAME,YEAR,TMDBID) VALUES(7,'Movie','2026','100')`); err != nil {
		t.Fatal(err)
	}
	w := performFormRequest(f.handler, "/api/v1/media/detail", f.token, url.Values{"type": {"MOV"}, "tmdbid": {"100"}})
	var envelope struct {
		Data struct {
			Fav, RSSID string
			Actors     []struct{ Profile string }
			Crews      []map[string]string
			Fact       []map[string]string
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Data.Fav != "1" || envelope.Data.RSSID != "7" || len(envelope.Data.Actors) != 1 || envelope.Data.Actors[0].Profile == "" || len(envelope.Data.Crews) != 1 || envelope.Data.Crews[0]["Director"] != "Director" || len(envelope.Data.Fact) == 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = performFormRequest(f.handler, "/api/v1/media/similar", f.token, url.Values{"type": {"TV"}, "tmdbid": {"200"}, "page": {"1"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"type":"TV"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestMediaDetailBangumiResolvesCanonicalIdentityWithoutPython(t *testing.T) {
	f := newMediaDetailFixture(t)
	d := readDisplayDetail(t, f.get("/api/v1/media/details/tv/BG%3A300"))
	if d.ID != "200" || d.SourceID != "BG:300" || d.Title != "Series" || f.calls["api.bgm.tv/v0/subjects/300"] != 1 {
		t.Fatal(d, f.calls)
	}
}
func TestMediaDetailDoubanResolvesCanonicalIdentityAndRetainsSourceLink(t *testing.T) {
	f := newMediaDetailFixture(t)
	d := readDisplayDetail(t, f.get("/api/v1/media/details/movie/DB%3A300"))
	if d.ID != "100" || d.DoubanID != "300" || d.SourceID != "DB:300" || d.DoubanLink != "https://movie.douban.com/subject/300/" || f.calls["frodo.douban.com/api/v2/movie/300"] != 1 {
		t.Fatal(d, f.calls)
	}
}
func TestMediaDetailPlaybackRequiresCurrentEvidenceAndConfiguration(t *testing.T) {
	f := newMediaDetailFixture(t)
	for _, kindID := range []string{"movie/100", "tv/200"} {
		w := f.get("/api/v1/media/details/" + kindID + "/playback")
		var envelope struct {
			Data struct {
				Link         string
				LiveVerified bool
			}
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || !envelope.Data.LiveVerified || !strings.HasPrefix(envelope.Data.Link, "https://play.example/web/") || strings.Contains(w.Body.String(), "private-media-token") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	f.mode = "live-absent"
	if w := f.get("/api/v1/media/details/movie/100/playback"); w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.mode = "live-unknown"
	if w := f.get("/api/v1/media/details/movie/100/playback"); w.Code != 502 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.mode = ""
	f.onInfo = func() {
		if err := config.NewStore(filepath.Join(filepath.Dir(f.path), "config.yaml")).Update(map[string]any{"emby.play_host": "https://changed.example"}); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.get("/api/v1/media/details/movie/100/playback"); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestMediaDetailRejectsInvalidOrUnauthorizedWithoutUpstreamAndNoFallback(t *testing.T) {
	f := newMediaDetailFixture(t)
	for _, route := range []string{"/api/v1/media/details/person/100", "/api/v1/media/details/movie/not-id", "/api/v1/media/details/tv/200/seasons/-1", "/api/v1/media/details/tv/200/seasons/1?page=0", "/api/v1/media/details/tv/200/related/other", "/api/v1/media/details/movie/100/related/similar?page=101"} {
		w := f.get(route)
		if w.Code != 400 {
			t.Fatal(route, w.Code, w.Body.String())
		}
	}
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/media/details/movie/100", "", "")
	if w.Code != 401 || len(f.calls) != 0 {
		t.Fatal(w.Code, f.calls)
	}
	for _, mode := range []string{"redirect", "wrong-id"} {
		f.mode = mode
		w = f.get("/api/v1/media/details/movie/100")
		if w.Code != 502 || strings.Contains(w.Body.String(), "private-tmdb-key") {
			t.Fatal(mode, w.Code, w.Body.String())
		}
	}
}
func TestMediaDetailSubscriptionUsesExistingExactSeasonAndPermissionLedger(t *testing.T) {
	f := newMediaDetailFixture(t)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/subscriptions", f.token, `{"name":"Series","year":"2025","type":"TV","mediaId":"200","season":"1","fuzzyMatch":false,"overEdition":false,"rssSites":[],"searchSites":[]}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	d := readDisplayDetail(t, f.get("/api/v1/media/details/tv/200"))
	if !d.SubscriptionsKnown || !d.Subscribed || len(d.Subscriptions) != 1 || strings.TrimLeft(strings.TrimPrefix(d.Subscriptions[0].Season, "S"), "0") != "1" {
		t.Fatal(d)
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"探索"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/subscriptions", viewer, `{"name":"Movie","year":"2026","type":"MOV","mediaId":"100"}`)
	if w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	var movieCount int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES`).Scan(&movieCount); err != nil || movieCount != 0 {
		t.Fatal("unauthorized request wrote subscription", movieCount, err)
	}
	for _, path := range []string{"/api/v1/subscribe/add", "/api/v1/subscribe/delete", "/api/v1/subscribe/redo", "/api/v1/subscriptions/TV/1/remove"} {
		w := nativeJSONRequest(f.handler, "POST", path, viewer, `{}`)
		if w.Code != 403 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	created = performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"subscriber"}, "password": {"strong-password"}, "pris": {"订阅管理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	subscriber := loginForTest(t, f.handler, "subscriber", "strong-password")
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/subscriptions", subscriber, `{"name":"Movie","year":"2026","type":"MOV","mediaId":"100"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := f.db.Exec(`UPDATE CONFIG_USERS SET PRIS='探索' WHERE NAME='subscriber'`); err != nil {
		t.Fatal(err)
	}
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/subscriptions", subscriber, `{"name":"Movie","year":"2026","type":"MOV","mediaId":"100"}`)
	if w.Code != 403 {
		t.Fatal("revoked permission still accepted", w.Code, w.Body.String())
	}
}
func TestMediaDetailOldPageLinksRedirectLocallyWithoutReadingOrWriting(t *testing.T) {
	f := newMediaDetailFixture(t)
	w := nativeJSONRequest(f.handler, "GET", "/media_detail?type=TV&id=BG%3A300", "", "")
	if w.Code != 303 || w.Header().Get("Location") != "/media/tv/BG:300" || len(f.calls) != 0 {
		t.Fatal(w.Code, w.Header(), f.calls)
	}
	w = nativeJSONRequest(f.handler, "GET", "/media_detail?type=MOV&id=https%3A%2F%2Fevil.test", "", "")
	if w.Code != 400 || len(f.calls) != 0 {
		t.Fatal(w.Code, f.calls)
	}
}
