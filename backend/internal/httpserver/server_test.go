package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestHealth(t *testing.T) {
	t.Parallel()

	handler, err := New(config.Config{LegacyBackendURL: "http://127.0.0.1:3000"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}

	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status body = %q, want ok", body["status"])
	}
}

func TestLegacyAPIProxy(t *testing.T) {
	t.Parallel()

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/existing" {
			t.Errorf("path = %q, want /api/v1/existing", request.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     http.Header{"X-Legacy": []string{"true"}},
			Body:       io.NopCloser(strings.NewReader("accepted")),
			Request:    request,
		}, nil
	})

	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/existing", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusAccepted)
	}
	if response.Header().Get("X-Legacy") != "true" {
		t.Fatal("legacy response header was not preserved")
	}
}

func TestNativeLoginProtectsMigratedRoutesAndLogoutRevokesToken(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	contents := []byte("app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: native-auth-test-secret\n")
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"code":0,"success":true,"data":{}}`
		switch request.URL.Path {
		case "/api/v1/library/mediaserver/statistics":
			body = `{"code":0,"success":true,"data":{}}`
		case "/api/v1/library/space":
			body = `{"code":0,"success":true,"data":{}}`
		case "/api/v1/library/mediaserver/resume":
			body = `{"code":0,"success":true,"data":{"list":[]}}`
		case "/api/v1/library/mediaserver/latest":
			body = `{"code":0,"list":[]}`
		default:
			t.Errorf("unexpected legacy path %q", request.URL.Path)
		}
		return jsonResponse(request, body), nil
	})
	handler, err := newHandler(config.Config{
		LegacyBackendURL:      "http://legacy:3000",
		ApplicationConfigPath: configPath,
	}, transport)
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}

	loginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/user/login", strings.NewReader("username=admin&password=password"))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", loginResponse.Code, loginResponse.Body.String())
	}
	var loginResult struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(loginResponse.Body).Decode(&loginResult); err != nil || loginResult.Data.Token == "" {
		t.Fatalf("decode login response: %v, %#v", err, loginResult)
	}

	dashboardRequest := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	dashboardRequest.Header.Set("Authorization", loginResult.Data.Token)
	dashboardResponse := httptest.NewRecorder()
	handler.ServeHTTP(dashboardResponse, dashboardRequest)
	if dashboardResponse.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, body = %s", dashboardResponse.Code, dashboardResponse.Body.String())
	}

	createForm := url.Values{
		"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"},
		"pris": {"我的媒体库,资源搜索"},
	}
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v1/user/manage", strings.NewReader(createForm.Encode()))
	createRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createRequest.Header.Set("Authorization", loginResult.Data.Token)
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create user status = %d, body = %s", createResponse.Code, createResponse.Body.String())
	}

	viewerLoginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/user/login", strings.NewReader("username=viewer&password=strong-password"))
	viewerLoginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	viewerLoginResponse := httptest.NewRecorder()
	handler.ServeHTTP(viewerLoginResponse, viewerLoginRequest)
	if viewerLoginResponse.Code != http.StatusOK {
		t.Fatalf("viewer login status = %d, body = %s", viewerLoginResponse.Code, viewerLoginResponse.Body.String())
	}

	listRequest := httptest.NewRequest(http.MethodPost, "/api/v1/user/list", nil)
	listRequest.Header.Set("Authorization", loginResult.Data.Token)
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), "viewer") {
		t.Fatalf("list users status = %d, body = %s", listResponse.Code, listResponse.Body.String())
	}

	deleteForm := url.Values{"oper": {"del"}, "name": {"viewer"}}
	deleteRequest := httptest.NewRequest(http.MethodPost, "/api/v1/user/manage", strings.NewReader(deleteForm.Encode()))
	deleteRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	deleteRequest.Header.Set("Authorization", loginResult.Data.Token)
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete user status = %d, body = %s", deleteResponse.Code, deleteResponse.Body.String())
	}

	logoutRequest := httptest.NewRequest(http.MethodPost, "/api/v1/system/logout", nil)
	logoutRequest.Header.Set("Authorization", loginResult.Data.Token)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusOK {
		t.Fatalf("logout status = %d, body = %s", logoutResponse.Code, logoutResponse.Body.String())
	}

	revokedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	revokedRequest.Header.Set("Authorization", loginResult.Data.Token)
	revokedResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokedResponse, revokedRequest)
	if revokedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want %d", revokedResponse.Code, http.StatusUnauthorized)
	}

	legacyRequest := httptest.NewRequest(http.MethodPost, "/api/v1/user/info", strings.NewReader("username=admin"))
	legacyRequest.Header.Set("Authorization", loginResult.Data.Token)
	legacyResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyResponse, legacyRequest)
	if legacyResponse.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token on legacy route status = %d, want %d", legacyResponse.Code, http.StatusUnauthorized)
	}
}

func TestDashboardWithoutNativeConfigurationDoesNotContactLegacy(t *testing.T) {
	t.Parallel()

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Errorf("unexpected legacy request %q", request.URL.Path)
		return nil, nil
	})

	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestNativeDashboardStorageDeduplicatesDiskWithoutLegacySpace(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "storage.yaml")
	contents := "media:\n  movie_path: " + strconv.Quote(directory) + "\n  tv_path:\n    - " + strconv.Quote(directory) + "\n  anime_path: /path/that/does/not/exist\n"
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := nativeLibrarySpace(config.NewStore(configPath))
	if err != nil || storage["TotalSpace"] == nil || storage["TotalSpace"] == 0 {
		t.Fatalf("native storage: %#v err=%v", storage, err)
	}
	if err := os.WriteFile(configPath, []byte("media:\n  movie_path: "+strconv.Quote(directory)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	onePath, err := nativeLibrarySpace(config.NewStore(configPath))
	if err != nil || storage["TotalSpace"] != onePath["TotalSpace"] {
		t.Fatalf("duplicate paths counted twice: multiple=%#v single=%#v err=%v", storage, onePath, err)
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/v1/library/space" {
			t.Errorf("dashboard still called legacy space endpoint")
		}
		return jsonResponse(request, `{"code":0,"data":{"list":[]}}`), nil
	})
	handler, token, _ := nativeServicesFixture(t, "media:\n  movie_path: "+strconv.Quote(directory)+"\n", nil, transport)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"total":"`) || !strings.Contains(response.Body.String(), ` GB"`) {
		t.Fatalf("native dashboard storage: status=%d body=%s", response.Code, response.Body.String())
	}
	compatRequest := httptest.NewRequest(http.MethodPost, "/api/v1/library/space", nil)
	compatResponse := httptest.NewRecorder()
	handler.ServeHTTP(compatResponse, compatRequest)
	if compatResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated library space: status=%d body=%s", compatResponse.Code, compatResponse.Body.String())
	}
	compatRequest = httptest.NewRequest(http.MethodPost, "/api/v1/library/space", nil)
	compatRequest.Header.Set("Authorization", token)
	compatResponse = httptest.NewRecorder()
	handler.ServeHTTP(compatResponse, compatRequest)
	var legacySpace struct {
		Code        int    `json:"code"`
		TotalSpace  string `json:"TotalSpace"`
		FreeSpace   string `json:"FreeSpace"`
		UsedSapce   string `json:"UsedSapce"`
		UsedPercent string `json:"UsedPercent"`
	}
	if compatResponse.Code != http.StatusOK || json.Unmarshal(compatResponse.Body.Bytes(), &legacySpace) != nil || legacySpace.Code != 0 || legacySpace.TotalSpace == "" || legacySpace.FreeSpace == "" || legacySpace.UsedSapce == "" || legacySpace.UsedPercent == "" {
		t.Fatalf("native legacy space response: status=%d body=%s", compatResponse.Code, compatResponse.Body.String())
	}
}

