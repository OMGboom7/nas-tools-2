package httpserver

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRawDownloaderTestUsesNativeClientAndProtectsConfiguration(t *testing.T) {
	calls := 0
	handler, _, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "aria.local:6800" || request.URL.Path != "/jsonrpc" {
			t.Errorf("unexpected destination: %s", request.URL)
			return nil, fmt.Errorf("legacy must not be called")
		}
		return jsonResponse(request, `{"jsonrpc":"2.0","id":"nastool-check","result":{"version":"1.37.0"}}`), nil
	}))
	response := performFormRequest(handler, "/api/v1/download/client/test", token, url.Values{"type": {"aria2"}, "config": {`{"host":"aria.local","port":6800,"secret":"server-secret"}`}})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"code":0`) || calls != 1 {
		t.Fatalf("native raw test: calls=%d %d %s", calls, response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "server-secret") {
		t.Fatal("response leaked secret")
	}
	for _, invalid := range []string{"", `[]`, `{"host":"aria.local"} trailing`, strings.Repeat(" ", 1<<20) + `{}`} {
		response = performFormRequest(handler, "/api/v1/download/client/test", token, url.Values{"type": {"aria2"}, "config": {invalid}})
		if response.Code != 400 {
			t.Fatalf("accepted invalid config: %d %s", response.Code, response.Body.String())
		}
	}
}

func TestRawCloudDownloaderTestKeepsExplicitCompatibilityBranch(t *testing.T) {
	calls := 0
	handler, _, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/api/v1/download/client/test" {
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}
		body, _ := io.ReadAll(request.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("type") != "pikpak" || !strings.Contains(form.Get("config"), "server-secret") {
			t.Fatalf("legacy form = %v", form)
		}
		return jsonResponse(request, `{"code":0,"success":true}`), nil
	}))
	response := performFormRequest(handler, "/api/v1/download/client/test", token, url.Values{"type": {"pikpak"}, "config": {`{"username":"user","password":"server-secret"}`}})
	if response.Code != 200 || calls != 1 || !strings.Contains(response.Body.String(), `"code":0`) {
		t.Fatalf("compatibility response: calls=%d %d %s", calls, response.Code, response.Body.String())
	}
}

func TestRawPan115DownloaderTestIsNative(t *testing.T) {
	calls := 0
	handler, _, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != "https://webapi.115.com/files/index_info" || request.Header.Get("Cookie") != "UID=server-secret" {
			t.Fatalf("unexpected destination or cookie: %s %v", request.URL, request.Header)
		}
		return jsonResponse(request, `{"state":true,"data":{"space_info":{}}}`), nil
	}))
	response := performFormRequest(handler, "/api/v1/download/client/test", token, url.Values{"type": {"pan115"}, "config": {`{"cookie":"UID=server-secret"}`}})
	if response.Code != 200 || calls != 1 || !strings.Contains(response.Body.String(), `"code":0`) || strings.Contains(response.Body.String(), "server-secret") {
		t.Fatalf("native 115 check: calls=%d %d %s", calls, response.Code, response.Body.String())
	}
}
