package douban

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(r *http.Request, b string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(b))}
}
func TestFrodoReadonlySignedSearchSubjectPeopleAndNoUserCredentials(t *testing.T) {
	calls := 0
	c := Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		if r.Method != "GET" || r.URL.Host != "frodo.douban.com" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || q.Get("apiKey") != projectKey {
			t.Fatal(r.URL)
		}
		mac := hmac.New(sha1.New, []byte(signingKey))
		mac.Write([]byte("GET&" + url.QueryEscape(r.URL.Path) + "&" + q.Get("_ts")))
		if q.Get("_sig") != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
			t.Fatal("wrong signature")
		}
		switch r.URL.Path {
		case "/api/v2/search/movie":
			if q.Get("q") != "Movie" {
				t.Fatal(q)
			}
			return reply(r, `{"items":[{"target_id":"300","target_type":"movie","target":{"id":"300","title":"Movie","year":"2026"}}]}`), nil
		case "/api/v2/movie/300":
			return reply(r, `{"id":"300","title":"Movie","year":2026,"imdb":"tt100"}`), nil
		case "/api/v2/movie/300/celebrities":
			return reply(r, `{"id":"300","directors":[],"actors":[{"id":"400","name":"張三","latin_name":"John Smith","character":"饰 英雄"}]}`), nil
		}
		t.Fatal("unexpected scope", r.URL)
		return nil, nil
	})}
	items, err := c.Search(t.Context(), "Movie")
	if err != nil || len(items) != 1 || items[0].ID != "300" {
		t.Fatal(items, err)
	}
	s, err := c.Subject(t.Context(), "movie", "300")
	if err != nil || s.Original != "Movie" || s.Year != "2026" {
		t.Fatal(s, err)
	}
	p, err := c.People(t.Context(), "movie", "300")
	if err != nil || len(p.Actors) != 1 || ID(p.Actors[0].ID) != "400" || calls != 3 {
		t.Fatal(p, err, calls)
	}
}
func TestFrodoInvalidSelectorsBodiesRedirectsAndDiagnosticsAreNotTrusted(t *testing.T) {
	for _, mode := range []string{"redirect", "lost", "wrong-id", "missing", "error", "oversize"} {
		calls := 0
		c := Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			switch mode {
			case "redirect":
				v := reply(r, "")
				v.StatusCode = 302
				v.Header.Set("Location", "https://attacker.test/private")
				return v, nil
			case "lost":
				return nil, errors.New("private credential")
			case "wrong-id":
				return reply(r, `{"id":"301","title":"Other"}`), nil
			case "error":
				return reply(r, `{"localized_message":"private diagnostic"}`), nil
			case "oversize":
				return reply(r, strings.Repeat("x", (2<<20)+1)), nil
			}
			return reply(r, `{}`), nil
		})}
		if _, err := c.Subject(t.Context(), "movie", "300"); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal(mode, err)
		}
		if calls != 1 {
			t.Fatal("readonly call replayed", calls)
		}
		if _, err := c.Subject(t.Context(), "movie", "../private"); err == nil || calls != 1 {
			t.Fatal("bad selector reached server")
		}
	}
	for _, raw := range []string{`"001"`, `1.2`, `{}`, `null`, `-1`} {
		if ID(json.RawMessage(raw)) != "" {
			t.Fatal(raw)
		}
	}
}

func TestFrodoSearchWithKnownTruncationCannotProveUniqueMedia(t *testing.T) {
	for _, body := range []string{`{"items":[],"has_more":true}`, `{"items":[],"total":1}`} {
		c := Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) { return reply(r, body), nil })}
		if _, err := c.Search(t.Context(), "Movie"); err == nil {
			t.Fatal("truncated search accepted", body)
		}
	}
}
