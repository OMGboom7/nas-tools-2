package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
)

type subscriptionRunFixture struct {
	noResults                               bool
	transport                               http.RoundTripper
	handler                                 http.Handler
	token, path                             string
	db                                      *sql.DB
	adds, queries                           int
	addError, loginFailure, libraryComplete bool
	afterAdd, afterSearch                   func()
}

func newSubscriptionRunFixture(t *testing.T) *subscriptionRunFixture {
	t.Helper()
	f := &subscriptionRunFixture{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "tmdb.test":
			if strings.HasPrefix(r.URL.Path, "/3/tv/") {
				return jsonResponse(r, `{"id":200,"name":"Show","first_air_date":"2026-01-01","seasons":[{"season_number":1,"episode_count":3}]}`), nil
			}
			if r.URL.Path == "/3/search/tv" {
				return jsonResponse(r, `{"results":[{"id":200,"name":"Show","first_air_date":"2026-01-01"}]}`), nil
			}
			if r.URL.Path == "/3/search/movie" {
				return jsonResponse(r, `{"results":[{"id":100,"title":"Movie","release_date":"2026-01-01"}]}`), nil
			}
			return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01"}`), nil
		case "emby.test":
			if f.libraryComplete {
				return jsonResponse(r, `{"Items":[{"Id":"movie","Type":"Movie","Name":"Movie","ProductionYear":2026,"ProviderIds":{"Tmdb":"100"}}]}`), nil
			}
			return jsonResponse(r, `{"Items":[]}`), nil
		case "indexer.local":
			if r.URL.Path == "/api/v1/indexerstats" {
				return jsonResponse(r, `{"indexers":[{"indexerId":42,"indexerName":"Selected"}]}`), nil
			}
			f.queries++
			if f.noResults {
				return jsonResponse(r, `[]`), nil
			}
			if f.afterSearch != nil {
				f.afterSearch()
			}
			if r.URL.Query().Get("query") == "Show" {
				return jsonResponse(r, fmt.Sprintf(`[{"indexerId":42,"title":"Show.S01E02.1080p.WEB-DL","size":1024,"downloadUrl":"magnet:?xt=urn:btih:%040x"},{"indexerId":42,"title":"Show.S01E03.1080p.WEB-DL","size":1024,"downloadUrl":"magnet:?xt=urn:btih:%040x"}]`, 2, 3)), nil
			}
			return jsonResponse(r, `[{"indexerId":42,"title":"Movie.2026.1080p.WEB-DL","size":1024,"downloadUrl":"`+testMagnet+`"}]`), nil
		case "qb.local":
			if strings.HasSuffix(r.URL.Path, "/auth/login") {
				if f.loginFailure {
					return jsonResponse(r, "Fails."), nil
				}
				return jsonResponse(r, "Ok."), nil
			}
			if !strings.HasSuffix(r.URL.Path, "/torrents/add") {
				t.Fatal("unexpected downloader operation", r.URL.Path)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("tags") != "NASTOOL" || !validMagnet(r.Form.Get("urls")) {
				t.Fatal("lost native preset or resource", r.Form)
			}
			f.adds++
			if f.afterAdd != nil {
				f.afterAdd()
			}
			if f.addError {
				return nil, errors.New("simulated connection lost after submission")
			}
			return jsonResponse(r, "Ok."), nil
		default:
			t.Fatal("unexpected Python/external request", r.URL.Hostname())
			return nil, context.Canceled
		}
	})
	f.transport = transport
	_, f.token, f.db, f.path = refreshFixture(t, transport)
	if err := config.NewStore(f.path).Update(map[string]any{"media.media_server": "emby", "emby.host": "http://emby.test", "emby.api_key": "library-secret"}); err != nil {
		t.Fatal(err)
	}
	var runner *rssRunAPI
	var err error
	f.handler, err = buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	downloaders := runner.download.downloaders
	downloader, err := downloaders.Upsert(t.Context(), downloaderconfig.Downloader{Name: "qB", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.local","port":8080,"username":"admin","password":"private-downloader"}`})
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"DefaultDownloader": text(downloader.ID), "UserIndexerSites": `["Selected-prowlarr"]`, "UserInstalledPlugins": `["Prowlarr"]`, "plugin.Prowlarr": `{"host":"http://indexer.local","api_key":"private-indexer"}`} {
		if err := runner.download.system.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`INSERT INTO RSS_MOVIES(ID,NAME,YEAR,TMDBID,STATE,FUZZY_MATCH,OVER_EDITION,FILTER_RULE,SEARCH_SITES) VALUES (1,'Movie','2026','100','D',0,0,-1,'["Selected-prowlarr"]'); INSERT INTO RSS_TVS(ID,NAME,YEAR,TMDBID,SEASON,STATE,FUZZY_MATCH,OVER_EDITION,FILTER_RULE,SEARCH_SITES,TOTAL,LACK) VALUES (2,'Show','2026','200','S01','D',0,0,-1,'["Selected-prowlarr"]',3,2); INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES ('2','2,3')`); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *subscriptionRunFixture) run(kind, id string) (int, string) {
	r := performFormRequest(f.handler, "/api/v1/subscriptions/search/run", f.token, url.Values{"type": {kind}, "id": {id}})
	return r.Code, r.Body.String()
}

