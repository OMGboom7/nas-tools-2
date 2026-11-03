package httpserver

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

const signinGuard = "native-site-signin"

type siteSigninPrompt struct {
	nonce     string
	challenge sitesignin.Challenge
	input     chan []string
	consumed  bool
}
type siteSigninAPI struct {
	store      *sitesignin.Store
	login      *siteLoginAPI
	system     *systemconfig.Store
	channels   *notificationconfig.Store
	cache      *sitesignin.FileCache
	mu         sync.Mutex
	running    map[string]context.CancelFunc
	cancels    map[string]context.CancelFunc
	prompts    map[string]*siteSigninPrompt
	sockets    map[string]context.CancelFunc
	socketDial func(context.Context, string) (net.Conn, *bufio.Reader, error)
	lifecycle  context.Context
	closed     bool
	cloudflare *cloudflareAPI
	statistics *siteStatisticsAPI
	wait       sync.WaitGroup
}

func (api *siteSigninAPI) acquire(ctx context.Context) (*organization.JobGuard, error) {
	if api.login == nil || api.login.guards == nil || *api.login.guards == nil {
		return nil, sitesignin.ErrState
	}
	return (*api.login.guards).AcquireJob(ctx, signinGuard)
}
func (api *siteSigninAPI) launch(ctx context.Context, b sitesignin.Batch, sources []sitesignin.Source, token string, held *organization.JobGuard, synchronous bool) ([]sitesignin.Job, error) {
	if _, rows, e := api.store.Batch(ctx, b.ID); e == nil {
		return rows, nil
	}
	guard := held
	var e error
	if guard == nil {
		guard, e = api.acquire(ctx)
		if e != nil {
			return nil, e
		}
	}
	ownGuard := held == nil
	transferred := false
	defer func() {
		if !transferred && ownGuard {
			guard.Close()
		}
	}()
	if e = api.store.Recover(ctx); e != nil {
		return nil, e
	}
	location, e := time.LoadLocation(b.Scope.Location)
	if e != nil {
		return nil, e
	}
	stamp := time.Now()
	day := stamp.In(location).Format("2006-01-02")
	created := stamp.UTC().Format(time.RFC3339Nano)
	b.Created = created
	b.Jobs = []string{}
	records := []sitesignin.Job{}
	for _, source := range sources {
		id := sitelogin.Hash([]any{"native-site-signin-v1", b.ID, source.SiteID})
		b.Jobs = append(b.Jobs, id)
		records = append(records, sitesignin.Job{ID: id, Batch: b.ID, Actor: b.Scope.Actor, ScopeID: b.Scope.ID, Day: day, Created: created, Updated: created, Source: source, State: "queued", Operations: []sitesignin.Operation{}})
	}
	rows, old, e := api.store.Create(ctx, b, records, api.base(b.Scope, token, b.Scope.PluginDigest))
	if e != nil {
		return nil, e
	}
	if old {
		return rows, nil
	}
	api.mu.Lock()
	if api.closed {
		api.mu.Unlock()
		for _, r := range rows {
			_ = api.store.Finish(ctx, r.ID, sitesignin.Outcome{State: "stopped", Reason: "原执行进程正在停止"})
		}
		return nil, sitesignin.ErrState
	}
	parent := api.lifecycle
	if parent == nil {
		parent = context.Background()
	}
	parent, cancel := context.WithCancel(parent)
	if api.running == nil {
		api.running = map[string]context.CancelFunc{}
	}
	if api.cancels == nil {
		api.cancels = map[string]context.CancelFunc{}
	}
	if api.prompts == nil {
		api.prompts = map[string]*siteSigninPrompt{}
	}
	api.running[b.ID] = cancel
	api.wait.Add(1)
	api.mu.Unlock()
	transferred = true
	run := func() {
		defer api.wait.Done()
		if ownGuard {
			defer guard.Close()
		}
		defer cancel()
		defer func() { api.mu.Lock(); delete(api.running, b.ID); api.mu.Unlock() }()
		concurrency := b.Scope.Settings.Concurrency
		if concurrency < 1 {
			concurrency = 10
		}
		if concurrency > 32 {
			concurrency = 32
		}
		if concurrency > len(rows) {
			concurrency = len(rows)
		}
		queue := make(chan sitesignin.Job)
		var group sync.WaitGroup
		for i := 0; i < concurrency; i++ {
			group.Add(1)
			go func() {
				defer group.Done()
				for r := range queue {
					api.run(parent, b, r, token)
				}
			}()
		}
		for _, r := range rows {
			queue <- r
		}
		close(queue)
		group.Wait()
		book, done := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer done()
		// Scheduled batches are completed after their durable next occurrence is
		// advanced. Manual batches complete here while still holding the guard.
		if !synchronous {
			if e = api.complete(book, b.ID); e != nil {
				slog.Warn("native site sign-in summary remains pending; provider actions will not be replayed")
			}
		}
	}
	if synchronous {
		run()
	} else {
		go run()
	}
	return rows, nil
}

