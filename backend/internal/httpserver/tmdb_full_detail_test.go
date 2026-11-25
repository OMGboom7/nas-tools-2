package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTMDBFullDetailCommand(t *testing.T) {
	var path, appendFields, language string
	type expectation struct{ path, language, appendFields, body string }
	pending := make(chan expectation, 1)
	var calls int
	var responseBody string
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := <-pending
		q := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != want.path || q.Get("api_key") != "fixture" || q.Get("language") != want.language || q.Get("include_adult") != "true" || q.Get("append_to_response") != want.appendFields {
			t.Errorf("unexpected TMDB detail request: path=%s language=%s append=%s", r.URL.Path, q.Get("language"), q.Get("append_to_response"))
		}
		io.WriteString(w, want.body)
	}))
	defer provider.Close()
	saved := http.DefaultTransport
	http.DefaultTransport = provider.Client().Transport
	defer func() { http.DefaultTransport = saved }()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig := func(lang string) {
		t.Helper()
		raw := fmt.Sprintf("app: {rmt_tmdbkey: fixture, tmdb_domain: %q}\nmedia: {tmdb_language: %q, tmdb_include_adult: true}\n", provider.URL, lang)
		if err := os.WriteFile(cfg, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(input string) (map[string]any, error) {
		pending <- expectation{path, language, appendFields, responseBody}
		var out bytes.Buffer
		err := TMDBDetailCommand(context.Background(), cfg, strings.NewReader(input), &out)
		select {
		case <-pending: // rejected before reaching the provider
		default:
			calls++
		}
		if err != nil {
			if out.Len() != 0 {
				t.Fatal("partial response on error")
			}
			return nil, err
		}
		decoder := json.NewDecoder(&out)
		decoder.UseNumber()
		var got map[string]any
		if err := decoder.Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got, nil
	}
	writeConfig("zh")
	path, language, appendFields = "/3/movie/17", "en", "images,credits,alternative_titles,translations,external_ids,releases"
	responseBody = `{"id":17,"title":"Original","genres":[{"id":18},{"id":16}],"credits":{"cast":[{"id":9007199254740993}]},"alternative_titles":{"titles":[{"iso_3166_1":"TW","title":"最早"},{"iso_3166_1":"CN","title":"繁體劇"},{"iso_3166_1":"CN","title":"简体片名"}]}}`
	got, err := run(`{"kind":"movie","id":"017","append":"all","language":"en","localize":true}`)
	if err != nil || got["title"] != "简体片名" || got["media_type"] != "movie" || fmt.Sprint(got["genre_ids"]) != "[18 16]" ||
		text(objectValue(slice(objectValue(got["credits"])["cast"])[0])["id"]) != "9007199254740993" {
		t.Fatal(got, err)
	}
	writeConfig("ja")
	language, appendFields = "ja", "alternative_titles,translations,external_ids,releases"
	if got, err = run(`{"kind":"movie","id":"17","localize":true}`); err != nil || got["title"] != "Original" {
		t.Fatal(got, err)
	}
	writeConfig("zh")
	path, language, appendFields = "/3/tv/17", "zh", "images"
	responseBody = `{"id":17,"name":"Series","alternative_titles":{"results":[{"iso_3166_1":"CN","title":"剧名"}]},"seasons":[{"season_number":2,"episode_count":9}]}`
	if got, err = run(`{"kind":"tv","id":"17","localize":true,"append":"images"}`); err != nil || got["name"] != "剧名" || len(slice(got["seasons"])) != 1 {
		t.Fatal(got, err)
	}
	appendFields = ""
	if got, err = run(`{"kind":"tv","id":"17","append":""}`); err != nil || got["name"] != "Series" {
		t.Fatal(got, err)
	}
	path = "/3/tv/17/season/0"
	responseBody = `{"id":209,"season_number":0,"episodes":[{"episode_number":1,"guest_stars":[{"name":"Actor"}]}]}`
	if got, err = run(`{"kind":"tv","id":"17","season":0}`); err != nil || text(got["id"]) != "209" || got["genre_ids"] != nil || len(slice(got["episodes"])) != 1 {
		t.Fatal(got, err)
	}
	for _, body := range []string{`{"id":17,"season_number":1}`, `{"id":17,"season_number":0,"episodes":[{"show_id":999}]}`} {
		responseBody = body
		if _, err = run(`{"kind":"tv","id":"17","season":0}`); err == nil {
			t.Fatal("accepted foreign season data")
		}
	}
	path = "/3/tv/17"
	for _, body := range []string{`{"id":18}`, `{"id":17}{}`, `{"id":17,"success":false}`, `{"id":"17"}`, `{"id":17,"title":"movie"}`, `{"id":17,"name":"show","media_type":"movie"}`} {
		responseBody = body
		if _, err = run(`{"kind":"tv","id":"17","append":""}`); err == nil {
			t.Fatal(body)
		}
	}
	before := calls
	for _, input := range []string{`{"kind":"movie","id":"17","season":1}`, `{"kind":"tv","id":"../movie/2"}`, `{"kind":"tv","id":"17","extra":true}`, `{"kind":"tv","id":"17"}{}`} {
		if _, err = run(input); err == nil {
			t.Fatal(input)
		}
	}
	if calls != before {
		t.Fatal("invalid selector contacted provider")
	}
}

func TestTMDBDetailRateLimit(t *testing.T) {
	settings := map[string]any{"app": map[string]any{"rmt_tmdbkey": "fixture"}}
	for _, tc := range []struct {
		wait          string
		status, calls int
		success       bool
	}{
		{"0", 200, 2, true}, {"0", 429, 2, false}, {"3600", 200, 1, false}, {"", 200, 1, false},
	} {
		t.Run(fmt.Sprintf("%s-%d", tc.wait, tc.status), func(t *testing.T) {
			calls := 0
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				res := jsonResponse(r, `{"id":17,"name":"fixture"}`)
				if res.Header == nil {
					res.Header = make(http.Header)
				}
				res.StatusCode = tc.status
				if calls == 1 {
					res.StatusCode = 429
					res.Header.Set("Retry-After", tc.wait)
				}
				return res, nil
			})
			_, err := fullTMDBDetail(context.Background(), settings, rt, tmdbDetailQuery{Kind: "tv", ID: "17"})
			if (err == nil) != tc.success || calls != tc.calls {
				t.Fatal(calls, err)
			}
		})
	}
}
