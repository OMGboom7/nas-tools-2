package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestSubscriptionOptionsAndLegacyIndexerRouteUseNativeCatalog(t *testing.T) {
	var calls atomic.Int32
	handler, store, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.Path != "/api/v1/site/indexers" {
			t.Errorf("unexpected request: %s", request.URL.Path)
			return nil, fmt.Errorf("unexpected request")
		}
		return jsonResponse(request, `{"code":0,"data":{"indexers":[{"id":"plugin-site","name":"Plugin"}]}}`), nil
	}), "../../../web/backend/user.sites.bin")
	if _, err := store.Upsert(context.Background(), siteconfig.Site{Name: "My tracker", SignURL: "https://piggo.me/private?token=hidden", Cookie: "secret-cookie"}); err != nil {
		t.Fatal(err)
	}
	response := performFormRequest(handler, "/api/v1/config/set", token, url.Values{"key": {"UserIndexerSites"}, "value": {`["zhuzhu"]`}})
	if response.Code != 200 {
		t.Fatalf("select indexers: %d %s", response.Code, response.Body.String())
	}
	response = nativeJSONRequest(handler, http.MethodGet, "/api/v1/subscriptions/options", token, "")
	var result struct {
		Data subscriptionOptions `json:"data"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Data.SearchSites) != 1 || result.Data.SearchSites[0].Value != "zhuzhu" || result.Data.SearchSites[0].Label != "My tracker" || len(result.Data.Warnings) != 0 || calls.Load() != 0 {
		t.Fatalf("native options: calls=%d %d %s", calls.Load(), response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "secret-cookie") || strings.Contains(response.Body.String(), "token=hidden") {
		t.Fatal("catalog options leaked credentials")
	}
	compat := performFormRequest(handler, "/api/v1/site/indexers", token, nil)
	if compat.Code != http.StatusOK || !strings.Contains(compat.Body.String(), `"id":"zhuzhu"`) || calls.Load() != 0 {
		t.Fatalf("legacy indexer route used Python: calls=%d %s", calls.Load(), compat.Body.String())
	}
	response = performFormRequest(handler, "/api/v1/config/set", token, url.Values{"key": {"UserInstalledPlugins"}, "value": {`["CustomReleaseGroups","Customization"]`}})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	compat = performFormRequest(handler, "/api/v1/site/indexers", token, nil)
	if compat.Code != 200 || calls.Load() != 0 || !strings.Contains(compat.Body.String(), `"id":"zhuzhu"`) {
		t.Fatalf("native metadata plugins blocked indexers: %d %s", compat.Code, compat.Body.String())
	}
	response = performFormRequest(handler, "/api/v1/config/set", token, url.Values{"key": {"UserInstalledPlugins"}, "value": {`["Plugin"]`}})
	if response.Code != 200 {
		t.Fatal("set installed plugins failed")
	}
	response = nativeJSONRequest(handler, http.MethodGet, "/api/v1/subscriptions/options", token, "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "searchSites unavailable") || calls.Load() != 0 {
		t.Fatalf("plugin indexers must report migration gap: calls=%d %s", calls.Load(), response.Body.String())
	}
	compat = performFormRequest(handler, "/api/v1/site/indexers", token, nil)
	if compat.Code != http.StatusNotImplemented || calls.Load() != 0 {
		t.Fatalf("plugin indexer route silently proxied: calls=%d %d %s", calls.Load(), compat.Code, compat.Body.String())
	}
}
