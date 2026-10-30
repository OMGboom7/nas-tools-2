package browserruntime

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

type exchangeResponse struct {
	status  int
	headers http.Header
	body    []byte
}
type broker struct {
	request                    Request
	client                     *http.Client
	jar                        http.CookieJar
	mu                         sync.Mutex
	fulfillMu                  sync.Mutex
	requests, bytes, documents int
	lastURL                    string
	fatal                      error
}

func newBroker(r Request) (*broker, error) {
	if e := validate(r); e != nil {
		return nil, e
	}
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	seed, _ := url.Parse(r.URL)
	h := http.Header{}
	h.Set("Cookie", r.Cookie)
	cookies := (&http.Request{Header: h}).Cookies()
	if len(cookies) > 128 {
		return nil, ErrLimit
	}
	jar.SetCookies(seed, cookies)
	transport := r.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		c := t.Clone()
		c.DisableKeepAlives = true
		c.ForceAttemptHTTP2 = false
		c.MaxResponseHeaderBytes = 64 << 10
		c.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		if r.Proxy != "" {
			proxy, _ := url.Parse(r.Proxy)
			c.Proxy = http.ProxyURL(proxy)
		}
		transport = c
	} else if r.Proxy != "" {
		return nil, ErrScope
	}
	return &broker{request: r, jar: jar, client: &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, lastURL: r.URL}, nil
}
func (b *broker) error(e error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fatal == nil {
		b.fatal = e
	}
}
func (b *broker) state() (int, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests, b.bytes, b.fatal
}
func (b *broker) allowed(address, method, kind string) bool {
	u, e := url.Parse(address)
	if e != nil || !validURL(u) {
		return false
	}
	same := Origin(address) == Origin(b.request.URL)
	if kind == "LoginImage" {
		return b.request.Login != nil && same && method == "GET"
	}
	if kind == "Other" && strings.EqualFold(u.Path, "/favicon.ico") {
		return false
	}
	if kind == "Image" || kind == "Media" || kind == "Font" || kind == "WebSocket" || kind == "EventSource" {
		return false
	}
	if kind == "Document" && !same {
		return false
	}
	if !same {
		return b.request.Assets && u.Scheme == "https" && (kind == "Script" || kind == "Stylesheet") && (method == "GET" || method == "HEAD")
	}
	if method == "GET" || method == "HEAD" {
		return true
	}
	if method == "OPTIONS" {
		return kind == "Preflight" || kind == "Fetch" || kind == "XHR"
	}
	if method != "POST" {
		return false
	}
	if b.request.ReadOnly {
		return false
	}
	return kind == "Document" || kind == "XHR" || kind == "Fetch"
}

// exchange serializes the actual HTTP calls. A Chrome request is fulfilled
// from Go's verified response; Chrome never owns/retries the upstream socket.
func (b *broker) exchange(ctx context.Context, address, method, kind string, headers http.Header, data []byte) (exchangeResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	zero := exchangeResponse{}
	if b.fatal != nil {
		return zero, b.fatal
	}
	if !b.allowed(address, method, kind) || len(data) > 64<<10 {
		return zero, ErrScope
	}
	if e := b.request.Guard(ctx); e != nil {
		b.fatal = ErrScope
		return zero, ErrScope
	}
	b.requests++
	if b.requests > MaxRequests {
		b.fatal = ErrLimit
		return zero, ErrLimit
	}
	if kind == "Document" {
		b.documents++
		if b.documents > 10 {
			b.fatal = ErrLimit
			return zero, ErrLimit
		}
	}
	ordinal := b.requests
	same := Origin(address) == Origin(b.request.URL)
	r, e := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(data))
	if e != nil {
		return zero, ErrScope
	}
	r.GetBody = nil
	r.Close = true
	for key, values := range headers {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "cookie" || lower == "authorization" || lower == "proxy-authorization" || lower == "accept-encoding" || lower == "content-length" || lower == "connection" {
			continue
		}
		if !same && lower != "accept" && lower != "accept-language" && lower != "user-agent" {
			continue
		}
		for _, v := range values {
			r.Header.Add(key, v)
		}
	}
	r.Header.Set("User-Agent", b.request.UserAgent)
	if same {
		u, _ := url.Parse(address)
		for _, c := range b.jar.Cookies(u) {
			r.AddCookie(c)
		}
	}
	var finish func(string, string) error
	if b.request.Before != nil {
		finish, e = b.request.Before(ctx, Exchange{Ordinal: ordinal, URL: address, Method: method, Kind: kind, Body: data})
		if e != nil || finish == nil {
			b.fatal = ErrScope
			return zero, ErrScope
		}
	}
	done := func(state, digest string) error {
		if finish == nil {
			return nil
		}
		if e := finish(state, digest); e != nil {
			b.fatal = ErrResponse
			return ErrResponse
		}
		return nil
	}
	response, e := b.client.Do(r)
	if e != nil {
		_ = done("uncertain", "")
		b.fatal = ErrResponse
		return zero, ErrResponse
	}
	defer response.Body.Close()
	limit := 8 << 20
	if kind == "LoginImage" {
		limit = 2 << 20
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if e != nil || len(raw) > limit {
		_ = done("uncertain", "")
		b.fatal = ErrLimit
		return zero, ErrLimit
	}
	b.bytes += len(raw)
	if b.bytes > MaxNetwork {
		_ = done("uncertain", "")
		b.fatal = ErrLimit
		return zero, ErrLimit
	}
	state := "received"
	if same && (response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 406 || response.StatusCode == 429) {
		state = "denied"
	}
	if e = done(state, Hash([]any{response.StatusCode, raw})); e != nil {
		return zero, e
	}
	if state == "denied" {
		b.fatal = ErrResponse
		return zero, ErrResponse
	}
	if same {
		u, _ := url.Parse(address)
		b.jar.SetCookies(u, response.Cookies())
	}
	if e = b.request.Guard(ctx); e != nil {
		b.fatal = ErrScope
		return zero, ErrScope
	}
	out := exchangeResponse{status: response.StatusCode, headers: response.Header.Clone(), body: raw}
	// net/http has decompressed a gzip body. Do not tell the renderer to decode
	// the already decoded bytes again, and never keep transport-hop headers.
	if response.Uncompressed {
		out.headers.Del("Content-Encoding")
	}
	for _, key := range []string{"Content-Length", "Transfer-Encoding", "Connection", "Proxy-Authenticate", "Alt-Svc"} {
		out.headers.Del(key)
	}
	out.headers.Set("Content-Length", strconv.Itoa(len(raw)))
	if kind == "Document" {
		b.lastURL = address
		if response.StatusCode < 200 || response.StatusCode >= 400 {
			b.fatal = ErrResponse
			return zero, ErrResponse
		}
	}
	return out, nil
}
func (b *broker) cookie(address string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, e := url.Parse(address)
	if e != nil || Origin(address) != Origin(b.request.URL) {
		return ""
	}
	r := &http.Request{Header: http.Header{}}
	for _, c := range b.jar.Cookies(u) {
		r.AddCookie(c)
	}
	return r.Header.Get("Cookie")
}