func TestSubscriptionExecutionCommitsTVProgressAndCompletion(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	f.afterAdd = func() {
		if f.adds != 2 {
			return
		}
		var lack int
		var episodes string
		if err := f.db.QueryRow(`SELECT LACK FROM RSS_TVS WHERE ID=2`).Scan(&lack); err != nil || lack != 1 {
			t.Fatal(lack, err)
		}
		if err := f.db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='2'`).Scan(&episodes); err != nil || episodes != "3" {
			t.Fatal(episodes, err)
		}
	}
	status, body := f.run("TV", "2")
	if status != 200 || !strings.Contains(body, `"submitted":2`) || !strings.Contains(body, `"completed":true`) || f.adds != 2 || strings.Contains(body, "magnet:") || strings.Contains(body, "private") {
		t.Fatal(status, body, f.adds)
	}
	var n int
	for query, want := range map[string]int{`SELECT COUNT(*) FROM RSS_TVS`: 0, `SELECT COUNT(*) FROM RSS_TV_EPISODES`: 0, `SELECT COUNT(*) FROM RSS_HISTORY WHERE TYPE='TV' AND TMDBID='200'`: 1, `SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE STATE='submitted'`: 2, `SELECT COUNT(*) FROM RSS_TORRENTS`: 2} {
		if err := f.db.QueryRow(query).Scan(&n); err != nil || n != want {
			t.Fatal(query, n, err)
		}
	}
	if status, _ := f.run("TV", "2"); status != 404 || f.adds != 2 {
		t.Fatal("completed subscription was resubmitted", status, f.adds)
	}
}

func TestSubscriptionExecutionUncertainResultBlocksRetryAndPreservesMissing(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	f.addError = true
	status, body := f.run("TV", "2")
	if status != 502 || !strings.Contains(body, `"uncertain":true`) || !strings.Contains(body, `"remaining":[2,3]`) || f.adds != 1 {
		t.Fatal(status, body, f.adds)
	}
	var episodes string
	if err := f.db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='2'`).Scan(&episodes); err != nil || episodes != "2,3" {
		t.Fatal(episodes, err)
	}
	f.addError = false
	var err error
	f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := f.run("TV", "2"); status != 409 || f.adds != 1 {
		t.Fatal("uncertain submission retried", status, f.adds)
	}
}

