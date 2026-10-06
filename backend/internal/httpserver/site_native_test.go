package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func nativeSiteFixture(t *testing.T, transport http.RoundTripper, catalogPaths ...string) (http.Handler, *siteconfig.Store, string) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte("app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: native-sites-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(directory, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE CONFIG_FILTER_GROUP (ID INTEGER PRIMARY KEY, GROUP_NAME TEXT, IS_DEFAULT TEXT, NOTE TEXT)",
		"INSERT INTO CONFIG_FILTER_GROUP VALUES (2, 'HD', 'Y', NULL)",
		"CREATE TABLE DOWNLOAD_HISTORY (ID INTEGER PRIMARY KEY, TITLE TEXT, YEAR TEXT, TYPE TEXT, TMDBID TEXT, SE TEXT, VOTE TEXT, POSTER TEXT, OVERVIEW TEXT, TORRENT TEXT, ENCLOSURE TEXT, SITE TEXT, DESC TEXT, DOWNLOADER TEXT, DOWNLOAD_ID TEXT, SAVE_PATH TEXT, DATE TEXT)",
		"INSERT INTO DOWNLOAD_HISTORY (ID,TITLE,YEAR,TYPE,TMDBID,POSTER,TORRENT,SITE,DATE) VALUES (1,'Fixture History','2026','电影','9001','/fixture.jpg','Fixture.Release','FixturePT','2026-09-01 12:00:00')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{ApplicationConfigPath: path, LegacyBackendURL: "http://legacy:3000"}
	if len(catalogPaths) != 0 {
		cfg.SiteCatalogPath = catalogPaths[0]
	}
	handler, err := newHandler(cfg, transport)
	if err != nil {
		t.Fatal(err)
	}
	store, err := siteconfig.Open(filepath.Join(directory, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return handler, store, loginForTest(t, handler, "admin", "password")
}

func nativeJSONRequest(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestNativeSiteLifecycleWithoutLegacyBackend(t *testing.T) {
	handler, store, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Errorf("unexpected outgoing request: %s", request.URL.Path)
		return nil, fmt.Errorf("legacy backend is disabled")
	}))
	ctx := context.Background()
	item, err := store.Upsert(ctx, siteconfig.Site{
		Name: "Original PT", Priority: "3", Include: "DT", Exclude: "keep-exclude", Size: "keep-size",
		SignURL: "https://tracker.example/private?token=hidden", RSSURL: "https://tracker.example/rss?passkey=hidden",
		Cookie: "secret-cookie", APIKey: "secret-api-key", Note: `{"ua":"secret-user-agent","extension":{"keep":true}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/sites/%d", item.ID)
	for _, endpoint := range []string{"/api/v1/sites", path} {
		response := nativeJSONRequest(handler, http.MethodGet, endpoint, token, "")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Original PT") {
			t.Fatalf("read %s: %d %s", endpoint, response.Code, response.Body.String())
		}
		for _, secret := range []string{"secret-cookie", "secret-api-key", "secret-user-agent", "token=hidden", "passkey=hidden"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("read %s leaked %q", endpoint, secret)
			}
		}
	}
	body := `{"name":"Renamed PT","priority":2,"siteUrl":"https://tracker.example","rssEnabled":true,"statisticEnabled":true}`
	response := nativeJSONRequest(handler, http.MethodPut, path, token, body)
	if response.Code != http.StatusOK {
		t.Fatalf("update: %d %s", response.Code, response.Body.String())
	}
	updated, err := store.Get(ctx, item.ID)
	if err != nil || updated.Name != "Renamed PT" || updated.Priority != "2" || updated.Cookie != item.Cookie || updated.APIKey != item.APIKey || updated.RSSURL != item.RSSURL || updated.SignURL != item.SignURL || updated.Exclude != item.Exclude || updated.Size != item.Size {
		t.Fatalf("update failed to preserve stored fields: %v", err)
	}
	var note map[string]any
	if err := json.Unmarshal([]byte(updated.Note), &note); err != nil || note["ua"] != "secret-user-agent" || note["extension"] == nil {
		t.Fatalf("update lost unexposed attributes: %v", err)
	}
	create := `{"name":"Renamed PT","priority":1,"siteUrl":"https://new.example"}`
	response = nativeJSONRequest(handler, http.MethodPost, "/api/v1/sites", token, create)
	if response.Code != http.StatusConflict {
		t.Fatalf("duplicate create: %d %s", response.Code, response.Body.String())
	}
	response = nativeJSONRequest(handler, http.MethodPost, "/api/v1/sites", token, strings.Replace(create, "Renamed PT", "New PT", 1))
	if response.Code != http.StatusOK {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	items, err := store.List(ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("created sites count = %d, err = %v", len(items), err)
	}
	clear := `{"name":"Renamed PT","priority":2,"siteUrl":"https://tracker.example","clearCookie":true,"clearApiKey":true,"clearRssUrl":true,"clearUserAgent":true}`
	response = nativeJSONRequest(handler, http.MethodPut, path, token, clear)
	if response.Code != http.StatusOK {
		t.Fatalf("clear secrets: %d %s", response.Code, response.Body.String())
	}
	updated, err = store.Get(ctx, item.ID)
	if err != nil || updated.Cookie != "" || updated.APIKey != "" || updated.RSSURL != "" || strings.Contains(updated.Note, "secret-user-agent") {
		t.Fatalf("explicit secret clearing failed: %v", err)
	}
	response = nativeJSONRequest(handler, http.MethodDelete, path, token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", response.Code, response.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		response = nativeJSONRequest(handler, method, path, token, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("deleted site %s: %d %s", method, response.Code, response.Body.String())
		}
	}
}

func TestNativeSiteAndDownloadOptionsReachPageResponses(t *testing.T) {
	handler, store, token := nativeSiteFixture(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Errorf("native options unexpectedly called %s", request.URL.Path)
		return nil, fmt.Errorf("legacy configuration access is disabled")
	}), "../../../web/backend/user.sites.bin")
	for _, item := range []siteconfig.Site{
		{Name: "RSS site", Include: "D", RSSURL: "https://tracker.example/rss"},
		{Name: "Disabled site", RSSURL: "https://tracker.example/rss"},
	} {
		if _, err := store.Upsert(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	for _, input := range []struct {
		path string
		form url.Values
	}{
		{"/api/v1/download/client/add", url.Values{"name": {"QB"}, "type": {"qbittorrent"}, "enabled": {"1"}, "transfer": {"1"}, "only_nastool": {"1"}, "config": {`{"host":"qb.local"}`}, "download_dir": {`[{"save_path":"/downloads/movies"}]`}}},
		{"/api/v1/config/set", url.Values{"key": {"DefaultDownloader"}, "value": {"1"}}},
		{"/api/v1/download/config/update", url.Values{"name": {"HD setting"}, "downloader": {"1"}, "is_paused": {"0"}}},
	} {
		response := performFormRequest(handler, input.path, token, input.form)
		if response.Code != http.StatusOK {
			t.Fatalf("seed %s: %d %s", input.path, response.Code, response.Body.String())
		}
	}
	response := nativeJSONRequest(handler, http.MethodGet, "/api/v1/subscriptions/options", token, "")
	var subscriptions struct {
		Data subscriptionOptions `json:"data"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &subscriptions) != nil {
		t.Fatalf("subscription options: %d %s", response.Code, response.Body.String())
	}
	options := subscriptions.Data
	if len(options.FilterRules) != 1 || options.FilterRules[0].Value != "2" || options.FilterRules[0].Label != "HD" {
		t.Fatalf("native filter options lost values: %+v", options.FilterRules)
	}
	if len(options.RSSSites) != 1 || options.RSSSites[0].Label != "RSS site" || len(options.DownloadSettings) != 2 || len(options.SavePaths) != 1 || options.SavePaths[0] != "/downloads/movies" || len(options.Warnings) != 0 {
		t.Fatalf("native subscription options lost values: %+v", options)
	}
	response = nativeJSONRequest(handler, http.MethodGet, "/api/v1/sites/options", token, "")
	var sites struct {
		Data siteOptions `json:"data"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &sites) != nil || len(sites.Data.DownloadSettings) != 2 || len(sites.Data.FilterRules) != 1 || sites.Data.FilterRules[0].Value != "2" || len(sites.Data.Warnings) != 0 {
		t.Fatalf("native site options lost values: %d %s", response.Code, response.Body.String())
	}
}
