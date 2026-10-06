package externalindexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const torznabFixture = `<rss xmlns:other="http://torznab.com/schemas/2015/feed"><channel><item><title>电影 &amp; S01E02</title><description>描述</description><comments>https://tracker.local/details?id=1</comments><enclosure url="https://indexer.local/download?apikey=download-secret" length="123456"/><other:attr name="seeders" value="7"/><other:attr name="peers" value="3"/><other:attr name="downloadvolumefactor" value="0"/><other:attr name="uploadvolumefactor" value="2"/><other:attr name="imdbid" value="tt12345"/></item><item><title>Missing URL</title></item></channel></rss>`

func TestSearchQueriesAndParsesWithoutPython(t *testing.T) {
	for _, kind := range []string{"Jackett", "Prowlarr"} {
		t.Run(kind, func(t *testing.T) {
			keyword := "电影 & q=evil + #中文"
			indexer := Indexer{ID: "selected-id", Name: "Selected", RemoteID: "tracker", Kind: kind, Domain: "https://untrusted.invalid/steal"}
			if kind == "Prowlarr" {
				indexer.RemoteID = "42"
			}
			calls := 0
			transport := handlerTransport(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Hostname() != "indexer.local" || r.URL.Query().Get("evil") != "" {
					t.Fatalf("bad request %s", r.URL)
				}
				if kind == "Jackett" {
					if r.URL.Path != "/prefix/api/v2.0/indexers/tracker/results/torznab/" || r.URL.Query().Get("q") != keyword || r.URL.Query().Get("apikey") != "api-secret" || r.URL.Query().Get("t") != "search" {
						t.Errorf("bad Jackett parameters")
					}
					fmt.Fprint(w, torznabFixture)
				} else {
					if r.URL.Path != "/prefix/api/v1/search" || r.URL.Query().Get("query") != keyword || r.URL.Query().Get("indexerIds") != "42" || r.Header.Get("X-Api-Key") != "api-secret" || strings.Contains(r.URL.String(), "api-secret") {
						t.Errorf("bad Prowlarr parameters")
					}
					fmt.Fprint(w, `[{"indexerId":42,"indexer":"Selected","title":"Movie","downloadUrl":"https://indexer.local/download?token=hidden","sortTitle":"Sort","size":987,"seeders":5,"guid":"https://tracker.local/details"}]`)
				}
			})
			items, err := Search(t.Context(), Config{Kind: kind, Host: "http://indexer.local/prefix", APIKey: "api-secret"}, indexer, keyword, transport)
			if err != nil || len(items) != 1 || calls != 1 {
				t.Fatalf("items=%+v err=%v calls=%d", items, err, calls)
			}
			item := items[0]
			encoded, err := json.Marshal(item)
			if err != nil || strings.Contains(string(encoded), "download-secret") || strings.Contains(string(encoded), "token=hidden") {
				t.Fatal("download credentials serialized")
			}
			if item.IndexerID != "selected-id" || item.Indexer != "Selected" {
				t.Fatal(item)
			}
			if kind == "Jackett" && (item.Title != "电影 & S01E02" || item.Size != 123456 || item.Seeders == nil || *item.Seeders != 7 || item.Peers == nil || *item.Peers != 3 || item.Freeleech == nil || !*item.Freeleech || *item.DownloadFactor != 0 || *item.UploadFactor != 2 || item.IMDbID != "tt12345") {
				t.Fatal(item)
			}
			if kind == "Prowlarr" && (item.Size != 987 || item.Seeders == nil || *item.Seeders != 5 || item.Freeleech != nil || item.DownloadFactor != nil || item.UploadFactor != nil || item.Peers != nil) {
				t.Fatal("unknown promotions fabricated", item)
			}
		})
	}
}

