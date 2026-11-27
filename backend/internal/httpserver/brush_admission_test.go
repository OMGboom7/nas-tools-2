package httpserver

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/database"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

func TestBrushAdmissionRemoteLimitsAndCompetingScans(t *testing.T) {
	dir := t.TempDir()
	if err := database.PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string) {
		t.Helper()
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	exec(brushReceiptSchema + brushFractionSchema + brushRevisionSchema + `
 INSERT INTO CONFIG_SITE(ID,INCLUDE,RSSURL,NOTE) VALUES(8,'S','feed','{"tags":"site"}');
 INSERT INTO SITE_BRUSH_TASK(ID,SITE,DOWNLOADER,STATE,RSS_RULE,LABEL) VALUES(1,8,3,'Y','{"dlcount":"2","current_site_count":"1"}','film');`)
	var speed, all string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			return jsonResponse(r, "Ok."), nil
		case "/api/v2/transfer/info":
			return jsonResponse(r, speed), nil
		case "/api/v2/torrents/info":
			if r.URL.Query().Get("filter") == "all" {
				return jsonResponse(r, all), nil
			}
			return jsonResponse(r, `[{"hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tags":"sitex,film"}]`), nil
		}
		t.Fatalf("unexpected request %s", r.URL)
		return nil, nil
	})
	s := downloadService{client: &http.Client{Transport: transport}}
	item := downloaderconfig.Downloader{ID: 3, Type: "qbittorrent", Config: `{"host":"qb.local","port":"8080"}`}
	check := func(want bool, slotsWant int64) *brushLimits {
		t.Helper()
		var slots int64
		b, e := s.brushAdmission(t.Context(), db, 1, 64, false, item, &slots)
		if e != nil || (b != nil) != want || slots != slotsWant {
			t.Fatal(b, e, slots, want, slotsWant)
		}
		return b
	}
	all = `[{"hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tags":"film, sitex"}]`
	first := check(true, 1)
	q := brushDownloadRequest{limits: first, TaskID: 1, SiteID: 8, FeedURL: "feed", Title: "Film", Enclosure: "a", Downloader: "3", RuleSource: first.rules}
	token, e := reserveBrushDownload(t.Context(), db, q, nil)
	if e != nil || token == "" {
		t.Fatal(token, e)
	}
	q.Enclosure = "second"
	if token, e = reserveBrushDownload(t.Context(), db, q, nil); e != nil || token != "" {
		t.Fatal("stale remote count reused", token, e)
	}
	check(false, 0)
	exec("DELETE FROM GO_BRUSH_SUBMISSION")
	all = `[{"hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tags":"film,site"}]`
	check(false, 0)
	all = "[]"
	exec("UPDATE SITE_BRUSH_TASK SET UP_LIMIT='2'")
	speed = `{"up_info_speed":2048,"dl_info_speed":0}`
	check(false, 0)
	speed = `{"up_info_speed":2047,"dl_info_speed":0}`
	check(true, 1)
	speed = "{}"
	var slots int64
	if _, e := s.brushAdmission(t.Context(), db, 1, 64, false, item, &slots); e == nil {
		t.Fatal("missing speed treated as zero")
	}
	exec("UPDATE SITE_BRUSH_TASK SET UP_LIMIT='', SEED_SIZE='1'")
	if b, e := s.brushAdmission(t.Context(), db, 1, 6<<30, false, item, &slots); e != nil || b != nil {
		t.Fatal("whole exceeds five GiB tolerance", b, e)
	}
	if b, e := s.brushAdmission(t.Context(), db, 1, 6<<30, true, item, &slots); e != nil || b == nil {
		t.Fatal("partial should budget only selected files", b, e)
	}
}
func TestBrushStoredRules(t *testing.T) {
	fields, e := brushRuleFields(`{'include': "Movie's \\d+", 'hr': None, 'dlcount': '2', 'active': True}`)
	if e != nil || fields["include"] != `Movie's \d+` || fields["hr"] != nil || fields["dlcount"] != "2" || fields["active"] != true {
		t.Fatal(fields, e)
	}
}
