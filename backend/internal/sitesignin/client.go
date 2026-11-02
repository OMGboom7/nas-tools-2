package sitesignin

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"golang.org/x/net/html/charset"
	"golang.org/x/net/publicsuffix"
)

type reply struct {
	body    []byte
	status  int
	header  http.Header
	address string
}
type session struct {
	Executor
	origin            string
	client            *http.Client
	aux               *http.Client
	jar               http.CookieJar
	action, uncertain bool
	requests          int
	mu                sync.Mutex
}

func httpClient(transport http.RoundTripper) *http.Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		c := t.Clone()
		c.DisableKeepAlives = true
		c.ForceAttemptHTTP2 = false
		c.MaxResponseHeaderBytes = 64 << 10
		c.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		transport = c
	}
	return &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func newSession(e Executor) (*session, error) {
	origin := browserruntime.Origin(e.Target.URL)
	if origin == "" {
		return nil, ErrScope
	}
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	u, _ := url.Parse(e.Target.URL)
	h := http.Header{}
	h.Set("Cookie", e.Target.Cookie)
	jar.SetCookies(u, (&http.Request{Header: h}).Cookies())
	s := &session{Executor: e, origin: origin, jar: jar, client: httpClient(e.Transport), aux: httpClient(e.AuxTransport)}
	// Browser callbacks and OCR also pass through this boundary. Keep uncertain
	// auxiliary responses visible to the final outcome, without a secret journal.
	s.Before = func(ctx context.Context, x Exchange) (func(string, string) error, error) {
		finish, err := e.Before(ctx, x)
		if err != nil || finish == nil {
			return nil, err
		}
		if x.Action {
			s.mu.Lock()
			s.action = true
			s.mu.Unlock()
		}
		return func(state, digest string) error {
			if state == "uncertain" {
				s.mu.Lock()
				s.uncertain = true
				s.mu.Unlock()
			}
			err := finish(state, digest)
			if err != nil {
				s.mu.Lock()
				s.uncertain = true
				s.mu.Unlock()
			}
			return err
		}, nil
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Location == nil {
		s.Location = time.Local
	}
	return s, nil
}
func (s *session) address(path string) string {
	u, err := url.Parse(path)
	if err != nil {
		return ""
	}
	base, _ := url.Parse(s.origin + "/")
	result := base.ResolveReference(u).String()
	if browserruntime.Origin(result) != s.origin {
		return ""
	}
	return result
}
func (s *session) call(ctx context.Context, method, path string, data url.Values, action bool) (reply, error) {
	var payload []byte
	content := ""
	if data != nil {
		payload = []byte(data.Encode())
		content = "application/x-www-form-urlencoded"
	}
	return s.request(ctx, Exchange{Kind: "site", Method: method, URL: s.address(path), Body: payload, Action: action}, content, nil, false)
}
func (s *session) json(ctx context.Context, path string, data any, headers http.Header) (reply, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return reply{}, ErrResponse
	}
	return s.request(ctx, Exchange{Kind: "site", Method: "POST", URL: s.address(path), Body: raw, Action: true}, "application/json", headers, false)
}
func (s *session) get(ctx context.Context, path string) (reply, error) {
	return s.call(ctx, "GET", path, nil, false)
}
func (s *session) act(ctx context.Context, method, path string, data url.Values) (reply, error) {
	return s.call(ctx, method, path, data, true)
}

