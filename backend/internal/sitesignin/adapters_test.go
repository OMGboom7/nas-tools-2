package sitesignin

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
)

type signinFixtureStep struct {
	method, path, body string
	headers            http.Header
	check              func(*testing.T, *http.Request, []byte)
}
type signinFixtureTransport struct {
	t     *testing.T
	steps []signinFixtureStep
	count int
}

func (f *signinFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Helper()
	if f.count >= len(f.steps) {
		f.t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}
	step := f.steps[f.count]
	f.count++
	if r.Method != step.method || r.URL.RequestURI() != step.path {
		f.t.Fatalf("step %d got %s %s, expected %s %s", f.count, r.Method, r.URL.RequestURI(), step.method, step.path)
	}
	if r.Header.Get("User-Agent") != "fixture-signin-agent" {
		f.t.Fatal("original user agent lost")
	}
	if !strings.Contains(r.Header.Get("Cookie"), "hdchina=") && !strings.Contains(r.Header.Get("Cookie"), "session=fixture-cookie") {
		f.t.Fatal("original site session lost")
	}
	raw, _ := io.ReadAll(r.Body)
	if step.check != nil {
		step.check(f.t, r, raw)
	}
	h := step.headers
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(step.body)), Request: r}, nil
}

type signinFixtureCache struct {
	values map[string][]string
	puts   int
	key    string
	answer []string
}

func (f *signinFixtureCache) Read(context.Context, string) (map[string][]string, string, error) {
	return f.values, "fixture-cache-token", nil
}
func (f *signinFixtureCache) Put(_ context.Context, _, token, key string, value []string, guard func(context.Context) error) error {
	if token != "fixture-cache-token" {
		return ErrScope
	}
	f.puts++
	f.key = key
	f.answer = append([]string{}, value...)
	return guard(context.Background())
}

type signinFixtureBrowser struct {
	render func(context.Context, browserruntime.Request) (browserruntime.Result, error)
}

func (signinFixtureBrowser) Available() bool { return true }
func (f signinFixtureBrowser) Render(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
	return f.render(ctx, r)
}
func signinFixtureImage(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			v := uint8(0)
			if x >= 5 {
				v = 255
			}
			img.SetNRGBA(x, y, color.NRGBA{v, v, v, 255})
		}
	}
	var b bytes.Buffer
	if e := png.Encode(&b, img); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func fixtureForm(expected url.Values) func(*testing.T, *http.Request, []byte) {
	return func(t *testing.T, r *http.Request, raw []byte) {
		t.Helper()
		got, e := url.ParseQuery(string(raw))
		if e != nil || !reflect.DeepEqual(got, expected) {
			t.Fatalf("original form mismatch: got %v expected %v", got, expected)
		}
	}
}

