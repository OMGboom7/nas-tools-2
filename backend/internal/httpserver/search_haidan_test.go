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

func TestHaiDanNativeSearchAPIUsesCatalog(t *testing.T) {
	adds, fetches := 0, 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "qb.local" {
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
		}
		if r.URL.Host != "www.haidan.video" || r.Header.Get("Cookie") != "session=private" {
			t.Fatalf("unexpected upstream; Python prohibited: %s", r.URL)
		}
		if r.URL.Path == "/download.php" && r.URL.Query().Get("id") == "10" {
			fetches++
			return jsonResponse(r, string(testTorrent)), nil
		}
		if r.URL.Path != "/torrents.php" {
			t.Fatal(r.URL)
		}
		return jsonResponse(r, `<div class="torrent_panel_inner"><div class="torrent_group"><a href="details.php?group_id=5">Movie.2026</a><div class="torrent_wrap"><a href="details.php?group_id=5&amp;torrent_id=10">1080p.WEB-DL</a><a href="download.php?id=10">download</a><div class="video_size">1 GB</div><div class="seeder_col">3</div></div></div></div>`), nil
	})
	downloader := downloaderconfig.Downloader{Name: "qB", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.local","port":8080,"username":"admin","password":"secret"}`}
	_, _, filename := nativeServicesFixture(t, "", &downloader, transport)
	sites, err := siteconfig.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer sites.Close()
	handler, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(filename), "config.yaml"), SiteCatalogPath: "../../../web/backend/user.sites.bin", DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, handler, "admin", "password")
	if _, err := sites.Upsert(t.Context(), siteconfig.Site{Name: "海胆", SignURL: "https://www.haidan.video/", Cookie: "session=private"}); err != nil {
		t.Fatal(err)
	}
	system, err := systemconfig.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	if err := system.Set(t.Context(), "UserIndexerSites", `["haidan"]`); err != nil {
		t.Fatal(err)
	}
	response := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie","quick":true}`)
	var payload struct{ Data searchData }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Data.Total != 1 || payload.Data.Items[0].Resources[0].Name != "Movie.2026 1080p.WEB-DL" {
		t.Fatal(response.Code, response.Body.String())
	}
	id := payload.Data.Items[0].Resources[0].ID
	for i := 0; i < 2; i++ {
		response = nativeJSONRequest(handler, "POST", "/api/v1/downloads/resource", token, `{"resourceId":"`+id+`"}`)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if adds != 1 || fetches != 1 {
		t.Fatal(adds, fetches)
	}
}
