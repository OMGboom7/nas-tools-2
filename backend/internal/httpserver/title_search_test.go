package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
)

func TestTitleSearchTMDBProviderRoutingAndRanking(t *testing.T) {
	cfg := map[string]any{"app": map[string]any{"rmt_tmdbkey": "fixture"}, "media": map[string]any{"tmdb_include_adult": true}}
	var requests []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if q.Get("api_key") != "fixture" || q.Get("include_adult") != "true" {
			t.Fatal(q)
		}
		requests = append(requests, r.URL.Path)
		var body string
		switch r.URL.Path {
		case "/3/search/multi":
			if q.Get("language") != "en" || q.Get("page") != "2" {
				t.Fatal(q)
			}
			body = `[{"id":3,"name":"Person","media_type":"person"},{"id":4,"media_type":"tv","original_name":"Show","nested":{"preserve":true}}]`
		case "/3/search/movie":
			if q.Get("language") != "zh" || q.Get("year") != "2025" || q.Get("query") != "作品" || q.Get("page") != "1" {
				t.Fatal(q)
			}
			var movies []string
			for i := 1; i <= 20; i++ {
				movies = append(movies, fmt.Sprintf(`{"id":%d,"title":"作品","release_date":"2025-01-%02d"}`, i, i))
			}
			body = "[" + strings.Join(movies, ",") + "]"
		case "/3/search/tv":
			if q.Get("first_air_date_year") != "2025" || q.Get("language") != "zh" {
				t.Fatal(q)
			}
			body = `[{"id":19,"name":"作品集","first_air_date":"2025-12-01"},{"id":55,"name":"Other"}]`
		default:
			t.Fatal(r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"results":` + body + `}`))}, nil
	})}
	multi, err := searchTMDBTitles(context.Background(), client, cfg, titleQuery{Title: "Show", Page: 2})
	if err != nil || len(multi) != 1 || multi[0].Kind != "tv" || !strings.Contains(string(multi[0].Info), `"preserve":true`) {
		t.Fatal(multi, err)
	}
	first, err := searchTMDBTitles(context.Background(), client, cfg, titleQuery{Title: "作品", Year: "2025", Page: 1})
	if err != nil || len(first) != 20 || first[0].Kind != "tv" || first[1].date != "2025-01-20" {
		t.Fatal(first, err)
	}
	second, err := searchTMDBTitles(context.Background(), client, cfg, titleQuery{Title: "作品", Year: "2025", Page: 2})
	if err != nil || len(second) != 1 || second[0].date != "2025-01-01" || len(requests) != 5 {
		t.Fatal(second, err)
	}
}

func TestChatChoiceCommandAcrossRestartsAndSources(t *testing.T) {
	cfg := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg), "user.db"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	call := func(op, channel, sender string, fields map[string]any) map[string]any {
		t.Helper()
		fields["channel"], fields["sender"] = channel, sender
		body, _ := json.Marshal(fields)
		var output strings.Builder
		if err := SearchTitlesCommand(t.Context(), cfg, op, strings.NewReader(string(body)), &output); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(output.String()), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	store, err := notificationconfig.Open(filepath.Join(filepath.Dir(cfg), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	channel := notificationconfig.Channel{Type: "telegram", Config: `{"token":"24:fixture","user_ids":"9007199254740993,2"}`, Enabled: 1, Interactive: 1}
	id, err := store.Save(t.Context(), channel)
	if err != nil {
		t.Fatal(err)
	}
	tg, u := fmt.Sprintf("TG/%d/%x", id, sha256.Sum256([]byte("24:fixture"))), "9007199254740993"
	start := func(msg string) map[string]any {
		return call("start", tg, u, map[string]any{"message": msg, "chatEnabled": true})
	}
	old := start("搜索：电影")
	fresh := start("订阅： 剧集")
	if fresh["query"] != "剧集" || fresh["action"] != "subscribe" {
		t.Fatal(fresh)
	}
	choices := []map[string]any{{"title": "A", "kind": "电影"}, {"title": "B", "kind": "电视剧", "tmdb_id": "DB:222", "search_sites": []string{"selected"}}}
	save := func(rev any) map[string]any {
		return call("choices", tg, u, map[string]any{"revision": rev, "choices": choices})
	}
	if save(old["revision"])["action"] != "superseded" || save(fresh["revision"])["action"] != "menu" || save(fresh["revision"])["action"] != "superseded" {
		t.Fatal("stale menu was published")
	}
	choice := start("2")
	if choice["action"] != "subscribe" || choice["selected"].(map[string]any)["tmdb_id"] != "DB:222" {
		t.Fatal(choice)
	}
	for _, who := range [][2]string{{"SLACK/24", u}, {tg, "2"}} {
		if r := call("start", who[0], who[1], map[string]any{"message": "1"}); r["action"] != "invalid" {
			t.Fatal(r)
		}
	}
	if r := call("start", "TG/99/other", u, map[string]any{"message": "1"}); r["action"] != "ignore" {
		t.Fatal(r)
	}

	if start("hello")["action"] != "ask" || start("1")["action"] != "invalid" {
		t.Fatal("old selection after chat")
	}
	single := start("下载 名称")
	if r := call("choices", tg, u, map[string]any{"revision": single["revision"], "choices": choices[:1]}); r["action"] != "search" || r["selected"] == nil {
		t.Fatal(r)
	}
	if start("https://example.invalid/test")["action"] != "download" || start("1")["action"] != "invalid" {
		t.Fatal("old selection after link")
	}
	last := start("订阅：节目")
	channel.ID = id
	channel.Config = `{"token":"24:fixture","user_ids":"2"}`
	if _, err := store.Save(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	if r := save(last["revision"]); r["action"] != "ignore" {
		t.Fatal("revoked during lookup", r)
	}

}

func TestTitleCriteriaKeepsOverlappingScopesAndFilmText(t *testing.T) {
	var result strings.Builder
	err := SearchTitlesCommand(t.Context(), "", "criteria", strings.NewReader(`{"title":"PT PT电影 站甲 PT 配置B 配置A","rss":["站甲","PT","PT"],"indexers":["PT"],"settings":[{"id":9007199254740993,"name":"配置A"},{"id":"7","name":"配置B"}]}`), &result)
	if err != nil || strings.TrimSpace(result.String()) != `{"indexers":["PT"],"rss":["站甲","PT"],"setting":9007199254740993,"title":"PT电影"}` {
		t.Fatal(result.String(), err)
	}
}
