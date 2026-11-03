package sitestats

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"golang.org/x/text/encoding/simplifiedchinese"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type rendererFunc func(context.Context, browserruntime.Request) (browserruntime.Result, error)

func (rendererFunc) Available() bool { return true }
func (f rendererFunc) Render(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
	return f(ctx, r)
}
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func TestClientScopesCredentialsRedirectsAndMessageReads(t *testing.T) {
	calls := 0
	c := Client{Origin: "https://fixture.invalid", APIOrigin: "https://api.fixture.invalid", Cookie: "fixture=private", APIKey: "fixture-key", UserAgent: "fixture-ua", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host == "api.fixture.invalid" {
			if r.Header.Get("Cookie") != "" || r.Header.Get("X-API-KEY") != "fixture-key" {
				t.Fatal("API authority lost")
			}
		} else if r.Header.Get("Cookie") != "fixture=private" || r.Header.Get("X-API-KEY") != "" {
			t.Fatal("home authority lost")
		}
		if r.URL.Path == "/redirect" {
			res := response("")
			res.StatusCode = 302
			res.Header.Set("Location", "https://outside.invalid/stolen")
			return res, nil
		}
		return response("fixture"), nil
	})}
	for _, r := range []Request{{Path: "/profile"}, {Path: "https://api.fixture.invalid/api/member/profile", JSON: map[string]any{}}} {
		if _, e := c.Fetch(t.Context(), r); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := c.Fetch(t.Context(), Request{Path: "/message", Message: true}); e == nil {
		t.Fatal("unapproved read")
	}
	for _, bad := range []string{"https://outside.invalid/", "//outside.invalid/", "https://user@fixture.invalid/", "javascript:alert(1)"} {
		if _, e := c.Fetch(t.Context(), Request{Path: bad}); e == nil {
			t.Fatal("external authority", bad)
		}
	}
	if calls != 2 {
		t.Fatal("external request dispatched", calls)
	}
	if _, e := c.Fetch(t.Context(), Request{Path: "/redirect"}); e == nil || calls != 3 {
		t.Fatal("redirect escaped", calls, e)
	}
}
func TestClientUsesEncodingGuardLimitsAndNativeBrowser(t *testing.T) {
	b, _ := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("上传：2 GiB"))
	admitted := 0
	c := Client{Origin: "https://fixture.invalid", Before: func(context.Context, Request) error { admitted++; return nil }, Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		r := response(string(b))
		r.Header.Set("Content-Type", "text/html; charset=gbk")
		return r, nil
	})}
	out, e := c.Fetch(t.Context(), Request{Path: "/"})
	if e != nil || string(out) != "上传：2 GiB" || admitted != 1 {
		t.Fatal(string(out), admitted, e)
	}
	c.Guard = func(context.Context) error { return ErrState }
	if _, e = c.Fetch(t.Context(), Request{Path: "/"}); e == nil || admitted != 1 {
		t.Fatal("guard admitted request")
	}
	c.Guard = func(context.Context) error { return nil }
	c.Emulate = true
	c.Renderer = rendererFunc(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
		if !r.ReadOnly || r.Guard == nil || r.Before == nil {
			t.Fatal("unguarded browser")
		}
		done, e := r.Before(ctx, browserruntime.Exchange{URL: r.URL, Method: "GET"})
		if e != nil {
			t.Fatal(e)
		}
		if e = done("received", ""); e != nil {
			t.Fatal(e)
		}
		return browserruntime.Result{HTML: []byte("native fixture"), URL: r.URL}, nil
	})
	out, e = c.Fetch(t.Context(), Request{Path: "/profile"})
	if e != nil || string(out) != "native fixture" {
		t.Fatal(string(out), e)
	}
	c.Emulate = false
	c.Transport = roundTrip(func(*http.Request) (*http.Response, error) { return response(strings.Repeat("x", (8<<20)+1)), nil })
	if _, e = c.Fetch(t.Context(), Request{Path: "/"}); e == nil {
		t.Fatal("oversized body accepted")
	}
}
