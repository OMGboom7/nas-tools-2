package httpserver

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/auth"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/searchcache"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func searchSettingsFixture(t *testing.T, transport http.RoundTripper) (downloadService, string) {
	t.Helper()
	item := downloaderconfig.Downloader{Name: "Default", Type: "qbittorrent", Enabled: 1, Config: `{"host":"default.local","port":8080,"username":"admin","password":"secret"}`, DownloadDir: `[{"save_path":"/downloads/default"}]`}
	_, token, path := nativeServicesFixture(t, "", &item, transport)
	appPath := filepath.Join(filepath.Dir(path), "config.yaml")
	application, err := config.LoadApplication(appPath)
	if err != nil {
		t.Fatal(err)
	}
	authService, err := auth.NewService(application)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := downloaderconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clients.Close() })
	system, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { system.Close() })
	sites, err := siteconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sites.Close() })
	return downloadService{client: &http.Client{Transport: transport}, auth: &nativeAuthentication{service: authService}, resources: searchcache.New(), downloaders: clients, systemConfig: system, configStore: config.NewStore(appPath), sites: sites}, token
}

func TestSearchDownloadSettingsPriorityAndConfiguredDirectories(t *testing.T) {
	service, _ := searchSettingsFixture(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("settings must not issue requests: %s", r.URL)
		return nil, nil
	}))
	ctx := context.Background()
	other, err := service.downloaders.Upsert(ctx, downloaderconfig.Downloader{Name: "Other", Type: "qbittorrent", Enabled: 1, DownloadDir: `[{"save_path":"/downloads/other"}]`})
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := service.downloaders.UpsertSetting(ctx, downloaderconfig.DownloadSetting{Name: "Default", Category: "default", DownloaderID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	siteSetting, err := service.downloaders.UpsertSetting(ctx, downloaderconfig.DownloadSetting{Name: "Site", Category: "site", Tags: "site-setting", DownloaderID: strconv.FormatInt(other.ID, 10)})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := service.downloaders.UpsertSetting(ctx, downloaderconfig.DownloadSetting{Name: "Explicit", Category: "explicit", DownloaderID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.systemConfig.Set(ctx, "DefaultDownloadSetting", strconv.FormatInt(defaults.ID, 10)); err != nil {
		t.Fatal(err)
	}
	site, err := service.sites.Upsert(ctx, siteconfig.Site{Name: "Tracker", Note: `{"download_setting":"` + strconv.FormatInt(siteSetting.ID, 10) + `","tags":"site-tag"}`})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, site, setting, path, category string
		downloader                          int64
		tags                                string
		invalid                             bool
	}{
		{"default", "Unknown", "", "", "default", 1, "", false},
		{"site", "Tracker", "", "/downloads/other", "site", other.ID, "site-setting;site-tag", false},
		{"explicit", "Tracker", strconv.FormatInt(explicit.ID, 10), "/downloads/default", "explicit", 1, "site-tag", false},
		{"preset", "Tracker", "-1", "", "", 1, "NASTOOL;site-tag", false},
		{"bypass", "Tracker", "-2", "", "", 1, "site-tag", false},
		{"removed setting fallback", "Tracker", "999999", "", "default", 1, "site-tag", false},
		{"wrong downloader directory", "Tracker", strconv.FormatInt(explicit.ID, 10), "/downloads/other", "", 0, "", true},
		{"arbitrary directory", "Unknown", "", "/etc", "", 0, "", true},
		{"control character", "Unknown", "", "/downloads/default\n", "", 0, "", true},
		{"invalid ID", "Unknown", "bad", "", "", 0, "", true},
		{"zero ID", "Unknown", "0", "", "", 0, "", true},
		{"negative ID", "Unknown", "-3", "", "", 0, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			downloader, options, err := service.searchDownloadSettings(ctx, test.site, addResourceRequest{Setting: test.setting, Directory: test.path})
			if test.invalid {
				if !errors.Is(err, errSearchDownloadSelection) {
					t.Fatal(err)
				}
				return
			}
			if err != nil || downloader.ID != test.downloader || options.Category != test.category || options.SavePath != test.path || strings.Join(options.Tags, ";") != test.tags {
				t.Fatal(downloader, options, err)
			}
		})
	}
	other.Enabled = 0
	if _, err := service.downloaders.Upsert(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.searchDownloadSettings(ctx, "Tracker", addResourceRequest{}); err == nil {
		t.Fatal("disabled selected downloader accepted")
	}
	site.Note = `{"tags":[]}`
	if _, err := service.sites.Upsert(ctx, site); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.searchDownloadSettings(ctx, "Tracker", addResourceRequest{Setting: "-1"}); err == nil {
		t.Fatal("invalid site settings accepted")
	}
	if err := service.systemConfig.Set(ctx, "DefaultDownloadSetting", "not-an-id"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.searchDownloadSettings(ctx, "Unknown", addResourceRequest{}); err == nil {
		t.Fatal("bad default accepted")
	}
	if err := service.systemConfig.Set(ctx, "DefaultDownloadSetting", "999999"); err != nil {
		t.Fatal(err)
	}
	if downloader, options, err := service.searchDownloadSettings(ctx, "Unknown", addResourceRequest{}); err != nil || downloader.ID != 1 || strings.Join(options.Tags, ";") != "NASTOOL" {
		t.Fatal("missing default must fall back to preset", downloader, options, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := service.searchDownloadSettings(cancelled, "Unknown", addResourceRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCachedSearchDownloadAppliesSelectedSettingsWithoutPython(t *testing.T) {
	adds := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "selected.local" {
			t.Fatalf("unexpected upstream; Python and default downloader prohibited: %s", r.URL)
		}
		if strings.HasSuffix(r.URL.Path, "/auth/login") {
			return jsonResponse(r, "Ok."), nil
		}
		if r.URL.Path != "/api/v2/torrents/add" || r.ParseForm() != nil {
			t.Fatal(r.URL)
		}
		for key, want := range map[string]string{"urls": testMagnet, "savepath": "/downloads/selected", "category": "tv", "tags": "setting-tag,site-tag", "paused": "true", "upLimit": "10240", "dlLimit": "20480", "ratioLimit": "1.5", "seedingTimeLimit": "60"} {
			if r.Form.Get(key) != want {
				t.Fatalf("%s: got %q want %q", key, r.Form.Get(key), want)
			}
		}
		adds++
		return jsonResponse(r, "Ok."), nil
	})
	service, token := searchSettingsFixture(t, transport)
	ctx := context.Background()
	selected, err := service.downloaders.Upsert(ctx, downloaderconfig.Downloader{Name: "Selected", Type: "qbittorrent", Enabled: 1, Config: `{"host":"selected.local","port":8080,"username":"admin","password":"secret"}`, DownloadDir: `[{"save_path":"/downloads/selected"}]`})
	if err != nil {
		t.Fatal(err)
	}
	setting, err := service.downloaders.UpsertSetting(ctx, downloaderconfig.DownloadSetting{Name: "TV", DownloaderID: strconv.FormatInt(selected.ID, 10), Category: "tv", Tags: "setting-tag", Paused: 1, UploadLimit: 10, DownloadLimit: 20, RatioLimit: 150, SeedingTimeLimit: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.sites.Upsert(ctx, siteconfig.Site{Name: "Tracker", Note: `{"tags":"site-tag"}`}); err != nil {
		t.Fatal(err)
	}
	ids, err := service.resources.Put("0:admin", []externalindexer.Resource{{Indexer: "Tracker", Title: "Movie", DownloadURL: testMagnet}})
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(service.addResource)
	base := `{"resourceId":"` + ids[0] + `","setting":"` + strconv.FormatInt(setting.ID, 10) + `","directory":"`
	bad := nativeJSONRequest(handler, "POST", "/api/v1/downloads/resource", token, base+`/unconfigured"}`)
	if bad.Code != 400 || adds != 0 {
		t.Fatal(bad.Code, bad.Body.String(), adds)
	}
	for i := 0; i < 2; i++ {
		response := nativeJSONRequest(handler, "POST", "/api/v1/downloads/resource", token, base+`/downloads/selected"}`)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if adds != 1 {
		t.Fatal("duplicate settings submission", adds)
	}
}

func TestSearchDownloadOptionsUnsupportedBeforeFetching(t *testing.T) {
	for _, test := range []struct {
		kind    string
		options downloadAddOptions
		want    bool
	}{
		{"qbittorrent", downloadAddOptions{Category: "tv", Tags: []string{"tag"}, RatioLimit: 1}, true},
		{"transmission", downloadAddOptions{SavePath: "/downloads", Tags: []string{"tag"}, Paused: true}, true},
		{"transmission", downloadAddOptions{RatioLimit: 1}, false},
		{"aria2", downloadAddOptions{SavePath: "/downloads", UploadLimitKB: 10}, true},
		{"aria2", downloadAddOptions{Tags: []string{"tag"}}, false},
		{"pan115", downloadAddOptions{}, true},
		{"pan115", downloadAddOptions{SavePath: "/downloads"}, false},
	} {
		if got := searchDownloadOptionsSupported(test.kind, test.options); got != test.want {
			t.Fatal(test.kind, test.options, got)
		}
	}
	service, token := searchSettingsFixture(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unsupported options must fail before fetching torrent or calling downloader: %s", r.URL)
		return nil, nil
	}))
	ctx := context.Background()
	aria, err := service.downloaders.Upsert(ctx, downloaderconfig.Downloader{Name: "Aria", Type: "aria2", Enabled: 1})
	if err != nil {
		t.Fatal(err)
	}
	setting, err := service.downloaders.UpsertSetting(ctx, downloaderconfig.DownloadSetting{Name: "Unsupported", DownloaderID: strconv.FormatInt(aria.ID, 10), Category: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := service.resources.Put("0:admin", []externalindexer.Resource{{Title: "Movie", DownloadURL: "https://tracker.local/download?private=hidden"}})
	if err != nil {
		t.Fatal(err)
	}
	response := nativeJSONRequest(http.HandlerFunc(service.addResource), "POST", "/api/v1/downloads/resource", token, `{"resourceId":"`+ids[0]+`","setting":"`+strconv.FormatInt(setting.ID, 10)+`"}`)
	if response.Code != 501 || strings.Contains(response.Body.String(), "hidden") {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, done, err := service.resources.Claim("0:admin", ids[0]); err != nil || done {
		t.Fatal("failed selection consumed cached resource", done, err)
	}
}
