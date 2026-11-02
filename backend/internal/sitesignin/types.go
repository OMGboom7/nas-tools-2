// Package sitesignin executes the original AutoSignIn protocols. Callers own
// durable authorization and receipts; credentials, HTML and images stay in memory.
package sitesignin

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

var (
	ErrScope    = errors.New("original sign-in source changed or receiver is not allowed")
	ErrUnknown  = errors.New("sign-in response is not confirmed; do not replay")
	ErrResponse = errors.New("site did not return the expected sign-in protocol")
	ErrInput    = errors.New("sign-in requires current user input")
)

const MaxSites = 1000

type Outcome struct {
	State         string `json:"state"`
	Reason        string `json:"reason"`
	Authenticated bool   `json:"authenticated"`
	Retryable     bool   `json:"retryable"`
	CacheState    string `json:"cacheState,omitempty"`
}

type Target struct {
	ID                           int64
	Name, URL, Cookie, UserAgent string
	Browser                      bool
}

type Receiver struct {
	Kind   string `json:"kind"`
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Path   string `json:"path"`
	Data   string `json:"data"`
}

// Options are reviewed independently of a site's credentials. External helpers
// never receive the site's Cookie, API key or login form credentials.
type Options struct {
	OCR        bool `json:"ocr"`
	Douban     bool `json:"douban"`
	Lens       bool `json:"lens"`
	AI         bool `json:"ai"`
	SavedLogin bool `json:"savedLogin"`
	Remote     bool `json:"remote"`
}

type Exchange struct {
	Kind, Method, URL string
	Body              []byte
	Action            bool
}
type Before func(context.Context, Exchange) (func(state, digest string) error, error)
type Challenge struct {
	Kind, MIME, Question string
	Image                []byte
	Choices              []Choice
	Multiple             bool
}
type Choice struct {
	Value string `json:"value"`
	Text  string `json:"text"`
}
type Input func(context.Context, Challenge) ([]string, error)

// Cache snapshots are opaque CAS tokens. Updates must retain unknown entries and
// the exact old cache when evidence, identity or authorization changed.
type Cache interface {
	Read(context.Context, string) (map[string][]string, string, error)
	Put(context.Context, string, string, string, []string, func(context.Context) error) error
}

type Executor struct {
	Target        Target
	Options       Options
	Configuration map[string]any
	Transport     http.RoundTripper
	AuxTransport  http.RoundTripper
	Browser       browserruntime.Renderer
	Guard         func(context.Context) error
	Before        Before
	Input         Input
	Cache         Cache
	Login         *browserruntime.Login
	Now           func() time.Time
	Location      *time.Location
}

var adapters = map[string]string{
	"carpt.net": "carpt", "hdfans.org": "hdfans", "hdtime.org": "hdtime",
	"hdcity.city": "hdcity", "haidan.video": "haidan", "hhanclub.top": "hhanclub",
	"hdarea.club": "hdarea", "club.hares.top": "hares", "pterclub.com": "pterclub",
	"hdchina.org": "hdchina", "pt.hdupt.com": "hdupt", "totheglory.im": "ttg",
	"zhuque.in": "zhuque", "pt.btschool.club": "btschool", "hdsky.me": "hdsky",
	"open.cd": "opencd", "u2.dmhy.org": "u2", "tjupt.org": "tjupt",
	"52pt.site": "52pt", "ptchdbits.co": "chdbits",
}

func Adapter(address string) string {
	u, err := url.Parse(address)
	if err != nil || browserruntime.Origin(address) == "" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if a := adapters[host]; a != "" {
		return a
	}
	// Original M-Team domains, with a label boundary rather than substring match.
	if host == "m-team.cc" || strings.HasSuffix(host, ".m-team.cc") || host == "m-team.io" || strings.HasSuffix(host, ".m-team.io") {
		return "mteam"
	}
	return "generic"
}

func signed(reason string) Outcome {
	return Outcome{State: "signed", Reason: reason, Authenticated: true}
}
func already() Outcome {
	return Outcome{State: "already", Reason: "今日已签到", Authenticated: true}
}
func failed(reason string, retry bool) Outcome {
	return Outcome{State: "failed", Reason: reason, Retryable: retry}
}
func unknown() Outcome {
	return Outcome{State: "unknown", Reason: "未获得明确签到结果，请核对站点；不会自动重试"}
}

func (e Executor) Run(ctx context.Context) Outcome {
	if e.Guard == nil || e.Before == nil || e.Guard(ctx) != nil || e.Target.ID <= 0 || e.Target.UserAgent == "" || !sitelogin.Text(e.Target.UserAgent, 512) || !sitelogin.Text(e.Target.Cookie, 16000) || Adapter(e.Target.URL) == "" {
		return Outcome{State: "blocked", Reason: "原站点或执行范围不可用"}
	}
	s, err := newSession(e)
	if err != nil {
		return Outcome{State: "blocked", Reason: "原站点客户端不可用"}
	}
	result, err := s.execute(ctx)
	if err == nil {
		return result
	}
	if errors.Is(err, ErrUnknown) {
		return unknown()
	}
	if errors.Is(err, ErrScope) || errors.Is(err, browserruntime.ErrScope) {
		return Outcome{State: "blocked", Reason: "原站点、辅助接收方或执行权限已变化"}
	}
	if errors.Is(err, browserruntime.ErrVerification) {
		return Outcome{State: "blocked", Reason: "站点需要人工浏览器验证"}
	}
	if errors.Is(err, ErrInput) || errors.Is(err, browserruntime.ErrInput) {
		return Outcome{State: "blocked", Reason: "未收到有效的单次输入"}
	}
	s.mu.Lock()
	uncertain, action := s.uncertain, s.action
	s.mu.Unlock()
	if uncertain || action {
		return unknown()
	}
	return failed("站点未返回预期签到页面或协议", !action)
}
