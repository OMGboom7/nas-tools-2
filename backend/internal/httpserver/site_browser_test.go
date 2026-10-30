package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/searchcache"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type browserStub func(context.Context, browserruntime.Request) (browserruntime.Result, error)

func (browserStub) Available() bool { return true }
func (f browserStub) Render(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
	return f(ctx, r)
}

func TestNativeBrowserSearchPrivateDownloadAndChangedScope(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if e := os.WriteFile(cfgPath, []byte("app:\n  user_agent: app-agent\n"), 0600); e != nil {
		t.Fatal(e)
	}
	cfg := config.NewStore(cfgPath)
	sites, e := siteconfig.Open(filepath.Join(dir, "user.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer sites.Close()
	site, e := sites.Upsert(t.Context(), siteconfig.Site{Name: "Browser Tracker", SignURL: "https://tracker.test/", Cookie: "saved=private", Note: `{"chrome":"Y","ua":"saved-agent"}`})
	if e != nil {
		t.Fatal(e)
	}
	system, e := systemconfig.Open(filepath.Join(dir, "user.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer system.Close()
	for k, v := range map[string]string{"UserInstalledPlugins": `["OpenSubtitles","Webhook"]`, "UserIndexerSites": `["rendered"]`} {
		if e := system.Set(t.Context(), k, v); e != nil {
			t.Fatal(e)
		}
	}
	catalog := `{"indexer":[{"id":"rendered","name":"Original","domain":"https://tracker.test/","search":{"paths":[{"path":"search","method":"chrome","params":{"keyword":"//*[@id='q']","submit":"//*[@id='go']","script":"return true;"}}]},"torrents":{"list":{"selector":"table.results tr"},"fields":{"title":{"selector":"a.title"},"download":{"selector":"a.download","attribute":"href"},"size":{"text":"1 GB"},"seeders":{"text":"2"}}}}],"conf":{}}`
	catalogPath := filepath.Join(dir, "catalog.bin")
	if e := os.WriteFile(catalogPath, []byte(base64.StdEncoding.EncodeToString([]byte(catalog))), 0600); e != nil {
		t.Fatal(e)
	}
	renders, downloads := 0, 0
	renderer := browserStub(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
		renders++
		if r.Form == nil || r.Form.Value != "Movie & Query" || r.ReadOnly || r.Cookie != "saved=private" || r.UserAgent != "saved-agent" {
			t.Error("original search scope lost")
		}
		if e := r.Guard(ctx); e != nil {
			return browserruntime.Result{}, e
		}
		finish, e := r.Before(ctx, browserruntime.Exchange{Ordinal: 1, URL: r.URL, Method: "GET", Kind: "Document"})
		if e != nil {
			return browserruntime.Result{}, e
		}
		if e = finish("received", browserruntime.Hash("page")); e != nil {
			return browserruntime.Result{}, e
		}
		return browserruntime.Result{URL: r.URL, UserAgent: r.UserAgent, Cookie: "saved=private; rendered=owned", HTML: []byte(`<table class="results"><tr><td><a class="title">Movie.2026</a><a class="download" href="/download/1">torrent</a></td></tr></table>`)}, nil
	})
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		downloads++
		if r.URL.String() != "https://tracker.test/download/1" || r.Header.Get("Cookie") != "saved=private; rendered=owned" || r.Header.Get("User-Agent") != "saved-agent" {
			t.Error("rendered private session not retained")
		}
		return jsonResponse(r, string(testTorrent)), nil
	})
	search := &nativeExternalResourceSearch{system: system, sites: sites, config: cfg, catalogPath: catalogPath, browser: renderer, transport: transport, siteLimits: &siteRequestLimiter{}}
	resources, e := search.fetchResources(t.Context(), "Movie & Query", nil)
	if e != nil || len(resources) != 1 || resources[0].Browser == nil || renders != 1 || downloads != 0 {
		t.Fatal("browser search failed", e, len(resources), renders, downloads)
	}
	resource := resources[0]
	public, _ := json.Marshal(resource)
	if strings.Contains(string(public), "private") || strings.Contains(string(public), "owned") || strings.Contains(string(public), "/download/") {
		t.Fatal("public resource leaked private data")
	}
	options, handled, e := (subscriptionService{configStore: cfg, systemConfig: system, sites: sites, catalogPath: catalogPath}).nativeIndexerOptions(t.Context())
	if e != nil || !handled || options == nil {
		t.Fatal("native non-indexer plugins forced fallback", e, handled)
	}
	cache := searchcache.New()
	ids, e := cache.Put("fixture", resources)
	if e != nil {
		t.Fatal(e)
	}
	copy, e := cache.Read("fixture", ids[0])
	if e != nil {
		t.Fatal(e)
	}
	copy.Browser.Cookie = "modified"
	original, e := cache.Read("fixture", ids[0])
	if e != nil || original.Browser.Cookie != resource.Browser.Cookie {
		t.Fatal("cache session was mutable", e)
	}
	candidate := fuzzyCandidateRecord{Resource: resource}
	encoded, e := json.Marshal(candidate)
	if e != nil {
		t.Fatal(e)
	}
	var restored fuzzyCandidateRecord
	if e = json.Unmarshal(encoded, &restored); e != nil || restored.Resource.Browser == nil || fuzzyDigest(restored) != fuzzyDigest(candidate) {
		t.Fatal("private durable session lost", e)
	}
	download := downloadService{client: &http.Client{Transport: transport}, sites: sites, configStore: cfg, siteLimits: &siteRequestLimiter{}}
	if magnet, body, e := download.prepareNativeSearchTorrent(t.Context(), restored.Resource); e != nil || magnet != "" || string(body) != string(testTorrent) || downloads != 1 {
		t.Fatal("download continuation failed", e, downloads)
	}
	current, e := sites.Get(t.Context(), site.ID)
	if e != nil || current.Cookie != "saved=private" || current.Note != site.Note {
		t.Fatal("render overwrote saved credentials", e)
	}
	expired := resource
	v := *resource.Browser
	expired.Browser = &v
	v.Created = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, _, e = download.prepareNativeSearchTorrent(t.Context(), expired); !errors.Is(e, browserruntime.ErrScope) || downloads != 1 {
		t.Fatal("expired session sent", e)
	}
	site.Cookie = "new=configuration"
	if _, e = sites.Upsert(t.Context(), site); e != nil {
		t.Fatal(e)
	}
	if _, _, e = download.prepareNativeSearchTorrent(t.Context(), resource); !errors.Is(e, browserruntime.ErrScope) || downloads != 1 {
		t.Fatal("changed site adopted old session", e)
	}
}

func TestNativeBrowserConnectionRenderedLoginAndCurrentRecordGuard(t *testing.T) {
	for _, kind := range []string{"logged-in", "login-page", "challenge", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			site := siteconfig.Site{Cookie: "fixture=private", SignURL: "https://tracker.test", Note: `{"chrome":"Y","ua":"fixture-agent"}`}
			var renderer browserruntime.Renderer
			calls := 0
			if kind != "unavailable" {
				renderer = browserStub(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
					calls++
					if r.URL != "https://tracker.test/" || !r.ReadOnly || r.Cookie != site.Cookie {
						t.Error("wrong browser request")
					}
					if kind == "challenge" {
						return browserruntime.Result{}, browserruntime.ErrVerification
					}
					body := `<a href="/logout.php">退出</a>`
					if kind == "login-page" {
						body = `<input type="password">`
					}
					return browserruntime.Result{HTML: []byte(body)}, nil
				})
			}
			ok, message, unsupported := checkSiteConnectionAdmitted(t.Context(), site, nil, nil, nil, renderer)
			if ok != (kind == "logged-in") || unsupported != (kind == "unavailable") || strings.Contains(message, "private") {
				t.Fatal(ok, message, unsupported)
			}
			if kind != "unavailable" && calls != 1 {
				t.Fatal(calls)
			}
		})
	}
}

