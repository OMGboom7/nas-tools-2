package httpserver

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNativeRSSParserLifecycleWithoutPython(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("RSS parser used legacy backend: %s", request.URL)
		return nil, fmt.Errorf("legacy backend disabled")
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO CONFIG_RSS_PARSER (ID,NAME,TYPE,FORMAT,PARAMS,NOTE,SYSDEF) VALUES (7,'Built in','XML','{}','old','preserve-note','Y')`); err != nil {
		t.Fatal(err)
	}
	unauthorized := performFormRequest(handler, "/api/v1/rss/parser/list", "", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list=%d %s", unauthorized.Code, unauthorized.Body.String())
	}
	list := performFormRequest(handler, "/api/v1/rss/parser/list", token, nil)
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"parsers":[{"id":7`) || !strings.Contains(list.Body.String(), `"note":"preserve-note"`) {
		t.Fatalf("list=%d %s", list.Code, list.Body.String())
	}
	updated := performFormRequest(handler, "/api/v1/rss/parser/update", token, url.Values{"id": {"7"}, "name": {"Updated"}, "type": {"JSON"}, "format": {`{"title":"$.title"}`}, "params": {"auth=1"}})
	if updated.Code != 200 || !strings.Contains(updated.Body.String(), `"code":0`) {
		t.Fatalf("update=%d %s", updated.Code, updated.Body.String())
	}
	info := performFormRequest(handler, "/api/v1/rss/parser/info", token, url.Values{"id": {"7"}})
	if info.Code != 200 || !strings.Contains(info.Body.String(), `"name":"Updated"`) || !strings.Contains(info.Body.String(), `"params":"auth=1"`) || !strings.Contains(info.Body.String(), `"note":"preserve-note"`) {
		t.Fatalf("info=%d %s", info.Code, info.Body.String())
	}
	var systemDefault string
	if err := db.QueryRow("SELECT SYSDEF FROM CONFIG_RSS_PARSER WHERE ID=7").Scan(&systemDefault); err != nil || systemDefault != "Y" {
		t.Fatalf("SYSDEF=%q err=%v", systemDefault, err)
	}
	created := performFormRequest(handler, "/api/v1/rss/parser/update", token, url.Values{"name": {"Custom"}, "type": {"XML"}, "format": {`{"link":"xpath"}`}})
	if created.Code != 200 || !strings.Contains(created.Body.String(), `"code":0`) {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	list = performFormRequest(handler, "/api/v1/rss/parser/list", token, nil)
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"name":"Custom"`) {
		t.Fatalf("list after create=%d %s", list.Code, list.Body.String())
	}
	removed := performFormRequest(handler, "/api/v1/rss/parser/delete", token, url.Values{"id": {"7"}})
	if removed.Code != 200 || !strings.Contains(removed.Body.String(), `"code":0`) {
		t.Fatalf("delete=%d %s", removed.Code, removed.Body.String())
	}
	missing := performFormRequest(handler, "/api/v1/rss/parser/info", token, url.Values{"id": {"7"}})
	if missing.Code != 200 || !strings.Contains(missing.Body.String(), `"detail":{}`) {
		t.Fatalf("missing info=%d %s", missing.Code, missing.Body.String())
	}
	invalid := performFormRequest(handler, "/api/v1/rss/parser/delete", token, url.Values{"id": {"all"}})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid delete=%d %s", invalid.Code, invalid.Body.String())
	}
}
