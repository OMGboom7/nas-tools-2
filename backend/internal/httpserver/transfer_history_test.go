package httpserver

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTransferHistoryNativeListAndStatistics(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("transfer history called Python: %s", request.URL)
		return nil, fmt.Errorf("legacy backend disabled")
	})
	handler, token, path := nativeServicesFixture(t, "", nil, transport)
	if got := performFormRequest(handler, "/api/v1/organization/history/list", "", url.Values{}); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list=%d", got.Code)
	}
	for _, route := range []string{"list", "statistics"} {
		got := performFormRequest(handler, "/api/v1/organization/history/"+route, token, url.Values{})
		if got.Code != 200 || !strings.Contains(got.Body.String(), `"code":0`) {
			t.Fatalf("missing table %s=%d %s", route, got.Code, got.Body.String())
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY, MODE TEXT, TYPE TEXT, CATEGORY TEXT, TMDBID INTEGER, TITLE TEXT, YEAR TEXT, SEASON_EPISODE TEXT, SOURCE TEXT, SOURCE_PATH TEXT, SOURCE_FILENAME TEXT, DEST TEXT, DEST_PATH TEXT, DEST_FILENAME TEXT, DATE TEXT)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	for _, row := range []struct{ title, mode, kind, filename, date string }{
		{"Older movie", "硬链接", "电影", "older.mkv", "2020-01-01 00:00:00"},
		{"New movie", "复制", "电影", "new.mkv", now},
		{"New TV", "移动", "电视剧", "tv.mkv", now},
	} {
		if _, err := db.Exec(`INSERT INTO TRANSFER_HISTORY (TITLE,MODE,TYPE,SOURCE_FILENAME,DATE) VALUES (?,?,?,?,?)`, row.title, row.mode, row.kind, row.filename, row.date); err != nil {
			t.Fatal(err)
		}
	}
	list := performFormRequest(handler, "/api/v1/organization/history/list", token, url.Values{"page": {"1"}, "pagenum": {"1"}, "keyword": {"movie"}})
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"total":2`) || !strings.Contains(list.Body.String(), `"totalPage":3`) || !strings.Contains(list.Body.String(), `"RMT_MODE":"copy"`) || strings.Contains(list.Body.String(), "Older movie") {
		t.Fatalf("filtered list=%d %s", list.Code, list.Body.String())
	}
	stats := performFormRequest(handler, "/api/v1/organization/history/statistics", token, url.Values{})
	if stats.Code != 200 || !strings.Contains(stats.Body.String(), `"MovieNums"`) || !strings.Contains(stats.Body.String(), `"TvNums"`) || strings.Contains(stats.Body.String(), "2020-01-01") {
		t.Fatalf("statistics=%d %s", stats.Code, stats.Body.String())
	}
	bad := performFormRequest(handler, "/api/v1/organization/history/list", token, url.Values{"pagenum": {"0"}})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid page size=%d", bad.Code)
	}
}
