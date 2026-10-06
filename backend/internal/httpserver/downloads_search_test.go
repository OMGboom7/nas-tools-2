package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/auth"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/searchcache"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestCachedSearchDownloadIsPrivateAndNeverCallsPython(t *testing.T) {
	calls := 0
	fetchFailure := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		switch r.URL.Hostname() {
		case "tracker.local":
			if r.URL.Query().Get("secret") != "hidden" {
				t.Fatal("private address missing")
			}
			response := jsonResponse(r, testMagnet)
			if fetchFailure {
				response.StatusCode = 500
			}
			return response, nil
		case "client.local":
			if strings.HasSuffix(r.URL.Path, "/auth/login") {
				return jsonResponse(r, "Ok."), nil
			}
			if r.URL.Path != "/api/v2/torrents/add" || r.ParseForm() != nil || r.PostForm.Get("urls") != testMagnet {
				t.Fatalf("bad submission %s", r.URL)
			}
			return jsonResponse(r, "Ok."), nil
		default:
			t.Fatalf("unexpected upstream (Python forbidden): %s", r.URL)
			return nil, nil
		}
	})
	downloader := downloaderconfig.Downloader{Name: "Default", Type: "qbittorrent", Enabled: 1, Config: `{"host":"client.local","port":8080,"username":"admin","password":"secret"}`}
	handler, token, path := nativeServicesFixture(t, "", &downloader, transport)
	appPath := filepath.Join(filepath.Dir(path), "config.yaml")
	application, err := config.LoadApplication(appPath)
	if err != nil {
		t.Fatal(err)
	}
	authService, err := auth.NewService(application)
	if err != nil {
		t.Fatal(err)
	}
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	clients, err := downloaderconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clients.Close()
	sites, err := siteconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sites.Close()
	cache := searchcache.New()
	service := downloadService{client: &http.Client{Transport: transport}, auth: &nativeAuthentication{service: authService}, resources: cache, downloaders: clients, systemConfig: store, configStore: config.NewStore(appPath), sites: sites}
	ids, err := cache.Put("0:admin", []externalindexer.Resource{{Title: "Movie", DownloadURL: "https://tracker.local/torrent?secret=hidden"}, {Title: "Retry", DownloadURL: "https://tracker.local/torrent?secret=hidden"}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(authorization, id, extra string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/downloads/resource", strings.NewReader(`{"resourceId":"`+id+`"`+extra+`}`))
		r.Header.Set("Authorization", authorization)
		w := httptest.NewRecorder()
		service.addResource(w, r)
		if strings.Contains(w.Body.String(), "hidden") {
			t.Fatal("download credential leaked")
		}
		return w
	}
	if w := call(token, ids[0], ""); w.Code != 200 {
		t.Fatalf("download=%d %s", w.Code, w.Body.String())
	}
	before := calls
	if w := call(token, ids[0], ""); w.Code != 200 || calls != before {
		t.Fatalf("duplicate=%d calls=%d", w.Code, calls)
	}
	if w := call(token, "go-search-missing", ""); w.Code != 404 || calls != before {
		t.Fatal("missing resource fetched")
	}
	if w := call("fake-token", ids[1], ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := call(token, ids[1], `,"directory":"override"`); w.Code != 501 {
		t.Fatal(w.Code)
	}
	fetchFailure = true
	if w := call(token, ids[1], ""); w.Code != 502 {
		t.Fatal(w.Code)
	}
	fetchFailure = false
	if w := call(token, ids[1], ""); w.Code != 200 {
		t.Fatalf("retry=%d %s", w.Code, w.Body.String())
	}
	created := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	if w := call(viewer, ids[0], ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
	// The real server reserves native IDs even when unknown; never forwards them.
	w := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/resource", token, `{"resourceId":"go-search-unknown"}`)
	if w.Code != 404 {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("server route=%d %s", w.Code, body)
	}
}
