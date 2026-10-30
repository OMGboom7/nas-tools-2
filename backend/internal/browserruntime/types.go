// Package browserruntime renders original site pages without Python, WebDriver,
// shared user profiles, browser-owned HTTP or automatic business-action retries.
package browserruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var ErrUnavailable = errors.New("native browser is not configured or could not start")
var ErrScope = errors.New("original browser request scope changed or is not allowed")
var ErrResponse = errors.New("browser did not complete the original page")
var ErrVerification = errors.New("site requires interactive verification; no automatic action performed")
var ErrLimit = errors.New("browser page or request budget exceeded")

const MaxHTML = 4 << 20
const MaxNetwork = 32 << 20
const MaxRequests = 128

type Form struct{ Input, Submit, Script, Value, Referer string }
type Exchange struct {
	Ordinal           int
	URL, Method, Kind string
	Body              []byte
}
type Before func(context.Context, Exchange) (func(state, digest string) error, error)
type Request struct {
	URL, Cookie, UserAgent, Proxy string
	Form                          *Form
	Login                         *Login `json:"-"`
	// Selectors are from the original trusted catalog, never supplied by page text.
	ReadySelector, EmptySelector string
	ReadOnly                     bool
	// Assets permits ordinary uncredentialed HTTPS script/style GETs. It never
	// permits navigating away from the selected site or sending its cookies there.
	Assets    bool
	Guard     func(context.Context) error
	Before    Before
	Transport http.RoundTripper
}
type Result struct {
	HTML                   []byte
	URL, Cookie, UserAgent string
	Requests               int
	LoginState             string
}
type Renderer interface {
	Available() bool
	Render(context.Context, Request) (Result, error)
}
type Engine struct {
	binary string
	slots  chan struct{}
	mu     sync.Mutex
	active int
}

func New(binary string) *Engine {
	e := &Engine{slots: make(chan struct{}, 2)}
	if binary == "" {
		return e
	}
	if binary == "auto" {
		choices := []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"}
		if runtime.GOOS == "darwin" {
			choices = append(choices, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
		}
		for _, name := range choices {
			if p, err := exec.LookPath(name); err == nil {
				e.binary = p
				break
			}
		}
	} else if p, err := exec.LookPath(binary); err == nil {
		e.binary = p
	}
	return e
}
func (e *Engine) Available() bool {
	if e == nil || e.binary == "" {
		return false
	}
	info, err := os.Stat(e.binary)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}
func (e *Engine) Active() int { e.mu.Lock(); defer e.mu.Unlock(); return e.active }
func Hash(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func text(s string, max int) bool {
	return utf8.ValidString(s) && len(s) <= max && !strings.ContainsAny(s, "\x00\r\n")
}
func Origin(raw string) string {
	u, e := url.Parse(raw)
	if e != nil {
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	if !validURL(u) {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host
}
func validURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.Opaque == "" && u.Fragment == "" && len(u.String()) <= 16000 && !strings.ContainsAny(u.String(), "\x00\r\n")
}
func validate(r Request) error {
	u, err := url.Parse(r.URL)
	if err != nil || !validURL(u) || Origin(r.URL) == "" || !text(r.Cookie, 16000) || !text(r.UserAgent, 512) || r.UserAgent == "" || r.Guard == nil {
		return ErrScope
	}
	if r.Proxy != "" {
		u, e := url.Parse(r.Proxy)
		if e != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path != "" && u.Path != "/" || !text(r.Proxy, 4096) || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return ErrScope
		}
	}
	if len(r.ReadySelector) > 4096 || len(r.EmptySelector) > 4096 {
		return ErrLimit
	}
	if r.Form != nil {
		f := r.Form
		if r.ReadOnly || f.Input == "" || f.Submit == "" || !text(f.Input, 4096) || !text(f.Submit, 4096) || !text(f.Value, 2048) || len(f.Script) > 16384 || !utf8.ValidString(f.Script) || strings.ContainsRune(f.Script, 0) || f.Referer != "" && Origin(f.Referer) != Origin(r.URL) {
			return ErrScope
		}
	}
	if r.Login != nil && (r.Form != nil || r.ReadOnly || !r.Login.valid()) {
		return ErrScope
	}
	return nil
}
func Challenge(html []byte) bool {
	value := strings.ToLower(string(html))
	return strings.Contains(value, "cf-chl-") && strings.Contains(value, "challenge") ||
		strings.Contains(value, "challenges.cloudflare.com") && strings.Contains(value, "turnstile") &&
			(strings.Contains(value, "verify you are human") || strings.Contains(value, "verification required")) ||
		strings.Contains(value, "id=\"challenge-running\"")
}
func profileDelete(dir string, identity os.FileInfo) {
	info, e := os.Lstat(dir)
	if e == nil && info.IsDir() && os.SameFile(identity, info) && filepath.Base(dir) != "." {
		_ = os.RemoveAll(dir)
	}
}

// Session travels only in private search/candidate records. It cannot replace a
// changed site's configured credentials or be exposed in a public resource DTO.
type Session struct {
	SiteID                                       int64
	Site, Application, Origin, Cookie, UserAgent string
	Created                                      string
}

func (s Session) Valid() bool {
	at, e := time.Parse(time.RFC3339Nano, s.Created)
	_, siteErr := hex.DecodeString(s.Site)
	_, appErr := hex.DecodeString(s.Application)
	return s.SiteID >= 0 && len(s.Site) == 64 && len(s.Application) == 64 && siteErr == nil && appErr == nil && Origin(s.Origin) == s.Origin && text(s.Cookie, 16000) && text(s.UserAgent, 512) && s.UserAgent != "" && e == nil && time.Since(at) >= 0 && time.Since(at) < 30*time.Minute
}
