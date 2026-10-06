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

const legacyPluginConfig = `{"code":0,"result":{"CookieCloud":{"name":"CookieCloud 同步","page":"同步记录","script":"dangerous()","config":{"server":"https://secret.example","key":"key-secret","password":"password-secret","notify":true,"mode":"all","sites":["one"],"err_hosts":"private error","internal_cache":{"token":"cache-secret"}},"fields":[{"type":"div","content":[[{"title":"服务器","required":"required","tooltip":"服务地址","type":"text","content":[{"id":"server","placeholder":"https://server"}]},{"title":"密钥","type":"password","content":[{"id":"key"},{"id":"password","placeholder":"加密密码"}]}],[{"title":"通知","type":"switch","id":"notify"},{"title":"模式","type":"select","content":[{"id":"mode","default":"white","onchange":"dangerous()","options":{"all":"全部","white":"白名单"}}]}],[{"id":"sites","type":"form-selectgroup","onclick":"dangerous()","content":{"one":{"name":"站点一"},"two":{"name":"站点二"}}}],[{"title":"错误信息","type":"textarea","readonly":true,"content":{"id":"err_hosts","rows":2}}]]},{"type":"details","summary":"高级配置","content":[[{"title":"备注","type":"textarea","content":{"id":"notes","placeholder":"每行一个"}}]]}]}}}`

func TestPluginConfigDetailNeverReturnsTextValuesOrScripts(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/plugin/list" {
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}
		return jsonResponse(request, legacyPluginConfig), nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/CookieCloud", nil)
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	for _, unsafe := range []string{"secret.example", "key-secret", "password-secret", "private error", "cache-secret", "dangerous()", "internal_cache"} {
		if strings.Contains(response.Body.String(), unsafe) {
			t.Fatalf("unsafe value %q leaked: %s", unsafe, response.Body.String())
		}
	}
	var result struct {
		Data pluginConfigDetail `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Data.ID != "CookieCloud" || len(result.Data.Fields) != 8 || !result.Data.Meta.HasPage {
		t.Fatalf("unexpected detail: %#v", result.Data)
	}
	if result.Data.Values["notify"] != true || result.Data.Values["mode"] != "all" {
		t.Fatalf("safe values missing: %#v", result.Data.Values)
	}
	if selected, ok := result.Data.Values["sites"].([]any); !ok || len(selected) != 1 || selected[0] != "one" {
		t.Fatalf("safe multi-select missing: %#v", result.Data.Values["sites"])
	}
	for _, field := range result.Data.Fields {
		if (field.Key == "server" || field.Key == "key" || field.Key == "password" || field.Key == "notes") && (!field.WriteOnly || (field.Key != "notes" && !field.Configured)) {
			t.Fatalf("text field was not protected: %#v", field)
		}
		if field.Key == "err_hosts" && (!field.WriteOnly || !field.ReadOnly) {
			t.Fatalf("readonly field was not protected: %#v", field)
		}
	}
}

func TestPluginConfigUpdateMergesServerSideValues(t *testing.T) {
	t.Parallel()
	saved := false
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v1/plugin/list":
			return jsonResponse(request, legacyPluginConfig), nil
		case "/api/v1/plugin/config":
			payload, _ := io.ReadAll(request.Body)
			form, _ := url.ParseQuery(string(payload))
			if form.Get("id") != "CookieCloud" {
				t.Fatalf("unexpected id: %#v", form)
			}
			var merged map[string]any
			if err := json.Unmarshal([]byte(form.Get("config")), &merged); err != nil {
				t.Fatalf("decode merged config: %v", err)
			}
			if merged["server"] != "https://secret.example" || merged["key"] != "replacement" || merged["password"] != "password-secret" {
				t.Fatalf("write-only values were not merged: %#v", merged)
			}
			if merged["notify"] != false || merged["mode"] != "white" {
				t.Fatalf("safe values were not updated: %#v", merged)
			}
			if _, ok := merged["internal_cache"].(map[string]any); !ok {
				t.Fatalf("internal config was not preserved: %#v", merged)
			}
			if _, exists := merged["notes"]; exists {
				t.Fatalf("cleared field is still present: %#v", merged)
			}
			sites, ok := merged["sites"].([]any)
			if !ok || len(sites) != 1 || sites[0] != "two" {
				t.Fatalf("multi-select was not updated: %#v", merged)
			}
			saved = true
			return jsonResponse(request, `{"code":0,"success":true,"message":"保存成功","data":{}}`), nil
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
			return nil, nil
		}
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	body := `{"values":{"key":"replacement","notify":false,"mode":"white","sites":["two"]},"clearConfig":["notes"]}`
	request := httptest.NewRequest(http.MethodPut, "/api/v1/plugins/CookieCloud", strings.NewReader(body))
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !saved {
		t.Fatalf("status = %d, saved = %v: %s", response.Code, saved, response.Body.String())
	}
}

func TestPluginConfigUpdateRejectsUnknownAndInvalidValues(t *testing.T) {
	t.Parallel()
	mutations := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/v1/plugin/config" {
			mutations++
			t.Fatalf("invalid configuration should not be forwarded")
		}
		return jsonResponse(request, legacyPluginConfig), nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	tests := []string{
		`{"values":{"forged":"value"},"clearConfig":[]}`,
		`{"values":{"mode":"forged"},"clearConfig":[]}`,
		`{"values":{"err_hosts":"changed"},"clearConfig":[]}`,
		`{"values":{},"clearConfig":["server"]}`,
	}
	for _, body := range tests {
		request := httptest.NewRequest(http.MethodPut, "/api/v1/plugins/CookieCloud", strings.NewReader(body))
		request.Header.Set("Authorization", "test-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d: %s", body, response.Code, response.Body.String())
		}
	}
	if mutations != 0 {
		t.Fatalf("unexpected mutations: %d", mutations)
	}
}

func TestPluginConfigRequiresInstalledVisiblePlugin(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(request, `{"code":0,"result":{}}`), nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/UnknownPlugin", nil)
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}