func TestTorznabMalformedAndEmptyResponses(t *testing.T) {
	indexer := Indexer{ID: "tracker-jackett", Kind: "Jackett", RemoteID: "tracker"}
	for _, body := range []string{`<html>Login</html>`, `<error code="100" description="invalid key"/>`, `<rss/>`, `<rss><channel/></rss><rss><channel/></rss>`, `<!DOCTYPE rss [<!ENTITY x "bad">]><rss><channel/></rss>`, strings.Repeat("<rss>", 65) + strings.Repeat("</rss>", 65), strings.Replace(torznabFixture, `value="7"`, `value="-1"`, 1), strings.Replace(torznabFixture, `value="0"`, `value="NaN"`, 1), strings.Replace(torznabFixture, `value="2"`, `value="+Inf"`, 1), strings.Replace(torznabFixture, `value="3"`, `value="3.5"`, 1), strings.Replace(torznabFixture, `length="123456"`, `length="9223372036854775808"`, 1), strings.Replace(torznabFixture, `</item>`, `<other:attr name="seeders" value="1"/></item>`, 1)} {
		if _, err := parseTorznab(t.Context(), []byte(body), indexer); !errors.Is(err, ErrResponse) {
			t.Fatalf("unexpected error=%v", err)
		}
	}
	items, err := parseTorznab(t.Context(), []byte(`<rss><channel/></rss>`), indexer)
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("empty items=%v err=%v", items, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := parseTorznab(ctx, []byte(torznabFixture), indexer); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSearchErrorsAreRedactedAndRedirectsNotFollowed(t *testing.T) {
	for _, kind := range []string{"Jackett", "Prowlarr"} {
		for _, status := range []int{302, 401, 500} {
			calls := 0
			transport := handlerTransport(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "https://collector.invalid/secret")
				w.WriteHeader(status)
			})
			_, err := Search(t.Context(), Config{Kind: kind, Host: "http://indexer.local", APIKey: "api-secret"}, Indexer{Kind: kind, RemoteID: "42"}, "keyword", transport)
			if !errors.Is(err, ErrResponse) || strings.Contains(err.Error(), "secret") || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		}
	}
	for _, remote := range []string{"../escape", "bad/id", "bad?id=1", "..", ""} {
		if _, err := Search(t.Context(), Config{Kind: "Jackett", Host: "http://indexer.local", APIKey: "key"}, Indexer{Kind: "Jackett", RemoteID: remote}, "keyword", nil); !errors.Is(err, ErrConfig) {
			t.Fatalf("remote=%s err=%v", remote, err)
		}
	}
}

func TestProwlarrRejectsCrossIndexerAndInvalidMetrics(t *testing.T) {
	indexer := Indexer{Kind: "Prowlarr", RemoteID: "42"}
	for _, body := range []string{`null`, `{}`, `[{"indexerId":7,"title":"Other","size":1,"seeders":1}]`, `[{"indexerId":42,"title":"Bad","size":-1,"seeders":1}]`, `[{"indexerId":42,"title":"Bad","size":1,"seeders":1.5}]`} {
		if _, err := parseProwlarr(t.Context(), []byte(body), indexer); !errors.Is(err, ErrResponse) {
			t.Fatalf("err=%v", err)
		}
	}
	items, err := parseProwlarr(t.Context(), []byte(`[]`), indexer)
	if err != nil || items == nil || len(items) != 0 {
		t.Fatal(items, err)
	}
}

func TestResourceDownloadURLValidation(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "javascript:alert(1)", "https://user:password@tracker.local/download", "magnet:?xt=urn:btih:x"} {
		if validDownloadURL(raw) {
			t.Fatalf("unsafe URL %s", raw)
		}
	}
	if !validDownloadURL("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567") {
		t.Fatal("valid magnet rejected")
	}
}

func TestSearchBodyLimitAndUnknownSeeders(t *testing.T) {
	indexer := Indexer{Kind: "Prowlarr", RemoteID: "42"}
	items, err := parseProwlarr(t.Context(), []byte(`[{"indexerId":42,"title":"Movie","size":123,"seeders":null,"downloadUrl":"https://tracker.local/torrent"}]`), indexer)
	if err != nil || len(items) != 1 || items[0].Seeders != nil {
		t.Fatalf("unknown seeders fabricated: %v %v", items, err)
	}
	transport := handlerTransport(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", maxResponse+1)) })
	if _, err := Search(t.Context(), Config{Kind: "Prowlarr", Host: "http://indexer.local", APIKey: "secret"}, indexer, "keyword", transport); !errors.Is(err, ErrResponse) {
		t.Fatal(err)
	}
}

func FuzzTorznabParser(f *testing.F) {
	f.Add(torznabFixture)
	f.Add(`<rss><channel/></rss>`)
	f.Add(`<error code="100"/>`)
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > maxResponse {
			t.Skip()
		}
		_, _ = parseTorznab(t.Context(), []byte(body), Indexer{Kind: "Jackett", RemoteID: "test"})
	})
}
