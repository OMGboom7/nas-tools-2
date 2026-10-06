package httpserver

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNativeTransmissionConnectionTestNeverCallsFlask(t *testing.T) {
	calls := 0
	handler, _, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "tr.local:9091" || request.URL.Path != "/transmission/rpc" {
			t.Errorf("unexpected destination: %s", request.URL)
			return nil, fmt.Errorf("unexpected request")
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "admin" || password != "server-secret" {
			t.Error("stored credentials not used")
		}
		if calls == 1 {
			response := jsonResponse(request, "session required")
			response.StatusCode = http.StatusConflict
			response.Header.Set("X-Transmission-Session-Id", "private-session")
			return response, nil
		}
		if request.Header.Get("X-Transmission-Session-Id") != "private-session" {
			t.Error("missing session header")
		}
		return jsonResponse(request, `{"result":"success","arguments":{"version":"4.0.6"}}`), nil
	}))
	response := performFormRequest(handler, "/api/v1/download/client/add", token, url.Values{"name": {"Transmission"}, "type": {"transmission"}, "enabled": {"1"}, "transfer": {"1"}, "only_nastool": {"1"}, "config": {`{"host":"tr.local","port":9091,"username":"admin","password":"server-secret"}`}})
	if response.Code != 200 {
		t.Fatalf("seed: %d %s", response.Code, response.Body.String())
	}
	response = nativeJSONRequest(handler, http.MethodPost, "/api/v1/services/downloader/1/test", token, "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"ok":true`) || calls != 2 {
		t.Fatalf("test: calls=%d %d %s", calls, response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "server-secret") || strings.Contains(response.Body.String(), "private-session") {
		t.Fatal("response leaked credentials")
	}
}
