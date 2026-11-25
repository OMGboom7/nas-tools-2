package httpserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestSubscriptionPluginHistoryPagesUseExistingRowsAndExactIDs(t *testing.T) {
	h, admin, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Set(t.Context(), "UserInstalledPlugins", `["DoubanRank","DoubanSync","MovieRandom"]`); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	get := func(id, token, query string) (int, pluginPageData) {
		t.Helper()
		w := nativeJSONRequest(h, http.MethodGet, "/api/v1/plugins/"+id+"/page"+query, token, "")
		var out struct{ Data pluginPageData }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || strings.Contains(w.Body.String(), "private-cache") {
			t.Fatal(err, w.Body.String())
		}
		return w.Code, out.Data
	}
	if code, p := get("MovieRandom", admin, ""); code != 200 || len(p.Tables) != 1 || len(p.Tables[0].Rows) != 0 {
		t.Fatal(code, p)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS PLUGIN_HISTORY(ID INTEGER PRIMARY KEY, PLUGIN_ID TEXT, KEY TEXT, VALUE TEXT, DATE TEXT)`); err != nil {
		t.Fatal(err)
	}
	const big = "9007199254740993"
	for _, p := range []string{"DoubanRank", "DoubanSync", "MovieRandom"} {
		if _, err = db.Exec(`INSERT INTO PLUGIN_HISTORY(PLUGIN_ID,KEY,VALUE) VALUES(?,?,?)`, p, big, `{"id":9007199254740993,"name":"Fixture","year":2026,"rating":8.5,"type":"电影","state":"RSS","add_time":"original","image":"private-cache","extra":{"key":"private-cache"}}`); err != nil {
			t.Fatal(err)
		}
		code, page := get(p, admin, "")
		if code != 200 || page.Title != systemconfig.SubscriptionHistoryTitle(p) || page.Tables[0].RowActions[0].RecordID != big || strings.Join(page.Tables[0].Rows[0], "|") != "Fixture (2026) 评分：8.5|电影|已订阅|original" {
			t.Fatal(code, page)
		}
		w := performFormRequest(h, "/api/v1/plugin/page", admin, url.Values{"id": {p}})
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"record_id":"`+big+`"`) || strings.Contains(w.Body.String(), "private-cache") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := performFormRequest(h, "/api/v1/user/manage", admin, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	viewer := loginForTest(t, h, "viewer", "strong-password")
	del := func(p, token, id string) int {
		return nativeJSONRequest(h, http.MethodDelete, "/api/v1/plugins/"+p+"/page/records", token, `{"recordId":"`+id+`"}`).Code
	}
	if code, _ := get("MovieRandom", viewer, ""); code != 403 || del("DoubanRank", viewer, big) != 403 || del("DoubanSync", "", big) != 401 {
		t.Fatal(code)
	}
	if _, err = db.Exec(`INSERT INTO PLUGIN_HISTORY(PLUGIN_ID,KEY,VALUE) VALUES('DoubanRank',?,?)`, big, `{"id":"9007199254740993","name":"duplicate"}`); err != nil {
		t.Fatal(err)
	}
	if del("DoubanRank", admin, big) != 200 || del("DoubanRank", admin, big) != 404 {
		t.Fatal("exact-key deletion or repeat failed")
	}
	if w := performFormRequest(h, "/api/v1/plugin/page/action", admin, url.Values{"id": {"MovieRandom"}, "record_id": {big}, "action": {"delete"}}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM PLUGIN_HISTORY WHERE KEY=? AND PLUGIN_ID='DoubanSync'`, big).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err = db.Exec(`INSERT INTO PLUGIN_HISTORY(PLUGIN_ID,KEY,VALUE) VALUES('DoubanSync',?,'{"id":"999"}'),('MovieRandom','11','corrupt')`, big); err != nil {
		t.Fatal(err)
	}
	if del("DoubanSync", admin, big) != 409 || del("MovieRandom", admin, "11") != 409 {
		t.Fatal("inconsistent record was removable")
	}
	if err = db.QueryRow(`SELECT count(*) FROM PLUGIN_HISTORY WHERE KEY=? AND PLUGIN_ID='DoubanSync'`, big).Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 201; i++ {
		key := strconv.Itoa(i)
		if _, err = tx.Exec(`INSERT INTO PLUGIN_HISTORY(ID,PLUGIN_ID,KEY,VALUE) VALUES(?,'DoubanRank',?,?)`, int64(9007199254740993)+int64(i), key, fmt.Sprintf(`{"id":"%s","name":"row %d"}`, key, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	code, page := get("DoubanRank", admin, "")
	if code != 200 || page.NextCursor != "9007199254741193" || len(page.Tables[0].Rows) != 200 {
		t.Fatal(code, page.NextCursor)
	}
	code, page = get("DoubanRank", admin, "?after="+page.NextCursor)
	if code != 200 || len(page.Tables[0].Rows) != 1 || page.Tables[0].RowActions[0].RecordID != "201" || page.NextCursor != "" {
		t.Fatal(code, page)
	}
	if code, _ = get("DoubanRank", admin, "?after=-1"); code != 400 {
		t.Fatal(code)
	}
	if err = store.Set(t.Context(), "UserInstalledPlugins", `[]`); err != nil {
		t.Fatal(err)
	}
	if code, _ = get("DoubanRank", admin, ""); code != 404 || del("DoubanRank", admin, "201") != 404 {
		t.Fatal(code)
	}
}
