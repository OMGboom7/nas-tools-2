package browserruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

type loginView struct {
	HTML, URL string
	Fields    map[string]string
}

func (b *broker) loginView(ctx context.Context) (loginView, error) {
	out := loginView{}
	if err := b.request.Guard(ctx); err != nil {
		return out, ErrScope
	}
	if _, _, err := b.state(); err != nil {
		return out, err
	}
	rules, _ := json.Marshal(loginRules)
	expression := `(()=>{const rules=` + string(rules) + `;const node=x=>document.evaluate(x,document,null,XPathResult.FIRST_ORDERED_NODE_TYPE,null).singleNodeValue;const visible=n=>{if(!n)return false;if(n.nodeType===2)n=n.ownerElement;const s=getComputedStyle(n);return n.getClientRects().length>0&&s.display!=="none"&&s.visibility!=="hidden";};const fields={};for(const[k,paths]of Object.entries(rules)){for(const p of paths){const n=node(p);if(visible(n)){fields[k]=k==="captcha_img"?new URL(n.textContent,location.href).href:p;break;}}}const raw=document.documentElement?.outerHTML||"";return {HTML:raw.slice(0,4194305),URL:location.href,Fields:fields};})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &out)); err != nil {
		return out, ErrResponse
	}
	if len(out.HTML) > MaxHTML {
		return out, ErrLimit
	}
	if Origin(out.URL) != Origin(b.request.URL) {
		return out, ErrScope
	}
	if Challenge([]byte(out.HTML)) {
		return out, ErrVerification
	}
	return out, nil
}
func (b *broker) login(ctx context.Context, l *Login) (string, error) {
	// Wait for the original page's normal scripts; this does not click or solve
	// a browser verification widget.
	if err := loginDelay(ctx, 3*time.Second); err != nil {
		return "", err
	}
	var view loginView
	readyAt := time.Now().Add(10 * time.Second)
	for {
		var err error
		view, err = b.loginView(ctx)
		if err == nil && l.LoggedIn([]byte(view.HTML)) {
			return "existing", nil
		}
		if err == ErrScope || err == ErrVerification || err == ErrLimit {
			return "", err
		}
		if err == nil && view.Fields["username"] != "" && view.Fields["password"] != "" && view.Fields["submit"] != "" {
			break
		}
		if !time.Now().Before(readyAt) {
			return "", ErrLogin
		}
		if err = loginDelay(ctx, 150*time.Millisecond); err != nil {
			return "", err
		}
	}
	values := map[string]string{"username": l.Username, "password": l.Password}
	if view.Fields["twostep"] != "" {
		code := l.TwoStep
		if code == "" {
			var err error
			code, err = l.Interact(ctx, LoginChallenge{Kind: "two_step"})
			if err != nil {
				return "", err
			}
		}
		if !text(code, 128) || code == "" {
			return "", ErrInput
		}
		values["twostep"] = code
	}
	if view.Fields["captcha"] != "" {
		imageAddress := view.Fields["captcha_img"]
		if imageAddress == "" {
			return "", ErrInput
		}
		raw, mime, err := b.loginImage(ctx, imageAddress)
		if err != nil {
			return "", err
		}
		code, err := l.Interact(ctx, LoginChallenge{Kind: "captcha", Image: raw, MIME: mime})
		if err != nil {
			return "", err
		}
		if !text(code, 128) || code == "" {
			return "", ErrInput
		}
		values["captcha"] = code
	}
	if err := b.loginClick(ctx, l, "credentials", view.Fields, values, view.Fields["submit"]); err != nil {
		return "", err
	}
	view, err := b.loginOutcome(ctx, l, 20*time.Second, true)
	if err != nil {
		return "", err
	}
	if l.LoggedIn([]byte(view.HTML)) {
		return "acquired", nil
	}
	u, _ := url.Parse(b.request.URL)
	if !strings.Contains(strings.ToLower(u.Hostname()), "m-team") || !strings.Contains(view.HTML, "郵箱驗證碼") && !strings.Contains(view.HTML, "邮箱验证码") {
		return "", ErrLogin
	}
	email, err := l.Interact(ctx, LoginChallenge{Kind: "email"})
	if err != nil {
		return "", err
	}
	if !text(email, 254) || email == "" {
		return "", ErrInput
	}
	emailFields := map[string]string{"email": `//input[@id="email"]`}
	if err = b.loginClick(ctx, l, "email-send", emailFields, map[string]string{"email": email}, `//*[@id="code"]/button`); err != nil {
		return "", err
	}
	if err = loginDelay(ctx, time.Second); err != nil {
		return "", err
	}
	code, err := l.Interact(ctx, LoginChallenge{Kind: "email_code"})
	if err != nil {
		return "", err
	}
	if !text(code, 128) || code == "" {
		return "", ErrInput
	}
	codeFields := map[string]string{"email_code": `//*[@id="code"]/input`}
	if err = b.loginClick(ctx, l, "email-verify", codeFields, map[string]string{"email_code": code}, `//*[@id="root"]/div/div/div[1]/div/div/div/div/form/button`); err != nil {
		return "", err
	}
	view, err = b.loginOutcome(ctx, l, 20*time.Second, false)
	if err != nil {
		return "", err
	}
	if !l.LoggedIn([]byte(view.HTML)) {
		return "", ErrLogin
	}
	return "acquired", nil
}
func loginDelay(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (b *broker) loginOutcome(ctx context.Context, l *Login, timeout time.Duration, allowEmail bool) (loginView, error) {
	deadline := time.Now().Add(timeout)
	var last loginView
	for {
		view, err := b.loginView(ctx)
		if err == nil {
			last = view
			if l.LoggedIn([]byte(view.HTML)) {
				return view, nil
			}
			if allowEmail && (strings.Contains(view.HTML, "郵箱驗證碼") || strings.Contains(view.HTML, "邮箱验证码")) {
				return view, nil
			}
		} else if err == ErrScope || err == ErrLimit || err == ErrVerification {
			return view, err
		}
		if !time.Now().Before(deadline) {
			return last, ErrLogin
		}
		if err = loginDelay(ctx, 200*time.Millisecond); err != nil {
			return last, err
		}
	}
}
func (b *broker) loginClick(ctx context.Context, l *Login, kind string, fields, values map[string]string, submit string) error {
	if err := b.request.Guard(ctx); err != nil {
		return ErrScope
	}
	if _, _, err := b.state(); err != nil {
		return err
	}
	input := struct {
		Fields, Values map[string]string
		Submit         string
	}{fields, values, submit}
	encoded, _ := json.Marshal(input)
	// Persist an action intent before filling/clicking. The receipt acknowledges
	// the one browser command only; provider success requires a later page check.
	finish, err := l.Action(ctx, kind)
	if err != nil || finish == nil {
		return ErrScope
	}
	expression := `(()=>{const p=` + string(encoded) + `;const get=x=>document.evaluate(x,document,null,XPathResult.FIRST_ORDERED_NODE_TYPE,null).singleNodeValue;const visible=n=>!!n&&n.getClientRects().length>0&&getComputedStyle(n).visibility!=="hidden";const button=get(p.Submit);if(!visible(button)||button.disabled)return false;const entries=[];for(const[k,value]of Object.entries(p.Values)){const node=get(p.Fields[k]);if(!visible(node)||node.disabled)return false;entries.push([node,value]);}const form=button.form;if(form?.action&&new URL(form.action,location.href).origin!==location.origin)return false;for(const[node,value]of entries){const proto=node instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;const setter=Object.getOwnPropertyDescriptor(proto,"value")?.set;if(setter)setter.call(node,value);else node.value=value;node.dispatchEvent(new Event("input",{bubbles:true}));node.dispatchEvent(new Event("change",{bubbles:true}));}button.click();return true;})()`
	var clicked bool
	err = chromedp.Run(ctx, chromedp.Evaluate(expression, &clicked))
	if err != nil {
		_ = finish("uncertain", "")
		return ErrResponse
	}
	if !clicked {
		_ = finish("denied", "")
		return ErrLogin
	}
	if err = finish("received", Hash([]any{"browser-command-ack", kind})); err != nil {
		return ErrResponse
	}
	return nil
}

func (b *broker) loginImage(ctx context.Context, address string) ([]byte, string, error) {
	var raw []byte
	if strings.HasPrefix(address, "data:") {
		prefix, value, ok := strings.Cut(address, ",")
		if !ok || len(value) > 3<<20 || !strings.HasSuffix(prefix, ";base64") {
			return nil, "", ErrLimit
		}
		var err error
		raw, err = base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, "", ErrInput
		}
	} else {
		base, _ := url.Parse(b.request.URL)
		u, err := base.Parse(address)
		if err != nil || Origin(u.String()) != Origin(b.request.URL) {
			return nil, "", ErrScope
		}
		for hop := 0; ; hop++ {
			b.fulfillMu.Lock()
			b.importCookies(ctx, u.String())
			response, err := b.exchange(ctx, u.String(), "GET", "LoginImage", http.Header{"Accept": {"image/png,image/jpeg,image/gif"}}, nil)
			if err == nil {
				err = b.loginResponseCookies(ctx, u.String(), response.headers)
			}
			b.fulfillMu.Unlock()
			if err != nil {
				return nil, "", err
			}
			if response.status == 200 {
				raw = response.body
				break
			}
			if hop >= 3 || response.status != 301 && response.status != 302 && response.status != 303 && response.status != 307 && response.status != 308 {
				return nil, "", ErrResponse
			}
			u, err = u.Parse(response.headers.Get("Location"))
			if err != nil || Origin(u.String()) != Origin(b.request.URL) {
				return nil, "", ErrScope
			}
		}
	}
	if len(raw) == 0 || len(raw) > 2<<20 {
		return nil, "", ErrLimit
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 4000000 {
		return nil, "", ErrInput
	}
	mime := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif"}[format]
	if mime == "" {
		return nil, "", ErrInput
	}
	return raw, mime, nil
}
func (b *broker) loginResponseCookies(ctx context.Context, address string, headers http.Header) error {
	state := chromedp.FromContext(ctx)
	if state == nil || state.Target == nil {
		return ErrResponse
	}
	target := cdp.WithExecutor(ctx, state.Target)
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" {
		return ErrScope
	}
	for _, cookie := range (&http.Response{Header: headers}).Cookies() {
		path := cookie.Path
		if !strings.HasPrefix(path, "/") {
			path = "/"
			if index := strings.LastIndex(u.Path, "/"); index > 0 {
				path = u.Path[:index]
			}
		}
		if cookie.MaxAge < 0 || cookie.MaxAge == 0 && !cookie.Expires.IsZero() && !cookie.Expires.After(time.Now()) {
			domain := cookie.Domain
			if domain == "" {
				domain = u.Hostname()
			}
			cmd := network.DeleteCookies(cookie.Name).WithDomain(domain).WithPath(path)
			if err := cmd.Do(target); err != nil {
				return ErrResponse
			}
			continue
		}
		c := &network.CookieParam{Name: cookie.Name, Value: cookie.Value, URL: address, Path: path, Domain: cookie.Domain, Secure: cookie.Secure, HTTPOnly: cookie.HttpOnly}
		if cookie.MaxAge > 0 {
			age := cookie.MaxAge
			if age > 400*24*60*60 {
				age = 400 * 24 * 60 * 60
			}
			expires := cdp.TimeSinceEpoch(time.Now().Add(time.Duration(age) * time.Second))
			c.Expires = &expires
		} else if !cookie.Expires.IsZero() {
			expires := cdp.TimeSinceEpoch(cookie.Expires)
			c.Expires = &expires
		}
		switch cookie.SameSite {
		case http.SameSiteLaxMode:
			c.SameSite = network.CookieSameSiteLax
		case http.SameSiteStrictMode:
			c.SameSite = network.CookieSameSiteStrict
		case http.SameSiteNoneMode:
			c.SameSite = network.CookieSameSiteNone
		}
		if err := network.SetCookies([]*network.CookieParam{c}).Do(target); err != nil {
			return ErrResponse
		}
	}
	return nil
}
