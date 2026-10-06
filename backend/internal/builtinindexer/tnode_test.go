package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func TestTNodeNativeCSRFSearch(t *testing.T) {
	definition := indexercatalog.Definition{ID: "zhuque", Name: "朱雀", Domain: "https://zhuque.in/", Parser: "TNodeSpider"}
	root := `<meta content="token&amp;private" name="x-csrf-token">`
	result := `{"statusCode":200,"data":{"torrents":[{"id":42,"title":"Movie.2026.1080p","subtitle":"中文","size":"1000","seeding":null,"leeching":"0","downloadRate":"0.5","uploadRate":2,"imdb":"tt1234567"}]}}`
	posts := 0
	transport := fakeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "zhuque.in" || r.Header.Get("Cookie") != "session=private" || r.Header.Get("User-Agent") != "Agent" {
			t.Fatal(r.URL, r.Header)
		}
		body := root
		if r.Method == "POST" {
			posts++
			if r.URL.Path != "/api/torrent/advancedSearch" || r.Header.Get("X-CSRF-TOKEN") != "token&private" {
				t.Fatal(r.URL, r.Header)
			}
			var params map[string]any
			if json.NewDecoder(r.Body).Decode(&params) != nil || params["keyword"] != "电影 & Query" || params["page"] != float64(3) || params["size"] != float64(100) {
				t.Fatal(params)
			}
			body = result
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	resources, err := SearchTNode(context.Background(), definition, "session=private", "Agent", "电影 & Query", 2, 100, transport)
	if err != nil || len(resources) != 1 {
		t.Fatal(resources, err)
	}
	r := resources[0]
	if r.Seeders != nil || r.Peers == nil || *r.Peers != 0 || r.DownloadFactor == nil || *r.DownloadFactor != 0.5 || r.UploadFactor == nil || *r.UploadFactor != 2 || r.DownloadURL != "https://zhuque.in/api/torrent/download/42" {
		t.Fatal(r)
	}
	for _, badRoot := range []string{`<input type="PaSsWoRd"><meta name="x-csrf-token" content="token">`, `<div id="challenge-form"></div>`, `<meta name="x-csrf-token" content="one"><meta name="x-csrf-token" content="two">`, `<meta name="x-csrf-token" content="&#10;injected">`, `<h1>login</h1>`} {
		root = badRoot
		before := posts
		if _, err := SearchTNode(context.Background(), definition, "session=private", "Agent", "电影 & Query", 2, 100, transport); !errors.Is(err, ErrResponse) || posts != before {
			t.Fatal(badRoot, posts, err)
		}
	}
	root = `<meta name="x-csrf-token" content="token&amp;private">`
	for _, badResult := range []string{`{"data":{"torrents":null}}`, `{"data":{"torrents":[{"id":-1}]}}`, `{"success":false,"data":{"torrents":[]}}`, `{"code":1,"data":{"torrents":[]}}`} {
		result = badResult
		if _, err := SearchTNode(context.Background(), definition, "session=private", "Agent", "电影 & Query", 2, 100, transport); !errors.Is(err, ErrResponse) {
			t.Fatal(badResult, err)
		}
	}
	result = `{"data":{"torrents":[]}}`
	resources, err = SearchTNode(context.Background(), definition, "session=private", "Agent", "电影 & Query", 2, 100, transport)
	if err != nil || len(resources) != 0 {
		t.Fatal(resources, err)
	}
}

func TestTNodeRejectsRedirectsAndInvalidConfiguration(t *testing.T) {
	definition := indexercatalog.Definition{ID: "zhuque", Domain: "https://zhuque.in/", Parser: "TNodeSpider"}
	transport := fakeTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": []string{"https://other.local/collect"}}}, nil
	})
	if _, err := SearchTNode(context.Background(), definition, "session=private", "Agent", "Movie", 0, 100, transport); !errors.Is(err, ErrResponse) {
		t.Fatal(err)
	}
	for _, domain := range []string{"http://zhuque.in/", "https://user:secret@zhuque.in/", "https://zhuque.in:8080/", "https://zhuque.in/path", "https://zhuque.in/?secret=x"} {
		definition.Domain = domain
		if _, err := SearchTNode(context.Background(), definition, "cookie", "Agent", "Movie", 0, 100, transport); !errors.Is(err, ErrConfig) {
			t.Fatal(domain, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SearchTNode(ctx, definition, "cookie", "Agent", "Movie", 0, 100, transport); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
