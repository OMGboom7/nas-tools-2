package httpserver

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestNativeRSSItemDownloadRecordsSuccessfulSubmissions(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	adds, fetches := 0, 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "tracker.example":
			fetches++
			if request.Header.Get("Cookie") != "session=private" || request.Header.Get("User-Agent") != "RSS Agent" {
				t.Fatalf("site headers=%v", request.Header)
			}
			return jsonResponse(request, string(testTorrent)), nil
		case "qb.local:8080":
			if request.Header.Get("Cookie") == "session=private" {
				t.Fatal("site cookie leaked to downloader")
			}
			if strings.HasSuffix(request.URL.Path, "/auth/login") {
				return jsonResponse(request, "Ok."), nil
			}
			adds++
			if strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/") {
				if err := request.ParseMultipartForm(1 << 20); err != nil {
					t.Fatal(err)
				}
				file, _, err := request.FormFile("torrents")
				if err != nil {
					t.Fatal(err)
				}
				contents, err := io.ReadAll(file)
				file.Close()
				if err != nil || string(contents) != string(testTorrent) {
					t.Fatalf("torrent=%q err=%v", contents, err)
				}
			} else {
				if err := request.ParseForm(); err != nil || request.Form.Get("urls") != magnet {
					t.Fatalf("magnet form=%v err=%v", request.Form, err)
				}
			}
			if request.Form.Get("savepath") != "/downloads/rss" || request.Form.Get("category") != "movies" || request.Form.Get("paused") != "true" || request.Form.Get("tags") != "rss" {
				t.Fatalf("settings=%v", request.Form)
			}
			return jsonResponse(request, "Ok."), nil
		default:
			t.Fatalf("RSS download called Python or unexpected host: %s", request.URL)
			return nil, fmt.Errorf("unexpected host")
		}
	})
	downloader := downloaderconfig.Downloader{Name: "qB", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.local","port":8080,"username":"admin","password":"secret"}`}
	handler, token, path := nativeServicesFixture(t, "", &downloader, transport)
	downloaders, err := downloaderconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	setting, err := downloaders.UpsertSetting(context.Background(), downloaderconfig.DownloadSetting{Category: "movies", Tags: "rss", Paused: 1, DownloaderID: text(downloader.ID)})
	if err != nil {
		t.Fatal(err)
	}
	downloaders.Close()
	sites, err := siteconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sites.Upsert(context.Background(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.example/signin", Cookie: "session=private", Note: `{"ua":"RSS Agent"}`}); err != nil {
		t.Fatal(err)
	}
	sites.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO CONFIG_USER_RSS (ID,NAME,ADDRESS,PARSER,USES,SAVE_PATH,DOWNLOAD_SETTING) VALUES (7,'Task','[]','[]','D','/downloads/rss',?)`, setting.ID); err != nil {
		t.Fatal(err)
	}
	articles := fmt.Sprintf(`[{"title":"Movie One","enclosure":%q,"year":"2026"},{"title":"Movie Two","enclosure":"https://tracker.example/download/2","year":"2025"}]`, magnet)
	if got := performFormRequest(handler, "/api/v1/rss/item/download", "", url.Values{"taskid": {"7"}, "articles": {articles}}); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated=%d", got.Code)
	}
	got := performFormRequest(handler, "/api/v1/rss/item/download", token, url.Values{"taskid": {"7"}, "articles": {articles}})
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"code":0`) || adds != 2 || fetches != 1 {
		t.Fatalf("download=%d %s adds=%d fetches=%d", got.Code, got.Body.String(), adds, fetches)
	}
	var statuses, history int
	if err := db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&statuses); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM USERRSS_TASK_HISTORY WHERE TASK_ID='7' AND DOWNLOADER='qB'`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if statuses != 2 || history != 2 {
		t.Fatalf("statuses=%d history=%d", statuses, history)
	}
	repeated := performFormRequest(handler, "/api/v1/rss/item/download", token, url.Values{"taskid": {"7"}, "articles": {articles}})
	if repeated.Code != 200 || adds != 2 || fetches != 1 {
		t.Fatal("manual/automatic RSS deduplication failed", repeated.Code, adds, fetches)
	}
	bad := performFormRequest(handler, "/api/v1/rss/item/download", token, url.Values{"taskid": {"7"}, "articles": {`[{"title":"X","enclosure":"file:///etc/passwd"}]`}})
	if bad.Code != http.StatusBadRequest || adds != 2 {
		t.Fatalf("invalid enclosure=%d adds=%d", bad.Code, adds)
	}
}

func TestRSSItemDownloadWithoutNativeConfigurationNeverCallsPython(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("called Python: %s", request.URL)
		return nil, http.ErrNotSupported
	})
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	got := performFormRequest(handler, "/api/v1/rss/item/download", "test-token", url.Values{"taskid": {"1"}, "articles": {`[{"title":"X","enclosure":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}]`}})
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("without config=%d %s", got.Code, got.Body.String())
	}
}

func TestRSSItemDownloadUnsupportedSettingsRejectBeforeSubmit(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected download: %s", request.URL)
		return nil, http.ErrNotSupported
	})
	downloader := downloaderconfig.Downloader{Name: "Transmission", Type: "transmission", Enabled: 1, Config: `{"host":"tr.local","port":9091}`}
	handler, token, path := nativeServicesFixture(t, "", &downloader, transport)
	settings, err := downloaderconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	setting, err := settings.UpsertSetting(context.Background(), downloaderconfig.DownloadSetting{Category: "unsupported-category", DownloaderID: text(downloader.ID)})
	settings.Close()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO CONFIG_USER_RSS (ID,NAME,USES,DOWNLOAD_SETTING) VALUES (8,'RSS','D',?)`, setting.ID); err != nil {
		t.Fatal(err)
	}
	got := performFormRequest(handler, "/api/v1/rss/item/download", token, url.Values{"taskid": {"8"}, "articles": {`[{"title":"Movie","enclosure":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}]`}})
	if got.Code != http.StatusNotImplemented {
		t.Fatalf("unsupported settings=%d %s", got.Code, got.Body.String())
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM USERRSS_TASK_HISTORY`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("history=%d err=%v", count, err)
	}
}
