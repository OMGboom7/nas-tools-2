package sitesignin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
)

func TestTJUPTReadsAndWritesOriginalOptionValueCache(t *testing.T) {
	for _, old := range []string{"2", "Fixture movie title"} {
		t.Run(old, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "temp", "signin", "tjupt.json")
			if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
				t.Fatal(e)
			}
			hash := strings.Repeat("0000011111", 10)
			quoted, _ := json.Marshal(old)
			raw := `{"` + hash + `":` + string(quoted) + `,"future":{"big":9007199254740993,"null":null}}`
			if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
				t.Fatal(e)
			}
			transport := &signinFixtureTransport{t: t, steps: []signinFixtureStep{
				{"GET", "/attendance.php", `<a href="logout.php">退出</a><table class="captcha"><tr><td><img src="/captcha.png"></td></tr></table><input name="answer" value="1">Other title<br><input name="answer" value="2">Fixture movie title`, nil, nil},
				{"GET", "/captcha.png", string(signinFixtureImage(t)), http.Header{"Content-Type": {"image/png"}}, nil},
				{"POST", "/attendance.php", "这是您的首次签到，本次签到获得10个魔力值。", nil, fixtureForm(url.Values{"answer": {"2"}, "submit": {"提交"}})},
			}}
			executor := Executor{Target: Target{ID: 1, Name: "Fixture", URL: "https://www.tjupt.org/", Cookie: "session=fixture-cookie", UserAgent: "fixture-signin-agent"}, Transport: transport, Cache: &FileCache{Directory: dir}, Guard: func(ctx context.Context) error { return ctx.Err() }, Before: func(context.Context, Exchange) (func(string, string) error, error) {
				return func(string, string) error { return nil }, nil
			}}
			out := executor.Run(t.Context())
			if out.State != "signed" || transport.count != 3 {
				t.Fatalf("original option-value answer was not used: %+v requests=%d", out, transport.count)
			}
			saved, e := os.ReadFile(path)
			var doc map[string]json.RawMessage
			var future map[string]json.RawMessage
			if e != nil || json.Unmarshal(saved, &doc) != nil || string(doc[hash]) != `"2"` || json.Unmarshal(doc["future"], &future) != nil || string(future["big"]) != "9007199254740993" || string(future["null"]) != "null" {
				t.Fatalf("original cache encoding or unknown values changed: %s %v", saved, e)
			}
		})
	}
}

func TestGenericBrowserRecognizesOriginalEarnedSignInMarker(t *testing.T) {
	for _, initial := range []bool{true, false} {
		t.Run(map[bool]string{true: "already-does-not-click", false: "post-click-is-confirmed"}[initial], func(t *testing.T) {
			actions := 0
			before := []byte(`<a href="logout.php">退出</a><a id="signed" href="attendance.php">签到</a>`)
			after := []byte(`<a href="logout.php">退出</a><p>[签到已得125, 补签卡: 0]</p>`)
			executor := Executor{Target: Target{ID: 1, Name: "Fixture", URL: "https://generic.fixture.test/", Cookie: "session=fixture-cookie", UserAgent: "fixture-signin-agent", Browser: true}, Guard: func(ctx context.Context) error { return ctx.Err() }, Before: func(_ context.Context, x Exchange) (func(string, string) error, error) {
				if x.Action {
					actions++
				}
				return func(string, string) error { return nil }, nil
			}}
			executor.Browser = signinFixtureBrowser{render: func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
				state := "unknown"
				if initial {
					before = append(before, after...)
				}
				if r.Checkin.Already(before) {
					state = "already"
				} else {
					done, e := r.Checkin.Action(ctx, "checkin-click")
					if e != nil {
						return browserruntime.Result{}, e
					}
					if e = done("received", strings.Repeat("a", 64)); e != nil {
						return browserruntime.Result{}, e
					}
					if r.Checkin.Completed(before, after) || r.Checkin.Already(after) {
						state = "signed"
					}
				}
				return browserruntime.Result{HTML: after, CheckinState: state}, nil
			}}
			out := executor.Run(t.Context())
			want, count := "signed", 1
			if initial {
				want, count = "already", 0
			}
			if out.State != want || actions != count {
				t.Fatalf("original result marker ignored: %+v actions=%d", out, actions)
			}
		})
	}
}
