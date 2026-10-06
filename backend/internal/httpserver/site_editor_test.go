package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestCreateSiteRequiresRSSForEnabledSubscription(t *testing.T) {
	t.Parallel()
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatal("legacy backend should not be called")
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	body := `{"name":"New PT","priority":1,"siteUrl":"https://new.example","rssEnabled":true,"brushEnabled":false,"statisticEnabled":false,"parseEnabled":true,"messageEnabled":true,"browserEnabled":false,"proxyEnabled":false,"subtitleEnabled":false,"tags":"","filterRule":"","downloadSetting":"","limitInterval":"","limitCount":"","limitSeconds":"","rssUrl":"","cookie":"","apiKey":"","userAgent":"","clearRssUrl":false,"clearCookie":false,"clearApiKey":false,"clearUserAgent":false}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sites", strings.NewReader(body))
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}
