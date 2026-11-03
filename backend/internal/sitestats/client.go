package sitestats

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"golang.org/x/net/html/charset"
)

type Client struct {
	Origin, APIOrigin, Cookie, APIKey, UserAgent string
	Transport                                    http.RoundTripper
	Renderer                                     browserruntime.Renderer
	Emulate, ReadMessages                        bool
	Before                                       func(context.Context, Request) error
	Guard                                        func(context.Context) error
	jar                                          http.CookieJar
}

func (c *Client) Fetch(ctx context.Context, r Request) ([]byte, error) {
	if r.Message && !c.ReadMessages {
		return nil, ErrOrigin
	}
	u, e := Resolve(c.Origin+"/", r.Path)
	api := false
	if e != nil && c.APIOrigin != "" {
		u, e = Resolve(c.APIOrigin+"/", r.Path)
		api = e == nil
	}
	if e != nil {
		return nil, e
	}
	if c.jar == nil {
		c.jar, _ = cookiejar.New(nil)
		base, _ := url.Parse(c.Origin)
		original := &http.Request{Header: http.Header{"Cookie": {c.Cookie}}}
		c.jar.SetCookies(base, original.Cookies())
	}
	if c.Guard != nil {
		if e = c.Guard(ctx); e != nil {
			return nil, e
		}
	}
	if c.Before != nil {
		if e = c.Before(ctx, r); e != nil {
			return nil, e
		}
	}
	if c.Emulate && !api && !r.Raw && r.Form == nil && r.JSON == nil {
		var currentCookies []string
		for _, cookie := range c.jar.Cookies(u) {
			currentCookies = append(currentCookies, cookie.Name+"="+cookie.Value)
		}
		c.Cookie = strings.Join(currentCookies, "; ")
		if c.Renderer == nil || !c.Renderer.Available() {
			return nil, browserruntime.ErrUnavailable
		}
		out, e := c.Renderer.Render(ctx, browserruntime.Request{URL: u.String(), Cookie: c.Cookie, UserAgent: c.UserAgent, Transport: c.Transport, ReadOnly: true, Assets: true, Guard: c.Guard, Before: func(ctx context.Context, x browserruntime.Exchange) (func(string, string) error, error) {
			if c.Before != nil {
				if e := c.Before(ctx, Request{Path: x.URL, Message: r.Message}); e != nil {
					return nil, e
				}
			}
			return func(string, string) error { return nil }, nil
		}})
		if e != nil {
			return nil, e
		}
		if _, e = Resolve(c.Origin+"/", out.URL); e != nil {
			return nil, e
		}
		{
			// A browser result is a full cookie snapshot, including deletions.
			c.jar, _ = cookiejar.New(nil)
			cookies := &http.Request{Header: http.Header{"Cookie": {out.Cookie}}}
			base, _ := url.Parse(c.Origin)
			c.jar.SetCookies(base, cookies.Cookies())
			c.Cookie = out.Cookie
		}
		if c.Guard != nil {
			if e = c.Guard(ctx); e != nil {
				return nil, e
			}
		}
		return out.HTML, nil
	}
	var body io.Reader
	method := http.MethodGet
	contentType := ""
	if r.Form != nil {
		method = http.MethodPost
		body = strings.NewReader(r.Form.Encode())
		contentType = "application/x-www-form-urlencoded"
	}
	if r.JSON != nil {
		raw, e := json.Marshal(r.JSON)
		if e != nil {
			return nil, e
		}
		body = bytes.NewReader(raw)
		method = http.MethodPost
		contentType = "application/json"
	}
	req, e := http.NewRequestWithContext(ctx, method, u.String(), body)
	if e != nil {
		return nil, e
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	if api {
		req.Header.Set("X-API-KEY", c.APIKey)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	jar := c.jar
	if api {
		jar = nil
	}
	client := http.Client{Transport: c.Transport, Timeout: 60 * time.Second, Jar: jar, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		base := c.Origin
		if api {
			base = c.APIOrigin
		}
		if _, e := Resolve(base+"/", next.URL.String()); e != nil {
			return e
		}
		if len(via) > 5 {
			return ErrPage
		}
		if c.Guard != nil {
			if e := c.Guard(ctx); e != nil {
				return e
			}
		}
		if c.Before != nil {
			return c.Before(ctx, r)
		}
		return nil
	}}
	res, e := client.Do(req)
	if e != nil {
		return nil, ErrResponse
	}
	defer res.Body.Close()
	if !api {
		var cookies []string
		for _, cookie := range c.jar.Cookies(u) {
			cookies = append(cookies, cookie.Name+"="+cookie.Value)
		}
		c.Cookie = strings.Join(cookies, "; ")
	}
	if res.StatusCode != 200 {
		return nil, ErrResponse
	}
	const limit = 8 << 20
	raw, e := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if e != nil || len(raw) > limit {
		return nil, ErrResponse
	}
	if c.Guard != nil {
		if e = c.Guard(ctx); e != nil {
			return nil, e
		}
	}
	if contains(res.Header.Get("Content-Type"), "image/", "application/octet-stream") {
		return raw, nil
	}
	if utf8.Valid(raw) && !contains(strings.ToLower(res.Header.Get("Content-Type")), "gbk", "gb2312", "big5") {
		return raw, nil
	}
	reader, e := charset.NewReader(bytes.NewReader(raw), res.Header.Get("Content-Type"))
	if e != nil {
		return nil, ErrResponse
	}
	raw, e = io.ReadAll(io.LimitReader(reader, limit+1))
	if e != nil || len(raw) > limit {
		return nil, ErrResponse
	}
	return raw, nil
}

func (c *Client) Home(ctx context.Context) ([]byte, error) {
	page, e := c.Fetch(ctx, Request{Path: "/"})
	if e != nil {
		return nil, e
	}
	// Preserve the legacy literal first-visit redirect, without evaluating script.
	if !contains(strings.ToLower(string(page)), "<title") {
		p := capture(string(page), `window\.location\s*=\s*["']([^"']+)["']\s*;`)
		if p != "" {
			page, e = c.Fetch(ctx, Request{Path: p})
			if e != nil {
				return nil, e
			}
		}
	}
	if len(nodes(string(page), `link[rel="search"], meta[name="csrf-token"], meta[name="x-csrf-token"]`)) == 0 && Identify(string(page)) == "NexusPhp" {
		// Fake home pages are replaced only when the index fetch succeeds.
		if indexed, err := c.Fetch(ctx, Request{Path: "/index.php"}); err == nil {
			page = indexed
		}
	}
	return page, nil
}

func Origin(raw string) string {
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
