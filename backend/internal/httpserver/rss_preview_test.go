package httpserver

import (
	"database/sql"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestNativeRSSPreviewXMLAndJSONWithoutPython(t *testing.T) {
	requests := []string{}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.String())
		var body string
		switch request.URL.Host {
		case "xml.example":
			body = `<rss xmlns:ns="https://feed.example/ns"><channel><item><title>Movie One</title><enclosure type="application/x-bittorrent" url="https://file.example/a"/><pubDate>Tue, 22 Sep 2026 10:00:00 +0800</pubDate><ns:link>https://feed.example/a</ns:link></item></channel></rss>`
		case "json.example":
			if request.URL.Query().Get("api_key") != "tmdb-secret" {
				t.Fatalf("missing parser params: %s", request.URL)
			}
			body = `{"items":[{"title":"Movie Two","release_date":"2025-02-01","size":"2048"}]}`
		default:
			t.Fatalf("RSS preview called Python or redirected: %s", request.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: request}, nil
	})
	handler, token, path := nativeServicesFixture(t, "", nil, transport)
	if err := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml")).Update(map[string]any{"app.rmt_tmdbkey": "tmdb-secret"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	xmlFormat := `{"list":"//channel/item","item":{"title":{"path":".//title/text()"},"enclosure":{"path":".//enclosure[@type='application/x-bittorrent']/@url"},"link":{"path":"link/text()","namespaces":"https://feed.example/ns"},"date":{"path":".//pubDate/text()"}}}`
	jsonFormat := `{"list":"$.items","item":{"title":{"path":"title"},"year":{"path":"release_date"},"size":{"path":"size"}}}`
	if _, err := db.Exec(`INSERT INTO CONFIG_RSS_PARSER (ID,NAME,TYPE,FORMAT,PARAMS) VALUES (1,'XML','XML',?,''),(2,'JSON','JSON',?,'api_key={TMDBKEY}')`, xmlFormat, jsonFormat); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO CONFIG_USER_RSS (ID,NAME,ADDRESS,PARSER,USES) VALUES (5,'Test','["https://xml.example/rss","https://json.example/feed"]','[1,2]','D')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO RSS_TORRENTS (TORRENT_NAME,ENCLOSURE) VALUES ('Movie One','https://file.example/a')`); err != nil {
		t.Fatal(err)
	}
	if got := performFormRequest(handler, "/api/v1/rss/preview", "", url.Values{"id": {"5"}}); got.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth=%d", got.Code)
	}
	if got := performFormRequest(handler, "/api/v1/rss/preview", "invalid", url.Values{"id": {"5"}}); got.Code != http.StatusUnauthorized {
		t.Fatalf("invalid auth=%d", got.Code)
	}
	got := performFormRequest(handler, "/api/v1/rss/preview", token, url.Values{"id": {"5"}})
	body := got.Body.String()
	if got.Code != 200 || !strings.Contains(body, `"count":2`) || !strings.Contains(body, `"address_count":2`) || !strings.Contains(body, `"title":"Movie One"`) || !strings.Contains(body, `"finish_flag":true`) || !strings.Contains(body, `"year":"2025"`) || !strings.Contains(body, `"size":"2K"`) || !strings.Contains(body, `"link":"https://feed.example/a"`) || !strings.Contains(body, `"address_index":2`) {
		t.Fatalf("preview=%d %s", got.Code, body)
	}
	if len(requests) != 2 {
		t.Fatalf("requests=%v", requests)
	}
	bad := performFormRequest(handler, "/api/v1/rss/preview", token, url.Values{"id": {"0"}})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid id=%d", bad.Code)
	}
}

func TestRSSPreviewRejectsRedirectAndOversize(t *testing.T) {
	for _, input := range []struct {
		name   string
		status int
		body   string
	}{
		{"redirect", 302, ""},
		{"oversize", 200, strings.Repeat("x", (4<<20)+1)},
	} {
		t.Run(input.name, func(t *testing.T) {
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != "feed.example" {
					t.Fatalf("unexpected fetch %s", request.URL)
				}
				header := http.Header{}
				if input.status == 302 {
					header.Set("Location", "https://elsewhere.example/")
				}
				return &http.Response{StatusCode: input.status, Header: header, Body: io.NopCloser(strings.NewReader(input.body)), Request: request}, nil
			})
			handler, token, path := nativeServicesFixture(t, "", nil, transport)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`INSERT INTO CONFIG_RSS_PARSER (ID,TYPE,FORMAT) VALUES (1,'JSON','{"list":"$.items","item":{"title":{"path":"title"}}}')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO CONFIG_USER_RSS (ID,ADDRESS,PARSER,USES) VALUES (1,'["https://feed.example/"]','[1]','D')`); err != nil {
				t.Fatal(err)
			}
			got := performFormRequest(handler, "/api/v1/rss/preview", token, url.Values{"id": {"1"}})
			if got.Code != 200 || !strings.Contains(got.Body.String(), `"code":1`) {
				t.Fatalf("%s=%d %s", input.name, got.Code, got.Body.String())
			}
		})
	}
}

func TestRSSPreviewWithoutNativeConfigurationNeverUsesPython(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("RSS preview called Python: %s", request.URL)
		return nil, http.ErrNotSupported
	})
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	got := performFormRequest(handler, "/api/v1/rss/preview", "test-token", url.Values{"id": {"1"}})
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("without config=%d %s", got.Code, got.Body.String())
	}
}

func TestRSSPreviewMalformedParserPathsDoNotPanic(t *testing.T) {
	for _, path := range []string{"$", "$.items[", "$.items[?(@.bad >)]", "$.items..", "title[", "$.items[*]"} {
		t.Run(path, func(t *testing.T) {
			_, _ = parseRSSPreview([]byte(`{"items":[{"title":"One"}]}`), "JSON", rssFormat{List: path, Item: map[string]rssFieldSpec{"title": {Path: "title"}}}, 1)
		})
	}
}
