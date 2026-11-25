package httpserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

func TestScheduledStatisticsPublishesOnlyCompleteSelectedAccounts(t *testing.T) {
	dir := t.TempDir()
	path, db := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "user.db")
	if err := os.WriteFile(path, []byte("app: {user_agent: fixture-agent}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sites, err := siteconfig.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer sites.Close()
	var requests atomic.Int32
	var change atomic.Bool
	site := siteconfig.Site{Name: "Fixture", Include: "T", Cookie: "account=one"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Cookie") != "account=one" || r.UserAgent() != "fixture-agent" {
			t.Error("request did not use the stored account")
		}
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(statsHome))
		case "/userdetails.php":
			_, _ = w.Write([]byte(statsProfile))
		case "/getusertorrentlistajax.php":
			if change.Load() {
				edited := site
				edited.Cookie = "account=two"
				if _, err := sites.Upsert(t.Context(), edited); err != nil {
					t.Error(err)
				}
				_, _ = w.Write([]byte(statsSeeds + `<a href="?page=2">下一页</a>`))
				return
			}
			_, _ = w.Write([]byte(statsSeeds))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	site.SignURL = server.URL + "/index.php"
	site, err = sites.Upsert(t.Context(), site)
	if err != nil {
		t.Fatal(err)
	}
	target := scheduledStatsTarget{ID: site.ID, Name: site.Name, URL: server.URL}
	run := func(targets ...scheduledStatsTarget) []scheduledStatsResult {
		t.Helper()
		input, err := json.Marshal(targets)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := ScheduledStatisticsCommand(t.Context(), path, bytes.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		var results []scheduledStatsResult
		if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != len(targets) {
			t.Fatal(out.String(), err)
		}
		return results
	}
	stale := scheduledStatsTarget{ID: site.ID + 1, Name: "Removed", URL: server.URL}
	result := run(target, stale)
	if result[0].Data["upload"] != float64(2<<30) || result[0].Data["err_msg"] != nil || result[1].Data["err_msg"] == nil {
		t.Fatal(result)
	}
	store, err := sitestats.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	before, err := store.List(t.Context(), []string{server.URL})
	if err != nil || len(before) != 1 || before[0].SeedingSize != 1610612736 {
		t.Fatal(before, err)
	}
	change.Store(true)
	if run(target)[0].Data["err_msg"] == nil {
		t.Fatal("accepted account change during pagination")
	}
	after, err := store.List(t.Context(), []string{server.URL})
	if err != nil || sitestats.Hash(before) != sitestats.Hash(after) {
		t.Fatal("replaced original statistics", err)
	}
	calls := requests.Load()
	target.Name = "Renamed"
	if run(target)[0].Data["err_msg"] == nil || calls != requests.Load() {
		t.Fatal("stale selection initiated account requests")
	}
}

func TestStatisticsReadCommandUsesOriginalTablesWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "user.db")
	query := func(action, input string) (string, error) {
		var out bytes.Buffer
		err := StatisticsReadCommand(t.Context(), path, "", action, strings.NewReader(input), &out)
		return strings.TrimSpace(out.String()), err
	}
	if _, err := query("min_join_date", "{}"); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatal("query created a database", err)
	}
	sites, err := siteconfig.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sites.Close()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO CONFIG_SITE(NAME,SIGNURL,INCLUDE,COOKIE) VALUES
 ('Current','https://current.invalid/','T','fixture'),('Hidden','https://hidden.invalid/','','hidden');
 CREATE TABLE SITE_USER_INFO_STATS(SITE,URL,USERNAME,USER_LEVEL,JOIN_AT,LAST_SEEN,UPDATE_AT,UPLOAD,DOWNLOAD,RATIO,SEEDING,LEECHING,SEEDING_SIZE,BONUS,MSG_UNREAD);
 INSERT INTO SITE_USER_INFO_STATS(SITE,URL,JOIN_AT,UPLOAD) VALUES
 ('Current','https://current.invalid','2025-01-01 10:00:00',9007199254740993),('Hidden','https://hidden.invalid','2010-01-01 10:00:00','damaged hidden counter')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ action, input, want string }{
		{"min_join_date", "{}", `"data":"2025-01-01"`},
		{"min_join_date", `{"sites":["Hidden"]}`, `"data":""`},
		{"get_site_user_statistics", "{}", `"upload":9007199254740993`},
		{"get_site_user_statistics", `{"sites":["Hidden"]}`, `"data":[]`},
		{"get_site_history", `{"days":"1"}`, `"code":1`},
		{"get_user_ranking_status", `{"site":"Current"}`, `"status_type":"not_configured"`},
		{"unsupported", "{}", `"code":1`},
	} {
		got, err := query(tc.action, tc.input)
		if err != nil || !strings.Contains(got, tc.want) || strings.Contains(got, "hidden.invalid") {
			t.Fatalf("%s: %s, %v", tc.action, got, err)
		}
	}
	after, err := os.ReadFile(dbPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("query changed original database", err)
	}
}
