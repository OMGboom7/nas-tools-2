package httpserver

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestSubscriptionSearchUsesCurrentCustomWordsWithoutPython(t *testing.T) {
	t.Parallel()
	var searches []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "tmdb.test" {
			t.Fatalf("unexpected legacy request: %s", request.URL)
		}
		switch request.URL.Path {
		case "/3/search/movie":
			if request.URL.Query().Get("year") != "2025" {
				t.Errorf("parsed search year = %q", request.URL.Query().Get("year"))
			}
			query := request.URL.Query().Get("query")
			searches = append(searches, query)
			if query == "Original" {
				return jsonResponse(request, `{"results":[{"id":100,"title":"Original","release_date":"2025-01-01"}]}`), nil
			}
			if query == "Updated" {
				return jsonResponse(request, `{"results":[{"id":101,"title":"Updated","release_date":"2025-01-01"}]}`), nil
			}
			t.Fatalf("unprocessed query: %q", query)
		case "/3/movie/100":
			return jsonResponse(request, `{"id":100,"title":"Original","release_date":"2025-01-01"}`), nil
		case "/3/movie/101":
			return jsonResponse(request, `{"id":101,"title":"Updated","release_date":"2025-01-01"}`), nil
		default:
			t.Fatalf("unexpected endpoint: %s", request.URL)
		}
		return nil, nil
	})
	handler, token, path := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	post := func(endpoint string, form url.Values) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		request.Header.Set("Authorization", token)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 || strings.Contains(response.Body.String(), `"code":1`) {
			t.Fatalf("%s: %d %s", endpoint, response.Code, response.Body.String())
		}
	}
	word := url.Values{"id": {"0"}, "gid": {"-1"}, "group_type": {"1"}, "type": {"2"}, "enabled": {"1"}, "regex": {"0"}, "new_replaced": {"Wrong"}, "new_replace": {"Original"}}
	post("/api/v1/words/item/update", word)
	form := url.Values{"type": {"MOV"}, "name": {"[Group] Wrong.2025.1080p.WEB-DL.H264.mkv"}, "in_form": {"manual"}}
	post("/api/v1/subscribe/add", form)
	word.Set("id", "1")
	word.Set("new_replace", "Updated")
	post("/api/v1/words/item/update", word)
	post("/api/v1/subscribe/add", form)
	if len(searches) != 2 || searches[0] != "Original" || searches[1] != "Updated" {
		t.Fatalf("queries = %#v", searches)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES WHERE (TMDBID='100' AND NAME='Original') OR (TMDBID='101' AND NAME='Updated')`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("stored subscriptions = %d, err = %v", count, err)
	}
}