func (api *siteSigninAPI) complete(ctx context.Context, id string) error {
	b, saved, e := api.store.Batch(ctx, id)
	if e != nil || b.Notified {
		return e
	}
	var summary strings.Builder
	summary.WriteString("批次 " + b.ID + "\n")
	failures, retries := 0, 0
	for _, r := range saved {
		if sitesignin.Active(r.State) {
			return sitesignin.ErrState
		}
		fmt.Fprintf(&summary, "【%s】%s：%s\n", r.Source.Name, r.Outcome.State, r.Outcome.Reason)
		if r.Outcome.State == "failed" && r.Outcome.Retryable {
			failures++
		}
		out := r.Outcome
		out.Reason = "【" + r.Source.Name + "】" + out.Reason
		if b.Scope.Settings.Retry(out) {
			retries++
		}
	}
	if b.Scope.Settings.AutoCF > 0 && failures >= b.Scope.Settings.AutoCF {
		original, e := api.system.ReadMany(ctx, []string{"plugin.CustomHosts", "plugin.CloudflareSpeedTest"})
		if e != nil {
			return e
		}
		if truthy(objectSigninRaw(original["plugin.CustomHosts"])["enable"]) && configuredPluginValue(objectSigninRaw(original["plugin.CloudflareSpeedTest"])["cf_ip"]) {
			state, reason := "blocked", "CF 优选尚未接管或未允许签到触发"
			if api.cloudflare != nil && api.cloudflare.store != nil {
				control, err := api.cloudflare.store.Control(ctx)
				if err == nil && control.FromSignin && api.cloudflare.validate(ctx, control) == nil {
					job, err := api.cloudflare.store.Queue(ctx, "signin:"+b.ID, true)
					if err == nil {
						state, reason = job.State, "原签到批次关联优选任务："+job.ID
					}
				}
			}
			if e = api.store.Dependency(ctx, b.ID, "CloudflareSpeedTest", state, reason); e != nil {
				return e
			}
		}
	}
	nextTime := "未安排"
	nextCount := 0
	control, e := api.store.Control(ctx)
	if e != nil {
		return e
	}
	if control.Enabled && sitelogin.Hash(control.Scope) == sitelogin.Hash(b.Scope) {
		var next time.Time
		for _, raw := range []string{control.NextOnce, control.NextRun, control.NextMissed} {
			if raw == "" {
				continue
			}
			at, e := time.Parse(time.RFC3339Nano, raw)
			if e != nil {
				return e
			}
			if next.IsZero() || at.Before(next) {
				next = at
			}
		}
		if !next.IsZero() {
			location, e := time.LoadLocation(b.Scope.Location)
			if e != nil {
				return e
			}
			nextTime = next.In(location).Format("2006-01-02 15:04:05 MST")
			pending, e := api.selected(ctx, b.Scope, false)
			if e != nil {
				return e
			}
			nextCount = len(pending)
		} else if b.Scope.Options.Remote {
			nextTime = "等待原渠道命令"
		}
	}
	plugin := fmt.Sprintf("本次签到数量: %d\n命中重试数量: %d\n强制签到数量: %d\n下次签到数量: %d\n下次签到时间: %s\n详见签到消息与本批次记录", len(saved), retries, len(b.Scope.Settings.Special), nextCount, nextTime)
	return api.store.Notify(ctx, b.ID, boundedSigninSummary(summary.String()), plugin)
}

