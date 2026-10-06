package httpserver

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNativeQbittorrentConnectionTestNeverCallsFlask(t *testing.T) {
	calls := 0
	handler, _, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "qb.local:8080" || request.Header.Get("Authorization") != "" {
			t.Errorf("unexpected destination/auth: %s", request.URL.Host)
			return nil, fmt.Errorf("unexpected request")
		}
		if request.URL.Path == "/api/v2/auth/login" {
			if err := request.ParseForm(); err != nil || request.Form.Get("password") != "server-secret" {
				t.Error("stored password not used")
			}
			response := jsonResponse(request, "Ok.")
			response.Header.Set("Set-Cookie", "SID=session-secret; Path=/")
			return response, nil
		}
		if request.URL.Path != "/api/v2/transfer/info" {
			t.Fatalf("unexpected endpoint: %s", request.URL.Path)
		}
		return jsonResponse(request, `{"dl_info_speed":0,"up_info_speed":0}`), nil
	}))
	response := performFormRequest(handler, "/api/v1/download/client/add", token, url.Values{"name": {"QB"}, "type": {"qbittorrent"}, "enabled": {"1"}, "transfer": {"1"}, "only_nastool": {"1"}, "config": {`{"host":"qb.local","port":8080,"username":"admin","password":"server-secret"}`}})
	if response.Code != 200 {
		t.Fatalf("seed: %d %s", response.Code, response.Body.String())
	}
	response = nativeJSONRequest(handler, http.MethodPost, "/api/v1/services/downloader/1/test", token, "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"ok":true`) || calls != 2 {
		t.Fatalf("test response: calls=%d %d %s", calls, response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "server-secret") || strings.Contains(response.Body.String(), "session-secret") {
		t.Fatal("response leaked credentials")
	}
}
