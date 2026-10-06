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

func TestMTeamSearchAndSignedDownload(t *testing.T) {
	definition := indexercatalog.Definition{ID: "mteam-kpcc", Name: "MTeam", Domain: "https://kp.m-team.cc/", Parser: "MTeamSpider"}
	const torrent = "d4:infod4:name4:Testee"
	searchBody := `{"code":"0","data":{"data":[{"id":"42","name":"Movie.2026.1080p","smallDescr":"description","size":"1000","status":{"discount":"_2X_PERCENT_50","seeders":"3","leechers":null}}]}}`
	signedURL := "https://api.m-team.cc/api/rss/dl?token=private"
	transport := fakeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.m-team.cc" {
			t.Fatal(r.URL)
		}
		body := ""
		switch r.URL.Path {
		case "/api/torrent/search":
			if r.Method != "POST" || r.Header.Get("x-api-key") != "server-secret" {
				t.Fatal(r.Method, r.Header)
			}
			var input map[string]any
			if json.NewDecoder(r.Body).Decode(&input) != nil || input["keyword"] != "Movie & 中文" || input["pageNumber"] != float64(3) {
				t.Fatal(input)
			}
			body = searchBody
		case "/api/torrent/genDlToken":
			if r.Header.Get("x-api-key") != "server-secret" || r.ParseForm() != nil || r.PostForm.Get("id") != "42" {
				t.Fatal(r.Header, r.PostForm)
			}
			encoded, _ := json.Marshal(map[string]any{"code": "0", "data": signedURL})
			body = string(encoded)
		case "/api/rss/dl":
			if r.Header.Get("x-api-key") != "" || r.Header.Get("Cookie") != "" {
				t.Fatal("API credentials forwarded to download")
			}
			body = torrent
		default:
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	resources, err := SearchMTeam(context.Background(), definition, "server-secret", "Agent", "Movie & 中文", 2, transport)
	if err != nil || len(resources) != 1 {
		t.Fatal(resources, err)
	}
	r := resources[0]
	if r.DownloadResolver != "mteam" || r.DownloadURL != "https://kp.m-team.cc/detail/42" || r.DownloadFactor == nil || *r.DownloadFactor != 0.5 || r.UploadFactor == nil || *r.UploadFactor != 2 || r.Peers != nil {
		t.Fatal(r)
	}
	contents, err := DownloadMTeam(context.Background(), r.DownloadURL, "server-secret", "Agent", transport)
	if err != nil || string(contents) != torrent {
		t.Fatal(string(contents), err)
	}
	searchBody = strings.ReplaceAll(searchBody, "_2X_PERCENT_50", "UNKNOWN")
	resources, err = SearchMTeam(context.Background(), definition, "server-secret", "Agent", "Movie & 中文", 2, transport)
	if err != nil || resources[0].DownloadFactor != nil || resources[0].Freeleech != nil {
		t.Fatal(resources, err)
	}
	signedURL = "https://other.example/steal?token=private"
	if _, err := DownloadMTeam(context.Background(), r.DownloadURL, "server-secret", "Agent", transport); !errors.Is(err, ErrResponse) {
		t.Fatal(err)
	}
	searchBody = `{"code":"0","data":{"data":[]}}`
	resources, err = SearchMTeam(context.Background(), definition, "server-secret", "Agent", "Movie & 中文", 2, transport)
	if err != nil || len(resources) != 0 {
		t.Fatal(resources, err)
	}
	for _, bad := range []string{`{"code":"1","data":{"data":[]}}`, `{"code":"0","data":{"data":null}}`, `{"code":"0","data":{"data":[{"id":"bad"}]}}`} {
		searchBody = bad
		if _, err := SearchMTeam(context.Background(), definition, "server-secret", "Agent", "Movie & 中文", 2, transport); !errors.Is(err, ErrResponse) {
			t.Fatal(bad, err)
		}
	}
}

func TestMTeamRejectsInvalidOriginsAndAuthentication(t *testing.T) {
	for _, domain := range []string{"http://kp.m-team.cc/", "https://evilm-team.cc/", "https://m-team.cc.evil/", "https://user:secret@kp.m-team.cc/", "https://kp.m-team.cc:123/", "https://kp.m-team.cc/prefix", "https://kp.m-team.cc/?key=secret"} {
		if _, _, err := mteamBase(domain); !errors.Is(err, ErrConfig) {
			t.Fatal(domain, err)
		}
	}
	definition := indexercatalog.Definition{ID: "mteam-kpcc", Domain: "https://kp.m-team.cc/", Parser: "MTeamSpider"}
	if _, err := SearchMTeam(context.Background(), definition, "", "", "keyword", 0, nil); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
}
