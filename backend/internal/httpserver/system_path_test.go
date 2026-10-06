package httpserver

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

func TestNativeSystemPathListsFoldersAndFileFiltersWithoutPython(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "Films")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"movie.MKV", "subtitle.srt", "track.mka", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("system path used Python: %s", request.URL)
		return nil, fmt.Errorf("legacy backend disabled")
	})
	downloader := downloaderconfig.Downloader{Name: "Local", Type: "qbittorrent", Enabled: 1, Config: `{}`, DownloadDir: fmt.Sprintf(`[{"save_path":"/remote","container_path":%s}]`, strconv.Quote(root))}
	handler, token, databasePath := nativeServicesFixture(t, "media:\n  movie_path: "+strconv.Quote(root)+"\n", &downloader, transport)
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE CONFIG_SYNC_PATHS (ID INTEGER PRIMARY KEY, SOURCE TEXT, DEST TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO CONFIG_SYNC_PATHS (SOURCE, DEST) VALUES (?,?)`, root, subdir); err != nil {
		t.Fatal(err)
	}
	if response := performFormRequest(handler, "/api/v1/system/path", "", url.Values{"dir": {root}, "filter": {"ALL"}}); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated path=%d", response.Code)
	}
	for _, item := range []struct {
		dir, filter string
		count       int
		contains    string
		excludes    string
	}{
		{root, "ALL", 5, "movie.MKV", ""},
		{root, "ONLYDIR", 1, "Films", "movie.MKV"},
		{root, "MEDIAFILE", 1, "movie.MKV", "subtitle.srt"},
		{root, "SUBFILE|AUDIOTRACKFILE", 2, "subtitle.srt", "movie.MKV"},
		{filepath.Join(root, "notes.txt"), "ONLYDIR", 1, "Films", "notes.txt"},
		{"*MEDIA-FOLDERS*", "ONLYDIR", 1, filepath.Base(root), "movie.MKV"},
		{"*DOWNLOAD-FOLDERS*", "ONLYDIR", 1, filepath.Base(root), "movie.MKV"},
		{"*SYNC-FOLDERS*", "ONLYDIR", 2, "Films", "movie.MKV"},
	} {
		response := performFormRequest(handler, "/api/v1/system/path", token, url.Values{"dir": {item.dir}, "filter": {item.filter}})
		body := response.Body.String()
		if response.Code != 200 || !strings.Contains(body, `"code":0`) || !strings.Contains(body, fmt.Sprintf(`"count":%d`, item.count)) || item.contains != "" && !strings.Contains(body, item.contains) || item.excludes != "" && strings.Contains(body, item.excludes) {
			t.Fatalf("dir=%q filter=%q status=%d body=%s", item.dir, item.filter, response.Code, body)
		}
	}
	invalid := performFormRequest(handler, "/api/v1/system/path", token, url.Values{"dir": {"%ZZ"}, "filter": {"ALL"}})
	if invalid.Code != 200 || !strings.Contains(invalid.Body.String(), `"code":-1`) {
		t.Fatalf("invalid path=%d %s", invalid.Code, invalid.Body.String())
	}
}

func TestNativeSystemPathWithoutConfigurationDoesNotUsePython(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("system path used Python: %s", request.URL)
		return nil, fmt.Errorf("legacy backend disabled")
	})
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	response := performFormRequest(handler, "/api/v1/system/path", "test-token", url.Values{"dir": {"/"}, "filter": {"ONLYDIR"}})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing native config=%d %s", response.Code, response.Body.String())
	}
}
