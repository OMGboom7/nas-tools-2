package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestGoOnlyModeNeverContactsLegacyBackend(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("Go-only mode contacted legacy backend: %s", request.URL)
		return nil, errors.New("unexpected request")
	})
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000", DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/api/v1/health", "", 200},
		{http.MethodPost, "/api/v1/unmigrated/endpoint", "", http.StatusNotImplemented},
		{http.MethodPost, "/api/v1/search/resources", `{"keyword":"movie"}`, http.StatusBadGateway},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "test-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("%s=%d %s", test.path, response.Code, response.Body.String())
		}
	}
}

func TestGoOnlyModeServesNativeAPIWithExistingConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: go-only-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("Go-only native API contacted legacy backend: %s", request.URL)
		return nil, errors.New("unexpected request")
	})
	handler, err := newHandler(config.Config{ApplicationConfigPath: configPath, LegacyBackendURL: "http://legacy:3000", DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, handler, "admin", "password")
	// The same Go-only handler must serve a native RSS endpoint and reject a
	// missing legacy endpoint without making any transport request.
	request := httptest.NewRequest(http.MethodPost, "/api/v1/rss/list", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"tasks":[]`) {
		t.Fatalf("RSS list=%d %s", response.Code, response.Body.String())
	}
}
