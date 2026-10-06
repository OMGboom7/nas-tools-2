package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func resultFixture(t *testing.T) (Plan, ResultOptions) {
	t.Helper()
	definition := indexercatalog.Definition{ID: "tracker", Name: "Tracker", Domain: "https://tracker.local/prefix/", Search: json.RawMessage(`{"paths":[{"path":"torrents.php"}],"params":{"search":"{keyword}"}}`), Torrents: json.RawMessage(`{
		"list":{"selector":"table.torrents > tr:has(a)"},
		"fields":{
			"title_default":{"selector":"a.title"},
			"title_optional":{"selector":"a.optional","attribute":"title"},
			"title":{"text":"{% if fields['title_optional'] %}{{ fields['title_optional'] }}{% else %}{{ fields['title_default'] }}{% endif %}"},
			"download":{"selector":"a.download","attribute":"href"},
			"details":{"selector":"a.title","attribute":"href"},
			"size":{"selector":".size"},"seeders":{"selector":".seeders"},"leechers":{"selector":".peers"},
			"downloadvolumefactor":{"case":{"img.free":0,"img.half":0.5}},
			"uploadvolumefactor":{"case":{"img.double":2,"*":1}}
		}
	}`)}
	plan, err := Build(definition, "Movie & test", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	return plan, ResultOptions{Now: time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC), Limit: 100, EmptySelector: ".no-results"}
}

const resultHTML = `<table class="torrents"><tr><td><a class="title" href="details.php?id=42&amp;passkey=private">Movie.2026.1080p</a><a class="download" href="download.php?id=42&amp;passkey=private">download</a><span class="size">1.5 GB</span><span class="seeders">20/3</span><span class="peers">0</span><img class="half"></td></tr></table>`

func TestFetchAndParseResults(t *testing.T) {
	ctx := context.Background()
	plan, options := resultFixture(t)
	body, err := Fetch(ctx, plan, "session=private", "Go test", fakeTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "tracker.local" || request.Header.Get("Cookie") != "session=private" {
			t.Fatal(request.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resultHTML)), Header: http.Header{}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	resources, err := ParseResults(ctx, plan, body, options)
	if err != nil || len(resources) != 1 {
		t.Fatal(resources, err)
	}
	r := resources[0]
	if r.Title != "Movie.2026.1080p" || r.Size != 1610612736 || r.Seeders == nil || *r.Seeders != 20 || r.Peers == nil || *r.Peers != 0 || r.DownloadFactor == nil || *r.DownloadFactor != 0.5 || r.Freeleech == nil || *r.Freeleech {
		t.Fatal(r)
	}
	if r.DownloadURL != "https://tracker.local/prefix/download.php?id=42&passkey=private" || r.PageURL != "https://tracker.local/prefix/details.php?id=42" {
		t.Fatal(r.PageURL, r.DownloadURL)
	}
	encoded, err := json.Marshal(r)
	if err != nil || strings.Contains(string(encoded), "private") {
		t.Fatal(string(encoded), err)
	}
	resources, err = ParseResults(ctx, plan, []byte(strings.ReplaceAll(resultHTML, `<img class="half">`, "")), options)
	if err != nil || resources[0].DownloadFactor != nil || resources[0].Freeleech != nil {
		t.Fatal(resources, err)
	}
}

func TestResultsDoNotTreatLoginOrBrokenHTMLAsEmpty(t *testing.T) {
	plan, options := resultFixture(t)
	ctx := context.Background()
	for _, body := range []string{`<form><input type="password"></form><div class="no-results"></div>`, `<div id="challenge-form"></div>`, `<h1>Server error</h1>`, strings.ReplaceAll(resultHTML, `download.php?id=42&amp;passkey=private`, `https://other.local/collect`), strings.ReplaceAll(resultHTML, "1.5 GB", "unknown"), strings.ReplaceAll(resultHTML, "20/3", "-1")} {
		if _, err := ParseResults(ctx, plan, []byte(body), options); !errors.Is(err, ErrResponse) {
			t.Fatal(body, err)
		}
	}
	resources, err := ParseResults(ctx, plan, []byte(`<div class="no-results">No torrents</div>`), options)
	if err != nil || len(resources) != 0 {
		t.Fatal(resources, err)
	}
	options.EmptySelector = ""
	if _, err := ParseResults(ctx, plan, []byte(`<div class="no-results">No torrents</div>`), options); !errors.Is(err, ErrResponse) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ParseResults(ctx, plan, []byte(resultHTML), options); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestResultAddressesAndSizeBounds(t *testing.T) {
	plan, _ := resultFixture(t)
	for _, raw := range []string{"javascript:alert(1)", "file:///tmp/a", "//other.local/a", "https://user:private@tracker.local/a", "/a#fragment", "magnet:?xt=bad"} {
		if _, err := resultURL(plan.URL, raw, true); !errors.Is(err, ErrResponse) {
			t.Fatal(raw, err)
		}
	}
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)
	if value, err := resultURL(plan.URL, magnet, true); err != nil || value != magnet {
		t.Fatal(value, err)
	}
	for _, raw := range []string{"-1 GB", "NaN", "1e20 GB", "999999999999999999999 TB", "1.2.3 GB"} {
		if _, err := resultSize(raw); !errors.Is(err, ErrResponse) {
			t.Fatal(raw, err)
		}
	}
}
