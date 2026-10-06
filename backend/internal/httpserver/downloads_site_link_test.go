package httpserver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestNativeSiteLinkFetchAndAddWithoutPython(t *testing.T) {
	t.Parallel()
	calls := []string{}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.URL.Host+request.URL.Path)
		switch request.URL.Host {
		case "tracker.example":
			if request.URL.Path != "/download/42" || request.Header.Get("Cookie") != "session=private-cookie" || request.Header.Get("User-Agent") != "Site Agent" {
				t.Fatalf("site fetch = %s %v", request.URL, request.Header)
			}
			return jsonResponse(request, string(testTorrent)), nil
		case "qb.local:8080":
			if request.Header.Get("Cookie") == "session=private-cookie" {
				t.Fatal("site cookie leaked to downloader")
			}
			if strings.HasSuffix(request.URL.Path, "/auth/login") {
				return jsonResponse(request, "Ok."), nil
			}
			if request.URL.Path != "/api/v2/torrents/add" || request.ParseMultipartForm(1<<20) != nil {
				t.Fatalf("bad qB request: %s", request.URL)
			}
			file, _, err := request.FormFile("torrents")
			if err != nil {
				t.Fatal(err)
			}
			contents, _ := io.ReadAll(file)
			_ = file.Close()
			if string(contents) != string(testTorrent) {
				t.Fatalf("torrent changed: %q", contents)
			}
			return jsonResponse(request, "Ok."), nil
		default:
			t.Fatalf("unexpected destination, including Python: %s", request.URL)
			return nil, nil
		}
	})
	downloader := downloaderconfig.Downloader{Name: "Default", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.local","port":8080,"username":"admin","password":"private-password"}`}
	handler, token, databasePath := nativeServicesFixture(t, "", &downloader, transport)
	store, err := siteconfig.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	site, err := store.Upsert(context.Background(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.example/signin", Cookie: "session=private-cookie", Note: `{"ua":"Site Agent"}`})
	if err != nil {
		t.Fatal(err)
	}
	response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/link", token, `{"siteId":"`+text(site.ID)+`","url":"https://tracker.example/download/42"}`)
	if response.Code != 200 || len(calls) != 3 || strings.Contains(response.Body.String(), "private-cookie") || strings.Contains(response.Body.String(), "private-password") {
		t.Fatalf("link add = %d %s calls=%v", response.Code, response.Body.String(), calls)
	}
}

func TestNativeSiteLinkRejectsCrossSiteAndRedirect(t *testing.T) {
	t.Parallel()
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "tracker.example" {
			t.Fatalf("followed redirect or called downloader: %s", request.URL)
		}
		response := jsonResponse(request, "redirect")
		response.StatusCode = http.StatusFound
		response.Header.Set("Location", "https://other.example/collect")
		return response, nil
	})
	downloader := downloaderconfig.Downloader{Name: "Default", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.local","port":8080}`}
	handler, token, databasePath := nativeServicesFixture(t, "", &downloader, transport)
	store, err := siteconfig.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	site, err := store.Upsert(context.Background(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.example/", Cookie: "session=secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{"https://other.example/file.torrent", "http://tracker.example/file.torrent", "https://user:pass@tracker.example/file.torrent", "file:///etc/passwd"} {
		response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/link", token, `{"siteId":"`+text(site.ID)+`","url":"`+link+`"}`)
		if response.Code != 400 || calls != 0 {
			t.Fatalf("accepted %s: %d %s calls=%d", link, response.Code, response.Body.String(), calls)
		}
	}
	response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/link", token, `{"siteId":"`+text(site.ID)+`","url":"https://tracker.example/file.torrent"}`)
	if response.Code != 502 || calls != 1 {
		t.Fatalf("redirect = %d %s calls=%d", response.Code, response.Body.String(), calls)
	}
	response = nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/link", "", `{"siteId":"`+text(site.ID)+`","url":"https://tracker.example/file.torrent"}`)
	if response.Code != 401 || calls != 1 {
		t.Fatalf("unauthenticated request = %d calls=%d", response.Code, calls)
	}
}
