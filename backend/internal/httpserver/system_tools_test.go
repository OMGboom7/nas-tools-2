package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNetworkTestUsesNativeTransport(t *testing.T) {
	var requested string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "legacy" {
			t.Fatalf("network test contacted Flask: %s", request.URL)
		}
		requested = request.URL.String()
		return jsonResponse(request, `{}`), nil
	})
	handler, token, _ := nativeServicesFixture(t, "", nil, transport)
	for _, test := range []struct{ target, want string }{
		{"example.com/check", "https://example.com/check"},
		{"image.tmdb.org", "https://image.tmdb.org/t/p/w500/wwemzKWzjKYJFfCeiB57q3r4Bcm.png"},
		{"qyapi.weixin.qq.com", "https://qyapi.weixin.qq.com/cgi-bin/message/send"},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/service/network/test", strings.NewReader(url.Values{"url": {test.target}}.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || requested != test.want {
			t.Fatalf("target %q: status=%d requested=%q body=%s", test.target, response.Code, requested, response.Body.String())
		}
		var result struct {
			Res  bool   `json:"res"`
			Time string `json:"time"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.Res || !strings.HasSuffix(result.Time, " 毫秒") {
			t.Fatalf("target %q: response=%s err=%v", test.target, response.Body.String(), err)
		}
	}
}

func TestNetworkTestRejectsUnauthorizedAndInvalidTarget(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("invalid network test made outbound request: %s", request.URL)
		return nil, nil
	})
	handler, token, _ := nativeServicesFixture(t, "", nil, transport)
	for _, test := range []struct {
		target, auth string
		status       int
	}{
		{"example.com", "", http.StatusUnauthorized},
		{"example.com", "invalid", http.StatusUnauthorized},
		{"https://example.com", token, http.StatusBadRequest},
		{"user:pass@example.com", token, http.StatusBadRequest},
		{"example.com#fragment", token, http.StatusBadRequest},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/service/network/test", strings.NewReader(url.Values{"url": {test.target}}.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Authorization", test.auth)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("target %q: got %d, want %d: %s", test.target, response.Code, test.status, response.Body.String())
		}
	}
}

func TestSystemVersionUsesGitHubWithoutLegacy(t *testing.T) {
	var paths []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "api.github.com" || request.Header.Get("User-Agent") == "" || request.Header.Get("Accept") != "application/vnd.github+json" {
			t.Fatalf("unexpected version request: %s", request.URL)
		}
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/repos/0xforee/nas-tools/releases/latest":
			return jsonResponse(request, `{"tag_name":"v1.2.3","html_url":"https://github.com/0xforee/nas-tools/releases/tag/v1.2.3"}`), nil
		case "/repos/0xforee/nas-tools/commits/master":
			return jsonResponse(request, `{"sha":"abcdef123456"}`), nil
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
			return nil, nil
		}
	})
	handler, token, _ := nativeServicesFixture(t, "", nil, transport)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/system/version", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(paths) != 2 || !strings.Contains(response.Body.String(), `"version":"v1.2.3"`) || !strings.Contains(response.Body.String(), `"code":0`) {
		t.Fatalf("version response=%d %s; paths=%v", response.Code, response.Body.String(), paths)
	}
}

func TestSystemVersionFailureStaysNative(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "api.github.com" {
			t.Fatalf("version contacted Flask: %s", request.URL)
		}
		result := jsonResponse(request, `{}`)
		result.StatusCode = http.StatusServiceUnavailable
		return result, nil
	})
	handler, token, _ := nativeServicesFixture(t, "", nil, transport)
	for _, auth := range []string{"", "invalid", token} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/system/version", nil)
		request.Header.Set("Authorization", auth)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if auth != token && response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized version response=%d %s", response.Code, response.Body.String())
		}
		if auth == token && (response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":-1`)) {
			t.Fatalf("failed version response=%d %s", response.Code, response.Body.String())
		}
	}
}
