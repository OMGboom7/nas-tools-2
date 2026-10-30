package httpserver

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type siteLoginPrompt struct {
	nonce, kind, mime string
	image             []byte
	input             chan string
	consumed          bool
}
type siteLoginAPI struct {
	store       *sitelogin.Store
	sites       *siteconfig.Store
	config      *config.Store
	auth        *nativeAuthentication
	browser     browserruntime.Renderer
	transport   http.RoundTripper
	catalogPath string
	limits      *siteRequestLimiter
	guards      **organization.Store
	pureGo      bool
	mu          sync.Mutex
	running     map[string]bool
	cancels     map[string]context.CancelFunc
	prompts     map[string]*siteLoginPrompt
	lifecycle   context.Context
	closed      bool
	wait        sync.WaitGroup
}

func startSiteLoginWorker(ctx context.Context, api *siteLoginAPI) func() {
	if api == nil {
		return func() {}
	}
	api.mu.Lock()
	api.lifecycle = ctx
	api.mu.Unlock()
	return func() {
		api.mu.Lock()
		api.closed = true
		for _, cancel := range api.cancels {
			cancel()
		}
		api.mu.Unlock()
		api.wait.Wait()
	}
}
func (api *siteLoginAPI) acquire(ctx context.Context) (*organization.JobGuard, error) {
	if api.guards == nil || *api.guards == nil {
		return nil, errSiteLoginUnavailable
	}
	return (*api.guards).AcquireJob(ctx, "native-site-cookie-login")
}
func (api *siteLoginAPI) launch(ctx context.Context, actor, token string, p siteLoginPlan, credentials sitelogin.Credentials) ([]sitelogin.Record, error) {
	// Repeating a confirmed token only reads its original batch.
	if _, records, err := api.store.Batch(ctx, p.ID); err == nil {
		return records, nil
	}
	if api.browser == nil || !api.browser.Available() {
		return nil, errSiteLoginUnavailable
	}
	guard, err := api.acquire(ctx)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			guard.Close()
		}
	}()
	if err = api.store.Recover(ctx); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	records := []sitelogin.Record{}
	batch := sitelogin.Batch{ID: p.ID, Actor: actor, Created: p.Created, Updated: p.Created, Remember: p.Options.Remember}
	batch.RememberState = "not_requested"
	if p.Options.Remember {
		batch.RememberState = "pending"
	}
	for _, source := range p.Sources {
		site, err := api.sites.Get(ctx, source.SiteID)
		if err != nil || !sitelogin.SourceMatches(source, site) {
			return nil, sitelogin.ErrScope
		}
		id := sitelogin.Hash([]any{"site-login-v1", p.ID, source.SiteID})
		records = append(records, sitelogin.Record{ID: id, Batch: p.ID, Actor: actor, Name: site.Name, Host: browserruntime.Origin(source.LoginURL), State: "queued", Created: now, Updated: now, Source: source, Operations: []sitelogin.Operation{}})
		batch.Jobs = append(batch.Jobs, id)
	}
	base := api.baseGuard(p.Sources[0], actor, token)
	records, repeated, err := api.store.Create(ctx, batch, records, base)
	if err != nil {
		return nil, err
	}
	if repeated {
		return records, nil
	}
	// An in-memory key protects request/response fingerprints from becoming
	// offline password checks in the durable journal. It is never recovered.
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	api.mu.Lock()
	if api.closed {
		api.mu.Unlock()
		for _, r := range records {
			_ = api.store.Finish(ctx, r.ID, "stopped", "original runtime is stopping")
		}
		return nil, sitelogin.ErrState
	}
	parent := api.lifecycle
	if parent == nil {
		parent = context.Background()
	}
	if api.running == nil {
		api.running = map[string]bool{}
	}
	if api.cancels == nil {
		api.cancels = map[string]context.CancelFunc{}
	}
	if api.prompts == nil {
		api.prompts = map[string]*siteLoginPrompt{}
	}
	api.running[p.ID] = true
	api.wait.Add(1)
	transferred = true
	api.mu.Unlock()
	go func() {
		defer api.wait.Done()
		defer guard.Close()
		defer func() {
			api.mu.Lock()
			delete(api.running, p.ID)
			api.mu.Unlock()
			for i := range key {
				key[i] = 0
			}
		}()
		for _, r := range records {
			api.run(parent, r, actor, token, credentials, key)
		}
		// Saving is explicit and independent of transient one-time codes. Keep
		// existing CookieUserInfo fields, and never replace a concurrent edit.
		if p.Options.Remember {
			book, done := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
			defer done()
			complete := parent.Err() == nil
			for _, original := range records {
				saved, e := api.store.Get(book, original.ID)
				if e != nil || saved.State != "completed" {
					complete = false
					break
				}
			}
			if !complete {
				_ = api.store.RememberResult(book, p.ID, "blocked", "not all original logins completed; original saved credentials retained")
				return
			}
			if p.Options.UseSaved {
				_ = api.store.RememberResult(book, p.ID, "preserved", "original stored credentials retained")
				return
			}
			raw, err := api.store.Saved(book)
			if err != nil || sitelogin.Hash(raw) != p.RememberSnapshot {
				_ = api.store.RememberResult(book, p.ID, "blocked", "stored credentials changed; no replacement")
				return
			}
			if err = api.store.Remember(book, raw, credentials.Username, credentials.Password, base); err != nil {
				_ = api.store.RememberResult(book, p.ID, "uncertain", "credential saving was not confirmed; inspect original configuration")
				return
			}
			_ = api.store.RememberResult(book, p.ID, "saved", "username and password saved; one-time input not persisted")
		}
	}()
	return records, nil
}
func (api *siteLoginAPI) run(parent context.Context, r sitelogin.Record, actor, token string, credentials sitelogin.Credentials, key []byte) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Minute)
	api.mu.Lock()
	api.cancels[r.ID] = cancel
	api.mu.Unlock()
	defer func() { cancel(); api.mu.Lock(); delete(api.cancels, r.ID); delete(api.prompts, r.ID); api.mu.Unlock() }()
	current, err := api.store.Get(ctx, r.ID)
	if err != nil || current.State != "queued" {
		return
	}
	base := api.baseGuard(r.Source, actor, token)
	finish := func(state, reason string) {
		book, done := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer done()
		_ = api.store.Finish(book, r.ID, state, reason)
	}
	if err = api.store.Start(ctx, r.ID, base); err != nil {
		finish("blocked", "original site or login authority changed before execution")
		return
	}
	if err = api.current(ctx, r, actor, token); err != nil {
		finish("blocked", "original site login source is no longer current")
		return
	}
	site, err := api.sites.Get(ctx, r.Source.SiteID)
	if err != nil {
		finish("blocked", "original site unavailable")
		return
	}
	cfg, err := api.config.Snapshot()
	if err != nil {
		finish("blocked", "original application configuration unavailable")
		return
	}
	var note map[string]any
	if site.Note != "" && site.Note != "null" {
		_ = json.Unmarshal([]byte(site.Note), &note)
	}
	transport := api.transport
	if note["proxy"] == "Y" || truthy(note["proxy"]) {
		transport, err = siteProxyTransport(transport, objectValue(objectValue(cfg["app"])["proxies"]), schemeOfLogin(r.Source.LoginURL))
		if err != nil {
			finish("blocked", "original site proxy configuration unavailable")
			return
		}
	}
	sequence := 0
	var sequenceMu sync.Mutex
	signature := func(parts ...any) string {
		sequenceMu.Lock()
		defer sequenceMu.Unlock()
		sequence++
		raw, _ := json.Marshal([]any{r.ID, sequence, parts})
		mac := hmac.New(sha256.New, key)
		mac.Write(raw)
		return hex.EncodeToString(mac.Sum(nil))
	}
	receipt := func(kind, signature string) (func(string, string) error, error) {
		index, err := api.store.Step(ctx, r.ID, sitelogin.Hash(r.Source), kind, signature, base)
		if err != nil {
			return nil, err
		}
		return func(state, digest string) error {
			if digest != "" {
				mac := hmac.New(sha256.New, key)
				mac.Write([]byte(digest))
				digest = hex.EncodeToString(mac.Sum(nil))
			}
			book, done := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
			defer done()
			return api.store.FinishStep(book, r.ID, sitelogin.Hash(r.Source), index, state, digest)
		}, nil
	}
	full := func(ctx context.Context) error { return api.current(ctx, r, actor, token) }
	before := func(ctx context.Context, x browserruntime.Exchange) (func(string, string) error, error) {
		if err := full(ctx); err != nil {
			return nil, err
		}
		if err := api.limits.Wait(ctx, api.sites, r.Source.SiteID); err != nil {
			return nil, err
		}
		return receipt("network:"+x.Method+":"+x.Kind, signature(x.Ordinal, x.Method, x.URL, x.Kind, x.Body))
	}
	login := &browserruntime.Login{Username: credentials.Username, Password: credentials.Password, TwoStep: credentials.TwoStep, LoggedIn: siteHTMLLoggedIn,
		Action: func(ctx context.Context, kind string) (func(string, string) error, error) {
			if err := full(ctx); err != nil {
				return nil, err
			}
			return receipt("action:"+kind, signature(kind))
		},
		Interact: func(ctx context.Context, c browserruntime.LoginChallenge) (string, error) {
			if err := full(ctx); err != nil {
				return "", err
			}
			if r.Source.OCR && c.Kind == "captcha" {
				ocr, err := sitelogin.OCRConfig(cfg)
				if err != nil {
					return "", err
				}
				return ocr.Read(ctx, c.Image, api.transport, sitelogin.Guard(full), func(ctx context.Context, kind, method, address string, payload []byte) (func(string, string) error, error) {
					if err := full(ctx); err != nil {
						return nil, err
					}
					return receipt(kind, signature(method, address, payload))
				})
			}
			return api.interact(ctx, r, actor, token, c)
		},
	}
	result, err := api.browser.Render(ctx, browserruntime.Request{URL: r.Source.LoginURL, Cookie: site.Cookie, UserAgent: r.Source.UserAgent, Login: login, ReadOnly: false, Assets: true, Transport: transport, Guard: full, Before: before})
	if err != nil {
		state, reason := "failed", "original site did not confirm login; saved session retained"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, browserruntime.ErrResponse) {
			state, reason = "uncertain", "original login was interrupted or response was not confirmed; no automatic retry"
		}
		if errors.Is(err, browserruntime.ErrVerification) {
			state, reason = "blocked", "site requires interactive browser verification; no automatic verification action"
		}
		if errors.Is(err, sitelogin.ErrScope) || errors.Is(err, browserruntime.ErrScope) {
			state, reason = "blocked", "original login source or authority changed; no session saved"
		}
		finish(state, reason)
		return
	}
	if err = api.current(ctx, r, actor, token); err != nil {
		finish("blocked", "original source changed before session publication")
		return
	}
	if result.LoginState == "" || !siteHTMLLoggedIn(result.HTML) {
		finish("failed", "original site did not confirm a logged-in page")
		return
	}
	if err = api.store.SaveCookie(ctx, r.ID, result.Cookie, result.UserAgent, result.LoginState, base); err != nil {
		finish("uncertain", "site login completed but original session publication was not confirmed")
		return
	}
}
func schemeOfLogin(address string) string {
	if len(address) >= 8 && address[:8] == "https://" {
		return "https"
	}
	return "http"
}
func (api *siteLoginAPI) interact(ctx context.Context, r sitelogin.Record, actor, token string, c browserruntime.LoginChallenge) (string, error) {
	nonce := sitelogin.Random()
	if nonce == "" {
		return "", sitelogin.ErrInput
	}
	deadline := time.Now().Add(60 * time.Second)
	prompt := &siteLoginPrompt{nonce: nonce, kind: c.Kind, mime: c.MIME, image: append([]byte(nil), c.Image...), input: make(chan string, 1)}
	api.mu.Lock()
	api.prompts[r.ID] = prompt
	api.mu.Unlock()
	defer func() {
		api.mu.Lock()
		if api.prompts[r.ID] == prompt {
			delete(api.prompts, r.ID)
		}
		api.mu.Unlock()
	}()
	if err := api.store.Prompt(ctx, r.ID, c.Kind, nonce, deadline, api.baseGuard(r.Source, actor, token)); err != nil {
		return "", err
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return "", sitelogin.ErrInput
		case value := <-prompt.input:
			if err := api.current(ctx, r, actor, token); err != nil {
				return "", err
			}
			return value, nil
		case <-ticker.C:
			if err := api.current(ctx, r, actor, token); err != nil {
				return "", err
			}
		}
	}
}