func TestNativeEmbyDashboardStatisticsAvoidLegacy(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "media.example" {
			if request.Header.Get("X-Emby-Token") != "media-secret" || request.URL.Query().Get("api_key") != "" {
				t.Errorf("media credential not sent in protected header: %s", request.URL.String())
			}
			switch request.URL.Path {
			case "/emby/Items/Counts":
				return jsonResponse(request, `{"MovieCount":1234,"SeriesCount":20,"EpisodeCount":300,"SongCount":0}`), nil
			case "/emby/Users/Query":
				return jsonResponse(request, `{"TotalRecordCount":5}`), nil
			}
		}
		if request.URL.Path == "/api/v1/library/mediaserver/statistics" {
			t.Errorf("dashboard still called legacy statistics endpoint")
		}
		return jsonResponse(request, `{"code":0,"data":{"list":[]}}`), nil
	})
	handler, token, _ := nativeServicesFixture(t, "media:\n  media_server: emby\nemby:\n  host: https://media.example\n  api_key: media-secret\n", nil, transport)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"movies":"1,234"`) || !strings.Contains(response.Body.String(), `"users":"5"`) {
		t.Fatalf("native media statistics: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestNativePlexDashboardStatisticsAvoidLegacy(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "plex.example" {
			if request.Header.Get("X-Plex-Token") != "plex-secret" {
				t.Errorf("missing protected Plex token")
			}
			if request.URL.Path == "/library/sections" {
				return jsonResponse(request, `{"MediaContainer":{"Directory":[{"key":"1","type":"movie"}]}}`), nil
			}
			if request.URL.Path == "/library/sections/1/all" {
				return jsonResponse(request, `{"MediaContainer":{"totalSize":120}}`), nil
			}
			if request.URL.Path == "/library/recentlyAdded" {
				return jsonResponse(request, `{"MediaContainer":{"Metadata":[{"key":"/library/metadata/7","type":"movie","title":"Plex新片","thumb":"/library/metadata/7/thumb"}]}}`), nil
			}
			if request.URL.Path == "/hubs/continueWatching/items" {
				return jsonResponse(request, `{"MediaContainer":{"Metadata":[{"key":"/library/metadata/8","type":"movie","title":"继续观看","art":"/library/metadata/8/art","viewOffset":500,"duration":1000}]}}`), nil
			}
			if request.URL.Path == "/" {
				return jsonResponse(request, `{"MediaContainer":{"machineIdentifier":"plex-machine"}}`), nil
			}
		}
		if request.URL.Path == "/api/v1/library/mediaserver/statistics" {
			t.Errorf("dashboard still called legacy statistics endpoint")
		}
		if request.URL.Path == "/api/v1/library/mediaserver/latest" {
			t.Errorf("dashboard still called legacy latest endpoint")
		}
		if request.URL.Path == "/api/v1/library/mediaserver/resume" {
			t.Errorf("dashboard still called legacy resume endpoint")
		}
		return jsonResponse(request, `{"code":0,"data":{"list":[]}}`), nil
	})
	handler, token, _ := nativeServicesFixture(t, "media:\n  media_server: plex\nplex:\n  host: https://plex.example\n  token: plex-secret\n", nil, transport)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"movies":"120"`) || !strings.Contains(response.Body.String(), `"users":"1"`) || !strings.Contains(response.Body.String(), `"name":"Plex新片"`) || !strings.Contains(response.Body.String(), `"name":"继续观看"`) || !strings.Contains(response.Body.String(), `"percent":50`) || strings.Contains(response.Body.String(), "plex-secret") {
		t.Fatalf("native Plex statistics: status=%d body=%s", response.Code, response.Body.String())
	}
	compatRequest := httptest.NewRequest(http.MethodPost, "/api/v1/library/mediaserver/statistics", nil)
	compatResponse := httptest.NewRecorder()
	handler.ServeHTTP(compatResponse, compatRequest)
	if compatResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated media statistics: status=%d", compatResponse.Code)
	}
	compatRequest = httptest.NewRequest(http.MethodPost, "/api/v1/library/mediaserver/statistics", nil)
	compatRequest.Header.Set("Authorization", token)
	compatResponse = httptest.NewRecorder()
	handler.ServeHTTP(compatResponse, compatRequest)
	if compatResponse.Code != http.StatusOK || !strings.Contains(compatResponse.Body.String(), `"Movie":"120"`) || !strings.Contains(compatResponse.Body.String(), `"User":1`) {
		t.Fatalf("native legacy statistics: status=%d body=%s", compatResponse.Code, compatResponse.Body.String())
	}
	latestRequest := httptest.NewRequest(http.MethodPost, "/api/v1/library/mediaserver/latest", strings.NewReader("num=5"))
	latestRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	latestResponse := httptest.NewRecorder()
	handler.ServeHTTP(latestResponse, latestRequest)
	if latestResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated latest: status=%d", latestResponse.Code)
	}
	latestRequest = httptest.NewRequest(http.MethodPost, "/api/v1/library/mediaserver/latest", strings.NewReader("num=5"))
	latestRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	latestRequest.Header.Set("Authorization", token)
	latestResponse = httptest.NewRecorder()
	handler.ServeHTTP(latestResponse, latestRequest)
	if latestResponse.Code != http.StatusOK || !strings.Contains(latestResponse.Body.String(), `"name":"Plex新片"`) || strings.Contains(latestResponse.Body.String(), "plex-secret") {
		t.Fatalf("native legacy latest: status=%d body=%s", latestResponse.Code, latestResponse.Body.String())
	}
	resumeRequest := httptest.NewRequest(http.MethodPost, "/api/v1/library/mediaserver/resume", strings.NewReader("num=5"))
	resumeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resumeResponse := httptest.NewRecorder()
	handler.ServeHTTP(resumeResponse, resumeRequest)
	if resumeResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated resume: status=%d", resumeResponse.Code)
	}
	resumeRequest = httptest.NewRequest(http.MethodPost, "/api/v1/library/mediaserver/resume", strings.NewReader("num=5"))
	resumeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resumeRequest.Header.Set("Authorization", token)
	resumeResponse = httptest.NewRecorder()
	handler.ServeHTTP(resumeResponse, resumeRequest)
	if resumeResponse.Code != http.StatusOK || !strings.Contains(resumeResponse.Body.String(), `"name":"继续观看"`) || !strings.Contains(resumeResponse.Body.String(), `"percent":50`) || strings.Contains(resumeResponse.Body.String(), "plex-secret") {
		t.Fatalf("native legacy resume: status=%d body=%s", resumeResponse.Code, resumeResponse.Body.String())
	}
}

