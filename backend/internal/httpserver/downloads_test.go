package httpserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

func TestDownloadsMissingNativeRuntimeNeverUsesConfiguredLegacy(t *testing.T) {
	calls := 0
	h, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("Python must not receive a task mutation or account token")
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/downloads", "/api/v1/downloads?historyPage=2", "/api/v1/downloads/task/stop", "/api/v1/downloads/task/remove", "/api/v1/downloads/task/start"} {
		method := "POST"
		if strings.HasPrefix(path, "/api/v1/downloads?") || path == "/api/v1/downloads" {
			method = "GET"
		}
		w := nativeJSONRequest(h, method, path, "private-token", `{}`)
		if w.Code != 503 || calls != 0 || strings.Contains(w.Body.String(), "private-token") {
			t.Fatal(path, w.Code, w.Body.String(), calls)
		}
	}
}

func TestDownloadOldSearchIDsRequireFreshSearchWithoutLegacyMutation(t *testing.T) {
	h, token, _ := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("stale resource reached backend", r.URL)
		return nil, nil
	}))
	for _, id := range []string{"42", "hash-1", "https://tracker.test/download?secret=private", "old-python-id"} {
		body, _ := json.Marshal(addResourceRequest{ResourceID: id, Directory: "/media", Setting: "3"})
		w := nativeJSONRequest(h, "POST", "/api/v1/downloads/resource", token, string(body))
		if w.Code != 409 || !strings.Contains(w.Body.String(), "重新搜索") || strings.Contains(w.Body.String(), "private") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := nativeJSONRequest(h, "POST", "/api/v1/downloads/resource", token, `{"resourceId":"go-search-0123456789abcdef0123456789abcdef"}`)
	if w.Code != 404 {
		t.Fatal("missing Go cache resource used fallback", w.Code, w.Body.String())
	}
}

func TestDownloadsNativeHistoryRemainsAvailableWhenClientFails(t *testing.T) {
	calls := 0
	d := downloaderconfig.Downloader{Name: "QB", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.local","port":8080}`}
	h, token, path := nativeServicesFixture(t, "", &d, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "qb.local:8080" {
			t.Fatal("failure used Python", r.URL)
		}
		return nil, fmt.Errorf("private-upstream-error")
	}))
	history, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close()
	if _, err := history.Exec(`CREATE TABLE DOWNLOAD_HISTORY(ID INTEGER PRIMARY KEY,TMDBID TEXT,TITLE TEXT,TYPE TEXT,YEAR TEXT,POSTER TEXT,TORRENT TEXT,DATE TEXT,SITE TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Exec(`INSERT INTO DOWNLOAD_HISTORY(TMDBID,TITLE,TYPE,YEAR,TORRENT,DATE,SITE) VALUES('100','Native History','电影','2026','Native.Movie.2026','2026-10-19 10:00:00','Tracker')`); err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(h, "GET", "/api/v1/downloads?historyPage=1", token, "")
	var result struct{ Data downloadsData }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Data.Active) != 0 || result.Data.HistoryPage != 1 || len(result.Data.History) != 1 || result.Data.History[0].Title != "Native History" || len(result.Data.Warnings) != 1 || result.Data.Warnings[0] != "active unavailable" || calls != 1 || strings.Contains(w.Body.String(), "private-upstream-error") {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
	w = nativeJSONRequest(h, "GET", "/api/v1/downloads?historyPage=2", token, "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Data.HistoryPage != 2 || len(result.Data.History) != 0 || calls != 2 {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
}

func TestDownloadsValidateBeforeAnyBackendRequest(t *testing.T) {
	h, token, _ := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("invalid query reached backend", r.URL)
		return nil, nil
	}))
	for _, page := range []string{"0", "-1", "1001", "bad"} {
		if w := nativeJSONRequest(h, "GET", "/api/v1/downloads?historyPage="+page, token, ""); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{}`, `{"resourceId":""}`, `{"resourceId":"42","extra":true}`} {
		if w := nativeJSONRequest(h, "POST", "/api/v1/downloads/resource", token, body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

// Shared by transport fixtures; never starts a real remote service.
func jsonResponse(request *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}
}
