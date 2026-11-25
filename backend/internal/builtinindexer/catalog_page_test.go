package builtinindexer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func TestCatalogPageSchedulerSearchBrowseAndRenderedRows(t *testing.T) {
	d := indexercatalog.Definition{
		ID: "fixture", Domain: "https://fixture.test/path/",
		Search:   json.RawMessage(`{"paths":[{"path":"search"}],"params":{"search":"{keyword}"},"batch":{"delimiter":"+","space_replace":"_"}}`),
		Browse:   json.RawMessage(`{"path":"recent/{page}","start":"1"}`),
		Torrents: json.RawMessage(`{"list":{"selector":"article"},"fields":{"title":{"selector":"h2"},"download":{"selector":"a","attribute":"href"},"labels":{"selector":"b"},"size":{"selector":"span"},"grabs":{"selector":"small"},"leechers":{"selector":"i"},"date_added":{"selector":"time"},"downloadvolumefactor":{"case":{"b":0.5,"*":1}}}}`),
	}
	const html = `<article><h2>A &amp; B</h2><a href="download?id=42"></a><span>9007199254740993</span><b>中字</b><b>HDR</b><small>17/30</small><time>2026-11-26</time></article><article><h2>B</h2></article>`
	q := CatalogPageRequest{Definition: d, Cookie: "fixture-cookie", UA: "fixture-ua", Limit: 1, Terms: []string{"A B", "剧 集"}, Page: 2}
	hits := 0
	transport := fakeTransport(func(r *http.Request) (*http.Response, error) {
		hits++
		if r.URL.Host != "fixture.test" || r.Header.Get("Cookie") != q.Cookie || r.UserAgent() != q.UA {
			t.Fatal("unexpected request or credentials")
		}
		query := r.URL.Query()
		switch hits {
		case 1:
			if r.URL.Path != "/path/search" || query.Get("search_mode") != "1" || query.Get("page") != "2" || query.Get("search") != "A_B+剧_集" {
				t.Fatal(r.URL)
			}
		case 2:
			if r.URL.Path != "/path/recent/3" || len(query) != 0 {
				t.Fatal(r.URL)
			}
		case 3:
			if r.URL.Path != "/path/search" || query.Get("page") != "2" || len(query) != 1 {
				t.Fatal(r.URL)
			}
		default:
			t.Fatal("unexpected fetch")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(html))}, nil
	})
	check := func() {
		t.Helper()
		rows, err := CatalogPage(context.Background(), q, transport)
		if err != nil || len(rows) != 1 {
			t.Fatal(rows, err)
		}
		row := rows[0].(map[string]any)
		if row["title"] != "A & B" || row["labels"] != "中字|HDR" || row["size"] != int64(9007199254740993) || row["grabs"] != "17" || row["peers"] != 0 || row["pubdate"] != "2026-11-26" || row["enclosure"] != "https://fixture.test/path/download?id=42" || row["downloadvolumefactor"] != 0.5 {
			t.Fatal(row)
		}
	}
	check()
	q.Terms = nil
	check()
	q.Definition.Browse = nil
	check()
	page := html
	q.Cookie = ""
	q.HTML = &page
	check()
	encoded, _ := json.Marshal(q)
	var output bytes.Buffer
	if err := CatalogPageCommand(t.Context(), bytes.NewReader(encoded), &output); err != nil || !strings.Contains(output.String(), "9007199254740993") {
		t.Fatal(output.String(), err)
	}
	page = `<input type=password>`
	if _, err := CatalogPage(t.Context(), q, transport); err == nil {
		t.Fatal("accepted login page")
	}
	page = "<html><p>No matching resource</p></html>"
	rows, err := CatalogPage(t.Context(), q, transport)
	if err != nil || len(rows) != 0 || hits != 3 {
		t.Fatal(rows, err, hits)
	}
}

func TestCatalogDisplayedSizes(t *testing.T) {
	for text, want := range map[string]int64{"1,234 KiB": 1234 << 10, "2.5 B": 2, "3.5 B": 4, "1 PiB": 1 << 50, "9,223,372,036,854,775,807": 1<<63 - 1} {
		if n, err := parseByteCount(text, true); err != nil || n != want {
			t.Fatal(text, n, err)
		}
	}
	for _, text := range []string{"1,23B", "-1", "9,223,372,036,854,775,808"} {
		if _, err := parseByteCount(text, true); err == nil {
			t.Fatal(text)
		}
	}
}
