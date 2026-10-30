package browserruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
	"golang.org/x/net/publicsuffix"
)

func (e *Engine) Render(parent context.Context, r Request) (Result, error) {
	out := Result{}
	if err := validate(r); err != nil {
		return out, err
	}
	if !e.Available() {
		return out, ErrUnavailable
	}
	select {
	case e.slots <- struct{}{}:
	case <-parent.Done():
		return out, parent.Err()
	}
	defer func() { <-e.slots }()
	if err := r.Guard(parent); err != nil {
		if parent.Err() != nil {
			return out, parent.Err()
		}
		return out, ErrScope
	}
	duration := 60 * time.Second
	if r.Login != nil {
		duration = 4 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	b, err := newBroker(r)
	if err != nil {
		return out, err
	}
	dir, err := os.MkdirTemp("", "nastool-browser-")
	if err != nil {
		return out, ErrUnavailable
	}
	identity, err := os.Lstat(dir)
	if err != nil {
		return out, ErrUnavailable
	}
	defer profileDelete(dir, identity)
	// All page HTTP is fulfilled by the Go broker. The dead loopback proxy also
	// prevents an unhandled popup/worker/background request from opening its own
	// upstream connection; no system proxy, profile or trust setting is changed.
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(e.binary), chromedp.UserDataDir(dir), chromedp.Headless, chromedp.NoFirstRun, chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("no-sandbox", false), chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-component-update", true), chromedp.Flag("disable-sync", true), chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-default-apps", true), chromedp.Flag("disable-quic", true), chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("dns-prefetch-disable", true), chromedp.Flag("remote-debugging-address", "127.0.0.1"),
		chromedp.Flag("proxy-server", "http://127.0.0.1:1"), chromedp.Flag("proxy-bypass-list", "<-loopback>"),
		chromedp.Flag("force-webrtc-ip-handling-policy", "disable_non_proxied_udp"),
	}
	alloc, stop := chromedp.NewExecAllocator(ctx, opts...)
	defer stop()
	tab, closeTab := chromedp.NewContext(alloc, chromedp.WithLogf(func(string, ...any) {}), chromedp.WithErrorf(func(string, ...any) {}))
	defer closeTab()
	e.mu.Lock()
	e.active++
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.active--; e.mu.Unlock() }()
	var wg sync.WaitGroup
	var events sync.Mutex
	closing := false
	finishTab := func() { events.Lock(); closing = true; events.Unlock(); closeTab(); wg.Wait() }
	// Callbacks cannot call CDP synchronously from the target's event reader.
	chromedp.ListenTarget(tab, func(event any) {
		paused, ok := event.(*fetch.EventRequestPaused)
		if !ok {
			return
		}
		events.Lock()
		defer events.Unlock()
		if closing {
			return
		}
		wg.Add(1)
		go func() { defer wg.Done(); b.fulfill(tab, paused) }()
	})
	seed, _ := url.Parse(r.URL)
	h := http.Header{}
	h.Set("Cookie", r.Cookie)
	cookies := []*network.CookieParam{}
	for _, c := range (&http.Request{Header: h}).Cookies() {
		cookies = append(cookies, &network.CookieParam{Name: c.Name, Value: c.Value, URL: seed.Scheme + "://" + seed.Host + "/", Path: "/", Secure: seed.Scheme == "https"})
	}
	extra := network.Headers{}
	if r.Form != nil && r.Form.Referer != "" {
		extra["Referer"] = r.Form.Referer
	}
	agentAction := chromedp.ActionFunc(func(ctx context.Context) error {
		if r.Login != nil && r.UserAgent == "auto" {
			_, _, _, agent, _, e := browser.GetVersion().Do(ctx)
			if e != nil || !text(agent, 512) || agent == "" {
				return ErrUnavailable
			}
			r.UserAgent = agent
			b.mu.Lock()
			b.request.UserAgent = agent
			b.mu.Unlock()
		}
		return emulation.SetUserAgentOverride(r.UserAgent).Do(ctx)
	})
	err = chromedp.Run(tab, network.Enable(), network.SetCacheDisabled(true), network.SetBypassServiceWorker(true), network.SetExtraHTTPHeaders(extra),
		network.SetBlockedURLs([]string{"file://*", "ftp://*", "ws://*", "wss://*"}),
		agentAction, network.SetCookies(cookies),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "http://*", RequestStage: fetch.RequestStageRequest}, {URLPattern: "https://*", RequestStage: fetch.RequestStageRequest}}),
		chromedp.Navigate(r.URL))
	if err != nil {
		finishTab()
		if parent.Err() != nil {
			return out, parent.Err()
		}
		if _, _, cause := b.state(); cause != nil {
			return out, cause
		}
		return out, ErrUnavailable
	}
	loginState := ""
	if r.Login != nil {
		loginState, err = b.login(tab, r.Login)
		if err != nil {
			finishTab()
			return out, err
		}
	}
	if r.Form != nil {
		if r.Form.Script != "" {
			// Only a previously validated catalog script reaches here. It has the
			// page's browser context, never a Go/native filesystem execution primitive.
			if err = chromedp.Run(tab, chromedp.Evaluate("(function(){"+r.Form.Script+"\n}).call(window)", nil)); err != nil {
				finishTab()
				return out, ErrResponse
			}
		}
		encoded, _ := json.Marshal(r.Form)
		var submitted bool
		readyExpression := `(()=>{const p=` + string(encoded) + `;const get=x=>document.evaluate(x,document,null,XPathResult.FIRST_ORDERED_NODE_TYPE,null).singleNodeValue;const a=get(p.Input),b=get(p.Submit);return !!a&&!!b&&!b.disabled;})()`
		if err = chromedp.Run(tab, chromedp.Poll(readyExpression, nil, chromedp.WithPollingInterval(150*time.Millisecond), chromedp.WithPollingTimeout(10*time.Second))); err != nil {
			finishTab()
			return out, ErrResponse
		}
		expression := `(()=>{const p=` + string(encoded) + `;const get=x=>document.evaluate(x,document,null,XPathResult.FIRST_ORDERED_NODE_TYPE,null).singleNodeValue;
 const input=get(p.Input),submit=get(p.Submit);if(!input||!submit||submit.disabled)return false;
 input.focus();const proto=input instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;
 const setter=Object.getOwnPropertyDescriptor(proto,"value")?.set;if(setter)setter.call(input,p.Value);else input.value=p.Value;
 input.dispatchEvent(new Event("input",{bubbles:true}));input.dispatchEvent(new Event("change",{bubbles:true}));submit.click();return true;})()`
		if err = chromedp.Run(tab, chromedp.Evaluate(expression, &submitted)); err != nil || !submitted {
			finishTab()
			return out, ErrResponse
		}
	}
	started := time.Now()
	var htmlText, address string
	lastCount := -1
	quiet := time.Now()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			finishTab()
			if parent.Err() != nil {
				return out, parent.Err()
			}
			return out, ErrResponse
		}
		count, _, cause := b.state()
		if cause != nil {
			finishTab()
			return out, cause
		}
		if count != lastCount {
			lastCount = count
			quiet = time.Now()
		}
		var ready struct {
			Loaded   bool
			Matched  bool
			Location string
			HTML     string
		}
		selectors, _ := json.Marshal([]string{r.ReadySelector, r.EmptySelector})
		expression := `(()=>{const selectors=` + string(selectors) + `;let matched=false;for(const s of selectors){if(!s)continue;try{if(document.querySelector(s))matched=true}catch{}}
 return {Loaded:document.readyState==="complete",Matched:matched,Location:location.href,HTML:document.documentElement?.outerHTML||""}})()`
		if err = chromedp.Run(tab, chromedp.Evaluate(expression, &ready)); err != nil {
			// A single form navigation can replace its JS context. Only repeat this
			// read; the form click and provider requests are never replayed.
			select {
			case <-ctx.Done():
			case <-ticker.C:
			}
			continue
		}
		if len(ready.HTML) > MaxHTML {
			finishTab()
			return out, ErrLimit
		}
		if Origin(ready.Location) != Origin(r.URL) {
			finishTab()
			return out, ErrScope
		}
		if ready.Loaded && time.Since(quiet) >= 500*time.Millisecond && (ready.Matched || time.Since(started) >= 5*time.Second) && (r.Form == nil || time.Since(started) >= 5*time.Second) {
			htmlText, address = ready.HTML, ready.Location
			break
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	if err = r.Guard(ctx); err != nil {
		finishTab()
		return out, ErrScope
	}
	if htmlText == "" || Challenge([]byte(htmlText)) {
		finishTab()
		return out, ErrVerification
	}
	b.importCookies(tab, address)
	cookie := b.cookie(address)
	if !text(cookie, 16000) {
		finishTab()
		return out, ErrLimit
	}
	finishTab()
	count, _, cause := b.state()
	if cause != nil {
		return out, cause
	}
	if r.Login != nil && (!r.Login.LoggedIn([]byte(htmlText)) || cookie == "") {
		return out, ErrLogin
	}
	return Result{HTML: []byte(htmlText), URL: address, Cookie: cookie, UserAgent: r.UserAgent, Requests: count, LoginState: loginState}, nil
}
func (b *broker) importCookies(ctx context.Context, address string) {
	if Origin(address) != Origin(b.request.URL) {
		return
	}
	target := chromedp.FromContext(ctx)
	if target == nil || target.Target == nil {
		return
	}
	cookies, e := storage.GetCookies().Do(cdp.WithExecutor(ctx, target.Target))
	if e != nil {
		b.error(ErrResponse)
		return
	}
	if len(cookies) > 128 {
		b.error(ErrLimit)
		return
	}
	u, _ := url.Parse(b.request.URL)
	values := []*http.Cookie{}
	for _, c := range cookies {
		domain := strings.TrimPrefix(c.Domain, ".")
		if !strings.EqualFold(u.Hostname(), domain) && !strings.HasSuffix(strings.ToLower(u.Hostname()), "."+strings.ToLower(domain)) {
			continue
		}
		values = append(values, &http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain, Secure: c.Secure, HttpOnly: c.HTTPOnly})
	}
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	jar.SetCookies(u, values)
	b.mu.Lock()
	b.jar = jar
	b.mu.Unlock()
}
func (b *broker) fulfill(ctx context.Context, event *fetch.EventRequestPaused) {
	b.fulfillMu.Lock()
	defer b.fulfillMu.Unlock()
	state := chromedp.FromContext(ctx)
	if state == nil || state.Target == nil {
		return
	}
	target := cdp.WithExecutor(ctx, state.Target)
	fail := func() { _ = fetch.FailRequest(event.RequestID, network.ErrorReasonBlockedByClient).Do(target) }
	req := event.Request
	if req == nil {
		b.error(ErrResponse)
		fail()
		return
	}
	kind := string(event.ResourceType)
	if !b.allowed(req.URL, req.Method, kind) {
		if kind == "Document" || kind == "XHR" || kind == "Fetch" {
			b.error(ErrScope)
		}
		fail()
		return
	}
	data := []byte{}
	for _, entry := range req.PostDataEntries {
		if entry == nil {
			continue
		}
		part, e := base64.StdEncoding.DecodeString(entry.Bytes)
		if e != nil || len(data)+len(part) > 64<<10 {
			b.error(ErrLimit)
			fail()
			return
		}
		data = append(data, part...)
	}
	if req.HasPostData && len(data) == 0 {
		if event.NetworkID == "" {
			b.error(ErrResponse)
			fail()
			return
		}
		value, e := network.GetRequestPostData(event.NetworkID).Do(target)
		if e != nil || len(value) > 64<<10 {
			b.error(ErrResponse)
			fail()
			return
		}
		data = []byte(value)
	}
	headers := http.Header{}
	for key, value := range req.Headers {
		if s, ok := value.(string); ok {
			headers.Add(key, s)
		}
	}
	b.importCookies(ctx, req.URL)
	response, e := b.exchange(ctx, req.URL, req.Method, kind, headers, data)
	if e != nil {
		fail()
		return
	}
	out := []*fetch.HeaderEntry{}
	for key, values := range response.headers {
		if strings.EqualFold(key, "Set-Cookie") && Origin(req.URL) != Origin(b.request.URL) {
			continue
		}
		for _, value := range values {
			out = append(out, &fetch.HeaderEntry{Name: key, Value: value})
		}
	}
	if e = fetch.FulfillRequest(event.RequestID, int64(response.status)).WithResponseHeaders(out).WithBody(base64.StdEncoding.EncodeToString(response.body)).Do(target); e != nil && ctx.Err() == nil {
		b.error(ErrResponse)
	}
}