func TestNativeLatestDashboardUsesProtectedImage(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/Users":
			if request.Header.Get("X-Emby-Token") != "media-secret" {
				t.Errorf("missing media token")
			}
			return jsonResponse(request, `[{"Id":"user-1","Name":"admin","Policy":{"IsAdministrator":true}}]`), nil
		case "/Users/user-1/Items/Latest":
			return jsonResponse(request, `[{"Id":"movie-1","Name":"新电影","Type":"Movie"}]`), nil
		case "/emby/System/Info":
			return jsonResponse(request, `{"Id":"server-1"}`), nil
		case "/emby/Items/Counts":
			return jsonResponse(request, `{"MovieCount":1,"SeriesCount":0,"SongCount":0}`), nil
		case "/emby/Users/Query":
			return jsonResponse(request, `{"TotalRecordCount":1}`), nil
		case "/api/v1/library/mediaserver/resume":
			return jsonResponse(request, `{"code":0,"list":[]}`), nil
		case "/api/v1/library/mediaserver/latest":
			t.Errorf("dashboard called Python latest endpoint")
		}
		return jsonResponse(request, `{"code":1}`), nil
	})
	handler, token, _ := nativeServicesFixture(t, "media:\n  media_server: emby\nemby:\n  host: https://media.example\n  api_key: media-secret\n", nil, transport)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"新电影"`) || !strings.Contains(response.Body.String(), `/api/v1/dashboard/image?`) || strings.Contains(response.Body.String(), "media-secret") {
		t.Fatalf("native latest dashboard: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDashboardRequiresAuthorization(t *testing.T) {
	t.Parallel()

	handler, err := New(config.Config{LegacyBackendURL: "http://127.0.0.1:3000"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestDashboardRejectsInvalidNativeToken(t *testing.T) {
	t.Parallel()
	handler, _, _ := nativeServicesFixture(t, "", nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard", nil)
	request.Header.Set("Authorization", "expired-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestServesFrontendAndSPAFallback(t *testing.T) {
	t.Parallel()

	frontend := t.TempDir()
	index := []byte("<!doctype html><title>NAS Tools</title>")
	if err := os.WriteFile(filepath.Join(frontend, "index.html"), index, 0o600); err != nil {
		t.Fatalf("write frontend fixture: %v", err)
	}

	handler, err := New(config.Config{
		LegacyBackendURL: "http://127.0.0.1:3000",
		FrontendDist:     frontend,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/media/library", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Body.String() != string(index) {
		t.Fatalf("body = %q, want SPA index", response.Body.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
