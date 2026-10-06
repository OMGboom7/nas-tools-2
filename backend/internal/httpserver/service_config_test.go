package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func nativeServicesFixture(t *testing.T, extraYAML string, downloader *downloaderconfig.Downloader, transport http.RoundTripper) (http.Handler, string, string) {
	t.Helper()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	contents := "app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: native-services-secret\n" + extraYAML
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if downloader != nil {
		store, err := downloaderconfig.Open(filepath.Join(directory, "user.db"))
		if err != nil {
			t.Fatal(err)
		}
		created, err := store.Upsert(context.Background(), *downloader)
		_ = store.Close()
		if err != nil {
			t.Fatal(err)
		}
		downloader.ID = created.ID
		system, err := systemconfig.Open(filepath.Join(directory, "user.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := system.Set(context.Background(), "DefaultDownloader", strconv.FormatInt(created.ID, 10)); err != nil {
			t.Fatal(err)
		}
		_ = system.Close()
	}
	if transport == nil {
		transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			t.Errorf("unexpected legacy request: %s", request.URL.Path)
			return nil, fmt.Errorf("legacy backend disabled")
		})
	}
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000", ApplicationConfigPath: configPath}, transport)
	if err != nil {
		t.Fatal(err)
	}
	return handler, loginForTest(t, handler, "admin", "password"), filepath.Join(directory, "user.db")
}

func TestDownloaderConfigNeverReturnsCredentials(t *testing.T) {
	t.Parallel()
	downloader := downloaderconfig.Downloader{Name: "主下载器", Type: "qbittorrent", Enabled: 1, Transfer: 1, OnlyNastool: 1, RmtMode: "link", Config: `{"host":"https://admin:host-secret@qb.local:8080/api?token=hidden","port":"8080","username":"admin","password":"secret-password","torrent_management":"manual"}`, DownloadDir: `[{"save_path":"/downloads"}]`}
	handler, token, _ := nativeServicesFixture(t, "", &downloader, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/services/downloader/"+strconv.FormatInt(downloader.ID, 10), nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"admin", "secret-password", "host-secret", "token=hidden"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("credential %q leaked: %s", secret, response.Body.String())
		}
	}
	var result struct {
		Data downloaderConfigDetail `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Data.Host != "https://qb.local:8080/api" || !result.Data.UsernameConfigured || !result.Data.PasswordConfigured || !result.Data.DirectoriesConfigured || len(result.Data.Directories) != 1 || result.Data.Directories[0].SavePath != "/downloads" {
		t.Fatalf("unexpected detail: %#v", result.Data)
	}
}

func TestDownloadDirectoryValidationNormalizesRules(t *testing.T) {
	t.Parallel()
	items, message := validateDownloadDirectories([]downloadDirectory{
		{Type: " 电影 ", Category: " 华语 ", SavePath: " /downloads/movies ", ContainerPath: " /media/movies ", Label: " movie-hd "},
	}, "qbittorrent")
	if message != "" || len(items) != 1 || items[0]["type"] != "电影" || items[0]["category"] != "华语" || items[0]["label"] != "movie-hd" {
		t.Fatalf("unexpected normalized rules: %#v, %s", items, message)
	}
	items, message = validateDownloadDirectories([]downloadDirectory{{Type: "电视剧", Category: "欧美", Label: "ignored"}}, "transmission")
	if message != "" || items[0]["label"] != "" {
		t.Fatalf("non-qBittorrent label was not removed: %#v, %s", items, message)
	}
}

func TestDownloadDirectoryValidationRejectsDuplicateRules(t *testing.T) {
	t.Parallel()
	_, message := validateDownloadDirectories([]downloadDirectory{
		{Type: "动漫", Category: "日漫", SavePath: "/one"},
		{Type: "动漫", Category: "日漫", SavePath: "/two"},
	}, "qbittorrent")
	if message == "" {
		t.Fatal("expected duplicate rule validation error")
	}
}

func TestDownloaderOptionsAggregatesCategories(t *testing.T) {
	t.Parallel()
	downloader := downloaderconfig.Downloader{Name: "目录", Type: "qbittorrent", Config: `{}`, DownloadDir: `[{"type":"电影","category":"华语"},{"type":"电影","category":"欧美"},{"type":"电影","category":"华语"},{"type":"电视剧","category":"国产剧"},{"type":"动漫","category":"日漫"}]`}
	handler, token, _ := nativeServicesFixture(t, "", &downloader, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/services/downloader-options", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Data downloaderOptions `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Data.Categories["电影"]) != 2 || result.Data.Categories["电视剧"][0] != "国产剧" || result.Data.Categories["动漫"][0] != "日漫" {
		t.Fatalf("unexpected options: %#v", result.Data)
	}
}

