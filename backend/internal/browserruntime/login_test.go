package browserruntime

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func loggedInFixture(raw []byte) bool {
	return bytes.Contains(raw, []byte("logout.php")) && !bytes.Contains(raw, []byte(`type="password"`))
}
func loginActionCount(t *testing.T, count *atomic.Int32) func(context.Context, string) (func(string, string) error, error) {
	t.Helper()
	return func(context.Context, string) (func(string, string) error, error) {
		count.Add(1)
		return func(state, digest string) error {
			if state != "received" {
				t.Error("browser action not acknowledged", state)
			}
			return nil
		}, nil
	}
}
func loginForm(extra, action string) string {
	return `<!doctype html><form action="` + action + `" method="post"><input name="username"><input name="password" type="password">` + extra + `<button type="submit">Login</button></form>`
}
func TestChromeOriginalLoginTwoStepImageAndCookiePublication(t *testing.T) {
	engine := realEngine(t)
	var pages, images, posts, actions atomic.Int32
	var picture bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	if err := png.Encode(&picture, img); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.UserAgent(), "Chrome") || r.UserAgent() == "auto" {
			t.Error("actual browser agent not used")
		}
		switch r.URL.Path {
		case "/login.php":
			pages.Add(1)
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, loginForm(`<input name="two_step_code"><input name="imagestring"><img alt="CAPTCHA" src="/captcha.png">`, "/take-login"))
		case "/captcha.png":
			images.Add(1)
			w.Header().Set("Content-Type", "image/png")
			http.SetCookie(w, &http.Cookie{Name: "captcha-session", Value: "image-owned", Path: "/", Secure: true, HttpOnly: true})
			w.Header().Add("Set-Cookie", "expired=must-delete; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Secure")
			w.Header().Add("Set-Cookie", "age-deleted=must-delete; Path=/; Max-Age=0; Secure")
			w.Write(picture.Bytes())
		case "/take-login":
			posts.Add(1)
			if r.ParseForm() != nil || r.PostForm.Get("username") != "fixture-user" || r.PostForm.Get("password") != "fixture-password" || r.PostForm.Get("two_step_code") != "246810" || r.PostForm.Get("imagestring") != "ABCD" || !strings.Contains(r.Header.Get("Cookie"), "captcha-session=image-owned") {
				t.Error("original login fields/session lost")
			}
			if strings.Contains(r.Header.Get("Cookie"), "must-delete") {
				t.Error("expired captcha response cookie was resurrected")
			}
			w.Header().Set("Content-Type", "text/html")
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "verified-fixture", Path: "/", Secure: true, HttpOnly: true})
			io.WriteString(w, `<a href="/logout.php">退出</a>`)
		default:
			t.Error("unexpected browser destination", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	prompts := []string{}
	login := &Login{Username: "fixture-user", Password: "fixture-password", LoggedIn: loggedInFixture, Action: loginActionCount(t, &actions), Interact: func(ctx context.Context, c LoginChallenge) (string, error) {
		prompts = append(prompts, c.Kind)
		if c.Kind == "two_step" {
			return "246810", nil
		}
		if c.Kind != "captcha" || c.MIME != "image/png" || !bytes.Equal(c.Image, picture.Bytes()) {
			t.Error("original captcha image lost")
		}
		return "ABCD", nil
	}}
	r := browserRequest()
	r.URL = server.URL + "/login.php"
	r.Cookie += "; expired=must-delete; age-deleted=must-delete"
	r.UserAgent = "auto"
	r.Login = login
	r.ReadOnly = false
	r.Transport = server.Client().Transport
	result, err := engine.Render(t.Context(), r)
	if err != nil || result.LoginState != "acquired" || !strings.Contains(result.Cookie, "SID=verified-fixture") || actions.Load() != 1 || pages.Load() != 1 || posts.Load() != 1 || images.Load() != 1 || strings.Join(prompts, ",") != "two_step,captcha" || engine.Active() != 0 {
		t.Fatal("login flow failed", err, result.LoginState, actions.Load(), pages.Load(), posts.Load(), images.Load(), prompts)
	}
}
func TestChromeExistingLoginSkipsAllCredentialAndCodeActions(t *testing.T) {
	engine := realEngine(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "saved=private" {
			t.Error("original cookie not retained")
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<a href="/logout.php">退出</a>`)
	}))
	defer server.Close()
	r := browserRequest()
	r.URL = server.URL + "/login.php"
	r.Login = &Login{Username: "never-send", Password: "never-send", LoggedIn: loggedInFixture, Action: func(context.Context, string) (func(string, string) error, error) {
		t.Error("existing session re-logged")
		return nil, ErrLogin
	}, Interact: func(context.Context, LoginChallenge) (string, error) {
		t.Error("existing session requested code")
		return "", ErrInput
	}}
	r.ReadOnly = false
	out, err := engine.Render(t.Context(), r)
	if err != nil || out.LoginState != "existing" || out.Cookie != "saved=private" || calls.Load() != 1 {
		t.Fatal(out.LoginState, err, calls.Load())
	}
}
func TestChromeMTeamEmailSendAndVerifyAreSeparateSingleActions(t *testing.T) {
	engine := realEngine(t)
	var loginPosts, emailSends, verifies, actions atomic.Int32
	emailPage := `<!doctype html><meta charset="utf-8"><p>郵箱驗證碼</p><div id="root">` + strings.Repeat("<div>", 7) + `<form><input id="email"><span id="code"><input><button type="button" id="send">Send</button></span><button type="button" id="verify">Verify</button></form>` + strings.Repeat("</div>", 7) + `</div><script>
 document.querySelector('#send').onclick=async()=>{await fetch('/email-send',{method:'POST',body:new URLSearchParams({email:document.querySelector('#email').value})})};
 document.querySelector('#verify').onclick=async()=>{const r=await fetch('/email-verify',{method:'POST',body:new URLSearchParams({code:document.querySelector('#code input').value})});document.body.innerHTML=await r.text()};
 </script>`
	transport := transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/login.php":
			return pageResponse(r, loginForm("", "/take-login")), nil
		case "/take-login":
			loginPosts.Add(1)
			return pageResponse(r, emailPage), nil
		case "/email-send":
			emailSends.Add(1)
			if r.ParseForm() != nil || r.PostForm.Get("email") != "fixture@example.test" {
				t.Error("original email request changed")
			}
			return pageResponse(r, "OK"), nil
		case "/email-verify":
			verifies.Add(1)
			if r.ParseForm() != nil || r.PostForm.Get("code") != "123456" {
				t.Error("original email code changed")
			}
			out := pageResponse(r, `<a href="/logout.php">退出</a>`)
			out.Header.Add("Set-Cookie", "SID=email-verified; Path=/; Secure; HttpOnly")
			return out, nil
		default:
			t.Error("unexpected original site request", r.URL)
			return pageResponse(r, ""), nil
		}
	})
	r := browserRequest()
	r.URL = "https://m-team.test/login.php"
	r.Transport = transport
	r.ReadOnly = false
	prompts := []string{}
	r.Login = &Login{Username: "fixture", Password: "password", LoggedIn: loggedInFixture, Action: loginActionCount(t, &actions), Interact: func(ctx context.Context, c LoginChallenge) (string, error) {
		prompts = append(prompts, c.Kind)
		if c.Kind == "email" {
			return "fixture@example.test", nil
		}
		return "123456", nil
	}}
	out, err := engine.Render(t.Context(), r)
	if err != nil || out.LoginState != "acquired" || loginPosts.Load() != 1 || emailSends.Load() != 1 || verifies.Load() != 1 || actions.Load() != 3 || strings.Join(prompts, ",") != "email,email_code" {
		t.Fatal(err, out.LoginState, loginPosts.Load(), emailSends.Load(), verifies.Load(), actions.Load(), prompts)
	}
}

func TestChromeForeignLoginFormNeverReceivesCredentialsOrSubmit(t *testing.T) {
	engine := realEngine(t)
	var requests, denied atomic.Int32
	r := browserRequest()
	r.URL = "https://original.test/login.php"
	r.ReadOnly = false
	r.Transport = transportFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		if request.Method != "GET" || request.URL.String() != r.URL {
			t.Error("foreign login request escaped original scope", request.URL)
		}
		return pageResponse(request, loginForm("", "https://foreign.test/take-login")), nil
	})
	r.Login = &Login{Username: "never-export", Password: "never-export", LoggedIn: loggedInFixture, Interact: func(context.Context, LoginChallenge) (string, error) {
		t.Error("unexpected prompt")
		return "", ErrInput
	}, Action: func(context.Context, string) (func(string, string) error, error) {
		return func(state, digest string) error {
			if state != "denied" {
				t.Error("foreign form action was not denied", state)
			}
			denied.Add(1)
			return nil
		}, nil
	}}
	_, err := engine.Render(t.Context(), r)
	if err != ErrLogin || requests.Load() != 1 || denied.Load() != 1 {
		t.Fatal("foreign form was submitted", err, requests.Load(), denied.Load())
	}
}
