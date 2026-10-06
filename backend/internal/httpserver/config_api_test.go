package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestNativeConfigurationUpdateIsAtomicAndReloadsAuthentication(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	original := "# preserve this comment\napp:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: native-config-secret\nemby:\n  host: http://old:8096\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	handler, err := New(config.Config{
		LegacyBackendURL:      "http://127.0.0.1:1",
		ApplicationConfigPath: configPath,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	token := loginForTest(t, handler, "admin", "password")

	body := `{"items":{"emby.host":"http://new:8096","app.login_password":"new-password"}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/config/update", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", response.Code, response.Body.String())
	}
	contents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	text := string(contents)
	if !strings.Contains(text, "# preserve this comment") || !strings.Contains(text, "http://new:8096") {
		t.Fatalf("updated config lost data:\n%s", text)
	}
	if strings.Contains(text, "new-password") || !strings.Contains(text, "[hash]scrypt:32768:8:1$") {
		t.Fatalf("updated config did not securely hash password:\n%s", text)
	}
	backup, err := os.ReadFile(configPath + ".bak")
	if err != nil || string(backup) != original {
		t.Fatalf("backup = %q, %v", backup, err)
	}

	failedLogin := httptest.NewRequest(http.MethodPost, "/api/v1/user/login", strings.NewReader("username=admin&password=password"))
	failedLogin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	failedResponse := httptest.NewRecorder()
	handler.ServeHTTP(failedResponse, failedLogin)
	if failedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("old password login status = %d", failedResponse.Code)
	}
	_ = loginForTest(t, handler, "admin", "new-password")
}

func TestNativeConfigurationInfoRequiresAdministrator(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	contents := "app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: native-config-secret\n"
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	handler, err := New(config.Config{LegacyBackendURL: "http://127.0.0.1:1", ApplicationConfigPath: configPath})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	adminToken := loginForTest(t, handler, "admin", "password")
	createForm := "oper=add&name=viewer&password=strong-password&pris="
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v1/user/manage", strings.NewReader(createForm))
	createRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createRequest.Header.Set("Authorization", adminToken)
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create user status = %d, body = %s", createResponse.Code, createResponse.Body.String())
	}
	viewerToken := loginForTest(t, handler, "viewer", "strong-password")

	request := httptest.NewRequest(http.MethodPost, "/api/v1/config/info", nil)
	request.Header.Set("Authorization", viewerToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("viewer config info status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestNativeDownloaderConfigurationLifecycle(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	contents := "app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: native-downloader-secret\n"
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	handler, err := New(config.Config{LegacyBackendURL: "http://127.0.0.1:1", ApplicationConfigPath: configPath})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	token := loginForTest(t, handler, "admin", "password")

	add := url.Values{
		"name": {"主下载器"}, "type": {"qbittorrent"}, "enabled": {"1"}, "transfer": {"1"},
		"only_nastool": {"1"}, "match_path": {"0"}, "rmt_mode": {"link"},
		"config":       {`{"host":"qb.local","password":"secret"}`},
		"download_dir": {`[{"save_path":"/downloads/movies","container_path":"/media/movies"}]`},
	}
	addResponse := performFormRequest(handler, "/api/v1/download/client/add", token, add)
	if addResponse.Code != http.StatusOK {
		t.Fatalf("add downloader status = %d, body = %s", addResponse.Code, addResponse.Body.String())
	}

	listResponse := performFormRequest(handler, "/api/v1/download/client/list", token, nil)
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), "qb.local") {
		t.Fatalf("list downloader status = %d, body = %s", listResponse.Code, listResponse.Body.String())
	}

	defaultResponse := performFormRequest(handler, "/api/v1/config/set", token, url.Values{"key": {"DefaultDownloader"}, "value": {"1"}})
	if defaultResponse.Code != http.StatusOK {
		t.Fatalf("set default status = %d, body = %s", defaultResponse.Code, defaultResponse.Body.String())
	}
	setting := url.Values{
		"name": {"高清"}, "category": {"电影"}, "tags": {"NASTOOL"}, "is_paused": {"0"},
		"upload_limit": {"0"}, "download_limit": {"2048"}, "ratio_limit": {"1.5"},
		"seeding_time_limit": {"120"}, "downloader": {"1"},
	}
	settingResponse := performFormRequest(handler, "/api/v1/download/config/update", token, setting)
	if settingResponse.Code != http.StatusOK {
		t.Fatalf("add download setting status = %d, body = %s", settingResponse.Code, settingResponse.Body.String())
	}
	settingsResponse := performFormRequest(handler, "/api/v1/download/config/list", token, nil)
	if settingsResponse.Code != http.StatusOK || !strings.Contains(settingsResponse.Body.String(), "高清") {
		t.Fatalf("list download settings status = %d, body = %s", settingsResponse.Code, settingsResponse.Body.String())
	}
	directoryResponse := performFormRequest(handler, "/api/v1/download/config/directory", token, url.Values{"sid": {"1"}})
	if directoryResponse.Code != http.StatusOK || !strings.Contains(directoryResponse.Body.String(), "/downloads/movies") {
		t.Fatalf("download directories status = %d, body = %s", directoryResponse.Code, directoryResponse.Body.String())
	}
	flagResponse := performFormRequest(handler, "/api/v1/download/client/check", token, url.Values{"did": {"1"}, "checked": {"0"}, "flag": {"enabled"}})
	if flagResponse.Code != http.StatusOK {
		t.Fatalf("set flag status = %d, body = %s", flagResponse.Code, flagResponse.Body.String())
	}
	deleteSettingResponse := performFormRequest(handler, "/api/v1/download/config/delete", token, url.Values{"sid": {"1"}})
	if deleteSettingResponse.Code != http.StatusOK {
		t.Fatalf("delete download setting status = %d, body = %s", deleteSettingResponse.Code, deleteSettingResponse.Body.String())
	}
	deleteResponse := performFormRequest(handler, "/api/v1/download/client/delete", token, url.Values{"did": {"1"}})
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete downloader status = %d, body = %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	finalList := performFormRequest(handler, "/api/v1/download/client/list", token, nil)
	if finalList.Code != http.StatusOK || strings.Contains(finalList.Body.String(), "qb.local") {
		t.Fatalf("final downloader list status = %d, body = %s", finalList.Code, finalList.Body.String())
	}
}

func performFormRequest(handler http.Handler, path, token string, values url.Values) *httptest.ResponseRecorder {
	body := ""
	if values != nil {
		body = values.Encode()
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestNativeDownloaderTypeIdentifiersMatchPersistedClientIDs(t *testing.T) {
	for _, value := range []string{"qbittorrent", "transmission", "aria2", "pan115", "pikpak"} {
		if !supportedDownloaderType(value) {
			t.Errorf("persisted downloader type %q was rejected", value)
		}
	}
	for _, value := range []string{"115", "QB", "", "unknown"} {
		if supportedDownloaderType(value) {
			t.Errorf("invalid downloader type %q was accepted", value)
		}
	}
}

func loginForTest(t *testing.T, handler http.Handler, username, password string) string {
	t.Helper()
	form := "username=" + username + "&password=" + password
	request := httptest.NewRequest(http.MethodPost, "/api/v1/user/login", strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("login %q status = %d, body = %s", username, response.Code, response.Body.String())
	}
	var result struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || result.Data.Token == "" {
		t.Fatalf("decode login response: %v, %#v", err, result)
	}
	return result.Data.Token
}