func (s *session) request(ctx context.Context, x Exchange, content string, headers http.Header, aux bool) (reply, error) {
	zero := reply{}
	if browserruntime.Origin(x.URL) == "" || !sitelogin.Text(x.URL, 16000) || len(x.Body) > 2<<20 || !aux && browserruntime.Origin(x.URL) != s.origin || s.requests >= 256 {
		return zero, ErrScope
	}
	if err := s.Guard(ctx); err != nil {
		return zero, ErrScope
	}
	s.requests++
	request, err := http.NewRequestWithContext(ctx, x.Method, x.URL, bytes.NewReader(x.Body))
	if err != nil {
		return zero, ErrScope
	}
	request.GetBody = nil
	request.Close = true
	if content != "" {
		request.Header.Set("Content-Type", content)
	}
	if !aux {
		request.Header.Set("User-Agent", s.Target.UserAgent)
		request.Header.Set("Referer", s.origin+"/")
		for k, values := range headers {
			for _, v := range values {
				request.Header.Add(k, v)
			}
		}
		for _, c := range s.jar.Cookies(request.URL) {
			request.AddCookie(c)
		}
	} else {
		// Auxiliary credentials are explicit headers supplied by the helper,
		// never copied from a tracker or another auxiliary response.
		for _, key := range []string{"Authorization", "Accept"} {
			if v := headers.Get(key); v != "" {
				request.Header.Set(key, v)
			}
		}
	}
	finish, err := s.Before(ctx, x)
	if err != nil || finish == nil {
		return zero, ErrScope
	}
	if s.Guard(ctx) != nil {
		_ = finish("denied", "")
		return zero, ErrScope
	}
	if x.Action {
		s.mu.Lock();s.action = true;s.mu.Unlock()
	}
	client := s.client
	if aux {
		client = s.aux
	}
	response, err := client.Do(request)
	if err != nil {
		s.mu.Lock();s.uncertain = true;s.mu.Unlock()
		_ = finish("uncertain", "")
		return zero, ErrUnknown
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		s.mu.Lock();s.uncertain = true;s.mu.Unlock()
		_ = finish("uncertain", "")
		return zero, ErrUnknown
	}
	state := "received"
	if response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 429 {
		state = "denied"
	}
	if err = finish(state, sitelogin.Hash([]any{response.StatusCode, raw})); err != nil {
		s.mu.Lock();s.uncertain = true;s.mu.Unlock()
		return zero, ErrUnknown
	}
	if !aux {
		s.jar.SetCookies(request.URL, response.Cookies())
	}
	if s.Guard(ctx) != nil {
		return zero, ErrScope
	}
	if state == "denied" {
		return zero, ErrScope
	}
	result := reply{raw, response.StatusCode, response.Header.Clone(), x.URL}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		// Only a same-origin GET observation follows an acknowledged redirect.
		// A 307/308 never resubmits a body. Every observation has a new receipt.
		if aux || response.StatusCode == 307 || response.StatusCode == 308 {
			return result, ErrResponse
		}
		next, err := request.URL.Parse(response.Header.Get("Location"))
		if err != nil || browserruntime.Origin(next.String()) != s.origin || next.String() == x.URL {
			return result, ErrScope
		}
		return s.get(ctx, next.String())
	}
	if response.StatusCode != 200 {
		return result, ErrResponse
	}
	if !aux && browserruntime.Challenge(raw) {
		return result, browserruntime.ErrVerification
	}
	return result, nil
}
func (r reply) text() string {
	reader, err := charset.NewReader(bytes.NewReader(r.body), r.header.Get("Content-Type"))
	if err != nil {
		return string(r.body)
	}
	raw, err := io.ReadAll(io.LimitReader(reader, (8<<20)+1))
	if err != nil || len(raw) > 8<<20 {
		return ""
	}
	return string(raw)
}
func (s *session) auxCall(ctx context.Context, kind, method, address string, body []byte, content string, headers http.Header) (reply, error) {
	return s.request(ctx, Exchange{Kind: kind, Method: method, URL: address, Body: body}, content, headers, true)
}
func expired(page string) bool {
	lower := strings.ToLower(page)
	return strings.Contains(lower, "type=\"password\"") || strings.Contains(lower, "type='password'") || strings.Contains(lower, "action=\"takelogin") || strings.Contains(lower, "action='takelogin")
}
func authenticated(page string) bool {
	if expired(page) {
		return false
	}
	for _,n:=range nodes(page,"a[href]"){value:=attr(n,"href");u,err:=url.Parse(value);if err!=nil{continue};path:=strings.ToLower(u.Path);if strings.HasSuffix(path,"logout.php")||strings.HasSuffix(path,"/logout")||strings.HasSuffix(path,"userdetails.php")&&u.Query().Get("id")!=""{return true}}
	visible:=pageText(page)
	for _,marker:=range []string{"退出登录","登出","魔力值"}{if strings.Contains(visible,marker){return true}}
	return false
}
