package browserruntime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func realEngine(t *testing.T) *Engine {
	t.Helper()
	binary := os.Getenv("NASTOOL_TEST_BROWSER_BINARY")
	if binary == "" {
		t.Skip("set NASTOOL_TEST_BROWSER_BINARY to run the real isolated browser integration")
	}
	e := New(binary)
	if !e.Available() {
		t.Fatal("required test browser is not executable")
	}
	return e
}
func TestChromeRendersFormOnceAndRetainsOnlyCurrentCookies(t *testing.T) {
	engine := realEngine(t)
	var searches, documents, steps atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "fixture-agent" {
			t.Error("saved UA lost")
		}
		switch r.URL.Path {
		case "/":
			documents.Add(1)
			w.Header().Set("Content-Type", "text/html")
			http.SetCookie(w, &http.Cookie{Name: "server", Value: "http-only", Path: "/", Secure: true, HttpOnly: true})
			io.WriteString(w, `<input id="keyword"><button id="go">Search</button><div id="result"></div><script>
   document.querySelector('#go').onclick=async()=>{const q=document.querySelector('#keyword').value;const res=await fetch('/search',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:new URLSearchParams({q})});document.querySelector('#result').innerHTML=await res.text();document.cookie='saved=; Max-Age=0; path=/';document.cookie='rendered=js-owned; path=/';};</script>`)
		case "/search":
			searches.Add(1)
			if e := r.ParseForm(); e != nil || r.Form.Get("q") != "电影 & query" || r.Header.Get("Referer") != serverReferer(r) {
				t.Error("literal form or referer lost")
			}
			io.WriteString(w, `<table class="results"><tr><td>Movie.2026</td></tr></table>`)
		default:
			t.Error("unexpected browser network", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	r := browserRequest()
	r.URL = server.URL + "/"
	r.Transport = server.Client().Transport
	r.ReadOnly = false
	r.ReadySelector = "table.results"
	r.Form = &Form{Input: `//*[@id="keyword"]`, Submit: `//*[@id="go"]`, Script: "return document.title = 'fixture';", Value: "电影 & query", Referer: server.URL + "/origin"}
	r.Before = func(context.Context, Exchange) (func(string, string) error, error) {
		steps.Add(1)
		return func(string, string) error { return nil }, nil
	}
	page, e := engine.Render(t.Context(), r)
	if e != nil || !strings.Contains(string(page.HTML), "Movie.2026") || !strings.Contains(page.Cookie, "rendered=js-owned") || !strings.Contains(page.Cookie, "server=http-only") || strings.Contains(page.Cookie, "saved=") || searches.Load() != 1 || documents.Load() != 1 || steps.Load() != 2 || engine.Active() != 0 {
		t.Fatal("render/session/once/lifecycle failed", e, searches.Load(), documents.Load(), steps.Load(), page.Cookie)
	}
}
func serverReferer(r *http.Request) string { return "https://" + r.Host + "/origin" }
func TestChromeRefusesForeignDocumentChallengeAndCancellation(t *testing.T) {
	engine := realEngine(t)
	for _, kind := range []string{"foreign", "challenge", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/html")
				switch kind {
				case "foreign":
					http.Redirect(w, r, "http://foreign.invalid/", 302)
				case "challenge":
					io.WriteString(w, `<div id="challenge-running">verification required</div>`)
				case "cancel":
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			r := browserRequest()
			r.URL = server.URL + "/"
			r.ReadySelector = "body"
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			if kind == "cancel" {
				cancel()
			} else {
				defer cancel()
			}
			_, e := engine.Render(ctx, r)
			if e == nil || engine.Active() != 0 {
				t.Fatal("unexpected success or process leak", e)
			}
			if kind == "foreign" && !errors.Is(e, ErrScope) || kind == "challenge" && !errors.Is(e, ErrVerification) || kind == "cancel" && !errors.Is(e, context.Canceled) {
				t.Fatal(kind, e)
			}
			if kind != "cancel" && calls.Load() != 1 || kind == "cancel" && calls.Load() != 0 {
				t.Fatal("unexpected upstream count", calls.Load())
			}
		})
	}
}
