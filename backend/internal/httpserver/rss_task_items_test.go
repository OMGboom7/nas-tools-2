package httpserver

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNativeRSSTaskHistoryAndArticleStateWithoutPython(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("RSS item used legacy backend: %s", request.URL)
		return nil, fmt.Errorf("legacy backend disabled")
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO CONFIG_USER_RSS (ID,NAME,USES) VALUES (7,'Download','D'),(8,'Subscribe','R')`); err != nil {
		t.Fatal(err)
	}
	if response := performFormRequest(handler, "/api/v1/rss/item/history", "", url.Values{"id": {"7"}}); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated history=%d", response.Code)
	}
	empty := performFormRequest(handler, "/api/v1/rss/item/history", token, url.Values{"id": {"7"}})
	if empty.Code != 200 || !strings.Contains(empty.Body.String(), `"code":1`) {
		t.Fatalf("empty history=%d %s", empty.Code, empty.Body.String())
	}
	if _, err := db.Exec(`INSERT INTO USERRSS_TASK_HISTORY (TASK_ID,TITLE,DOWNLOADER,DATE) VALUES ('7','Older','qB','2026-01-01 10:00:00'),('7','Newer','115','2026-01-02 10:00:00'),('8','Other','qB','2026-01-03 10:00:00')`); err != nil {
		t.Fatal(err)
	}
	history := performFormRequest(handler, "/api/v1/rss/item/history", token, url.Values{"id": {"7"}})
	if history.Code != 200 || !strings.Contains(history.Body.String(), `"count":2`) || strings.Contains(history.Body.String(), "Other") || strings.Index(history.Body.String(), "Newer") > strings.Index(history.Body.String(), "Older") {
		t.Fatalf("history=%d %s", history.Code, history.Body.String())
	}
	if _, err := db.Exec(`CREATE TRIGGER block_second_rss BEFORE INSERT ON RSS_TORRENTS WHEN NEW.TORRENT_NAME='Fail' BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	articles := `[{"title":"Good","enclosure":"https://site.example/good","year":"2026"},{"title":"Fail","enclosure":"https://site.example/fail"}]`
	failed := performFormRequest(handler, "/api/v1/rss/item/set", token, url.Values{"taskid": {"7"}, "flag": {"set_finished"}, "articles": {articles}})
	if failed.Code != http.StatusBadGateway {
		t.Fatalf("failed batch=%d %s", failed.Code, failed.Body.String())
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial RSS batch persisted: count=%d err=%v", count, err)
	}
	if _, err := db.Exec(`DROP TRIGGER block_second_rss`); err != nil {
		t.Fatal(err)
	}
	saved := performFormRequest(handler, "/api/v1/rss/item/set", token, url.Values{"taskid": {"7"}, "flag": {"set_finished"}, "articles": {articles}})
	if saved.Code != 200 || !strings.Contains(saved.Body.String(), `"code":0`) {
		t.Fatalf("save batch=%d %s", saved.Code, saved.Body.String())
	}
	saved = performFormRequest(handler, "/api/v1/rss/item/set", token, url.Values{"taskid": {"7"}, "flag": {"set_finished"}, "articles": {articles}})
	if saved.Code != 200 {
		t.Fatalf("repeat batch=%d %s", saved.Code, saved.Body.String())
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate RSS articles: count=%d err=%v", count, err)
	}
	removed := performFormRequest(handler, "/api/v1/rss/item/set", token, url.Values{"taskid": {"7"}, "flag": {"set_unfinish"}, "articles": {`[{"title":"Good","enclosure":"https://site.example/good","year":"2026"}]`}})
	if removed.Code != 200 || !strings.Contains(removed.Body.String(), `"code":0`) {
		t.Fatalf("unset article=%d %s", removed.Code, removed.Body.String())
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unset count=%d err=%v", count, err)
	}
	subscribed := performFormRequest(handler, "/api/v1/rss/item/set", token, url.Values{"taskid": {"8"}, "flag": {"set_finished"}, "articles": {`[{"title":"Movie","enclosure":"https://site.example/movie","year":"2026"}]`}})
	if subscribed.Code != 200 {
		t.Fatalf("set subscription article=%d %s", subscribed.Code, subscribed.Body.String())
	}
	var enclosure string
	if err := db.QueryRow(`SELECT ENCLOSURE FROM RSS_TORRENTS WHERE TORRENT_NAME='Movie 2026'`).Scan(&enclosure); err != nil || enclosure != "Movie 2026" {
		t.Fatalf("subscription enclosure=%q err=%v", enclosure, err)
	}
	invalid := performFormRequest(handler, "/api/v1/rss/item/set", token, url.Values{"taskid": {"7"}, "flag": {"invalid"}, "articles": {articles}})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid flag=%d %s", invalid.Code, invalid.Body.String())
	}
}
