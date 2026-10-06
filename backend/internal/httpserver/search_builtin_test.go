package httpserver

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestMixedBuiltinAndExternalSearchDownloadWithoutPython(t *testing.T) {
	page := `<table class="torrents"><tr><td><a class="title">Movie.2026.1080p.WEB-DL</a><a class="download" href="` + testMagnet + `">download</a><span class="size">1 GB</span><span class="seeders">3</span></td></tr></table>`
	searches, downloads := 0, 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "tracker.local":
			if r.Header.Get("Cookie") != "session=private" || r.Header.Get("User-Agent") != "site-agent" || r.URL.Query().Get("search") != "Movie & Query" {
				t.Fatal(r.URL, r.Header)
			}
			searches++
			return jsonResponse(r, page), nil
		case "indexer.local":
			if r.URL.Path == "/api/v1/indexerstats" {
				return jsonResponse(r, `{"indexers":[{"indexerId":42,"indexerName":"External"}]}`), nil
			}
			return jsonResponse(r, `[{"indexerId":42,"title":"Movie.2026.2160p.WEB-DL","size":2000,"seeders":9,"downloadUrl":"`+testMagnet+`"}]`), nil
		case "client.local":
			if strings.HasSuffix(r.URL.Path, "/auth/login") {
				return jsonResponse(r, "Ok."), nil
			}
			if r.URL.Path != "/api/v2/torrents/add" || r.ParseForm() != nil || r.PostForm.Get("urls") != testMagnet {
				t.Fatal(r.URL)
			}
			downloads++
			return jsonResponse(r, "Ok."), nil
		case "tmdb.test":
			switch r.URL.Path {
			case "/3/search/movie":
				return jsonResponse(r, `{"results":[{"id":100,"title":"Movie","release_date":"2026-01-01"}]}`), nil
			case "/3/movie/100":
				return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01"}`), nil
			default:
				t.Fatal(r.URL)
			}
		default:
			t.Fatalf("unexpected upstream; Python prohibited: %s", r.URL)
		}
		return nil, nil
	})
	downloader := downloaderconfig.Downloader{Name: "Default", Type: "qbittorrent", Enabled: 1, Config: `{"host":"client.local","port":8080,"username":"admin","password":"secret"}`}
	_, _, database := nativeServicesFixture(t, "", &downloader, transport)
	catalogPath := filepath.Join(filepath.Dir(database), "catalog.bin")
	catalog := `{"indexer":[{"id":"native-tracker","name":"Builtin","domain":"https://tracker.local/","search":{"paths":[{"path":"torrents.php"}],"params":{"search":"{keyword}"}},"torrents":{"list":{"selector":"table.torrents > tr:has(a)"},"fields":{"title":{"selector":"a.title"},"download":{"selector":"a.download","attribute":"href"},"size":{"selector":".size"},"seeders":{"selector":".seeders"},"downloadvolumefactor":{"case":{"*":0.5}},"uploadvolumefactor":{"case":{"*":1}}}}}],"conf":{}}`
	if err := os.WriteFile(catalogPath, []byte(base64.StdEncoding.EncodeToString([]byte(catalog))), 0600); err != nil {
		t.Fatal(err)
	}
	sites, err := siteconfig.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer sites.Close()
	site, err := sites.Upsert(t.Context(), siteconfig.Site{Name: "Native", SignURL: "https://tracker.local/login.php", Cookie: "session=private", Note: `{"ua":"site-agent","go_indexer_empty_selector":".no-results"}`})
	if err != nil {
		t.Fatal(err)
	}
	system, err := systemconfig.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	for key, value := range map[string]string{"UserInstalledPlugins": `["Prowlarr"]`, "UserIndexerSites": `["native-tracker","External-prowlarr"]`, "plugin.Prowlarr": `{"host":"http://indexer.local","api_key":"private-key"}`} {
		if err := system.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(database), "config.yaml"), SiteCatalogPath: catalogPath, DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, handler, "admin", "password")
	search := func() searchData {
		t.Helper()
		response := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie & Query","quick":true}`)
		var payload struct{ Data searchData }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &payload) != nil {
			t.Fatal(response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "magnet:") {
			t.Fatal("search leaked secrets")
		}
		return payload.Data
	}
	data := search()
	if data.Total != 2 || searches != 1 {
		t.Fatal(data, searches)
	}
	resource := data.Items[0].Resources[1]
	if resource.Site != "Native" || resource.DownloadFactor != 0.5 || resource.PromotionKnown == nil || !*resource.PromotionKnown {
		t.Fatal(resource)
	}
	for i := 0; i < 2; i++ {
		response := nativeJSONRequest(handler, "POST", "/api/v1/downloads/resource", token, `{"resourceId":"`+resource.ID+`"}`)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if downloads != 1 {
		t.Fatal(downloads)
	}
	if err := config.NewStore(filepath.Join(filepath.Dir(database), "config.yaml")).Update(map[string]any{"app.rmt_tmdbkey": "metadata-secret", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	normal := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie & Query","quick":false}`)
	var normalPayload struct{ Data searchData }
	if normal.Code != 200 || json.Unmarshal(normal.Body.Bytes(), &normalPayload) != nil || normalPayload.Data.Total != 2 || len(normalPayload.Data.Items) != 1 || normalPayload.Data.Items[0].Key != "movie:100" {
		t.Fatal(normal.Code, normal.Body.String())
	}
	if len(normalPayload.Data.Warnings) != 1 {
		t.Fatal("missing unknown inventory warning")
	}
	if err := system.Set(t.Context(), "UserIndexerSites", `["native-tracker"]`); err != nil {
		t.Fatal(err)
	}
	page = `<div class="no-results">No results</div>`
	if data := search(); data.Total != 0 {
		t.Fatal(data)
	}
	page = `<form><input type="password"></form>`
	response := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie & Query","quick":true}`)
	if response.Code != 502 {
		t.Fatal(response.Code, response.Body.String())
	}
	site.Cookie = ""
	if _, err := sites.Upsert(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	before := searches
	response = nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie & Query","quick":true}`)
	if response.Code != 502 || searches != before {
		t.Fatal(response.Code, searches)
	}
	if err := system.Set(t.Context(), "UserIndexerSites", `["unknown-builtin"]`); err != nil {
		t.Fatal(err)
	}
	response = nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie & Query","quick":true}`)
	if response.Code != 502 || strings.Contains(response.Body.String(), "legacy") {
		t.Fatal(response.Code, response.Body.String())
	}
}
