package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
)

func candidateFixture(t *testing.T, title string, n int, seed *int64) identifiedSubscriptionResource {
	t.Helper()
	meta, err := mediameta.ParseWithOptions(t.Context(), title, "", mediameta.LabelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return identifiedSubscriptionResource{resource: externalindexer.Resource{Title: title, Indexer: "saved-site", DownloadURL: fmt.Sprintf("magnet:?xt=urn:btih:%040x", n), Size: 1 << 30, Seeders: seed}, meta: meta, revised: title}
}

func TestSubscriptionCandidatesCoverMissingEpisodesWithoutDuplicates(t *testing.T) {
	seed := int64(5)
	resources := []identifiedSubscriptionResource{
		candidateFixture(t, "Show.S01E02.1080p.WEB-DL-Team", 1, &seed),
		candidateFixture(t, "Show.S01E01-E03.1080p.WEB-DL-Team", 2, &seed),
		candidateFixture(t, "Show.S01E04.1080p.WEB-DL-Team", 3, nil),
		candidateFixture(t, "Show.S02E03.1080p.WEB-DL-Team", 4, &seed),
		candidateFixture(t, "Show.S01E01.1080p.WEB-DL-Team", 5, &seed),
		candidateFixture(t, "Show.S01E04.720p.WEB-DL-Team", 6, &seed),
	}
	input := subscriptionCandidateInput{TV: true, Season: 1, Total: 4, Missing: []int{2, 3, 4}, Group: -1, Quality: "WEB", Resolution: "1080p", Team: "Team", Include: "Show", Exclude: "Exclude"}
	selected, remaining, err := planSubscriptionCandidates(t.Context(), input, nil, resources)
	if err != nil || len(selected) != 2 || len(remaining) != 0 || len(selected[0].Episodes) != 2 || selected[0].Episodes[0] != 2 || selected[0].Episodes[1] != 3 || selected[1].Episodes[0] != 4 {
		t.Fatal(selected, remaining, err)
	}
	encoded, err := json.Marshal(selected)
	if err != nil || strings.Contains(string(encoded), "magnet:") {
		t.Fatal("private download locator leaked", string(encoded), err)
	}
	if len(input.Missing) != 3 {
		t.Fatal("planner mutated persisted input")
	}
	chain := candidateFixture(t, "Show.S01E02E04.1080p.WEB-DL-Team", 12, &seed)
	selected, remaining, err = planSubscriptionCandidates(t.Context(), input, nil, []identifiedSubscriptionResource{chain})
	if err != nil || len(selected) != 1 || len(selected[0].Episodes) != 2 || len(remaining) != 1 || remaining[0] != 3 {
		t.Fatal("explicit chain invented intermediate episodes", selected, remaining, err)
	}
	chain.revised = "Show.S01.1080p.WEB-DL-Team"
	chain.subtitle = "E02E04"
	selected, remaining, err = planSubscriptionCandidates(t.Context(), input, nil, []identifiedSubscriptionResource{chain})
	if err != nil || len(selected) != 1 || len(remaining) != 1 || remaining[0] != 3 {
		t.Fatal("subtitle chain invented intermediate episodes", selected, remaining, err)
	}
	input.Include = "["
	if _, _, err := planSubscriptionCandidates(t.Context(), input, nil, resources); err == nil {
		t.Fatal("invalid regex accepted")
	}
	input.Include = "Show"
	input.Resolution = "unsupported"
	if _, _, err := planSubscriptionCandidates(t.Context(), input, nil, resources); err == nil {
		t.Fatal("unknown label widened filters")
	}
}

func TestSubscriptionCandidatesKeepUnknownPromotionAndMovieRanking(t *testing.T) {
	low, high := int64(1), int64(9)
	resources := []identifiedSubscriptionResource{candidateFixture(t, "Movie.2026.1080p.WEB-DL", 1, nil), candidateFixture(t, "Movie.2026.1080p.WEB-DL", 2, &low), candidateFixture(t, "Movie.2026.1080p.WEB-DL", 3, &high)}
	input := subscriptionCandidateInput{Group: -1}
	selected, _, err := planSubscriptionCandidates(t.Context(), input, nil, resources)
	if err != nil || len(selected) != 1 || selected[0].resource.DownloadURL != resources[2].resource.DownloadURL {
		t.Fatal(selected, err)
	}
	groups := []filterconfig.GroupInfo{{Group: filterconfig.Group{ID: 1}, Rules: []filterconfig.Rule{{ID: 1, Priority: "1", Free: "1 0"}}}}
	input.Group = 1
	selected, _, err = planSubscriptionCandidates(t.Context(), input, groups, resources)
	if err != nil || len(selected) != 0 {
		t.Fatal("unknown promotion bypassed restriction", selected, err)
	}
	up, down := float64(1), float64(0)
	resources[0].resource.UploadFactor = &up
	resources[0].resource.DownloadFactor = &down
	selected, _, err = planSubscriptionCandidates(t.Context(), input, groups, resources)
	if err != nil || len(selected) != 1 || selected[0].Priority != 99 {
		t.Fatal(selected, err)
	}
}

func TestNativeSubscriptionSearchPlanIsReadOnlyAndIdentityVerified(t *testing.T) {
	queries := 0
	tv := false
	inventoryFailure := false
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
				name := "Movie"
				id := 100
				if strings.Contains(r.URL.Query().Get("query"), "Other") {
					name = "Other"
					id = 101
				}
				return jsonResponse(r, fmt.Sprintf(`{"results":[{"id":%d,"title":"%s","release_date":"2026-01-01"}]}`, id, name)), nil
			}
			if r.URL.Path == "/3/movie/101" {
				return jsonResponse(r, `{"id":101,"title":"Other","release_date":"2026-01-01"}`), nil
			}
			return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01"}`), nil
		case "emby.test":
			response := jsonResponse(r, `{"Items":[]}`)
			if inventoryFailure {
				response.StatusCode = 500
			}
			return response, nil
		case "indexer.local":
			if r.URL.Path == "/api/v1/indexerstats" {
				return jsonResponse(r, `{"indexers":[{"indexerId":42,"indexerName":"Selected"}]}`), nil
			}
			queries++
			if tv {
				return jsonResponse(r, `[{"indexerId":42,"title":"Show.S01E02-E03.1080p.WEB-DL","size":1024,"downloadUrl":"`+testMagnet+`"}]`), nil
			}
			return jsonResponse(r, `[{"indexerId":42,"title":"Other.2026.1080p.WEB-DL","size":1024,"downloadUrl":"`+testMagnet+`"},{"indexerId":42,"title":"Movie.2026.1080p.WEB-DL","size":1024,"downloadUrl":"`+testMagnet+`"}]`), nil
		default:
			t.Error("unexpected downloader/Python request")
			return nil, context.Canceled
		}
	})
	handler, token, db, path := refreshFixture(t, transport)
	if err := config.NewStore(path).Update(map[string]any{"media.media_server": "emby", "emby.host": "http://emby.test", "emby.api_key": "private-library"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO RSS_MOVIES(ID,NAME,YEAR,TMDBID,STATE,FUZZY_MATCH,OVER_EDITION,FILTER_RULE,SEARCH_SITES,FILTER_RESTYPE,FILTER_PIX) VALUES (1,'Movie','2026','100','D',0,0,-1,'["Selected-prowlarr"]','WEB','1080p')`); err != nil {
		t.Fatal(err)
	}
	var runner *rssRunAPI
	var err error
	handler, err = buildHandler(config.Config{ApplicationConfigPath: path, DisableLegacy: true}, transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	store := runner.recognition.service.systemConfig
	for key, value := range map[string]string{"UserIndexerSites": `["Selected-prowlarr"]`, "UserInstalledPlugins": `["Prowlarr"]`, "plugin.Prowlarr": `{"host":"http://indexer.local","api_key":"private-indexer"}`} {
		if err := store.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	response := performFormRequest(handler, "/api/v1/subscriptions/search/plan", token, url.Values{"type": {"MOV"}, "id": {"1"}})
	if response.Code != 200 || strings.Contains(response.Body.String(), "Other") || !strings.Contains(response.Body.String(), "Movie.2026") || strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "magnet:") {
		t.Fatal(response.Code, response.Body.String())
	}
	var state string
	if err := db.QueryRow(`SELECT STATE FROM RSS_MOVIES WHERE ID=1`).Scan(&state); err != nil || state != "D" {
		t.Fatal("planner changed subscription", state, err)
	}
	tv = true
	if _, err := db.Exec(`INSERT INTO RSS_TVS(ID,NAME,YEAR,TMDBID,SEASON,STATE,FUZZY_MATCH,OVER_EDITION,FILTER_RULE,SEARCH_SITES,TOTAL,LACK) VALUES (2,'Show','2026','200','S01','D',0,0,-1,'["Selected-prowlarr"]',3,2);INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES ('2','2,3')`); err != nil {
		t.Fatal(err)
	}
	response = performFormRequest(handler, "/api/v1/subscriptions/search/plan", token, url.Values{"type": {"TV"}, "id": {"2"}})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"episodes":[2,3]`) || !strings.Contains(response.Body.String(), `"remaining":[]`) {
		t.Fatal(response.Code, response.Body.String())
	}
	var episodes string
	if err := db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='2'`).Scan(&episodes); err != nil || episodes != "2,3" {
		t.Fatal("planning consumed actual missing episodes", episodes, err)
	}
	inventoryFailure = true
	before := queries
	response = performFormRequest(handler, "/api/v1/subscriptions/search/plan", token, url.Values{"type": {"MOV"}, "id": {"1"}})
	if response.Code != 503 || queries != before {
		t.Fatal("unknown inventory caused search", response.Code, queries)
	}
	inventoryFailure = false
	if _, err := db.Exec(`UPDATE RSS_MOVIES SET SEARCH_SITES='malformed-json' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	response = performFormRequest(handler, "/api/v1/subscriptions/search/plan", token, url.Values{"type": {"MOV"}, "id": {"1"}})
	if response.Code != 422 || queries != before {
		t.Fatal("bad stored scope widened search", response.Code, queries)
	}
	if response := performFormRequest(handler, "/api/v1/subscriptions/search/plan", "", url.Values{"type": {"MOV"}, "id": {"1"}}); response.Code != 401 {
		t.Fatal(response.Code)
	}
	created := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	response = performFormRequest(handler, "/api/v1/subscriptions/search/plan", viewer, url.Values{"type": {"MOV"}, "id": {"1"}})
	if response.Code != 403 || queries != before {
		t.Fatal(response.Code, queries)
	}
	transition, err := newHandler(config.Config{ApplicationConfigPath: path, LegacyBackendURL: "http://legacy.local"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	response = performFormRequest(transition, "/api/v1/subscriptions/search/plan", token, url.Values{"type": {"MOV"}, "id": {"1"}})
	if response.Code != 501 || queries != before {
		t.Fatal(response.Code, queries)
	}
}