func (api *siteSigninAPI) reconcileSummaries(ctx context.Context) error {
	ids, e := api.store.PendingSummaries(ctx)
	if e != nil {
		return e
	}
	for _, id := range ids {
		if e = api.complete(ctx, id); e != nil {
			return e
		}
	}
	return nil
}
func boundedSigninSummary(raw string) string {
	if len(raw) <= 10000 {
		return raw
	}
	raw = raw[:9900]
	for !utf8.ValidString(raw) {
		raw = raw[:len(raw)-1]
	}
	return raw + "\n其余结果请按原批次查看"
}
func (api *siteSigninAPI) run(parent context.Context, b sitesignin.Batch, r sitesignin.Job, token string) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	api.mu.Lock()
	api.cancels[r.ID] = cancel
	api.mu.Unlock()
	defer func() { cancel(); api.mu.Lock(); delete(api.cancels, r.ID); delete(api.prompts, r.ID); api.mu.Unlock() }()
	finish := func(outcome sitesignin.Outcome) {
		book, done := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer done()
		if e := api.store.Finish(book, r.ID, outcome); e != nil && !errors.Is(e, sitesignin.ErrState) {
			slog.Warn("native site sign-in receipt requires inspection")
		}
	}
	base := api.base(b.Scope, token, b.Scope.PluginDigest)
	if e := api.store.Start(ctx, r.ID, base); e != nil {
		finish(sitesignin.Outcome{State: "blocked", Reason: "原站点、配置或执行权限已变化"})
		return
	}
	full := func(ctx context.Context) error { return api.current(ctx, b, r, token) }
	if e := full(ctx); e != nil {
		finish(sitesignin.Outcome{State: "blocked", Reason: "原签到范围已变化"})
		return
	}
	site, e := api.login.sites.Get(ctx, r.Source.SiteID)
	if e != nil {
		finish(sitesignin.Outcome{State: "blocked", Reason: "原站点不可用"})
		return
	}
	cfg, e := api.login.config.Snapshot()
	if e != nil {
		finish(sitesignin.Outcome{State: "blocked", Reason: "原配置不可用"})
		return
	}
	transport := api.login.transport
	var note map[string]any
	_ = json.Unmarshal([]byte(site.Note), &note)
	if note["proxy"] == "Y" || truthy(note["proxy"]) {
		transport, e = siteProxyTransport(transport, objectValue(objectValue(cfg["app"])["proxies"]), schemeOfLogin(r.Source.URL))
		if e != nil {
			finish(sitesignin.Outcome{State: "blocked", Reason: "原站点代理不可用"})
			return
		}
	}
	key := make([]byte, 32)
	if _, e = rand.Read(key); e != nil {
		finish(sitesignin.Outcome{State: "blocked", Reason: "无法生成本次请求密钥"})
		return
	}
	defer func() {
		for i := range key {
			key[i] = 0
		}
	}()
	var sequenceMu sync.Mutex
	sequence := 0
	mac := func(value any) string {
		raw, _ := json.Marshal(value)
		h := hmac.New(sha256.New, key)
		h.Write(raw)
		return hex.EncodeToString(h.Sum(nil))
	}
	before := func(ctx context.Context, x sitesignin.Exchange) (func(string, string) error, error) {
		if e := full(ctx); e != nil {
			return nil, e
		}
		if browserruntime.Origin(x.URL) == browserruntime.Origin(r.Source.URL) {
			if e := api.login.limits.Wait(ctx, api.login.sites, r.Source.SiteID); e != nil {
				return nil, e
			}
		}
		sequenceMu.Lock()
		sequence++
		ordinal := sequence
		sequenceMu.Unlock()
		action := x.Action || strings.HasPrefix(x.Kind, "browser:") && x.Method == "POST"
		parts := []any{x.Kind, x.Method, x.URL, x.Body}
		if !action {
			parts = append(parts, ordinal)
		}
		index, e := api.store.Step(ctx, r.ID, sitesignin.Operation{Kind: x.Kind + ":" + x.Method, Signature: mac(parts), State: "intent", Action: action}, base)
		if e != nil {
			return nil, e
		}
		return func(state, digest string) error {
			book, done := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
			defer done()
			if digest != "" {
				digest = mac(digest)
			}
			return api.store.Acknowledge(book, r.ID, index, state, digest)
		}, nil
	}
	input := func(ctx context.Context, c sitesignin.Challenge) ([]string, error) {
		return api.interact(ctx, b, r, token, c)
	}
	var login *browserruntime.Login
	if b.Scope.Options.SavedLogin && sitesignin.Adapter(r.Source.URL) == "mteam" {
		raw, e := api.login.store.Saved(ctx)
		if e != nil {
			finish(sitesignin.Outcome{State: "blocked", Reason: "原保存账户不可用"})
			return
		}
		credentials, e := sitelogin.ParseCredentials(raw)
		if e != nil {
			finish(sitesignin.Outcome{State: "blocked", Reason: "原保存账户无效"})
			return
		}
		login = &browserruntime.Login{Username: credentials.Username, Password: credentials.Password, TwoStep: credentials.TwoStep, LoggedIn: siteHTMLLoggedIn, Action: func(ctx context.Context, kind string) (func(string, string) error, error) {
			return before(ctx, sitesignin.Exchange{Kind: "action:" + kind, Method: "DOM", URL: r.Source.URL, Action: true})
		}, Interact: func(ctx context.Context, c browserruntime.LoginChallenge) (string, error) {
			if c.Kind == "captcha" && b.Scope.Options.OCR {
				ocr, e := sitelogin.OCRConfig(cfg)
				if e != nil {
					return "", e
				}
				return ocr.Read(ctx, c.Image, api.login.transport, sitelogin.Guard(full), func(ctx context.Context, kind, method, address string, payload []byte) (func(string, string) error, error) {
					return before(ctx, sitesignin.Exchange{Kind: kind, Method: method, URL: address, Body: payload})
				})
			}
			values, e := input(ctx, sitesignin.Challenge{Kind: c.Kind, MIME: c.MIME, Image: c.Image})
			if e != nil || len(values) != 1 {
				return "", sitesignin.ErrInput
			}
			return values[0], nil
		}}
	}
	location, _ := time.LoadLocation(b.Scope.Location)
	aux := signinAuxTransport{base: api.login.transport, configuration: cfg}
	result := (sitesignin.Executor{Target: sitesignin.Target{ID: site.ID, Name: site.Name, URL: r.Source.URL, Cookie: site.Cookie, UserAgent: r.Source.UserAgent, Browser: r.Source.Browser}, Options: b.Scope.Options, Configuration: cfg, Transport: transport, AuxTransport: aux, Browser: api.login.browser, Guard: full, Before: before, Input: input, Cache: api.cache, Login: login, Location: location}).Run(ctx)
	finish(result)
}

