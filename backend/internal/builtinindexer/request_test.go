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

func TestRequestParametersAndCategoryCompatibility(t *testing.T) {
	definition := indexercatalog.Definition{ID: "test", Domain: "https://tracker.local/prefix/", Search: json.RawMessage(`{"paths":[{"path":"torrents.php?fixed=yes","method":"get"}],"params":{"search":"{keyword}"}}`), Category: json.RawMessage(`{"movie":[{"id":401}],"tv":[{"id":402}]}`)}
	keyword := "电影 & cat999=1 + 中文"
	plan, err := Build(definition, keyword, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	query := plan.URL.Query()
	if plan.URL.Path != "/prefix/torrents.php" || query.Get("search") != keyword || query.Get("page") != "2" || query.Get("search_mode") != "0" || query.Get("notnewword") != "1" || query.Get("cat401") != "1" || query.Get("cat402") != "1" || query.Get("cat999") != "" || query.Get("fixed") != "yes" {
		t.Fatal(plan.URL)
	}
	definition.Category = json.RawMessage(`{"field":"categories","delimiter":",","movie":[{"id":"movie"}],"tv":[{"id":"tv"}]}`)
	plan, err = Build(definition, keyword, 0, "tv")
	if err != nil || plan.URL.Query().Get("categories") != ",tv" {
		t.Fatal(plan.URL, err)
	}
	definition.Search = json.RawMessage(`{"paths":[{"path":"search/{keyword}?page={page}","method":"get"}]}`)
	plan, err = Build(definition, "中文/A & q=evil", 3, "movie")
	if err != nil || !strings.Contains(plan.URL.EscapedPath(), "%2F") || plan.URL.Query().Get("page") != "3" || plan.URL.Query().Get("q") != "" {
		t.Fatal(plan.URL, err)
	}
}

func TestRequestRejectsUnsupportedAndMalformedDefinitions(t *testing.T) {
	for _, fixture := range []struct {
		search, parser, domain string
		unsupported            bool
	}{
		{`{"paths":[{"path":"api/search","method":"post"}]}`, "", "https://tracker.local", true},
		{`{"paths":[{"path":"search","method":"chrome"}]}`, "", "https://tracker.local", true},
		{`{"paths":[{"path":"search","method":"get"}]}`, "MTeamSpider", "https://tracker.local", true},
		{`{"paths":[{"path":"search?word={unknown}"}]}`, "", "https://tracker.local", false},
		{`{"paths":[{"path":"//other.local/collect"}]}`, "", "https://tracker.local", false},
		{`{"paths":[{"path":"search"}]}`, "", "https://user:secret@tracker.local", false},
		{`{"paths":[{"path":"search"}]}`, "", "file:///tmp/data", false},
		{`{"paths":[]}`, "", "https://tracker.local", false},
	} {
		_, err := Build(indexercatalog.Definition{Domain: fixture.domain, Parser: fixture.parser, Search: json.RawMessage(fixture.search)}, "word", 0, "")
		wanted := ErrConfig
		if fixture.unsupported {
			wanted = ErrUnsupported
		}
		if !errors.Is(err, wanted) {
			t.Fatalf("error=%v expected=%v", err, wanted)
		}
	}
}

func TestBundledDefinitionsRetainSearchAndParsingRules(t *testing.T) {
	catalog, err := indexercatalog.Load("../../../web/backend/user.sites.bin")
	if err != nil {
		t.Fatal(err)
	}
	supported, unsupported, invalid := 0, 0, 0
	for _, definition := range catalog.Indexers {
		if len(definition.Search) == 0 {
			t.Fatalf("search/parsing schema lost for %s", definition.ID)
		}
		plan, err := Build(definition, "电影 & Query", 0, "")
		switch {
		case err == nil:
			if len(definition.Torrents) == 0 {
				t.Fatalf("generic parsing schema lost for %s", definition.ID)
			}
			supported++
			if plan.URL.Hostname() == "" {
				t.Fatal(definition.ID)
			}
		case errors.Is(err, ErrUnsupported):
			unsupported++
		case errors.Is(err, ErrConfig):
			invalid++
		default:
			t.Fatal(err)
		}
	}
	if supported < 100 || supported+unsupported+invalid != 113 {
		t.Fatalf("supported=%d unsupported=%d invalid=%d", supported, unsupported, invalid)
	}
	t.Logf("bundled request plans: supported=%d dedicated=%d invalid=%d", supported, unsupported, invalid)
}

type fakeTransport func(*http.Request) (*http.Response, error)

func (fn fakeTransport) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestFetchBoundsCookiesRedirectsAndErrors(t *testing.T) {
	plan, err := Build(indexercatalog.Definition{Domain: "https://tracker.local/", Search: json.RawMessage(`{"paths":[{"path":"search"}]}`)}, "word", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{200, 302, 403} {
		calls := 0
		transport := fakeTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Hostname() != "tracker.local" || r.Header.Get("Cookie") != "session=private" || r.Header.Get("User-Agent") != "site-agent" {
				t.Fatal("credentials sent incorrectly")
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("<html>body</html>")), Header: http.Header{"Location": []string{"https://collector.invalid/"}}, Request: r}, nil
		})
		body, err := Fetch(t.Context(), plan, "session=private", "site-agent", transport)
		if calls != 1 || status == 200 && (err != nil || len(body) == 0) || status != 200 && !errors.Is(err, ErrResponse) {
			t.Fatalf("status=%d err=%v calls=%d", status, err, calls)
		}
	}
	oversized := fakeTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (4<<20)+1))), Header: http.Header{}, Request: r}, nil
	})
	if _, err := Fetch(t.Context(), plan, "", "", oversized); !errors.Is(err, ErrResponse) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Fetch(ctx, plan, "", "", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