func TestAllOriginalAdapters(t *testing.T) {
	imageBytes := signinFixtureImage(t)
	hash, e := PictureHash(imageBytes)
	if e != nil {
		t.Fatal(e)
	}
	home := `<a href="logout.php">退出</a>`
	steps := func(entries ...signinFixtureStep) []signinFixtureStep { return entries }
	const csrf = `<meta name="x-csrf" content="fixture-csrf">`
	const quiz = `<a href="logout.php">退出</a><table><tr><td class="text">请问：Fixture question</td></tr></table><input name="questionid" value="Q1"><input name="choice[]" value="2">A<br><input name="choice[]" value="4">B`
	tests := []struct {
		name, host, state string
		steps             []signinFixtureStep
		cache             *signinFixtureCache
		browser           bool
	}{
		{"CarPT", "carpt.net", "signed", steps(signinFixtureStep{"GET", "/attendance.php", "<p>签到成功</p>", nil, nil}), nil, false},
		{"HDFans", "hdfans.org", "signed", steps(signinFixtureStep{"GET", "/attendance.php", "<p>签到成功</p>", nil, nil}), nil, false},
		{"HDTime", "hdtime.org", "signed", steps(signinFixtureStep{"GET", "/attendance.php", "<p>签到成功</p>", nil, nil}), nil, false},
		{"HDCity", "hdcity.city", "signed", steps(signinFixtureStep{"GET", "/sign", "本次签到获得魅力 2", nil, nil}), nil, false},
		{"HaiDan", "www.haidan.video", "already", steps(signinFixtureStep{"GET", "/signin.php", `<input value="已经打卡">`, nil, nil}), nil, false},
		{"Hhanclub", "hhanclub.top", "signed", steps(signinFixtureStep{"GET", "/attendance.php", `<div class="rule-tips">今日签到排名：3 / 10</div>`, nil, nil}), nil, false},
		{"HDArea", "hdarea.club", "signed", steps(signinFixtureStep{"POST", "/sign_in.php", "此次签到您获得 3", nil, fixtureForm(url.Values{"action": {"sign_in"}})}), nil, false},
		{"Hares", "club.hares.top", "signed", steps(signinFixtureStep{"GET", "/", home, nil, nil}, signinFixtureStep{"GET", "/attendance.php?action=sign", `{"code":0,"msg":"签到成功"}`, nil, func(t *testing.T, r *http.Request, _ []byte) {
			if r.Header.Get("Accept") != "application/json" {
				t.Fatal("JSON acceptance lost")
			}
		}}), nil, false},
		{"PTerClub", "pterclub.com", "signed", steps(signinFixtureStep{"GET", "/attendance-ajax.php", `{"status":"1","message":"签到已成功"}`, nil, nil}), nil, false},
		{"HDChina", "hdchina.org", "signed", steps(signinFixtureStep{"GET", "/index.php", home + csrf, http.Header{"Set-Cookie": {"hdchina=new-fixture; Path=/", "SESSION=fresh-fixture; Path=/"}}, func(t *testing.T, r *http.Request, _ []byte) {
			if strings.Contains(r.Header.Get("Cookie"), "session=") {
				t.Fatal("old daily session was sent")
			}
		}}, signinFixtureStep{"POST", "/plugin_sign-in.php?cmd=signin", `{"state":"success","signindays":10,"integral":20}`, nil, func(t *testing.T, r *http.Request, raw []byte) {
			fixtureForm(url.Values{"csrf": {"fixture-csrf"}})(t, r, raw)
			if !strings.Contains(r.Header.Get("Cookie"), "hdchina=new-fixture") || !strings.Contains(r.Header.Get("Cookie"), "SESSION=fresh-fixture") {
				t.Fatal("new response session lost")
			}
		}}), nil, false},
		{"HDUpt", "pt.hdupt.com", "signed", steps(signinFixtureStep{"GET", "/", home, nil, nil}, signinFixtureStep{"POST", "/added.php?action=qiandao", ".23", nil, nil}), nil, false},
		{"TTG", "totheglory.im", "signed", steps(signinFixtureStep{"GET", "/", home + `<script>const x={signed_timestamp: 1793577600,signed_token: "fixture-token"}</script>`, nil, nil}, signinFixtureStep{"POST", "/signed.php", "您已连续签到 10 天", nil, fixtureForm(url.Values{"signed_timestamp": {"1793577600"}, "signed_token": {"fixture-token"}})}), nil, false},
		{"ZhuQue", "zhuque.in", "signed", steps(signinFixtureStep{"GET", "/", home + `<meta name="x-csrf-token" content="fixture-csrf">`, nil, nil}, signinFixtureStep{"POST", "/api/gaming/fireGenshinCharacterMagic", `{"status":200,"data":{"code":"FIRE_GENSHIN_CHARACTER_MAGIC_SUCCESS","bonus":0}}`, nil, func(t *testing.T, r *http.Request, raw []byte) {
			var v map[string]any
			if json.Unmarshal(raw, &v) != nil || v["all"] != float64(1) || v["resetModal"] != "true" || r.Header.Get("X-Csrf-Token") != "fixture-csrf" {
				t.Fatal("original skill request changed")
			}
		}}), nil, false},
		{"BTSchool", "pt.btschool.club", "signed", steps(signinFixtureStep{"GET", "/index.php", home + "魔力值 每日签到", nil, nil}, signinFixtureStep{"GET", "/index.php?action=addbonus", home + "魔力值 10", nil, nil}), nil, false},
		{"M-Team", "kp.m-team.cc", "authenticated", nil, nil, true},
		{"HDSky", "hdsky.me", "signed", steps(signinFixtureStep{"GET", "/", home, nil, nil}, signinFixtureStep{"POST", "/image_code_ajax.php", `{"success":true,"code":"fixture-imagehash"}`, nil, fixtureForm(url.Values{"action": {"new"}})}, signinFixtureStep{"GET", "/image.php?action=regimage&imagehash=fixture-imagehash", string(imageBytes), nil, nil}, signinFixtureStep{"POST", "/showup.php", `{"success":true}`, nil, fixtureForm(url.Values{"action": {"showup"}, "imagehash": {"fixture-imagehash"}, "imagestring": {"123456"}})}), nil, false},
		{"OpenCD", "www.open.cd", "signed", steps(signinFixtureStep{"GET", "/", home, nil, nil}, signinFixtureStep{"GET", "/plugin_sign-in.php", `<form id="frmSignin"><img src="/captcha.png"><input name="imagehash" value="fixture-imagehash"></form>`, nil, nil}, signinFixtureStep{"GET", "/captcha.png", string(imageBytes), nil, nil}, signinFixtureStep{"POST", "/plugin_sign-in.php?cmd=signin", `{"state":"success"}`, nil, fixtureForm(url.Values{"imagehash": {"fixture-imagehash"}, "imagestring": {"123456"}})}), nil, false},
		{"U2", "u2.dmhy.org", "signed", steps(signinFixtureStep{"GET", "/showup.php", home + `<form><table><tr><td><input name="req" value="R"><input name="hash" value="H"><input name="form" value="F"><input type="submit" name="a" value="A"><input type="submit" name="b" value="B"><input type="submit" name="c" value="C"><input type="submit" name="d" value="D"></td></tr></table></form>`, nil, nil}, signinFixtureStep{"POST", "/showup.php?action=show", `<script>window.location.href = 'showup.php';</script>`, nil, func(t *testing.T, _ *http.Request, raw []byte) {
			v, _ := url.ParseQuery(string(raw))
			if v.Get("req") != "R" || v.Get("hash") != "H" || v.Get("form") != "F" || v.Get("message") != "一切随缘~" || len(v) != 5 {
				t.Fatal("original U2 fields changed")
			}
			for _, key := range []string{"a", "b", "c", "d"} {
				if value := v.Get(key); value != "" && value != strings.ToUpper(key) {
					t.Fatal("submit name/value no longer paired")
				}
			}
		}}), nil, false},
		{"TJUPT", "www.tjupt.org", "signed", steps(signinFixtureStep{"GET", "/attendance.php", home + `<table class="captcha"><tr><td><img src="/captcha.png"></td></tr></table><input name="answer" value="a">Fixture A<br><input name="answer" value="b">Fixture B`, nil, nil}, signinFixtureStep{"GET", "/captcha.png", string(imageBytes), nil, nil}, signinFixtureStep{"POST", "/attendance.php", "这是您的首次签到，本次签到获得10个魔力值。", nil, fixtureForm(url.Values{"answer": {"a"}, "submit": {"提交"}})}), &signinFixtureCache{values: map[string][]string{hash: {"Fixture A"}}}, false},
		{"52PT", "52pt.site", "signed", steps(signinFixtureStep{"GET", "/bakatest.php", quiz, nil, nil}, signinFixtureStep{"POST", "/bakatest.php", "获得20点魔力值", nil, fixtureForm(url.Values{"questionid": {"Q1"}, "choice[]": {"2", "4"}, "usercomment": {"太难了！"}, "wantskip": {"不会"}})}), &signinFixtureCache{values: map[string][]string{"Fixture question": {"2", "4"}}}, false},
		{"CHDBits", "ptchdbits.co", "signed", steps(signinFixtureStep{"GET", "/bakatest.php", quiz, nil, nil}, signinFixtureStep{"POST", "/bakatest.php", "获得20点魔力值", nil, fixtureForm(url.Values{"questionid": {"Q1"}, "choice[]": {"2", "4"}, "usercomment": {"太难了！"}, "wantskip": {"不会"}})}), &signinFixtureCache{values: map[string][]string{"Fixture question": {"2", "4"}}}, false},
	}
	if len(tests) != 21 {
		t.Fatal("original adapter matrix incomplete")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &signinFixtureTransport{t: t, steps: test.steps}
			operations := 0
			executor := Executor{Target: Target{ID: 9007199254740993, Name: test.name, URL: "https://" + test.host + "/", Cookie: "session=fixture-cookie; hdchina=old-fixture", UserAgent: "fixture-signin-agent"}, Transport: transport, AuxTransport: transport, Guard: func(ctx context.Context) error { return ctx.Err() }, Before: func(_ context.Context, x Exchange) (func(string, string) error, error) {
				operations++
				return func(state, digest string) error {
					if state != "received" {
						t.Fatalf("unexpected receipt %s", state)
					}
					return nil
				}, nil
			}, Now: func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }, Location: time.UTC, Input: func(_ context.Context, c Challenge) ([]string, error) {
				if c.Kind != "captcha" || !bytes.Equal(c.Image, imageBytes) {
					t.Fatal("original captcha changed")
				}
				return []string{"123456"}, nil
			}}
			if test.cache != nil {
				executor.Cache = test.cache
			}
			if test.browser {
				executor.Browser = signinFixtureBrowser{render: func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
					if r.URL != executor.Target.URL || r.Cookie != executor.Target.Cookie {
						t.Fatal("original browser source changed")
					}
					ack, e := r.Before(ctx, browserruntime.Exchange{URL: r.URL, Method: "GET", Kind: "Document", Ordinal: 1})
					if e != nil {
						t.Fatal(e)
					}
					_ = ack("received", strings.Repeat("a", 64))
					return browserruntime.Result{HTML: []byte(home + "魔力值"), Requests: 1}, nil
				}}
			}
			out := executor.Run(context.Background())
			if out.State != test.state || !out.Authenticated || out.Retryable {
				t.Fatalf("result %+v expected %s", out, test.state)
			}
			if transport.count != len(test.steps) || operations == 0 {
				t.Fatal("incomplete request/receipt chain")
			}
			if test.cache != nil && test.cache.puts != 1 {
				t.Fatal("confirmed answer was not saved once")
			}
		})
	}
}

func TestAdapterDoesNotTurnFailureOrScriptIntoSuccess(t *testing.T) {
	for _, test := range []struct{ host, body string }{{"pterclub.com", `{"status":"0","message":"unexpected error"}`}, {"carpt.net", `<script>const template="签到成功"</script><p>请登录</p>`}, {"carpt.net", `<input type="password" name="password"><p>签到成功</p>`}} {
		t.Run(test.host+test.body, func(t *testing.T) {
			path := "/attendance.php"
			if test.host == "pterclub.com" {
				path = "/attendance-ajax.php"
			}
			transport := &signinFixtureTransport{t: t, steps: []signinFixtureStep{{"GET", path, test.body, nil, nil}}}
			out := (Executor{Target: Target{ID: 1, URL: "https://" + test.host + "/", Cookie: "session=fixture-cookie", UserAgent: "fixture-signin-agent"}, Transport: transport, Guard: func(context.Context) error { return nil }, Before: func(context.Context, Exchange) (func(string, string) error, error) {
				return func(string, string) error { return nil }, nil
			}}).Run(context.Background())
			if out.State == "signed" || out.State == "already" || out.State == "authenticated" || out.Retryable {
				t.Fatalf("false success/retry %+v", out)
			}
		})
	}
}
