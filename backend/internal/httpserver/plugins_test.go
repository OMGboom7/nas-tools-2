package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

const legacyPluginApps = `{"code":0,"result":{"AutoSub":{"id":"AutoSub","installed":true,"name":"AI字幕","desc":"生成字幕\n任务","version":"1.0","author":"olly","author_url":"https://github.com/lightolly","icon":"secret.svg","color":"#fff"},"CookieCloud":{"id":"CookieCloud","installed":false,"name":"CookieCloud","desc":"同步 Cookie","version":"2.0","author":"dev","author_url":"javascript:alert(1)"}}}`

const legacyPluginList = `{"code":0,"result":{"AutoSub":{"name":"AI字幕","state":true,"fields":[{"type":"text","content":{"id":"token"}}],"page":"运行记录","script":"alert('secret-script')","config":{"token":"plugin-secret"},"prefix":"autosub"}}}`

func TestPluginListReturnsSafeSummary(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v1/plugin/apps":
			return jsonResponse(request, legacyPluginApps), nil
		case "/api/v1/plugin/list":
			return jsonResponse(request, legacyPluginList), nil
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
			return nil, nil
		}
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"plugin-secret", "secret-script", "prefix", "secret.svg", "color"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("unsafe plugin value %q leaked: %s", secret, response.Body.String())
		}
	}
	var result struct {
		Data pluginsData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Data.Items) != 2 || result.Data.InstalledCount != 1 || result.Data.RunningCount != 1 {
		t.Fatalf("unexpected data: %#v", result.Data)
	}
	installed := result.Data.Items[0]
	if installed.ID != "AutoSub" || !installed.Installed || !installed.Running || !installed.Configurable || !installed.HasPage || strings.Contains(installed.Description, "\n") {
		t.Fatalf("unexpected installed plugin: %#v", installed)
	}
	if result.Data.Items[1].AuthorURL != "" {
		t.Fatalf("unsafe author URL returned: %#v", result.Data.Items[1])
	}
}

func TestPluginInstallValidatesMarketIDAndForwardsString(t *testing.T) {
	t.Parallel()
	installed := false
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v1/plugin/apps":
			return jsonResponse(request, legacyPluginApps), nil
		case "/api/v1/plugin/list":
			return jsonResponse(request, legacyPluginList), nil
		case "/api/v1/plugin/install":
			body, _ := io.ReadAll(request.Body)
			form, _ := url.ParseQuery(string(body))
			if form.Get("id") != "CookieCloud" {
				t.Fatalf("unexpected plugin id: %#v", form)
			}
			installed = true
			return jsonResponse(request, `{"code":0,"msg":"插件安装成功"}`), nil
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
			return nil, nil
		}
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/CookieCloud/install", nil)
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !installed {
		t.Fatalf("status = %d, installed = %v: %s", response.Code, installed, response.Body.String())
	}
}

func TestPluginMutationRejectsUnknownAndInvalidIDs(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Path == "/api/v1/plugin/apps" {
			return jsonResponse(request, legacyPluginApps), nil
		}
		if request.URL.Path == "/api/v1/plugin/list" {
			return jsonResponse(request, legacyPluginList), nil
		}
		t.Fatalf("mutation should not be forwarded: %s", request.URL.Path)
		return nil, nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)

	invalid := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/bad.id/install", nil)
	invalid.Header.Set("Authorization", "test-token")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest || requests != 0 {
		t.Fatalf("invalid status = %d, requests = %d", invalidResponse.Code, requests)
	}

	unknown := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/UnknownPlugin/install", nil)
	unknown.Header.Set("Authorization", "test-token")
	unknownResponse := httptest.NewRecorder()
	handler.ServeHTTP(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusNotFound || requests != 2 {
		t.Fatalf("unknown status = %d, requests = %d: %s", unknownResponse.Code, requests, unknownResponse.Body.String())
	}
}

func TestPluginUninstallRequiresInstalledPlugin(t *testing.T) {
	t.Parallel()
	uninstalled := false
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v1/plugin/apps":
			return jsonResponse(request, legacyPluginApps), nil
		case "/api/v1/plugin/list":
			return jsonResponse(request, legacyPluginList), nil
		case "/api/v1/plugin/uninstall":
			body, _ := io.ReadAll(request.Body)
			form, _ := url.ParseQuery(string(body))
			if form.Get("id") != "AutoSub" {
				t.Fatalf("unexpected plugin id: %#v", form)
			}
			uninstalled = true
			return jsonResponse(request, `{"code":0,"msg":"插件卸载成功"}`), nil
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
			return nil, nil
		}
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/AutoSub", nil)
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !uninstalled {
		t.Fatalf("status = %d, uninstalled = %v: %s", response.Code, uninstalled, response.Body.String())
	}
}
