package browserruntime

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func pageResponse(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func browserRequest() Request {
	return Request{URL: "https://site.test/search", Cookie: "saved=private", UserAgent: "fixture-agent", ReadOnly: true, Assets: true, Guard: func(ctx context.Context) error { return ctx.Err() }}
}

func TestBrokerOnlyGoOwnsVerifiedNetworkAndResponseCookie(t *testing.T) {
	var calls, steps, finishes atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "saved=private" || r.Header.Get("User-Agent") != "fixture-agent" {
			t.Error("original session lost")
		}
		http.SetCookie(w, &http.Cookie{Name: "next", Value: "owned", Path: "/", Secure: true})
		io.WriteString(w, "<p>page</p>")
	}))
	defer server.Close()
	r := browserRequest()
	r.URL = server.URL + "/search"
	r.Transport = server.Client().Transport
	r.Before = func(ctx context.Context, x Exchange) (func(string, string) error, error) {
		if x.Ordinal != 1 || x.Kind != "Document" {
			t.Error(x)
		}
		steps.Add(1)
		return func(state, digest string) error {
			if state != "received" || len(digest) != 64 {
				t.Error(state)
			}
			finishes.Add(1)
			return nil
		}, nil
	}
	b, e := newBroker(r)
	if e != nil {
		t.Fatal(e)
	}
	got, e := b.exchange(t.Context(), r.URL, "GET", "Document", http.Header{}, nil)
	if e != nil || string(got.body) != "<p>page</p>" || !strings.Contains(b.cookie(r.URL), "next=owned") || calls.Load() != 1 || steps.Load() != 1 || finishes.Load() != 1 {
		t.Fatal(e, calls.Load(), steps.Load(), finishes.Load())
	}
	// The same server without its CA is rejected, rather than disabling TLS.
	r.Transport = nil
	r.Before = nil
	b, _ = newBroker(r)
	if _, e = b.exchange(t.Context(), r.URL, "GET", "Document", http.Header{}, nil); !errors.Is(e, ErrResponse) || calls.Load() != 1 {
		t.Fatal("unverified TLS accepted", e)
	}
}
func TestBrokerIntentFailureLostResponseAndDenialCannotReplay(t *testing.T) {
	for _, kind := range []string{"intent", "lost", "receipt", "denied"} {
		t.Run(kind, func(t *testing.T) {
			var calls, finishes int
			var state string
			r := browserRequest()
			r.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if kind == "lost" {
					return nil, io.ErrUnexpectedEOF
				}
				out := pageResponse(req, "received")
				if kind == "denied" {
					out.StatusCode = 429
				}
				return out, nil
			})
			r.Before = func(context.Context, Exchange) (func(string, string) error, error) {
				if kind == "intent" {
					return nil, errors.New("journal refused")
				}
				return func(s, d string) error {
					finishes++
					state = s
					if kind == "receipt" {
						return errors.New("receipt lost")
					}
					return nil
				}, nil
			}
			b, _ := newBroker(r)
			for i := 0; i < 2; i++ {
				if _, e := b.exchange(t.Context(), r.URL, "GET", "Document", nil, nil); e == nil {
					t.Fatal("failure accepted")
				}
			}
			want := 1
			if kind == "intent" {
				want = 0
			}
			if calls != want || finishes != want {
				t.Fatal(calls, finishes)
			}
			if kind == "lost" && state != "uncertain" || kind == "denied" && state != "denied" {
				t.Fatal(state)
			}
		})
	}
}
func TestBrokerScopeBeforeBytesAndUncredentialedAssets(t *testing.T) {
	r := browserRequest()
	var calls int
	r.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host == "cdn.test" {
			for _, h := range []string{"Cookie", "Authorization", "Origin", "Referer", "X-Private", "X-Api-Key"} {
				if req.Header.Get(h) != "" {
					t.Error("foreign credential", h)
				}
			}
		}
		return pageResponse(req, "asset"), nil
	})
	b, _ := newBroker(r)
	if _, e := b.exchange(t.Context(), "https://cdn.test/app.js", "GET", "Script", http.Header{"X-Private": {"secret"}, "X-Api-Key": {"secret"}, "Referer": {r.URL}}, nil); e != nil {
		t.Fatal(e)
	}
	for _, c := range []struct{ url, method, kind string }{{"https://other.test/", "GET", "Document"}, {r.URL, "POST", "XHR"}, {r.URL, "DELETE", "Fetch"}, {"file:///etc/passwd", "GET", "Document"}, {"https://cdn.test/api", "GET", "XHR"}, {r.URL, "GET", "Image"}} {
		if _, e := b.exchange(t.Context(), c.url, c.method, c.kind, nil, nil); !errors.Is(e, ErrScope) {
			t.Fatal(c, e)
		}
	}
	if calls != 1 {
		t.Fatal("forbidden request sent", calls)
	}
	changed := true
	r.Guard = func(context.Context) error {
		if changed {
			return ErrScope
		}
		return nil
	}
	b, _ = newBroker(r)
	if _, e := b.exchange(t.Context(), r.URL, "GET", "Document", nil, nil); e == nil || calls != 1 {
		t.Fatal("changed source sent", e)
	}
}
func TestBrokerBudgetsAndPrivateSessionExpiry(t *testing.T) {
	if Hash(map[string]any{"unsupported": math.NaN()}) != "" {
		t.Fatal("failed serialization produced a usable fingerprint")
	}
	r := browserRequest()
	r.Transport = transportFunc(func(req *http.Request) (*http.Response, error) { return pageResponse(req, "ok"), nil })
	b, _ := newBroker(r)
	b.requests = MaxRequests
	if _, e := b.exchange(t.Context(), r.URL, "GET", "Document", nil, nil); !errors.Is(e, ErrLimit) {
		t.Fatal(e)
	}
	b, _ = newBroker(r)
	if _, e := b.exchange(t.Context(), r.URL, "POST", "XHR", nil, make([]byte, 65537)); !errors.Is(e, ErrScope) {
		t.Fatal(e)
	}
	s := Session{SiteID: 1, Site: Hash("site"), Application: Hash("app"), Origin: Origin(r.URL), Cookie: r.Cookie, UserAgent: r.UserAgent, Created: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
	if !s.Valid() {
		t.Fatal("valid session refused")
	}
	s.Created = time.Now().Add(-31 * time.Minute).Format(time.RFC3339Nano)
	if s.Valid() {
		t.Fatal("expired session accepted")
	}
	s.Created = time.Now().Add(time.Minute).Format(time.RFC3339Nano)
	if s.Valid() {
		t.Fatal("future session accepted")
	}
}
