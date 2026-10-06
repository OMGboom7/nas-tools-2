package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNativeAria2ConnectionTestNeverCallsFlask(t *testing.T) {
	calls := 0
	handler, _, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "aria.local:6800" || request.URL.Path != "/jsonrpc" {
			t.Errorf("unexpected destination: %s", request.URL)
			return nil, fmt.Errorf("unexpected request")
		}
		var payload struct {
			Method string   `json:"method"`
			Params []string `json:"params"`
		}
		if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Method != "aria2.getVersion" || len(payload.Params) != 1 || payload.Params[0] != "token:server-secret" {
			t.Errorf("payload = %+v", payload)
		}
		return jsonResponse(request, `{"jsonrpc":"2.0","id":"nastool-check","result":{"version":"1.37.0"}}`), nil
	}))
	response := performFormRequest(handler, "/api/v1/download/client/add", token, url.Values{"name": {"Aria"}, "type": {"aria2"}, "enabled": {"1"}, "transfer": {"1"}, "only_nastool": {"1"}, "config": {`{"host":"aria.local","port":6800,"secret":"server-secret"}`}})
	if response.Code != 200 {
		t.Fatalf("seed: %d %s", response.Code, response.Body.String())
	}
	response = nativeJSONRequest(handler, http.MethodPost, "/api/v1/services/downloader/1/test", token, "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"ok":true`) || calls != 1 {
		t.Fatalf("test: calls=%d %d %s", calls, response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "server-secret") {
		t.Fatal("response leaked token")
	}
}
