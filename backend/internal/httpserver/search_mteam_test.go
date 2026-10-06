package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestMTeamSearchAndDownloadAPIWithoutPython(t *testing.T) {
	adds, tokens, fetches := 0, 0, 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "api.m-team.cc":
			switch r.URL.Path {
			case "/api/torrent/search":
				if r.Header.Get("x-api-key") != "mteam-server-secret" {
					t.Fatal("missing key")
				}
				return jsonResponse(r, `{"code":"0","data":{"data":[{"id":"42","name":"Movie.2026.1080p.WEB-DL","size":"1024","status":{"discount":"FREE","seeders":"3","leechers":"0"}}]}}`), nil
			case "/api/torrent/genDlToken":
				if r.Header.Get("x-api-key") != "mteam-server-secret" || r.ParseForm() != nil || r.PostForm.Get("id") != "42" {
					t.Fatal(r.Header, r.PostForm)
				}
				tokens++
				return jsonResponse(r, `{"code":"0","data":"https://api.m-team.cc/api/rss/dl?token=private"}`), nil
			case "/api/rss/dl":
				if r.Header.Get("x-api-key") != "" {
					t.Fatal("key leaked")
				}
				fetches++
				return jsonResponse(r, string(testTorrent)), nil
			default:
				t.Fatal(r.URL)
			}
		case "qb.local":
			if strings.HasSuffix(r.URL.Path, "/auth/login") {
				return jsonResponse(r, "Ok."), nil
			}
			if r.URL.Path != "/api/v2/torrents/add" || r.ParseMultipartForm(4<<20) != nil {
				t.Fatal(r.URL)
			}
			file, _, err := r.FormFile("torrents")
			if err != nil {
				t.Fatal(err)
			}
			contents, err := io.ReadAll(file)
			file.Close()
			if err != nil || string(contents) != string(testTorrent) {
				t.Fatal(err)
			}
			adds++
			return jsonResponse(r, "Ok."), nil
		default:
			t.Fatalf("unexpected request; Python prohibited: %s", r.URL)
		}
		return nil, nil
	})
	downloader := downloaderconfig.Downloader{Name: "qB", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.local","port":8080,"username":"admin","password":"secret"}`}
	_, _, database := nativeServicesFixture(t, "", &downloader, transport)
	sites, err := siteconfig.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer sites.Close()
	if _, err := sites.Upsert(t.Context(), siteconfig.Site{Name: "MTeam", SignURL: "https://kp.m-team.cc/", APIKey: "mteam-server-secret", Note: `{"ua":"MTeam Agent"}`}); err != nil {
		t.Fatal(err)
	}
	system, err := systemconfig.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	if err := system.Set(t.Context(), "UserIndexerSites", `["mteam-kpcc"]`); err != nil {
		t.Fatal(err)
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(database), "config.yaml"), SiteCatalogPath: "../../../web/backend/user.sites.bin", DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, handler, "admin", "password")
	response := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie","quick":true}`)
	var payload struct{ Data searchData }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Data.Total != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "/detail/") {
		t.Fatal("private metadata exposed")
	}
	resource := payload.Data.Items[0].Resources[0]
	if resource.DownloadFactor != 0 || resource.PromotionKnown == nil || !*resource.PromotionKnown {
		t.Fatal(resource)
	}
	for i := 0; i < 2; i++ {
		response = nativeJSONRequest(handler, "POST", "/api/v1/downloads/resource", token, `{"resourceId":"`+resource.ID+`"}`)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if adds != 1 || tokens != 1 || fetches != 1 {
		t.Fatal(adds, tokens, fetches)
	}
}
