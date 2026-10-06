package httpserver

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNativeRSSTaskLifecycleWithoutPython(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("RSS task used legacy backend: %s", request.URL)
		return nil, fmt.Errorf("legacy backend disabled")
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO CONFIG_RSS_PARSER (ID,NAME,TYPE,FORMAT) VALUES (3,'General','XML','{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO CONFIG_FILTER_GROUP (ID,GROUP_NAME,IS_DEFAULT) VALUES (5,'HD','N')`); err != nil {
		t.Fatal(err)
	}
	if response := performFormRequest(handler, "/api/v1/rss/list", "", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list=%d", response.Code)
	}
	created := performFormRequest(handler, "/api/v1/rss/update", token, url.Values{
		"name": {"Two feeds"}, "uses": {"R"}, "interval": {"30"}, "state": {"Y"},
		"address_parser": {`{"address_2":"https://feed.example/two","parser_2":"3","address_1":"https://feed.example/one","parser_1":"3"}`},
		"rule":           {"5"}, "proxy": {"Y"}, "sites": {`{"rss_sites":["site-a"],"search_sites":[]}`},
		"restype": {"BluRay"}, "save_path": {"/downloads"},
	})
	if created.Code != 200 || !strings.Contains(created.Body.String(), `"code":0`) {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	var id int64
	if err := db.QueryRow(`SELECT ID FROM CONFIG_USER_RSS WHERE NAME='Two feeds'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	info := performFormRequest(handler, "/api/v1/rss/info", token, url.Values{"id": {fmt.Sprint(id)}})
	if info.Code != 200 || !strings.Contains(info.Body.String(), `"address":["https://feed.example/one","https://feed.example/two"]`) || !strings.Contains(info.Body.String(), `"filter_name":"HD"`) || !strings.Contains(info.Body.String(), `"proxy":true`) || !strings.Contains(info.Body.String(), `"state":true`) {
		t.Fatalf("info=%d %s", info.Code, info.Body.String())
	}
	list := performFormRequest(handler, "/api/v1/rss/list", token, nil)
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"tasks":[{`) || !strings.Contains(list.Body.String(), `"parsers":[{`) || !strings.Contains(list.Body.String(), `"success":false`) {
		t.Fatalf("list=%d %s", list.Code, list.Body.String())
	}
	if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET PROCESS_COUNT='9', MEDIAINFOS='[{"id":"42"}]', NOTE='{"proxy":"Y","preserve":"original"}' WHERE ID=?`, id); err != nil {
		t.Fatal(err)
	}
	updated := performFormRequest(handler, "/api/v1/rss/update", token, url.Values{
		"id": {fmt.Sprint(id)}, "name": {"Updated"}, "uses": {"D"}, "interval": {"15"}, "state": {"N"},
		"address": {"https://feed.example/new"}, "parser": {"3"}, "proxy": {"N"},
	})
	if updated.Code != 200 || !strings.Contains(updated.Body.String(), `"code":0`) {
		t.Fatalf("update=%d %s", updated.Code, updated.Body.String())
	}
	var count, mediaInfos, note string
	if err := db.QueryRow(`SELECT PROCESS_COUNT, MEDIAINFOS, NOTE FROM CONFIG_USER_RSS WHERE ID=?`, id).Scan(&count, &mediaInfos, &note); err != nil || count != "9" || mediaInfos != `[{"id":"42"}]` || !strings.Contains(note, `"preserve":"original"`) || !strings.Contains(note, `"proxy":"N"`) {
		t.Fatalf("preserved count=%q media=%q note=%q err=%v", count, mediaInfos, note, err)
	}
	info = performFormRequest(handler, "/api/v1/rss/info", token, url.Values{"id": {fmt.Sprint(id)}})
	if info.Code != 200 || !strings.Contains(info.Body.String(), `"name":"Updated"`) || !strings.Contains(info.Body.String(), `"counter":"9"`) || !strings.Contains(info.Body.String(), `"state":false`) || !strings.Contains(info.Body.String(), `"proxy":false`) {
		t.Fatalf("updated info=%d %s", info.Code, info.Body.String())
	}
	removed := performFormRequest(handler, "/api/v1/rss/delete", token, url.Values{"id": {fmt.Sprint(id)}})
	if removed.Code != 200 || !strings.Contains(removed.Body.String(), `"code":0`) {
		t.Fatalf("delete=%d %s", removed.Code, removed.Body.String())
	}
	missing := performFormRequest(handler, "/api/v1/rss/info", token, url.Values{"id": {fmt.Sprint(id)}})
	if missing.Code != 200 || !strings.Contains(missing.Body.String(), `"detail":{}`) {
		t.Fatalf("missing info=%d %s", missing.Code, missing.Body.String())
	}
	invalid := performFormRequest(handler, "/api/v1/rss/update", token, url.Values{"name": {"Invalid"}, "uses": {"R"}, "interval": {"30"}, "state": {"Y"}, "address_parser": {`{"address_1":"https://feed.example","parser_2":"3"}`}})
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("unpaired address accepted=%d %s", invalid.Code, invalid.Body.String())
	}
	if _, err := db.Exec(`INSERT INTO CONFIG_USER_RSS (ID,NAME,ADDRESS,PARSER,INTERVAL,USES,STATE,NOTE) VALUES (99,'Legacy','https://old.example/rss','3','60','S','1','{"proxy":true,"save_path":"/legacy","recognization":"N"}')`); err != nil {
		t.Fatal(err)
	}
	legacy := performFormRequest(handler, "/api/v1/rss/info", token, url.Values{"id": {"99"}})
	if legacy.Code != 200 || !strings.Contains(legacy.Body.String(), `"address":["https://old.example/rss"]`) || !strings.Contains(legacy.Body.String(), `"parser":["3"]`) || !strings.Contains(legacy.Body.String(), `"uses":"R"`) || !strings.Contains(legacy.Body.String(), `"uses_text":"搜索"`) || !strings.Contains(legacy.Body.String(), `"save_path":"/legacy"`) || !strings.Contains(legacy.Body.String(), `"recognization":"N"`) {
		t.Fatalf("legacy info=%d %s", legacy.Code, legacy.Body.String())
	}
}