func TestSubscriptionExecutionSharesRSSResourceReservations(t *testing.T) {
	for _, rssFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(rssFirst), func(t *testing.T) {
			f := newSubscriptionRunFixture(t)
			if _, err := f.db.Exec(`INSERT INTO CONFIG_USER_RSS(ID,NAME,USES) VALUES (7,'RSS','D')`); err != nil {
				t.Fatal(err)
			}
			store, err := rsstaskconfig.Open(filepath.Join(filepath.Dir(f.path), "user.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if rssFirst {
				if reserved, err := store.ReserveDownload(t.Context(), 7, testMagnet); err != nil || !reserved {
					t.Fatal(reserved, err)
				}
				if status, body := f.run("MOV", "1"); status != 409 || f.adds != 0 {
					t.Fatal(status, body, f.adds)
				}
			} else {
				f.addError = true
				if status, _ := f.run("MOV", "1"); status != 502 || f.adds != 1 {
					t.Fatal(status, f.adds)
				}
				if reserved, err := store.ReserveDownload(t.Context(), 7, testMagnet); err != nil || reserved {
					t.Fatal("RSS bypassed subscription claim", reserved, err)
				}
			}
		})
	}
}

func TestSubscriptionExecutionReactRouteAndPermissions(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	route := "/api/v1/subscriptions/MOV/1/refresh"
	if r := performFormRequest(f.handler, route, "", url.Values{}); r.Code != 401 || f.queries != 0 || f.adds != 0 {
		t.Fatal(r.Code)
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	if r := performFormRequest(f.handler, route, viewer, url.Values{}); r.Code != 403 || f.queries != 0 || f.adds != 0 {
		t.Fatal(r.Code)
	}
	if r := performFormRequest(f.handler, route, f.token, url.Values{}); r.Code != 200 || f.adds != 1 {
		t.Fatal(r.Code, r.Body.String())
	}
	transition, err := newHandler(config.Config{ApplicationConfigPath: f.path, LegacyBackendURL: "http://legacy.local"}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	if r := performFormRequest(transition, "/api/v1/subscriptions/search/run", f.token, url.Values{"type": {"MOV"}, "id": {"1"}}); r.Code != 501 || f.adds != 1 {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestSubscriptionExecutionAuthenticationRejectionReleasesClaim(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	f.loginFailure = true
	if status, body := f.run("MOV", "1"); status != 422 || f.adds != 0 {
		t.Fatal(status, body, f.adds)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	f.loginFailure = false
	if status, body := f.run("MOV", "1"); status != 200 || f.adds != 1 || !strings.Contains(body, `"completed":true`) {
		t.Fatal(status, body, f.adds)
	}
}

func TestSubscriptionExecutionPersistenceFailureKeepsPendingClaim(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_progress BEFORE UPDATE OF LACK ON RSS_TVS BEGIN SELECT RAISE(ABORT,'reject'); END`); err != nil {
		t.Fatal(err)
	}
	if status, body := f.run("TV", "2"); status != 502 || f.adds != 1 || !strings.Contains(body, `"uncertain":true`) {
		t.Fatal(status, body, f.adds)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE STATE='pending'`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&n); err != nil || n != 0 {
		t.Fatal("partial transaction escaped rollback", n, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_progress`); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.run("TV", "2"); status != 409 || f.adds != 1 {
		t.Fatal(status, f.adds)
	}
}

func TestSubscriptionExecutionPartialSuccessConsumesOnlyAcceptedEpisodes(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	f.afterAdd = func() {
		if f.adds == 2 {
			f.addError = true
		}
	}
	status, body := f.run("TV", "2")
	if status != 502 || f.adds != 2 || !strings.Contains(body, `"submitted":1`) || !strings.Contains(body, `"remaining":[3]`) {
		t.Fatal(status, body, f.adds)
	}
	var episodes, state string
	var lack int
	if err := f.db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='2'`).Scan(&episodes); err != nil || episodes != "3" {
		t.Fatal(episodes, err)
	}
	if err := f.db.QueryRow(`SELECT STATE,LACK FROM RSS_TVS WHERE ID=2`).Scan(&state, &lack); err != nil || state != "R" || lack != 1 {
		t.Fatal(state, lack, err)
	}
	f.addError, f.afterAdd = false, nil
	if status, _ := f.run("TV", "2"); status != 409 || f.adds != 2 {
		t.Fatal(status, f.adds)
	}
}

func TestSubscriptionReservationsSerializeSeparateRunnersAndVerifyOwner(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	rows, err := readSubscriptionRows(t.Context(), f.db, "RSS_MOVIES")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	service := subscriptionService{databasePath: filepath.Join(filepath.Dir(f.path), "user.db")}
	plan := subscriptionSearchPlan{raw: rows[0], mediaID: "100"}
	type reservation struct {
		candidate subscriptionCandidate
		owner     string
		err       error
	}
	results := make(chan reservation, 2)
	for _, n := range []int{12, 13} {
		item := candidateFixture(t, "Movie.2026.1080p.WEB-DL", n, nil)
		candidate := subscriptionCandidate{Title: item.resource.Title, resource: item.resource}
		go func() {
			owner, err := service.reserveSubscriptionSubmission(t.Context(), "MOV", 1, plan, candidate)
			results <- reservation{candidate, owner, err}
		}()
	}
	accepted := []reservation{}
	for range 2 {
		r := <-results
		if r.err == nil {
			accepted = append(accepted, r)
		}
	}
	if len(accepted) != 1 {
		t.Fatal("different URLs bypassed subscription reservation", accepted)
	}
	winner := accepted[0]
	if err := service.releaseSubscriptionSubmission(t.Context(), winner.candidate.resource.DownloadURL, "wrong-owner"); !errors.Is(err, errSubscriptionSubmissionConflict) {
		t.Fatal(err)
	}
	if err := service.releaseSubscriptionSubmission(t.Context(), winner.candidate.resource.DownloadURL, winner.owner); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionExecutionRejectsEditsBeforeAndAfterSubmission(t *testing.T) {
	for _, afterAcceptance := range []bool{false, true} {
		t.Run(fmt.Sprint(afterAcceptance), func(t *testing.T) {
			f := newSubscriptionRunFixture(t)
			edit := func() {
				if _, err := f.db.Exec(`UPDATE RSS_MOVIES SET FILTER_INCLUDE='user-edit' WHERE ID=1`); err != nil {
					t.Fatal(err)
				}
			}
			if afterAcceptance {
				f.afterAdd = edit
			} else {
				f.afterSearch = edit
			}
			status, body := f.run("MOV", "1")
			want, adds := 409, 0
			if afterAcceptance {
				want, adds = 502, 1
			}
			if status != want || f.adds != adds {
				t.Fatal(status, body, f.adds)
			}
			var filter string
			if err := f.db.QueryRow(`SELECT FILTER_INCLUDE FROM RSS_MOVIES WHERE ID=1`).Scan(&filter); err != nil || filter != "user-edit" {
				t.Fatal(filter, err)
			}
		})
	}
}

func TestSubscriptionExecutionExistingLibraryFinishesWithoutSearch(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	f.libraryComplete = true
	status, body := f.run("MOV", "1")
	if status != 200 || f.adds != 0 || f.queries != 0 || !strings.Contains(body, `"completed":true`) {
		t.Fatal(status, body, f.adds, f.queries)
	}
}

func TestSubscriptionExecutionNoResourcesKeepsSubscriptionActive(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	f.noResults = true
	for kind, id := range map[string]string{"MOV": "1", "TV": "2"} {
		status, body := f.run(kind, id)
		if status != 200 || f.adds != 0 || !strings.Contains(body, `"completed":false`) {
			t.Fatal(status, body, f.adds)
		}
		var state string
		if err := f.db.QueryRow("SELECT STATE FROM "+subscriptionTable(kind)+" WHERE ID=?", id).Scan(&state); err != nil || state != "R" {
			t.Fatal(state, err)
		}
	}
	var episodes string
	if err := f.db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='2'`).Scan(&episodes); err != nil || episodes != "2,3" {
		t.Fatal(episodes, err)
	}
}
