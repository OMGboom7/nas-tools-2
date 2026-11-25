package builtinindexer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func TestTorrentLeechClientAndNativeResults(t *testing.T) {
	d := indexercatalog.Definition{ID: "installed-tl", Name: "Configured", Domain: "https://tracker.test/prefix/", Parser: "TorrentLeech"}
	const data = `{"torrentList":[{"fid":"12","name":"Fixture","filename":"A ?# 中文.torrent","size":"9007199254740993","seeders":5,"leechers":"2","completed":"6","addedTimestamp":"2026-11-26 08:00:00","download_multiplier":"0","imdbID":1234567}]}`
	for _, tc := range []struct {
		word, path string
		page       int
	}{
		{"", "page/1", 0}, {"", "page/3", 2}, {"A/B & 片", "query/A%2FB%20&%20%E7%89%87", 0}, {"Film", "query/Film/page/2", 1},
	} {
		transport := fakeTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "tracker.test" || r.URL.EscapedPath() != "/prefix/torrents/browse/list/"+tc.path || r.URL.RawQuery != "" || r.Header.Get("Cookie") != "fixture-session" || r.UserAgent() != "fixture-agent" {
				t.Fatal(r.URL)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
		})
		rows, err := ResourcePage(t.Context(), d.Parser, ResourcePageRequest{ID: d.ID, Domain: d.Domain, Name: d.Name, Cookie: "fixture-session", UA: "fixture-agent", Keyword: &tc.word, Page: tc.page}, transport)
		if err != nil || len(rows) != 1 {
			t.Fatal(rows, err)
		}
		b, err := json.Marshal(rows[0])
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if json.Unmarshal(b, &row) != nil || len(row) != 12 || row["size"] != "9007199254740993" || row["downloadvolumefactor"] != "0" || row["seeders"] != 5.0 || row["peers"] != "2" || row["pubdate"] != "2026-11-26 08:00:00" || row["imdbid"] != 1234567.0 || row["grabs"] != "6" || row["enclosure"] != "https://tracker.test/prefix/download/12/A%20%3F%23%20%E4%B8%AD%E6%96%87.torrent" {
			t.Fatal(string(b))
		}
		items, err := SearchTorrentLeech(t.Context(), d, "fixture-session", "fixture-agent", tc.word, tc.page, transport)
		if err != nil || len(items) != 1 {
			t.Fatal(items, err)
		}
		item := items[0]
		if item.IMDbID != "tt1234567" || item.Size != 9007199254740993 || item.Freeleech == nil || !*item.Freeleech || item.PageURL != "https://tracker.test/prefix/torrent/12" || item.DownloadURL != row["enclosure"] {
			t.Fatal(item)
		}
	}
}

func TestTorrentLeechEmptyErrorsAndPageIdentity(t *testing.T) {
	d := indexercatalog.Definition{ID: "fixture", Domain: "https://tracker.test", Parser: "TorrentLeech"}
	hits := 0
	body := `{"page":"2","torrentList":[]}`
	status := 200
	transport := fakeTransport(func(r *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://other.test/"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	fetch := func() error {
		_, err := SearchTorrentLeech(context.Background(), d, "fixture-cookie", "", "", 1, transport)
		return err
	}
	if err := fetch(); err != nil {
		t.Fatal(err)
	}
	for _, body = range []string{`{"page":1,"torrentList":[]}`, `{}`, `{"torrentList":null}`, `<html>Login</html>`, `{"torrentList":[{"fid":1,"name":"Fixture","filename":"../other"}]}`, `{"torrentList":[{"fid":1,"name":"Fixture","filename":"ok","size":"bad"}]}`} {
		if fetch() == nil {
			t.Fatal(body)
		}
	}
	status = 302
	count := hits
	if fetch() == nil || hits != count+1 {
		t.Fatal("unexpected redirect")
	}
	if _, err := SearchTorrentLeech(t.Context(), d, "", "", "", 1, transport); err == nil || hits != count+1 {
		t.Fatal("missing session")
	}
}