func TestNativeBrowserRenderRejectsConfigurationReplacementBeforePublish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := []byte("app:\n  user_agent: before\n")
	if e := os.WriteFile(path, initial, 0600); e != nil {
		t.Fatal(e)
	}
	cfg := config.NewStore(path)
	snapshot, e := cfg.Snapshot()
	if e != nil {
		t.Fatal(e)
	}
	renderer := browserStub(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
		if e := r.Guard(ctx); e != nil {
			return browserruntime.Result{}, e
		}
		if e := os.WriteFile(path, []byte("app:\n  user_agent: changed\n"), 0600); e != nil {
			t.Fatal(e)
		}
		return browserruntime.Result{URL: r.URL, UserAgent: r.UserAgent, HTML: []byte("<p>stale</p>")}, nil
	})
	if _, _, e := (siteBrowser{renderer: renderer, config: cfg}).render(t.Context(), siteconfig.Site{}, snapshot, "https://site.test/", nil, "", "", nil); !errors.Is(e, browserruntime.ErrScope) {
		t.Fatal("changed settings published stale page", e)
	}
}

func TestBrowserSessionPublicCodecRemainsUnchangedWithoutSession(t *testing.T) {
	record := fuzzyCandidateRecord{Resource: externalindexer.Resource{Title: "legacy", DownloadURL: testMagnet}}
	raw, e := json.Marshal(record)
	if e != nil || strings.Contains(string(raw), `"Browser"`) {
		t.Fatal("legacy private document changed", e, string(raw))
	}
}

func TestNativeBrowserRejectsUnhashableSavedConfigurationBeforeRendering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("app:\n  user_agent: fixture\nunknown:\n  value: .nan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewStore(path)
	snapshot, err := cfg.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	renderer := browserStub(func(context.Context, browserruntime.Request) (browserruntime.Result, error) {
		calls++
		return browserruntime.Result{}, nil
	})
	_, _, err = (siteBrowser{renderer: renderer, config: cfg}).render(t.Context(), siteconfig.Site{}, snapshot, "https://site.test/", nil, "", "", nil)
	if !errors.Is(err, browserruntime.ErrScope) || calls != 0 {
		t.Fatal("unhashable settings started browser", err, calls)
	}
}
