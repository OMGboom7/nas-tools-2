package browserruntime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeChromeCheckinOneClickAndAlreadySigned(t *testing.T) {
	engine := New("auto")
	if !engine.Available() {
		t.Skip("native Chrome/Chromium is not installed")
	}
	for _, initialAlready := range []bool{false, true} {
		t.Run(fmt.Sprint(initialAlready), func(t *testing.T) {
			var mutations, actions, receipts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cookie, e := r.Cookie("session")
				if e != nil || cookie.Value != "fixture-session" {
					http.Error(w, "no original cookie", 403)
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				if r.URL.Path == "/attendance.php" {
					mutations.Add(1)
					fmt.Fprint(w, `<html><body><a href="logout.php">退出</a><p>今日已签到</p></body></html>`)
					return
				}
				if r.URL.Path != "/" {
					http.NotFound(w, r)
					return
				}
				if initialAlready {
					fmt.Fprint(w, `<html><body><a href="logout.php">退出</a><p>今日已签到</p><a id="do-attendance" href="attendance.php">签到</a></body></html>`)
				} else {
					fmt.Fprint(w, `<html><body><a href="logout.php">退出</a><a id="do-attendance" href="attendance.php">签到</a></body></html>`)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			c := &Checkin{LoggedIn: func(raw []byte) bool { return strings.Contains(string(raw), "logout.php") }, Already: func(raw []byte) bool { return strings.Contains(string(raw), "今日已签到") }, Completed: func(before, after []byte) bool {
				return !strings.Contains(string(before), "今日已签到") && strings.Contains(string(after), "今日已签到")
			}, Action: func(context.Context, string) (func(string, string) error, error) {
				actions.Add(1)
				return func(state, digest string) error {
					if state != "received" || digest == "" {
						t.Error("DOM action did not acknowledge")
					}
					return nil
				}, nil
			}}
			result, e := engine.Render(ctx, Request{URL: server.URL + "/", Cookie: "session=fixture-session", UserAgent: "fixture-chrome-agent", Assets: true, Checkin: c, Guard: func(ctx context.Context) error { return ctx.Err() }, Before: func(_ context.Context, x Exchange) (func(string, string) error, error) {
				if Origin(x.URL) != Origin(server.URL) {
					return nil, ErrScope
				}
				return func(state, digest string) error {
					if state != "received" || digest == "" {
						t.Error("request receipt missing")
					}
					receipts.Add(1)
					return nil
				}, nil
			}})
			if e != nil {
				t.Fatal(e)
			}
			want := "signed"
			count := int32(1)
			if initialAlready {
				want = "already"
				count = 0
			}
			if result.CheckinState != want || actions.Load() != count || mutations.Load() != count || receipts.Load() < 1 {
				t.Fatalf("result=%s action=%d effect=%d receipts=%d", result.CheckinState, actions.Load(), mutations.Load(), receipts.Load())
			}
		})
	}
}
