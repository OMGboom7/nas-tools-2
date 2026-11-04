package httpserver

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

func TestSiteStatisticsRankingAndLegacyReadsUseSameStoredSnapshot(t *testing.T) {
	f, api := newStatsFixture(t)
	const catalog = `{"indexer":[
		{"id":"wrong","name":"Fixture site","domain":"https://wrong.fixture.test","user_ranking_info":{"enabled":false}},
		{"id":"right","name":"Fixture site","domain":"https://site.fixture.test","user_ranking_info":{"enabled":true,"keep_account_level":"User","rule_url":"https://site.fixture.test/rules","ranking_system":{"levels":[{"name":"User","order":1},{"name":"Power User","order":2,"requirements":{"download_gb":2,"ratio":2}}]}}}
	]}`
	if err := os.WriteFile(f.api.catalogPath, []byte(base64.StdEncoding.EncodeToString([]byte(catalog))), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const rawLevel = `<img alt="User（原备注）" src="old.png">`
	if _, err = db.ExecContext(t.Context(), `INSERT INTO SITE_USER_INFO_STATS
		(SITE,USERNAME,USER_LEVEL,JOIN_AT,UPDATE_AT,UPLOAD,DOWNLOAD,RATIO,SEEDING,SEEDING_SIZE,URL,MSG_UNREAD,EXT_INFO)
		VALUES ('Fixture site','fixture-user',?,'2024-01-01 00:00:00','2026-11-03 01:00:00',9007199254740993,1073741824,1,2,1073741824,'https://site.fixture.test',3,'{"unknown":9007199254740993}'),
		('Hidden site','hidden-fixture-user','User','2024-01-01','','1',0,0,0,0,'https://hidden.fixture.test',0,NULL)`, rawLevel); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `INSERT INTO SITE_STATISTICS_HISTORY
		(SITE,DATE,UPLOAD,DOWNLOAD,URL) VALUES ('Fixture site','2026-11-02','10','10','https://site.fixture.test'),
		('Fixture site','2026-11-03','20','15','https://site.fixture.test');
		INSERT INTO SITE_USER_SEEDING_INFO(SITE,SEEDING_INFO,URL) VALUES ('Fixture site','[[3,9007199254740993]]','https://site.fixture.test');`); err != nil {
		t.Fatal(err)
	}
	var upstream atomic.Int32
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstream.Add(1)
		return nil, errors.New("statistics reads must stay local")
	})
	if _, err = f.api.auth.service.CreateUser(t.Context(), "fixture-rank-viewer", "fixture-password", nil); err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, f.handler, "fixture-rank-viewer", "fixture-password")
	id := strconv.FormatInt(f.site.ID, 10)
	path := "/api/v1/site-statistics/ranking/" + id
	overview := nativeJSONRequest(f.handler, "GET", "/api/v1/site-statistics/overview", token, "")
	if overview.Code != 200 || !strings.Contains(overview.Body.String(), `"userLevel":"User"`) ||
		!strings.Contains(overview.Body.String(), `"upload":"9007199254740993"`) ||
		strings.Contains(overview.Body.String(), `"jobs"`) || strings.Contains(overview.Body.String(), `"control"`) ||
		strings.Contains(overview.Body.String(), "hidden-fixture-user") {
		t.Fatal("viewer overview returned the wrong scope", overview.Code, overview.Body.String())
	}
	detail := nativeJSONRequest(f.handler, "GET", "/api/v1/site-statistics/detail/"+id, token, "")
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), `["3","9007199254740993"]`) {
		t.Fatal("viewer detail lost stored seeding", detail.Code, detail.Body.String())
	}
	legacy := func(cmd, input string) (int, string) {
		r := nativeJSONRequest(f.handler, "POST", "/do?random=0.5", token, `{"cmd":"`+cmd+`","data":`+input+`}`)
		return r.Code, r.Body.String()
	}
	w := nativeJSONRequest(f.handler, "GET", path, token, "")
	var modern struct {
		Data struct {
			SiteID  string
			Ranking sitestats.RankStatus
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &modern) != nil || modern.Data.SiteID != id ||
		modern.Data.Ranking.OverallProgress != 50 || modern.Data.Ranking.CurrentLevel != "User" || !modern.Data.Ranking.CurrentIsKeep {
		t.Fatal(w.Code, w.Body.String())
	}
	code, body := legacy("get_user_ranking_status", `{"site":"Fixture site"}`)
	var old struct{ Data sitestats.RankStatus }
	if code != 200 || json.Unmarshal([]byte(body), &old) != nil || stringValueJSON(old.Data) != stringValueJSON(modern.Data.Ranking) {
		t.Fatal(code, body)
	}
	code, body = legacy("get_site_user_statistics", `{"sites":["Fixture site"],"encoding":"DICT","sort_by":"upload","sort_on":"desc","site_hash":"Y"}`)
	for _, field := range []string{`"upload":9007199254740993`, `"user_level":"User"`, `"msg_unread":3`, `"keep_account_level":"User"`, `"is_keep_account_level":true`, `"site_hash":"`} {
		if code != 200 || !strings.Contains(body, field) || strings.Contains(body, "hidden-fixture-user") {
			t.Fatal(code, body)
		}
	}
	w = nativeJSONRequest(f.handler, "GET", "/api/v1/site/statistics?apikey=native-site-login-fixture", "", "")
	for _, field := range []string{`"upload":9007199254740993`, `"user_level":"User"`, `"msg_unread":3`, `"message_unread":3`, `"level_description":""`, `"is_keep_account_level":true`} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), field) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if err = f.api.config.Update(map[string]any{"security.api_key": "fixture.rank.key"}); err != nil {
		t.Fatal(err)
	}
	if r := nativeJSONRequest(f.handler, "GET", "/api/v1/site/statistics", "fixture.rank.key", ""); r.Code != 200 {
		t.Fatal("the API key was treated as a JWT", r.Code, r.Body.String())
	}
	if r := nativeJSONRequest(f.handler, "GET", "/api/v1/site/statistics", token, ""); r.Code != 401 {
		t.Fatal("JWT replaced the API key", r.Code)
	}
	for _, tt := range []struct{ action, input, expected string }{
		{"get_site_activity", `{"name":"Fixture site"}`, `"dataset":[["time","upload","download","bonus","seeding","seeding_size"]`},
		{"get_site_history", `{"days":7,"end_day":"2026-11-03"}`, `["Fixture site",10,5]`},
		{"get_site_seeding_info", `{"name":"Fixture site"}`, `[3,9007199254740993]`},
		{"get_site_user_statistics", `{"sites":["Hidden site"]}`, `"data":[]`},
		{"get_user_ranking_status", `{"site":"Hidden site"}`, `"code":1`},
	} {
		if code, body := legacy(tt.action, tt.input); code != 200 || !strings.Contains(body, tt.expected) {
			t.Fatal(tt.action, code, body)
		}
	}
	if r := nativeJSONRequest(f.handler, "GET", path, "", ""); r.Code != 401 {
		t.Fatal("anonymous rank returned data", r.Code)
	}
	if r := nativeJSONRequest(f.handler, "POST", "/do", "", `{"cmd":"get_user_ranking_status","data":{"site":"Fixture site"}}`); r.Code != 401 {
		t.Fatal("anonymous legacy rank returned data", r.Code)
	}
	if r := nativeJSONRequest(f.handler, "POST", "/api/v1/site-statistics/refresh", token, `{}`); r.Code != 403 {
		t.Fatal("viewer was authorized for writes", r.Code)
	}
	if _, err = db.ExecContext(t.Context(), `INSERT INTO CONFIG_SITE(ID,NAME,PRI,SIGNURL,COOKIE,INCLUDE)
		VALUES (9007199254740993,'Fixture site','2','https://wrong.fixture.test/','fixture-cookie','T');
		INSERT INTO SITE_USER_INFO_STATS(SITE,USER_LEVEL,URL) VALUES ('Fixture site','VIP','https://wrong.fixture.test');
		INSERT INTO SITE_STATISTICS_HISTORY(SITE,DATE,UPLOAD,DOWNLOAD,URL) VALUES
		('Fixture site','2026-11-02','1000','2000','https://wrong.fixture.test'),
		('Fixture site','2026-11-03','1010','2020','https://wrong.fixture.test');`); err != nil {
		t.Fatal(err)
	}
	w = nativeJSONRequest(f.handler, "GET", "/api/v1/site-statistics/ranking/9007199254740993", token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"siteId":"9007199254740993"`) || !strings.Contains(w.Body.String(), `"status_type":"not_configured"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if code, body := legacy("get_user_ranking_status", `{"site":"Fixture site"}`); code != 200 || !strings.Contains(body, `"code":1`) {
		t.Fatal("ambiguous legacy name returned another site's rules", code, body)
	}
	code, body = legacy("get_site_history", `{"days":7,"end_day":"2026-11-03"}`)
	if code != 200 || !strings.Contains(body, `["Fixture site (https://site.fixture.test)",10,5]`) ||
		!strings.Contains(body, `["Fixture site (https://wrong.fixture.test)",10,20]`) {
		t.Fatal("history mixed same-name tracker counters", code, body)
	}
	saved, err := api.store.Snapshot(t.Context(), "https://site.fixture.test")
	if err != nil || saved.Level != rawLevel || saved.Upload != 9007199254740993 || upstream.Load() != 0 {
		t.Fatal("ranking changed original data or sent a request", saved, err, upstream.Load())
	}
	jobs, err := api.store.Jobs(t.Context())
	if err != nil || len(jobs) != 0 {
		t.Fatal("read generated a refresh", jobs, err)
	}
}
