package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestNativeAddDownloadWithTaskOptions(t *testing.T) {
	addCalls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "qb.example:8080" {
			t.Fatalf("called unexpected service: %s", request.URL)
		}
		if strings.HasSuffix(request.URL.Path, "/auth/login") {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("Ok."))}, nil
		}
		addCalls++
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("savepath") != "/downloads/rss" || request.Form.Get("category") != "series" || request.Form.Get("paused") != "true" || request.Form.Get("upLimit") != "10240" {
			t.Fatalf("add form=%v", request.Form)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("Ok."))}, nil
	})
	downloader := downloaderconfig.Downloader{Name: "qB", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.example","port":"8080","username":"admin","password":"secret"}`}
	_, _, path := nativeServicesFixture(t, "", &downloader, transport)
	store, err := downloaderconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	system, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	service := downloadService{client: &http.Client{Transport: transport}, downloaders: store, systemConfig: system}
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	id, handled, err := service.nativeAddDownloadWithOptions(context.Background(), magnet, nil, "", downloadAddOptions{SavePath: "/downloads/rss", Category: "series", Tags: []string{"rss"}, Paused: true, UploadLimitKB: 10})
	if err != nil || !handled || id != "0123456789abcdef0123456789abcdef01234567" || addCalls != 1 {
		t.Fatalf("add id=%q handled=%v err=%v calls=%d", id, handled, err, addCalls)
	}
	if _, handled, err := service.nativeAddDownloadWithOptions(context.Background(), magnet, nil, "", downloadAddOptions{Tags: []string{"bad,tag"}}); !handled || !errors.Is(err, qbittorrent.ErrConfiguration) {
		t.Fatalf("invalid settings handled=%v err=%v", handled, err)
	}
}