func TestDownloaderUpdatePreservesWriteOnlyCredentialsAndDirectories(t *testing.T) {
	t.Parallel()
	downloader := downloaderconfig.Downloader{Name: "主下载器", Type: "qbittorrent", Enabled: 1, Transfer: 1, OnlyNastool: 1, RmtMode: "link", Config: `{"host":"https://admin:host-secret@qb.local:8080/api?token=hidden","port":"8080","username":"admin","password":"secret-password","torrent_management":"manual"}`, DownloadDir: `[{"save_path":"/downloads"}]`}
	handler, token, databasePath := nativeServicesFixture(t, "", &downloader, nil)
	body := `{"name":"主下载器","type":"qbittorrent","host":"https://qb.local:8080/api","port":"8080","username":"","password":"","secret":"","cookie":"","proxy":"","torrentManagement":"manual","rmtMode":"link","enabled":true,"transfer":true,"onlyNastool":true,"matchPath":true,"clearUsername":false,"clearPassword":false,"clearSecret":false,"clearCookie":false}`
	request := httptest.NewRequest(http.MethodPut, "/api/v1/services/downloader/"+strconv.FormatInt(downloader.ID, 10), strings.NewReader(body))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	store, err := downloaderconfig.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.Get(context.Background(), downloader.ID)
	if err != nil {
		t.Fatal(err)
	}
	var savedConfig map[string]any
	if json.Unmarshal([]byte(saved.Config), &savedConfig) != nil || savedConfig["username"] != "admin" || savedConfig["password"] != "secret-password" || savedConfig["host"] != "https://admin:host-secret@qb.local:8080/api?token=hidden" || !strings.Contains(saved.DownloadDir, "/downloads") || saved.MatchPath != 1 {
		t.Fatalf("credentials or directories were not preserved: %#v, %#v", saved, savedConfig)
	}
}

func TestDownloaderUpdateSavesDirectoryRules(t *testing.T) {
	t.Parallel()
	downloader := downloaderconfig.Downloader{Name: "主下载器", Type: "qbittorrent", Config: `{"host":"https://qb.local:8080/api","port":"8080","username":"admin","password":"secret-password","torrent_management":"manual"}`, DownloadDir: `[]`}
	handler, token, databasePath := nativeServicesFixture(t, "", &downloader, nil)
	body := `{"name":"主下载器","type":"qbittorrent","host":"https://qb.local:8080/api","port":"8080","username":"","password":"","secret":"","cookie":"","proxy":"","torrentManagement":"manual","rmtMode":"link","enabled":true,"transfer":true,"onlyNastool":true,"matchPath":true,"clearUsername":false,"clearPassword":false,"clearSecret":false,"clearCookie":false,"directories":[{"type":"电影","category":"华语","savePath":"/downloads/movies","containerPath":"/media/movies","label":"movie-hd"}]}`
	request := httptest.NewRequest(http.MethodPut, "/api/v1/services/downloader/"+strconv.FormatInt(downloader.ID, 10), strings.NewReader(body))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	store, err := downloaderconfig.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.Get(context.Background(), downloader.ID)
	if err != nil {
		t.Fatal(err)
	}
	var directories []map[string]string
	if json.Unmarshal([]byte(saved.DownloadDir), &directories) != nil || len(directories) != 1 || directories[0]["type"] != "电影" || directories[0]["category"] != "华语" || directories[0]["save_path"] != "/downloads/movies" || directories[0]["container_path"] != "/media/movies" || directories[0]["label"] != "movie-hd" {
		t.Fatalf("unexpected directories: %#v", directories)
	}
}

func TestMediaConfigNeverReturnsSecrets(t *testing.T) {
	t.Parallel()
	extra := "media:\n  media_server: plex\nplex:\n  host: 'https://user:host-pass@plex.local:32400/web?token=hidden'\n  play_host: https://app.plex.tv/desktop\n  token: plex-secret\n  servername: Home\n  username: owner@example.com\n  password: account-secret\n"
	handler, token, _ := nativeServicesFixture(t, extra, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/services/media/plex", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"host-pass", "token=hidden", "plex-secret", "owner@example.com", "account-secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("credential %q leaked: %s", secret, response.Body.String())
		}
	}
	var result struct {
		Data mediaConfigDetail `json:"data"`
	}
	_ = json.NewDecoder(response.Body).Decode(&result)
	if result.Data.Host != "https://plex.local:32400/web" || !result.Data.Active || !result.Data.TokenConfigured || !result.Data.UsernameConfigured || !result.Data.PasswordConfigured {
		t.Fatalf("unexpected detail: %#v", result.Data)
	}
}

func TestMediaUpdatePreservesAPIKeyAndActivatesServer(t *testing.T) {
	t.Parallel()
	extra := "media:\n  media_server: plex\njellyfin:\n  host: http://media.local:8096\n  api_key: hidden-key\n  play_host: http://play.local\n"
	handler, token, databasePath := nativeServicesFixture(t, extra, nil, nil)
	body := `{"host":"http://media.local:8096","playHost":"http://play.local","serverName":"","apiKey":"","token":"","username":"","password":"","activate":true,"clearApiKey":false,"clearToken":false,"clearUsername":false,"clearPassword":false}`
	request := httptest.NewRequest(http.MethodPut, "/api/v1/services/media/jellyfin", strings.NewReader(body))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	jellyfin := objectValue(snapshot["jellyfin"])
	media := objectValue(snapshot["media"])
	if text(jellyfin["api_key"]) != "hidden-key" || text(jellyfin["play_host"]) != "http://play.local" || text(media["media_server"]) != "jellyfin" {
		t.Fatalf("unexpected config: %#v", snapshot)
	}
}

func TestDownloaderCreateRejectsMissingWriteOnlyToken(t *testing.T) {
	t.Parallel()
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatal("legacy backend should not be called")
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	body := `{"name":"Aria","type":"aria2","host":"127.0.0.1","port":"6800","username":"","password":"","secret":"","cookie":"","proxy":"","torrentManagement":"","rmtMode":"link","enabled":true,"transfer":false,"onlyNastool":false,"matchPath":false,"clearUsername":false,"clearPassword":false,"clearSecret":false,"clearCookie":false}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/services/downloader", strings.NewReader(body))
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}
