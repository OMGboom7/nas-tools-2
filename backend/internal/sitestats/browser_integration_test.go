package sitestats

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
)

func TestStatisticsRealBrowserSessionFlowsIntoTNodeRawAPI(t *testing.T) {
	binary := os.Getenv("NASTOOL_TEST_BROWSER_BINARY")
	if binary == "" {
		t.Skip("set NASTOOL_TEST_BROWSER_BINARY for the real isolated browser integration")
	}
	engine := browserruntime.New(binary)
	if !engine.Available() {
		t.Fatal("configured native browser unavailable")
	}
	var profile, seeds atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!doctype html><html><head><title>Owned TNode fixture</title><meta name="x-csrf-token" content="fixture-native-csrf"></head><body>Powered By TNode<a href="logout.php">Logout</a><script>document.cookie='session=fixture-browser-acquired; path=/';</script></body></html>`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !strings.Contains(r.Header.Get("Cookie"), "initial=fixture-initial") || !strings.Contains(r.Header.Get("Cookie"), "session=fixture-browser-acquired") || r.Header.Get("X-CSRF-TOKEN") != "fixture-native-csrf" {
				t.Error("real browser session or API header lost", r.URL.Path, r.Header)
				w.WriteHeader(403)
				return
			}
			w.Header().Set("Content-Type", "application/json")
		}
		switch r.URL.Path {
		case "/api/user/getMainInfo":
			profile.Add(1)
			_, _ = w.Write([]byte(`{"status":200,"data":{"id":42,"username":"fixture-native-browser","class":{"name":"VIP"},"upload":"9007199254740993","download":1073741824,"bonus":"1.25","regTime":"2020-01-02"}}`))
		case "/api/user/listTorrentActivity":
			seeds.Add(1)
			_, _ = w.Write([]byte(`{"status":200,"data":{"total":1,"torrents":[{"size":1610612736,"seeding":3}]}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := &Client{Origin: server.URL, Cookie: "initial=fixture-initial", UserAgent: "fixture-native-browser-agent", Renderer: engine, Emulate: true, Guard: func(ctx context.Context) error { return ctx.Err() }}
	home, e := client.Home(ctx)
	if e != nil {
		t.Fatal(e)
	}
	v, e := Parse(ctx, client, home, Options{Site: "Native browser fixture", URL: server.URL})
	if e != nil || v.Username != "fixture-native-browser" || v.Upload != 9007199254740993 || v.Seeding != 1 || v.SeedingSize != 1610612736 || profile.Load() != 1 || seeds.Load() != 1 {
		t.Fatalf("snapshot=%+v profile=%d seeds=%d err=%v", v, profile.Load(), seeds.Load(), e)
	}
	if engine.Active() != 0 {
		t.Fatal("owned native browser still active")
	}
}
