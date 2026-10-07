package httpserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

func rssRunFixture(t *testing.T, transport http.RoundTripper) (http.Handler, string, *sql.DB, string) {
	t.Helper()
	downloader := downloaderconfig.Downloader{Name: "qB", Type: "qbittorrent", Enabled: 1, Config: `{"host":"client.local","port":8080,"username":"admin","password":"secret"}`}
	_, _, path := nativeServicesFixture(t, "", &downloader, transport)
	appPath := filepath.Join(filepath.Dir(path), "config.yaml")
	// Polling from a separate connection must use the same lock wait as the
	// native stores; SQLite's default zero timeout spuriously fails while the
	// worker commits its atomic subscription/marker/counter transaction.
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	format := `{"list":"$.items","item":{"title":{"path":"title"},"enclosure":{"path":"enclosure"},"size":{"path":"size"},"type":{"path":"type"}}}`
	if _, err := db.Exec(`INSERT INTO CONFIG_RSS_PARSER (ID,NAME,TYPE,FORMAT) VALUES (1,'JSON','JSON',?)`, format); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO CONFIG_USER_RSS (ID,NAME,ADDRESS,PARSER,USES,RECOGNIZATION,INCLUDE,EXCLUDE,FILTER,PROCESS_COUNT) VALUES (7,'Run','["https://feed.local/rss"]','[1]','D','N','Movie|Show','Exclude','-1','0')`); err != nil {
		t.Fatal(err)
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: appPath, DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	return handler, loginForTest(t, handler, "admin", "password"), db, appPath
}

func rssRunFeed(title string, n int, kind string) string {
	body, _ := json.Marshal(map[string]any{"items": []map[string]any{{"title": title, "enclosure": fmt.Sprintf("magnet:?xt=urn:btih:%040x", n), "size": "1024", "type": kind}}})
	return string(body)
}

func TestRSSRunDownloadsFiltersAndPersistsWithoutPython(t *testing.T) {
	adds, feeds := 0, 0
	feed := `{"items":[{"title":"Movie.2026.1080p","enclosure":"` + testMagnet + `","size":1024},{"title":"Movie.duplicate","enclosure":"` + testMagnet + `"},{"title":"Exclude Movie","enclosure":"magnet:?xt=urn:btih:1111111111111111111111111111111111111111"},{"title":""}]}`
	failAdd := false
	rejectAuth := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "feed.local":
			feeds++
			return jsonResponse(r, feed), nil
		case "client.local":
			if strings.HasSuffix(r.URL.Path, "/auth/login") {
				if rejectAuth {
					response := jsonResponse(r, "Forbidden")
					response.StatusCode = 403
					return response, nil
				}
				return jsonResponse(r, "Ok."), nil
			}
			if r.URL.Path != "/api/v2/torrents/add" || r.ParseForm() != nil || !validMagnet(r.Form.Get("urls")) {
				t.Fatal(r.URL, r.Form)
			}
			adds++
			if failAdd {
				return nil, errors.New("uncertain network response with private detail")
			}
			return jsonResponse(r, "Ok."), nil
		default:
			t.Fatalf("Python/network request prohibited: %s", r.URL)
		}
		return nil, nil
	})
	handler, token, db, path := rssRunFixture(t, transport)
	run := func() string {
		t.Helper()
		response := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "magnet:") {
			t.Fatal("credentials leaked")
		}
		return response.Body.String()
	}
	if body := run(); !strings.Contains(body, `"downloaded":1`) || !strings.Contains(body, `"skipped":3`) || adds != 1 {
		t.Fatal(body, adds)
	}
	var count string
	if err := db.QueryRow(`SELECT PROCESS_COUNT FROM CONFIG_USER_RSS WHERE ID=7`).Scan(&count); err != nil || count != "1" {
		t.Fatal(count, err)
	}
	// Rebuild the HTTP service to prove deduplication is database-backed.
	var err error
	handler, err = newHandler(config.Config{ApplicationConfigPath: path, DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	if body := run(); !strings.Contains(body, `"downloaded":0`) || adds != 1 {
		t.Fatal(body, adds)
	}
	feed = rssRunFeed("Movie.2026.new", 2, "movie")
	failAdd = true
	if body := run(); !strings.Contains(body, `"uncertain":1`) || !strings.Contains(body, `"success":false`) || adds != 2 {
		t.Fatal(body, adds)
	}
	if body := run(); !strings.Contains(body, `"uncertain":1`) || adds != 2 {
		t.Fatal(body, adds)
	}
	manual := performFormRequest(handler, "/api/v1/rss/item/download", token, url.Values{"taskid": {"7"}, "articles": {fmt.Sprintf(`[{"title":"Movie","enclosure":"magnet:?xt=urn:btih:%040x"}]`, 2)}})
	if manual.Code != 409 || adds != 2 {
		t.Fatal("manual path bypassed uncertain reservation", manual.Code, adds)
	}
	feed = rssRunFeed("Movie.2026.retry", 3, "movie")
	rejectAuth = true
	rejected := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if rejected.Code != 422 || adds != 2 {
		t.Fatal(rejected.Code, rejected.Body.String(), adds)
	}
	rejectAuth, failAdd = false, false
	if body := run(); !strings.Contains(body, `"downloaded":1`) || adds != 3 {
		t.Fatal("proven rejection was not retryable", body, adds)
	}
	before := feeds
	if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET INCLUDE='[' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	response := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if response.Code != 422 || feeds != before {
		t.Fatal(response.Code, response.Body.String(), feeds)
	}
	if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET USES='X' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	response = performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if response.Code != 422 || feeds != before {
		t.Fatal(response.Code, response.Body.String(), feeds)
	}
	if response := performFormRequest(handler, "/api/v1/rss/run", "", url.Values{"id": {"7"}}); response.Code != 401 {
		t.Fatal(response.Code)
	}
	created := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	if response := performFormRequest(handler, "/api/v1/rss/run", viewer, url.Values{"id": {"7"}}); response.Code != 403 || feeds != before {
		t.Fatal(response.Code, response.Body.String())
	}
	transition, err := newHandler(config.Config{ApplicationConfigPath: path, LegacyBackendURL: "http://legacy.local"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	response = performFormRequest(transition, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if response.Code != 501 || feeds != before {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestRSSRunRecognitionInventoryAndTVIdentity(t *testing.T) {
	adds := 0
	feed := rssRunFeed("Movie.2026.1080p", 10, "movie")
	exists, inventoryError := false, false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "feed.local":
			return jsonResponse(r, feed), nil
		case "tmdb.test":
			switch r.URL.Path {
			case "/3/search/movie":
				return jsonResponse(r, `{"results":[{"id":100,"title":"Movie","release_date":"2026-01-01"}]}`), nil
			case "/3/movie/100":
				return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01"}`), nil
			case "/3/search/tv":
				return jsonResponse(r, `{"results":[{"id":200,"name":"Show","first_air_date":"2026-01-01"}]}`), nil
			case "/3/tv/200":
				return jsonResponse(r, `{"id":200,"name":"Show","first_air_date":"2026-01-01","seasons":[{"season_number":1,"episode_count":2}]}`), nil
			}
		case "emby.test":
			if inventoryError {
				response := jsonResponse(r, `{"private":"hidden"}`)
				response.StatusCode = 500
				return response, nil
			}
			if exists {
				return jsonResponse(r, `{"Items":[{"Id":"movie","Name":"Movie","ProductionYear":2026,"ProviderIds":{"Tmdb":"100"}}]}`), nil
			}
			return jsonResponse(r, `{"Items":[]}`), nil
		case "client.local":
			if strings.HasSuffix(r.URL.Path, "/auth/login") {
				return jsonResponse(r, "Ok."), nil
			}
			adds++
			return jsonResponse(r, "Ok."), nil
		}
		t.Fatalf("unexpected upstream; Python prohibited: %s", r.URL)
		return nil, nil
	})
	handler, token, db, path := rssRunFixture(t, transport)
	if err := config.NewStore(path).Update(map[string]any{"app.rmt_tmdbkey": "tmdb-secret", "app.tmdb_domain": "tmdb.test", "media.media_server": "emby", "emby.host": "http://emby.test", "emby.api_key": "library-secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET RECOGNIZATION='Y' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	run := func() *http.Response {
		response := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		return response.Result()
	}
	exists = true
	run()
	if adds != 0 {
		t.Fatal("existing movie was downloaded")
	}
	exists = false
	run()
	if adds != 1 {
		t.Fatal(adds)
	}
	// No season marker: the declared feed type must select TV, not movie.
	feed = rssRunFeed("Show.2026.1080p", 11, "tv")
	run()
	if adds != 2 {
		t.Fatal(adds)
	}
	var identities string
	if err := db.QueryRow(`SELECT MEDIAINFOS FROM CONFIG_USER_RSS WHERE ID=7`).Scan(&identities); err != nil || !strings.Contains(identities, `"id":"200"`) || !strings.Contains(identities, `"season":1`) {
		t.Fatal(identities, err)
	}
	feed = rssRunFeed("Movie.2026.2160p", 12, "movie")
	inventoryError = true
	response := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if response.Code != 503 || adds != 2 || strings.Contains(response.Body.String(), "hidden") {
		t.Fatal(response.Code, response.Body.String(), adds)
	}
}

func TestRSSRunRejectsOverlappingExecution(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "feed.local" {
			return jsonResponse(r, rssRunFeed("Movie.2026", 20, "movie")), nil
		}
		if r.URL.Hostname() != "client.local" {
			return nil, errors.New("unexpected upstream")
		}
		if strings.HasSuffix(r.URL.Path, "/auth/login") {
			return jsonResponse(r, "Ok."), nil
		}
		close(entered)
		select {
		case <-release:
			return jsonResponse(r, "Ok."), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	handler, token, _, _ := rssRunFixture(t, transport)
	done := make(chan int, 1)
	go func() {
		response := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
		done <- response.Code
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("RSS execution did not reach downloader")
	}
	response := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if response.Code != 409 {
		t.Fatal(response.Code, response.Body.String())
	}
	once.Do(func() { close(release) })
	select {
	case code := <-done:
		if code != 200 {
			t.Fatal(code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RSS execution did not finish")
	}
}
