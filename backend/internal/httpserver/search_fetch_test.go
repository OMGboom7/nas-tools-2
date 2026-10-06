package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestNativeFetchScopedIndexersAndHTTPPermission(t *testing.T) {
	queries := []string{}
	discoveries := 0
	failSecond := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "indexer.local" || r.Header.Get("X-Api-Key") != "private-key" {
			t.Fatal("unexpected credential destination")
		}
		if r.URL.Path == "/api/v1/indexerstats" {
			discoveries++
			return jsonResponse(r, `{"indexers":[{"indexerId":42,"indexerName":"First"},{"indexerId":43,"indexerName":"Second"}]}`), nil
		}
		id := r.URL.Query().Get("indexerIds")
		queries = append(queries, id)
		if id == "43" && failSecond {
			response := jsonResponse(r, "private upstream failure")
			response.StatusCode = 500
			return response, nil
		}
		if id != "42" && id != "43" {
			t.Fatal("invalid selected indexer", id)
		}
		return jsonResponse(r, `[{"indexerId":`+id+`,"title":"Movie.2026.1080p","size":1024,"downloadUrl":"`+testMagnet+`"}]`), nil
	})
	handler, token, path := nativeServicesFixture(t, "", nil, transport)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for key, value := range map[string]string{"UserIndexerSites": `["First-prowlarr","Second-prowlarr"]`, "UserInstalledPlugins": `["Prowlarr"]`, "plugin.Prowlarr": `{"host":"http://indexer.local","api_key":"private-key"}`} {
		if err := store.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	// The private fetch layer needs neither a fabricated JWT nor a browser cache.
	service := nativeExternalResourceSearch{system: store, transport: transport}
	resources, err := service.fetchResources(t.Context(), "Movie", []string{"Second-prowlarr", "Second-prowlarr"})
	if err != nil || len(resources) != 1 || len(queries) != 1 || queries[0] != "43" || discoveries != 1 {
		t.Fatal(resources, err, queries, discoveries)
	}
	before := discoveries
	for _, requested := range [][]string{{"Missing-prowlarr"}, {"First-prowlarr", "Missing-prowlarr"}, {""}, {"https://private.local/key"}} {
		if resources, err := service.fetchResources(t.Context(), "Movie", requested); !errors.Is(err, errNativeSearchSelection) || resources != nil || discoveries != before {
			t.Fatal("invalid subset widened or fetched", requested, err, discoveries)
		}
	}
	response := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie","quick":true,"indexers":["First-prowlarr"]}`)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"total":1`) || strings.Contains(response.Body.String(), "private-key") || strings.Contains(response.Body.String(), "magnet:") || queries[len(queries)-1] != "42" {
		t.Fatal(response.Code, response.Body.String(), queries)
	}
	before = discoveries
	response = nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie","indexers":["Missing-prowlarr"]}`)
	if response.Code != 400 || discoveries != before {
		t.Fatal(response.Code, response.Body.String())
	}
	created := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	response = nativeJSONRequest(handler, "POST", "/api/v1/search/resources", viewer, `{"keyword":"Movie","indexers":["First-prowlarr"]}`)
	if response.Code != 403 || discoveries != before {
		t.Fatal(response.Code, discoveries)
	}
	queries = nil
	resources, err = service.fetchResources(t.Context(), "Movie", nil)
	if err != nil || len(resources) != 2 || len(queries) != 2 {
		t.Fatal(len(resources), err, queries)
	}
	failSecond = true
	resources, err = service.fetchResources(t.Context(), "Movie", nil)
	if err == nil || resources != nil {
		t.Fatal("partially failed fetch returned successful resources", resources, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before = discoveries
	if _, err := service.fetchResources(ctx, "Movie", nil); !errors.Is(err, context.Canceled) || discoveries != before {
		t.Fatal(err, discoveries)
	}
	if _, err := service.fetchResources(t.Context(), " ", nil); err == nil || discoveries != before {
		t.Fatal("empty query fetched")
	}
	failSecond = false
	if err := store.Set(t.Context(), "UserIndexerSites", `["First-prowlarr","unrelated-unavailable-builtin"]`); err != nil {
		t.Fatal(err)
	}
	resources, err = service.fetchResources(t.Context(), "Movie", []string{"First-prowlarr"})
	if err != nil || len(resources) != 1 {
		t.Fatal("an unrelated global tracker broke the requested subset", err)
	}
	before = discoveries
	if resources, err := service.fetchResources(t.Context(), "Movie", nil); err == nil || resources != nil || discoveries != before {
		t.Fatal("unsupported global tracker was hidden", resources, err)
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `[]`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.fetchResources(t.Context(), "Movie", []string{"First-prowlarr"}); err == nil || discoveries != before {
		t.Fatal("uninstalled provider was queried")
	}
}

func TestScopedSearchDoesNotDropSelectionIntoLegacy(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("scoped request must not reach the old backend")
		return nil, context.Canceled
	})
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy.local"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	response := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", "token", `{"keyword":"Movie","indexers":["First-prowlarr"]}`)
	if response.Code != 501 {
		t.Fatal(response.Code, response.Body.String())
	}
}