type signinAuxTransport struct {
	base          http.RoundTripper
	configuration map[string]any
}

func (t signinAuxTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	transport := t.base
	if transport == nil {
		transport = http.DefaultTransport
	}
	ai, e := sitesignin.QuestionAI(t.configuration)
	// The original built-in OpenAI endpoint and Google Lens used app proxies.
	// A custom OpenAI base and ordinary OCR/Douban retain their original route.
	if e == nil && ai.URL == r.URL.String() && ai.URL == "https://api.openai.com/v1/chat/completions" || r.URL.Hostname() == "lens.google.com" {
		var err error
		transport, err = siteProxyTransport(transport, objectValue(objectValue(t.configuration["app"])["proxies"]), r.URL.Scheme)
		if err != nil {
			return nil, err
		}
	}
	return transport.RoundTrip(r)
}
func (api *siteSigninAPI) interact(ctx context.Context, b sitesignin.Batch, r sitesignin.Job, token string, c sitesignin.Challenge) ([]string, error) {
	if len(c.Image) > 2<<20 || len(c.Choices) > 64 || len(c.Question) > 12000 {
		return nil, sitesignin.ErrInput
	}
	nonce := sitelogin.Random()
	deadline := time.Now().Add(90 * time.Second)
	p := &siteSigninPrompt{nonce: nonce, challenge: c, input: make(chan []string, 1)}
	api.mu.Lock()
	api.prompts[r.ID] = p
	api.mu.Unlock()
	defer func() {
		api.mu.Lock()
		if api.prompts[r.ID] == p {
			delete(api.prompts, r.ID)
		}
		api.mu.Unlock()
	}()
	if e := api.store.Prompt(ctx, r.ID, c.Kind, nonce, deadline, api.base(b.Scope, token, b.Scope.PluginDigest)); e != nil {
		return nil, e
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, sitesignin.ErrInput
		case values := <-p.input:
			if e := api.current(ctx, b, r, token); e != nil {
				return nil, e
			}
			return values, nil
		case <-ticker.C:
			if e := api.current(ctx, b, r, token); e != nil {
				return nil, e
			}
		}
	}
}
func startSiteSigninWorker(ctx context.Context, api *siteSigninAPI) func() {
	if api == nil || api.store == nil || api.login == nil || !api.login.pureGo {
		return func() {}
	}
	api.mu.Lock()
	api.lifecycle = ctx
	api.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if e := api.tick(ctx, now); e != nil && !errors.Is(e, organization.ErrBusy) && !errors.Is(e, context.Canceled) {
					slog.Warn("native site sign-in worker stopped this occurrence; original receipts retained")
				}
			}
		}
	}()
	return func() {
		api.mu.Lock()
		api.closed = true
		for _, cancel := range api.running {
			cancel()
		}
		for _, cancel := range api.sockets {
			cancel()
		}
		api.mu.Unlock()
		<-done
		api.wait.Wait()
	}
}
