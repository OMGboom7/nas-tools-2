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

func TestTNodeSearchAndDownloadAPIWithoutPython(t *testing.T) {
	adds, fetches := 0, 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "zhuque.in":
			if r.Header.Get("Cookie") != "session=private" || r.Header.Get("User-Agent") != "Site Agent" {
				t.Fatal(r.Header)
			}
			switch r.URL.Path {
			case "/":
				return jsonResponse(r, `<meta name="x-csrf-token" content="server-token">`), nil
			case "/api/torrent/advancedSearch":
				if r.Header.Get("X-CSRF-TOKEN") != "server-token" {
					t.Fatal("missing CSRF")
				}
				return jsonResponse(r, `{"data":{"torrents":[{"id":42,"title":"Movie.2026.1080p.WEB-DL","size":1024,"seeding":3,"downloadRate":0,"uploadRate":1}]}}`), nil
			case "/api/torrent/download/42":
				if r.Header.Get("X-CSRF-TOKEN") != "" {
					t.Fatal("CSRF leaked")
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
			body, err := io.ReadAll(file)
			file.Close()
			if err != nil || string(body) != string(testTorrent) {
				t.Fatal(err)
			}
			adds++
			return jsonResponse(r, "Ok."), nil
		default:
			t.Fatalf("unexpected upstream; Python prohibited: %s", r.URL)
		}
		return nil, nil
	})
	downloader := downloaderconfig.Downloader{Name: "qB", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.local","port":8080,"username":"admin","password":"secret"}`}
	_, _, path := nativeServicesFixture(t, "", &downloader, transport)
	sites, err := siteconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sites.Close()
	if _, err := sites.Upsert(t.Context(), siteconfig.Site{Name: "朱雀", SignURL: "https://zhuque.in/", Cookie: "session=private", Note: `{"ua":"Site Agent"}`}); err != nil {
		t.Fatal(err)
	}
	system, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	if err := system.Set(t.Context(), "UserIndexerSites", `["zhuque"]`); err != nil {
		t.Fatal(err)
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(path), "config.yaml"), SiteCatalogPath: "../../../web/backend/user.sites.bin", DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, handler, "admin", "password")
	response := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie","quick":true}`)
	var payload struct{ Data searchData }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Data.Total != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "server-token") {
		t.Fatal("search leaked authentication")
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
	if adds != 1 || fetches != 1 {
		t.Fatal(adds, fetches)
	}
}
