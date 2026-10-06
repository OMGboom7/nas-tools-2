package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestLegacySiteFormRoutesUseNativeStoreAndConnection(t *testing.T) {
	calls := 0
	handler, store, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != "https://tracker.example/" || request.Header.Get("Cookie") != "session=secret" {
			t.Fatalf("unexpected outgoing request: %s", request.URL)
		}
		return jsonResponse(request, `<a href="/logout.php">Log out</a>`), nil
	}))
	unauthorized := performFormRequest(handler, "/api/v1/site/list", "", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized list: %d %s", unauthorized.Code, unauthorized.Body.String())
	}
	created := performFormRequest(handler, "/api/v1/site/update", token, url.Values{
		"site_name": {"Tracker"}, "site_pri": {"3"}, "site_signurl": {"https://tracker.example/private?token=hidden"},
		"site_rssurl": {"https://tracker.example/rss"}, "site_cookie": {"session=secret"},
		"site_include": {"D"}, "site_note": {`{"ua":"private-agent","extension":{"keep":true}}`},
	})
	if created.Code != http.StatusOK || !strings.Contains(created.Body.String(), `"code":"200"`) {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	items, err := store.List(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("created sites: %#v %v", items, err)
	}
	id := text(items[0].ID)
	listed := performFormRequest(handler, "/api/v1/site/list", token, url.Values{"basic": {"1"}, "rss": {"1"}})
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), "session=secret") {
		t.Fatalf("basic list: %d %s", listed.Code, listed.Body.String())
	}
	var list struct {
		Sites []map[string]any `json:"sites"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil || len(list.Sites) != 1 || list.Sites[0]["name"] != "Tracker" || len(list.Sites[0]) != 2 {
		t.Fatalf("basic list: %#v %v", list, err)
	}
	updated := performFormRequest(handler, "/api/v1/site/update", token, url.Values{"site_id": {id}, "site_name": {"Renamed Tracker"}, "site_note": {`{"ua":"new-agent"}`}})
	if updated.Code != http.StatusOK {
		t.Fatalf("update: %d %s", updated.Code, updated.Body.String())
	}
	item, err := store.Get(context.Background(), items[0].ID)
	if err != nil || item.Name != "Renamed Tracker" || item.Cookie != "session=secret" || item.SignURL != items[0].SignURL || item.RSSURL != items[0].RSSURL || !strings.Contains(item.Note, `"keep":true`) {
		t.Fatalf("update lost stored fields: %#v %v", item, err)
	}
	tested := performFormRequest(handler, "/api/v1/site/test", token, url.Values{"id": {id}})
	if tested.Code != http.StatusOK || !strings.Contains(tested.Body.String(), `"code":0`) || calls != 1 || strings.Contains(tested.Body.String(), "session=secret") {
		t.Fatalf("test: %d calls=%d %s", tested.Code, calls, tested.Body.String())
	}
	deleted := performFormRequest(handler, "/api/v1/site/delete", token, url.Values{"id": {id}})
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"code":true`) {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body.String())
	}
	if _, err := store.Get(context.Background(), items[0].ID); err != siteconfig.ErrNotFound {
		t.Fatalf("site still present: %v", err)
	}
}

func TestLegacySiteInfoReadsCatalogCapabilities(t *testing.T) {
	handler, store, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("site info unexpectedly used network: %s", request.URL)
		return nil, nil
	}), "../../../web/backend/user.sites.bin")
	item, err := store.Upsert(context.Background(), siteconfig.Site{Name: "Piggo", SignURL: "https://piggo.me/private?token=hidden", Cookie: "session=secret"})
	if err != nil {
		t.Fatal(err)
	}
	response := performFormRequest(handler, "/api/v1/site/info", token, url.Values{"id": {text(item.ID)}})
	if response.Code != http.StatusOK {
		t.Fatalf("info: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Site      map[string]any `json:"site"`
		Free      bool           `json:"site_free"`
		Double    bool           `json:"site_2xfree"`
		HitAndRun bool           `json:"site_hr"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.Free || !result.Double || result.HitAndRun || result.Site["name"] != "Piggo" {
		t.Fatalf("info: %+v err=%v", result, err)
	}
}

func TestLegacySiteCookieUpdateUsesAPIKeyAndPreservesAttributes(t *testing.T) {
	handler, store, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("cookie update unexpectedly used network: %s", request.URL)
		return nil, nil
	}))
	item, err := store.Upsert(context.Background(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.example", Cookie: "old-secret", Note: `{"ua":"old-agent","extension":{"keep":true}}`})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"site_id": {text(item.ID)}, "site_cookie": {"new-secret"}, "site_ua": {"new-agent"}}
	for _, credential := range []string{"invalid", token} {
		response := performFormRequest(handler, "/api/v1/site/cookie/update", credential, form)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("invalid API key accepted: %d %s", response.Code, response.Body.String())
		}
	}
	response := performFormRequest(handler, "/api/v1/site/cookie/update", "native-sites-secret", form)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "new-secret") {
		t.Fatalf("cookie update: %d %s", response.Code, response.Body.String())
	}
	updated, err := store.Get(context.Background(), item.ID)
	if err != nil || updated.Cookie != "new-secret" || !strings.Contains(updated.Note, `"ua":"new-agent"`) || !strings.Contains(updated.Note, `"keep":true`) {
		t.Fatalf("cookie update lost attributes: %+v %v", updated, err)
	}
	query := performFormRequest(handler, "/api/v1/site/cookie/update?apikey=native-sites-secret", "invalid", url.Values{"site_id": {text(item.ID)}, "site_cookie": {"query-secret"}})
	if query.Code != http.StatusOK {
		t.Fatalf("query API key compatibility: %d %s", query.Code, query.Body.String())
	}
	updated, err = store.Get(context.Background(), item.ID)
	if err != nil || updated.Cookie != "query-secret" || !strings.Contains(updated.Note, `"ua":"new-agent"`) {
		t.Fatalf("query update changed unrelated fields: %+v %v", updated, err)
	}
}

func TestLegacySiteAPISitesRequiresAPIKeyAndReadsNativeStore(t *testing.T) {
	handler, store, _ := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("site API unexpectedly used network: %s", request.URL)
		return nil, nil
	}))
	if _, err := store.Upsert(context.Background(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.example", Cookie: "session=secret"}); err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		path, key string
		want      int
	}{
		{"/api/v1/site/sites", "", http.StatusUnauthorized},
		{"/api/v1/site/sites", "bad-key", http.StatusUnauthorized},
		{"/api/v1/site/sites", "native-sites-secret", http.StatusOK},
		{"/api/v1/site/sites?apikey=native-sites-secret", "", http.StatusOK},
	} {
		request := httptest.NewRequest(http.MethodGet, input.path, nil)
		if input.key != "" {
			request.Header.Set("Authorization", input.key)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != input.want {
			t.Fatalf("API sites %q: %d %s", input.path, response.Code, response.Body.String())
		}
		if input.want == http.StatusOK && !strings.Contains(response.Body.String(), `"user_sites"`) {
			t.Fatalf("native API site list is empty: %s", response.Body.String())
		}
	}
}
