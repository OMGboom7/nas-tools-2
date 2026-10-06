package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

func TestServicesOverviewWhitelistsCredentials(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("service overview reached legacy backend: %s", request.URL)
		return nil, nil
	})
	extra := "media:\n  media_server: jellyfin\njellyfin:\n  host: 'https://user:password@media.local:8096/private?token=secret'\n  api_key: secret-media-key\nemby:\n  host: ''\nplex:\n  token: secret-plex-token\npt:\n  search_indexer: builtin\n"
	downloader := downloaderconfig.Downloader{Name: "主下载器", Type: "qbittorrent", Enabled: 1, Transfer: 1, Config: `{"host":"qb.local","port":"8080","username":"admin","password":"secret-password"}`, DownloadDir: `[]`}
	handler, token, _ := nativeServicesFixture(t, extra, &downloader, transport)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/services", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"secret-password", "secret-media-key", "secret-plex-token", "user:password", "token=secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("secret %q leaked: %s", secret, response.Body.String())
		}
	}
	var result struct {
		Data servicesData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Data.Downloaders) != 1 || result.Data.Downloaders[0].Host != "qb.local:8080" || !result.Data.Downloaders[0].Default {
		t.Fatalf("unexpected downloaders: %#v", result.Data.Downloaders)
	}
	if len(result.Data.MediaServers) != 3 || !result.Data.MediaServers[1].Active || result.Data.MediaServers[1].Host != "media.local:8096" {
		t.Fatalf("unexpected media servers: %#v", result.Data.MediaServers)
	}
	if len(result.Data.Indexers) != 1 || result.Data.Indexers[0].SourceCount != 0 || !strings.Contains(strings.Join(result.Data.Warnings, "\n"), "indexers unavailable") {
		t.Fatalf("unexpected indexer: %#v", result.Data.Indexers)
	}
}

func TestDownloaderServiceTestUsesServerSideConfig(t *testing.T) {
	t.Parallel()
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "qb.local:8080" {
			t.Fatalf("unexpected downloader host: %s", request.URL)
		}
		if request.URL.Path == "/api/v2/auth/login" {
			payload, _ := io.ReadAll(request.Body)
			form, _ := url.ParseQuery(string(payload))
			if form.Get("password") != "server-side-secret" {
				t.Fatalf("server-side credential missing: %v", form)
			}
			return jsonResponse(request, "Ok."), nil
		}
		if request.URL.Path != "/api/v2/transfer/info" {
			t.Fatalf("unexpected downloader test: %s", request.URL.Path)
		}
		return jsonResponse(request, `{"dl_info_speed":0,"up_info_speed":0}`), nil
	})
	downloader := downloaderconfig.Downloader{Name: "主下载器", Type: "qbittorrent", Config: `{"host":"qb.local","port":"8080","username":"admin","password":"server-side-secret"}`, DownloadDir: `[]`}
	handler, token, _ := nativeServicesFixture(t, "", &downloader, transport)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/services/downloader/"+strconv.FormatInt(downloader.ID, 10)+"/test", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status = %d, calls = %d: %s", response.Code, calls, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "server-side-secret") {
		t.Fatalf("secret leaked in test response: %s", response.Body.String())
	}
	var result struct {
		Data serviceTestData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || !result.Data.OK {
		t.Fatalf("unexpected test result: %#v, %v", result.Data, err)
	}
}

func TestMediaServiceTestUsesServerSideConfig(t *testing.T) {
	t.Parallel()
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != "http://plex.local:32400/" {
			t.Fatalf("unexpected media test URL: %s", request.URL)
		}
		if request.Header.Get("X-Plex-Token") != "server-side-plex-token" || request.Header.Get("X-Plex-Client-Identifier") == "" {
			t.Fatalf("server-side Plex headers missing: %v", request.Header)
		}
		if request.URL.Query().Get("X-Plex-Token") != "" {
			t.Fatal("Plex token leaked into URL")
		}
		return jsonResponse(request, `{"MediaContainer":{"machineIdentifier":"plex-id"}}`), nil
	})
	handler, token, _ := nativeServicesFixture(t, "media:\n  media_server: plex\nplex:\n  host: http://plex.local:32400\n  token: server-side-plex-token\n", nil, transport)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/services/media/plex/test", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status = %d, calls = %d: %s", response.Code, calls, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "server-side-plex-token") {
		t.Fatalf("secret leaked in test response: %s", response.Body.String())
	}
	var result struct {
		Data serviceTestData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || !result.Data.OK {
		t.Fatalf("unexpected test result: %#v, %v", result.Data, err)
	}
}
