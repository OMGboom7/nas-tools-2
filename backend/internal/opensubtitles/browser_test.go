package opensubtitles

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
)

type renderFunc func(context.Context, browserruntime.Request) (browserruntime.Result, error)

func (renderFunc) Available() bool { return true }
func (f renderFunc) Render(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
	return f(ctx, r)
}

func TestLegacyRenderedSearchIntentAndArchiveCookieContinuation(t *testing.T) {
	ops := []OpenTestStep{}
	rendered, archives := 0, 0
	archive := zipBody(t, map[string][]byte{"movie.srt": []byte("subtitle")})
	renderer := renderFunc(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
		rendered++
		if r.URL != "https://www.opensubtitles.org/zh/search/moviename-Movie%202026/sublanguageid-chi" || r.Cookie != "original=fixture" || !r.ReadOnly || r.Assets {
			t.Error("wrong original provider scope", r.URL)
		}
		if e := r.Guard(ctx); e != nil {
			return browserruntime.Result{}, e
		}
		for i := 1; i <= 2; i++ {
			finish, e := r.Before(ctx, browserruntime.Exchange{Ordinal: i, URL: r.URL, Method: "GET", Kind: "Document"})
			if e != nil {
				return browserruntime.Result{}, e
			}
			if e = finish("received", Digest([]byte("rendered response"))); e != nil {
				return browserruntime.Result{}, e
			}
		}
		return browserruntime.Result{URL: r.URL, UserAgent: r.UserAgent, Cookie: "original=fixture; page=rendered", HTML: []byte(`<table id="search_results"><tbody><tr><td><strong><a class="bnone">Movie (2026)</a></strong></td><td></td><td></td><td></td><td><a href="/download/1">zip</a></td></tr></tbody></table>`)}, nil
	})
	c := &Client{Config: Config{Provider: Legacy, Cookie: "original=fixture", UserAgent: "fixture-agent"}, Renderer: renderer, Guard: func(ctx context.Context) error { return ctx.Err() }, Step: testSteps(t, &ops), Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		archives++
		if r.URL.String() != "https://www.opensubtitles.org/download/1" || !strings.Contains(r.Header.Get("Cookie"), "page=rendered") || r.Header.Get("Authorization") != "" {
			t.Error("browser cookie not passed to original archive")
		}
		return testResponse(r, 200, archive, nil), nil
	})}
	rows, e := c.Search(t.Context(), Query{Movie: true, Name: "Movie", Year: "2026"})
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	files, e := c.Download(t.Context(), rows[0])
	if e != nil || len(files) != 1 || rendered != 1 || archives != 1 || len(ops) != 3 || ops[0].Signature == ops[1].Signature {
		t.Fatal(e, rendered, archives, len(ops))
	}
}
func TestLegacyRendererFailureDoesNotFallbackOrFetchArchive(t *testing.T) {
	calls := 0
	c := &Client{Config: Config{Provider: Legacy}, Step: func(context.Context, string, string) (func(string, string) error, error) {
		return func(string, string) error { return nil }, nil
	}, Guard: func(ctx context.Context) error { return ctx.Err() }, Renderer: renderFunc(func(context.Context, browserruntime.Request) (browserruntime.Result, error) {
		return browserruntime.Result{}, browserruntime.ErrVerification
	}), Transport: testTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("forbidden") })}
	if _, e := c.Search(t.Context(), Query{Movie: true, Name: "Movie"}); !errors.Is(e, browserruntime.ErrVerification) || calls != 0 {
		t.Fatal("render failure replayed HTTP", e, calls)
	}
}
