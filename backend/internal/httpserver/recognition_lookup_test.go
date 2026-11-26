package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"golang.org/x/net/html"
)

func TestRecognitionWebsiteUniqueIdentityAndScope(t *testing.T) {
	for _, tc := range []struct {
		body     string
		tv       bool
		kind, id string
	}{
		{`<a data-id="9" href="/movie/9-a"></a><a data-id="9" href="/movie/9-b"></a>`, false, "movie", "9"},
		{`<a data-id href="/movie/9"></a><a data-id data-media-type="tv" href="/tv/9"></a>`, false, "", ""},
		{`<a data-id href="/movie/9"></a><a data-id data-media-type="tv" href="/tv/9"></a>`, true, "tv", "9"},
		{`<a data-id href="//outside.test/movie/9"></a><a href="/tv/1"></a>`, false, "", ""},
		{`<a data-id="12" href="/movie/9"></a>`, false, "", ""},
		{`<a data-id href="/movie/1"></a><a data-id href="/movie/2/images"></a>`, false, "", ""},
	} {
		doc, err := html.Parse(strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		k, id := singleWebIdentity(doc, tc.tv)
		if k != tc.kind || id != tc.id {
			t.Fatal(k, id, tc)
		}
	}
}

func TestRecognitionAIThenKeywordAndFileScope(t *testing.T) {
	settings := map[string]any{"app": map[string]any{"rmt_tmdbkey": "fixture", "tmdb_domain": "details.test", "user_agent": "fixture agent"}, "laboratory": map[string]any{"search_tmdbweb": true, "chatgpt_enable": true, "search_keyword": true}, "openai": map[string]any{"api_url": "https://ai.test", "api_key": "ai-secret"}}
	ctx := context.Background()
	q := recognitionLookup{Title: "ExampleHD", Raw: "Folder/Example.mkv", Kind: "movie", Mode: "file", Language: "en"}
	var hosts []string
	mode := "AI"
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		switch r.URL.Host {
		case "ai.test":
			if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer ai-secret" {
				t.Fatal("AI destination/header")
			}
			var req struct {
				Messages    []map[string]string
				Model, User string
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.User != "NAStool" || req.Model != "gpt-3.5-turbo" || len(req.Messages) != 2 || req.Messages[1]["content"] != q.Raw {
				t.Fatal(req, err)
			}
			if mode == "partial" {
				return nil, errors.New("fixture failure")
			}
			if mode == "AI" {
				return jsonResponse(r, `{"choices":[{"message":{"content":"{\"title\":\"Real.Show / alias\",\"year\":2024,\"season\":0,\"episode\":[2,3]}"}}]}`), nil
			}
			return jsonResponse(r, `{"choices":[{"message":{"content":"{}"}}]}`), nil
		case "www.cn.bing.com", "www.baidu.com":
			if r.Header.Get("Authorization") != "" || r.URL.Query().Has("api_key") || r.Header.Get("User-Agent") != "fixture agent" || strings.Contains(r.URL.String(), "Folder") {
				t.Fatal("public request exceeded scope")
			}
			if mode == "partial" {
				return nil, errors.New("fixture failure")
			}
			if r.URL.Host == "www.cn.bing.com" {
				if r.URL.Query().Get("q") != "Example " {
					t.Fatal(r.URL.RawQuery)
				}
				return jsonResponse(r, `<aside><h2 class=" b_entityTitle">Example Movie (2024)</h2><div data-feedbk-ids="Movie"></div></aside><h2><strong>Example</strong></h2>`), nil
			}
			return jsonResponse(r, "<em>Example</em>"), nil
		case "details.test":
			if r.URL.Query().Get("api_key") != "fixture" || r.Header.Get("Authorization") != "" {
				t.Fatal("API authorization")
			}
			if strings.HasPrefix(r.URL.Path, "/3/search/") {
				switch r.URL.Query().Get("query") {
				case "Real Show":
					if r.URL.Path != "/3/search/tv" || r.URL.Query().Get("first_air_date_year") != "2024" {
						t.Fatal(r.URL)
					}
					return jsonResponse(r, `{"results":[{"id":8,"name":"Real Show","first_air_date":"2024-01-01"}]}`), nil
				case "Example Movie":
					if r.URL.Path != "/3/search/movie" {
						t.Fatal(r.URL)
					}
					return jsonResponse(r, `{"results":[{"id":9,"title":"Example Movie"}]}`), nil
				default:
					return jsonResponse(r, `{"results":[]}`), nil
				}
			}
			if r.URL.Query().Get("language") != "en" {
				t.Fatal(r.URL)
			}
			if r.URL.Path == "/3/tv/8" {
				return jsonResponse(r, `{"id":8,"name":"Real Show","genres":[]}`), nil
			}
			if r.URL.Path == "/3/movie/9" {
				return jsonResponse(r, `{"id":9,"title":"Example Movie","genres":[]}`), nil
			}
		}
		t.Fatalf("unexpected request %s", r.URL)
		return nil, nil
	})
	result, err := lookupParsedMedia(ctx, settings, tr, q)
	if err != nil || !result.TV || len(result.Season) != 1 || result.Season[0] != 0 || len(result.Episode) != 2 || text(result.Row["id"]) != "8" || strings.Contains(strings.Join(hosts, " "), ".bing.") {
		t.Fatal(result, hosts, err)
	}
	hosts = nil
	mode = "keyword"
	result, err = lookupParsedMedia(ctx, settings, tr, q)
	if err != nil || text(result.Row["id"]) != "9" || !strings.Contains(strings.Join(hosts, " "), "ai.test www.cn.bing.com www.baidu.com details.test") {
		t.Fatal(result, hosts, err)
	}
	mode = "partial"
	result, err = lookupParsedMedia(ctx, settings, tr, q)
	if err == nil || result.Row != nil {
		t.Fatal("partial failure cached as a miss", result)
	}
}

