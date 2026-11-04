package httpserver

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

func signinHelperImage(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 23), G: uint8(y * 23), B: uint8(x * y), A: 255})
		}
	}
	var out bytes.Buffer
	if e := png.Encode(&out, img); e != nil {
		t.Fatal(e)
	}
	return out.Bytes()
}
func signinHelperReview(t *testing.T, f *loginFixture, options map[string]bool) siteSigninReviewView {
	t.Helper()
	r := f.request(t, "POST", "/api/v1/site-signin/review", map[string]any{"mode": "manual", "siteIds": []string{strconv.FormatInt(f.site.ID, 10)}, "options": options})
	var out struct{ Data siteSigninReviewView }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return out.Data
}
func signinHelperStart(t *testing.T, f *loginFixture, p siteSigninReviewView) siteSigninBatchView {
	t.Helper()
	r := f.request(t, "POST", "/api/v1/site-signin/start", map[string]any{"id": p.ID, "fingerprint": p.Fingerprint, "confirm": true})
	var out struct{ Data siteSigninBatchView }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return out.Data
}
func signinHelperWait(t *testing.T, f *loginFixture, id string, wanted func(siteSigninBatchView) bool) siteSigninBatchView {
	t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	for {
		b := signinTestBatch(t, f, id)
		if wanted(b) {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatal("helper chain did not reach expected state", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSiteSigninHelperChainsKeepProviderScopeCacheAndOriginalReceipts(t *testing.T) {
	for _, mode := range []string{"douban", "lens", "ai"} {
		t.Run(mode, func(t *testing.T) {
			f, api := newSigninQuietFixture(t)
			site := f.site
			site.SignURL = "https://tjupt.org/"
			if mode == "ai" {
				site.SignURL = "https://52pt.site/"
			}
			var e error
			f.site, e = f.api.sites.Upsert(t.Context(), site)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "ai" {
				if e = f.api.config.Update(map[string]any{"openai.api_url": "http://ai.fixture.test/base", "openai.api_key": "fixture-ai-key"}); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "lens" {
				binary := os.Getenv("NASTOOL_TEST_BROWSER_BINARY")
				if binary == "" {
					t.Skip("set NASTOOL_TEST_BROWSER_BINARY for real Lens renderer integration")
				}
				engine := browserruntime.New(binary)
				if !engine.Available() {
					t.Fatal("native browser unavailable")
				}
				f.api.browser = engine
				t.Cleanup(func() {
					if engine.Active() != 0 {
						t.Error("owned Lens browser still active")
					}
				})
			}
			picture := signinHelperImage(t)
			var actions, helpers, posters, assets atomic.Int32
			f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := ""
				content := "text/html; charset=utf-8"
				headers := http.Header{}
				host := r.URL.Hostname()
				source := host == "tjupt.org" || host == "52pt.site"
				if source {
					if !strings.Contains(r.Header.Get("Cookie"), "original=fixture-private") {
						t.Error("site Cookie lost")
					}
				} else if r.Header.Get("Cookie") != "" {
					t.Error("helper received site Cookie", host)
				}
				switch {
				case source && r.URL.Path == "/attendance.php" && r.Method == "GET":
					body = `<a href="logout.php">退出</a><table class="captcha"><tr><td><img src="/fixture-picture.png"><input name="answer" value="2">正确片名2<br><input name="answer" value="4">Wrong</td></tr></table>`
				case source && r.URL.Path == "/fixture-picture.png":
					content = "image/png"
					body = string(picture)
				case source && r.URL.Path == "/attendance.php" && r.Method == "POST":
					actions.Add(1)
					if r.ParseForm() != nil || r.PostForm.Get("answer") != "2" {
						t.Error("picture answer changed", r.PostForm)
					}
					body = "这是您的首次签到，本次签到获得10个魔力值。"
				case source && r.URL.Path == "/bakatest.php" && r.Method == "GET":
					body = `<a href="logout.php">退出</a><table><tr><td class="text">请问：Fixture question</td></tr></table><input name="questionid" value="Q1"><input name="choice[]" value="2">A<br><input name="choice[]" value="4">B`
				case source && r.URL.Path == "/bakatest.php" && r.Method == "POST":
					actions.Add(1)
					if r.ParseForm() != nil || strings.Join(r.PostForm["choice[]"], ",") != "2,4" || r.PostForm.Get("questionid") != "Q1" {
						t.Error("original multi-choice form changed", r.PostForm)
					}
					body = "获得10点魔力值"
				case host == "movie.douban.com" && r.URL.Path == "/j/subject_suggest":
					helpers.Add(1)
					if r.URL.Query().Get("q") != "正确片名2" {
						t.Error("poster query changed", r.URL.RawQuery)
					}
					content = "application/json"
					body = `[{"img":"https://img9.doubanio.com/fixture-poster.png"}]`
				case host == "img9.doubanio.com" && r.URL.Path == "/fixture-poster.png":
					posters.Add(1)
					content = "image/png"
					body = string(picture)
				case host == "lens.google.com" && r.URL.Path == "/uploadbyurl":
					helpers.Add(1)
					if r.URL.Query().Get("url") != "https://tjupt.org/fixture-picture.png" {
						t.Error("Lens picture URL changed", r.URL.RawQuery)
					}
					body = `<!doctype html><meta charset="utf-8"><link rel="stylesheet" href="https://www.gstatic.com/fixture.css"><div class="UAiK1e">正確片名</div><div class="UAiK1e">正確片名</div><div class="UAiK1e">正確片名</div>`
				case host == "www.gstatic.com" && r.URL.Path == "/fixture.css":
					assets.Add(1)
					content = "text/css"
					body = "body { color: black; }"
				case host == "ai.fixture.test" && r.URL.Path == "/base/v1/chat/completions":
					helpers.Add(1)
					if r.Header.Get("Authorization") != "Bearer fixture-ai-key" {
						t.Error("original AI key lost")
					}
					var payload map[string]any
					if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["model"] != "gpt-3.5-turbo" || !strings.Contains(stringValueJSON(payload["messages"]), "Fixture question") {
						t.Error("original AI protocol changed", payload)
					}
					content = "application/json"
					body = `{"choices":[{"message":{"content":"2,4"},"finish_reason":"stop"}]}`
				case r.URL.Path == "/favicon.ico":
					return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
				default:
					t.Errorf("unexpected helper request %s %s", r.Method, r.URL)
					return nil, io.EOF
				}
				if !source && host != "ai.fixture.test" && r.Header.Get("Authorization") != "" {
					t.Error("helper inherited another service key", host)
				}
				headers.Set("Content-Type", content)
				return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			cacheDir := filepath.Join(filepath.Dir(f.configPath), "temp", "signin")
			if e = os.MkdirAll(cacheDir, 0700); e != nil {
				t.Fatal(e)
			}
			cacheName := "tjupt"
			if mode == "ai" {
				cacheName = "52pt"
			}
			cachePath := filepath.Join(cacheDir, cacheName+".json")
			original := `{"future":{"big":9007199254740993,"null":null},"old-question":["2"]}`
			if e = os.WriteFile(cachePath, []byte(original), 0600); e != nil {
				t.Fatal(e)
			}
			p := signinHelperReview(t, f, map[string]bool{mode: true})
			if len(p.Helpers) != 1 || p.Helpers[0].Kind == "" {
				t.Fatal("helper receiver absent", p.Helpers)
			}
			b := signinHelperStart(t, f, p)
			b = signinHelperWait(t, f, b.ID, func(v siteSigninBatchView) bool {
				return len(v.Items) == 1 && v.Items[0].State == "finished" && v.Notified
			})
			if b.Items[0].Outcome.State != "signed" || b.Items[0].Outcome.CacheState != "saved" || actions.Load() != 1 || helpers.Load() != 1 {
				t.Fatal("incomplete native helper chain", b, actions.Load(), helpers.Load())
			}
			if mode == "douban" && posters.Load() != 1 {
				t.Fatal("poster not compared")
			}
			if mode == "lens" && assets.Load() != 1 {
				t.Fatal("Lens ordinary asset not loaded")
			}
			data, e := os.ReadFile(cachePath)
			var compact bytes.Buffer
			if e != nil || json.Compact(&compact, data) != nil ||
				!bytes.Contains(compact.Bytes(), []byte(`"future":{"big":9007199254740993,"null":null}`)) ||
				!bytes.Contains(compact.Bytes(), []byte(`"old-question":["2"]`)) {
				t.Fatal("cache unknown values changed", string(data), e)
			}
			before, e := api.store.Get(t.Context(), b.Items[0].ID)
			if e != nil {
				t.Fatal(e)
			}
			same := signinHelperStart(t, f, p)
			after, e := api.store.Get(t.Context(), b.Items[0].ID)
			if e != nil || same.ID != b.ID || stringValueJSON(before) != stringValueJSON(after) || actions.Load() != 1 || helpers.Load() != 1 {
				t.Fatal("original helper receipt replayed", e)
			}
			current, e := f.api.sites.Get(t.Context(), f.site.ID)
			if e != nil || current.Cookie != f.site.Cookie || current.Note != f.site.Note {
				t.Fatal("original site changed", current, e)
			}
		})
	}
}
func stringValueJSON(v any) string { raw, _ := json.Marshal(v); return string(raw) }

func TestSiteSigninInputRejectsWrongLengthWithoutSpendingNonce(t *testing.T) {
	f, api := newSigninQuietFixture(t)
	site := f.site
	site.SignURL = "https://hdsky.me/"
	var e error
	f.site, e = f.api.sites.Upsert(t.Context(), site)
	if e != nil {
		t.Fatal(e)
	}
	picture := signinHelperImage(t)
	var actions atomic.Int32
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := ""
		content := "text/html; charset=utf-8"
		switch r.URL.Path {
		case "/":
			body = `<a href="logout.php">退出</a>`
		case "/image_code_ajax.php":
			body = `{"success":true,"code":"fixture-hash"}`
			content = "application/json"
		case "/image.php":
			body = string(picture)
			content = "image/png"
		case "/showup.php":
			actions.Add(1)
			if r.ParseForm() != nil || r.PostForm.Get("imagestring") != "ABC123" {
				t.Error("original input lost", r.PostForm)
			}
			body = `{"success":true}`
			content = "application/json"
		default:
			t.Error("unexpected site request", r.URL)
			return nil, io.EOF
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {content}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	p := signinHelperReview(t, f, map[string]bool{})
	b := signinHelperStart(t, f, p)
	b = signinHelperWait(t, f, b.ID, func(v siteSigninBatchView) bool { return len(v.Items) == 1 && v.Items[0].Challenge != nil })
	j := b.Items[0]
	if j.Challenge.MinLength != 6 || j.Challenge.MaxLength != 6 {
		t.Fatal("provider bounds not exposed", j.Challenge)
	}
	input := func(value string) *httptest.ResponseRecorder {
		return f.request(t, "POST", "/api/v1/site-signin/jobs/"+j.ID+"/input", map[string]any{"nonce": j.Challenge.Nonce, "values": []string{value}, "confirm": true})
	}
	bad := input("ABC")
	if bad.Code != 409 {
		t.Fatal("bad length accepted", bad.Code)
	}
	pending, e := api.store.Get(t.Context(), j.ID)
	if e != nil || pending.State != "waiting_input" || pending.Nonce != j.Challenge.Nonce || actions.Load() != 0 {
		t.Fatal("invalid input spent nonce", pending, e)
	}
	if good := input("ABC123"); good.Code != 200 {
		t.Fatal(good.Code, good.Body.String())
	}
	b = signinHelperWait(t, f, b.ID, func(v siteSigninBatchView) bool { return v.Items[0].State == "finished" && v.Notified })
	if b.Items[0].Outcome.State != "signed" || actions.Load() != 1 {
		t.Fatal(b, actions.Load())
	}
	if duplicate := input("ABC123"); duplicate.Code != 409 || actions.Load() != 1 {
		t.Fatal("spent input replayed", duplicate.Code)
	}
	private, e := api.store.Get(t.Context(), j.ID)
	if e != nil || strings.Contains(stringValueJSON(private), "ABC123") {
		t.Fatal("answer persisted in receipt", e)
	}
}

func TestSiteSigninSavedLoginKeepsOriginalAccountAndChallengeSequence(t *testing.T) {
	binary := os.Getenv("NASTOOL_TEST_BROWSER_BINARY")
	if binary == "" {
		t.Skip("set NASTOOL_TEST_BROWSER_BINARY for saved-account browser integration")
	}
	f, api := newSigninQuietFixture(t)
	engine := browserruntime.New(binary)
	if !engine.Available() {
		t.Fatal("native browser unavailable")
	}
	f.api.browser = engine
	site := f.site
	site.SignURL = "https://m-team.cc/login.php"
	var err error
	f.site, err = f.api.sites.Upsert(t.Context(), site)
	if err != nil {
		t.Fatal(err)
	}
	const saved = `{"username":"fixture-mteam-user","password":"fixture-mteam-password","future":{"big":9007199254740993}}`
	if err := api.system.Set(t.Context(), sitelogin.SavedKey, saved); err != nil {
		t.Fatal(err)
	}
	picture := signinHelperImage(t)
	const loginPage = `<!doctype html><meta charset="utf-8"><form action="/take-login" method="post"><input name="username"><input name="password" type="password"><input name="captcha"><img alt="CAPTCHA" src="/fixture.png"><button type="submit">Login</button></form>`
	emailPage := `<!doctype html><meta charset="utf-8"><p>郵箱驗證碼</p><div id="root">` + strings.Repeat("<div>", 7) +
		`<form><input id="email"><span id="code"><input><button type="button" id="send">Send</button></span><button type="button" id="verify">Verify</button></form>` +
		strings.Repeat("</div>", 7) + `</div><script>
document.querySelector('#send').onclick=async()=>{await fetch('/email-send',{method:'POST',body:new URLSearchParams({email:document.querySelector('#email').value})})};
document.querySelector('#verify').onclick=async()=>{const response=await fetch('/email-verify',{method:'POST',body:new URLSearchParams({code:document.querySelector('#code input').value})});document.body.innerHTML=await response.text()};
</script>`
	var logins, sends, verifies atomic.Int32
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "m-team.cc" {
			t.Error("request left the original site", r.URL)
			return nil, io.EOF
		}
		body := ""
		status := http.StatusOK
		headers := http.Header{"Content-Type": {"text/html; charset=utf-8"}}
		switch {
		case r.Method == "GET" && r.URL.Path == "/login.php":
			body = loginPage
		case r.Method == "GET" && r.URL.Path == "/fixture.png":
			headers.Set("Content-Type", "image/png")
			body = string(picture)
		case r.Method == "GET" && r.URL.Path == "/favicon.ico":
			status = http.StatusNotFound
		case r.Method == "POST" && r.URL.Path == "/take-login":
			logins.Add(1)
			if r.ParseForm() != nil || r.PostForm.Get("username") != "fixture-mteam-user" ||
				r.PostForm.Get("password") != "fixture-mteam-password" || r.PostForm.Get("captcha") != "ABCD" {
				t.Error("saved login form values changed")
			}
			body = emailPage
		case r.Method == "POST" && r.URL.Path == "/email-send":
			sends.Add(1)
			if r.ParseForm() != nil || r.PostForm.Get("email") != "fixture@example.test" {
				t.Error("email request changed")
			}
			body = "OK"
		case r.Method == "POST" && r.URL.Path == "/email-verify":
			verifies.Add(1)
			if r.ParseForm() != nil || r.PostForm.Get("code") != "123456" {
				t.Error("email code changed")
			}
			headers.Add("Set-Cookie", "SID=fixture-email-verified; Path=/; Secure; HttpOnly")
			body = `<a href="/logout.php">退出</a>`
		default:
			t.Error("unexpected saved-login request", r.Method, r.URL)
			return nil, io.EOF
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	review := signinHelperReview(t, f, map[string]bool{"savedLogin": true})
	batch := signinHelperStart(t, f, review)
	t.Cleanup(func() {
		for _, item := range signinTestBatch(t, f, batch.ID).Items {
			if item.CanStop {
				f.request(t, "POST", "/api/v1/site-signin/jobs/"+item.ID+"/stop", map[string]bool{"confirm": true})
			}
		}
	})
	var lastNonce string
	for _, step := range []struct{ kind, value string }{
		{"captcha", "ABCD"}, {"email", "fixture@example.test"}, {"email_code", "123456"},
	} {
		view := signinHelperWait(t, f, batch.ID, func(v siteSigninBatchView) bool {
			return len(v.Items) == 1 && v.Items[0].Challenge != nil && v.Items[0].Challenge.Kind == step.kind
		})
		item := view.Items[0]
		if item.Challenge.Nonce == lastNonce || item.Challenge.Nonce == "" {
			t.Fatal("login stages shared an input nonce")
		}
		lastNonce = item.Challenge.Nonce
		submit := func(value string) *httptest.ResponseRecorder {
			return f.request(t, "POST", "/api/v1/site-signin/jobs/"+item.ID+"/input", map[string]any{
				"nonce": item.Challenge.Nonce, "values": []string{value}, "confirm": true,
			})
		}
		if step.kind == "email" {
			if response := submit("bad-address"); response.Code != 409 || sends.Load() != 0 {
				t.Fatal("invalid email left the prompt", response.Code, sends.Load())
			}
		}
		if response := submit(step.value); response.Code != 200 {
			t.Fatal(step.kind, response.Code, response.Body.String())
		}
	}
	done := signinHelperWait(t, f, batch.ID, func(v siteSigninBatchView) bool {
		return len(v.Items) == 1 && v.Items[0].State == "finished" && v.Notified
	})
	if done.Items[0].Outcome.State != "authenticated" || logins.Load() != 1 || sends.Load() != 1 || verifies.Load() != 1 || engine.Active() != 0 {
		t.Fatal("saved login failed or changed signin semantics", done, logins.Load(), sends.Load(), verifies.Load(), engine.Active())
	}
	original, err := api.login.store.Saved(t.Context())
	if err != nil || original != saved {
		t.Fatal("saved account record was changed", err)
	}
	current, err := f.api.sites.Get(t.Context(), f.site.ID)
	if err != nil || current.Cookie != f.site.Cookie || current.Note != f.site.Note {
		t.Fatal("signin published temporary site credentials", err)
	}
	record, err := api.store.Get(t.Context(), done.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-mteam-user", "fixture-mteam-password", "ABCD", "fixture@example.test", "123456", "fixture-email-verified"} {
		if strings.Contains(stringValueJSON(record), secret) {
			t.Fatal("signin receipt retained an input value")
		}
	}
}

func TestSiteSigninAuxiliaryRoutesRespectOriginalProxySelection(t *testing.T) {
	baseCalls := 0
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		baseCalls++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	for _, tt := range []struct {
		name, address, custom string
		proxies               any
		lens, rejected        bool
	}{
		{name: "built-in AI direct", address: "https://api.openai.com/v1/chat/completions"},
		{name: "Lens direct", address: "https://lens.google.com/uploadbyurl", lens: true},
		{name: "unconfigured scheme", address: "https://lens.google.com/uploadbyurl", proxies: map[string]any{"http": "invalid"}, lens: true},
		{name: "invalid explicit scheme", address: "https://lens.google.com/uploadbyurl", proxies: map[string]any{"https": "invalid"}, lens: true, rejected: true},
		{name: "invalid explicit map", address: "https://lens.google.com/uploadbyurl", proxies: "invalid", lens: true, rejected: true},
		{name: "custom AI on built-in host", address: "https://api.openai.com/v1/chat/completions", custom: "https://api.openai.com", proxies: "invalid"},
		{name: "ordinary Douban", address: "https://movie.douban.com/j/subject_suggest", proxies: "invalid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := map[string]any{"app": map[string]any{"proxies": tt.proxies}, "openai": map[string]any{"api_url": tt.custom, "api_key": "fixture-key"}}
			transport := signinAuxTransport{base: base, configuration: cfg, lens: tt.lens}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tt.address, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := baseCalls
			_, err = transport.RoundTrip(request)
			if tt.rejected && (err == nil || baseCalls != before) || !tt.rejected && (err != nil || baseCalls != before+1) {
				t.Fatal("auxiliary route changed", err, baseCalls-before)
			}
		})
	}
	const asset = "http://assets.fixture.test/fixture.css"
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.URL.String() != asset {
			t.Error("Lens asset arrived with unexpected target", r.URL)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	transport := signinAuxTransport{base: &http.Transport{}, lens: true, configuration: map[string]any{
		"app": map[string]any{"proxies": map[string]any{"http": proxy.URL}},
	}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, asset, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || proxyCalls.Load() != 1 {
		t.Fatal("ordinary Lens asset did not follow its explicit proxy")
	}
}
