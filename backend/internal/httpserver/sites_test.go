package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestNativeSiteConnectionUsesStoredCookieAndChecksLogin(t *testing.T) {
	responseBody := `<html><a href="/logout.php">退出</a></html>`
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != "https://tracker.example/" || request.Header.Get("Cookie") != "session=server-secret" || request.Header.Get("User-Agent") != "site-agent" {
			t.Fatalf("unexpected site request: %s %v", request.URL, request.Header)
		}
		return jsonResponse(request, responseBody), nil
	})
	handler, store, token := nativeSiteFixture(t, transport)
	item, err := store.Upsert(context.Background(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.example/private?passkey=hidden", Cookie: "session=server-secret", Note: `{"ua":"site-agent"}`})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/sites/" + text(item.ID) + "/test"
	response := nativeJSONRequest(handler, http.MethodPost, path, token, "")
	if response.Code != http.StatusOK || calls != 1 || strings.Contains(response.Body.String(), "server-secret") {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
	}
	var result struct {
		Data siteTestData `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.Data.OK || result.Data.Message != "连接成功" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	responseBody = `<html><a href="/logout.php">退出</a><input type="password"></html>`
	response = nativeJSONRequest(handler, http.MethodPost, path, token, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Cookie失效") {
		t.Fatalf("expired cookie: %d %s", response.Code, response.Body.String())
	}
}

func TestNativeSiteConnectionDoesNotFollowRedirectWithCookie(t *testing.T) {
	calls := 0
	handler, store, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Hostname() != "tracker.example" {
			t.Fatal("cookie sent to redirect target")
		}
		response := jsonResponse(request, "redirect")
		response.StatusCode = http.StatusFound
		response.Header.Set("Location", "https://other.example/collect")
		return response, nil
	}))
	item, err := store.Upsert(context.Background(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.example", Cookie: "session=secret"})
	if err != nil {
		t.Fatal(err)
	}
	response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/sites/"+text(item.ID)+"/test", token, "")
	if response.Code != http.StatusOK || calls != 1 || !strings.Contains(response.Body.String(), "302") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
	}
}

func TestNativeMTeamConnectionUsesAPIKey(t *testing.T) {
	handler, store, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://api.m-team.cc/api/member/profile" || request.Header.Get("X-API-KEY") != "private-key" {
			t.Fatalf("unexpected M-Team request: %s %s", request.Method, request.URL)
		}
		return jsonResponse(request, `{"data":{"username":"member"}}`), nil
	}))
	item, err := store.Upsert(context.Background(), siteconfig.Site{Name: "M-Team", SignURL: "https://m-team.cc/details/1", Cookie: "session=secret", APIKey: "private-key"})
	if err != nil {
		t.Fatal(err)
	}
	response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/sites/"+text(item.ID)+"/test", token, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok":true`) || strings.Contains(response.Body.String(), "private-key") {
		t.Fatalf("M-Team result: %d %s", response.Code, response.Body.String())
	}
}

func TestNativeSiteConnectionRejectsUnsupportedBrowserMode(t *testing.T) {
	handler, store, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatal("unsupported browser mode must not make a request")
		return nil, nil
	}))
	item, err := store.Upsert(context.Background(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.example", Cookie: "session=secret", Note: `{"chrome":"Y"}`})
	if err != nil {
		t.Fatal(err)
	}
	response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/sites/"+text(item.ID)+"/test", token, "")
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("unsupported mode: %d %s", response.Code, response.Body.String())
	}
}

func TestSiteProxyTransportUsesConfiguredSchemeWithoutChangingSharedClient(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	configured, err := siteProxyTransport(base, map[string]any{"https": "socks5://proxy.example:1080"}, "https")
	if err != nil {
		t.Fatal(err)
	}
	request := &http.Request{URL: &url.URL{Scheme: "https", Host: "tracker.example"}}
	proxy, err := configured.(*http.Transport).Proxy(request)
	if err != nil || proxy.String() != "socks5://proxy.example:1080" || configured == base {
		t.Fatalf("proxy=%v transport=%p original=%p err=%v", proxy, configured, base, err)
	}
	if _, err := siteProxyTransport(base, map[string]any{"https": ""}, "https"); err == nil {
		t.Fatal("accepted missing proxy")
	}
}

func TestSiteConnectionTestRejectsInvalidID(t *testing.T) {
	t.Parallel()
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatal("legacy backend should not be called")
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sites/not-a-number/test", nil)
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}