func TestNativeTitleUsesWebsiteAfterMissAndChecksDetail(t *testing.T) {
	var order []string
	h, token, path := nativeServicesFixture(t, "laboratory:\n  search_tmdbweb: true\n", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		order = append(order, r.URL.Host+r.URL.Path)
		switch r.URL.Host + r.URL.Path {
		case "tmdb.test/3/search/movie", "tmdb.test/3/search/tv":
			return jsonResponse(r, `{"results":[]}`), nil
		case "www.themoviedb.org/search":
			if r.URL.Query().Get("query") != "Example" || r.URL.Query().Has("api_key") || r.Header.Get("Authorization") != "" {
				t.Fatal(r.URL)
			}
			return jsonResponse(r, `<a data-id="8" data-media-type="tv" href="/tv/8-example">Example</a>`), nil
		case "tmdb.test/3/tv/8":
			return jsonResponse(r, `{"id":8,"name":"Catalog Title","first_air_date":"2024-01-01","genres":[]}`), nil
		}
		t.Fatalf("unexpected request %s", r.URL)
		return nil, nil
	}))
	if err := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml")).Update(map[string]any{"app.rmt_tmdbkey": "fixture", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	w := performFormRequest(h, "/api/v1/service/name/test", token, url.Values{"name": {"Example.S00E02"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Catalog Title") || !strings.Contains(w.Body.String(), "S00E02") || strings.Join(order, " ") != "tmdb.test/3/search/tv www.themoviedb.org/search tmdb.test/3/tv/8" {
		t.Fatal(w.Code, w.Body.String(), order)
	}
}

func TestRecognitionCommandDisabledProvidersAndPrimaryError(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("app: {rmt_tmdbkey: fake, tmdb_domain: fixture.test}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	saved := http.DefaultTransport
	defer func() { http.DefaultTransport = saved }()
	bad := false
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "fixture.test" {
			t.Fatal(r.URL)
		}
		if bad {
			return nil, errors.New("fixture offline")
		}
		return jsonResponse(r, `{"results":[]}`), nil
	})
	run := func() (string, error) {
		var out strings.Builder
		err := SearchTitlesCommand(context.Background(), configPath, "recognize", strings.NewReader(`{"title":"A","raw":"A","mode":"title","kind":"movie","strict":true}`), &out)
		return out.String(), err
	}
	if data, err := run(); err != nil || !strings.Contains(data, `"row":null`) {
		t.Fatal(data, err)
	}
	bad = true
	if data, err := run(); err == nil || data != "" {
		t.Fatal(data, err)
	}
}

func TestScopedRecognitionSkipsWrongWebsiteTypeAndContinues(t *testing.T) {
	settings := map[string]any{"app": map[string]any{"rmt_tmdbkey": "fixture", "tmdb_domain": "api.test"}, "laboratory": map[string]any{"search_tmdbweb": true, "search_keyword": true}}
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "www.themoviedb.org":
			return jsonResponse(r, `<a data-id="8" href="/tv/8-show">show</a>`), nil
		case "www.cn.bing.com":
			return jsonResponse(r, "<h2><strong>Example</strong></h2>"), nil
		case "www.baidu.com":
			return jsonResponse(r, ""), nil
		case "api.test":
			if r.URL.Path != "/3/search/movie" {
				t.Fatal("foreign scope requested", r.URL)
			}
			return jsonResponse(r, `{"results":[{"id":7,"title":"Example"}]}`), nil
		}
		t.Fatal(r.URL)
		return nil, nil
	})
	found, err := resourceTitleAssistance(context.Background(), settings, tr, "Example", "Example.S01", "movie", "movie")
	if err != nil || text(found.Row["id"]) != "7" {
		t.Fatal(found, err)
	}
}
