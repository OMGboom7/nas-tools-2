package httpserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/database"
)

func TestBrushSelectionBoundariesAndEvidence(t *testing.T) {
	now := time.Now()
	hour := now.Add(-time.Hour)
	yes, no, zero := true, false, int64(0)
	for _, tc := range []struct{ key, rule, want string }{
		{"include", `(?<=S)01`, "match"}, {"include", "s01", "skip"}, {"exclude", "S01", "skip"}, {"include", "[", "skip"},
		{"size", "gt#1", "match"}, {"size", "lt#1", "match"}, {"size", "bw#1,2", "skip"}, {"size", "gt#NaN", "skip"},
		{"peercount", "1", "match"}, {"peercount", "lt#0", "skip"}, {"peercount", "bw#0,0", "match"},
		{"pubdate", "gt#1", "skip"}, {"pubdate", "lt#1", "skip"}, {"pubdate", "bw#1,2", "match"},
		{"free", "2XFREE", "skip"}, {"free", "FREE", "match"}, {"hr", "HR", "match"}, {"peercount", "#", "match"},
	} {
		s := brushSelection{map[string]any{tc.key: tc.rule}, "Film.S01", 1 << 30, &hour}
		got := s.match(brushAttributes{Free: &yes, HitAndRun: &no, Seeders: &zero}, now, func() bool { return true })
		if got != tc.want {
			t.Errorf("%v: %s", tc, got)
		}
	}
	for key, value := range map[string]string{"pubdate": "lt#1", "free": "FREE", "hr": "HR", "peercount": "1"} {
		s := brushSelection{Rules: map[string]any{key: value}}
		if got := s.match(brushAttributes{}, now, func() bool { return true }); got != "retry" {
			t.Fatal(key, got)
		}
	}
	s := brushSelection{Rules: map[string]any{"include": "Film"}, Title: "Film"}
	if got := s.match(brushAttributes{}, now, func() bool { return false }); got != "retry" {
		t.Fatal(got)
	}
}

func TestBrushDownloadRecoveryAndConcurrency(t *testing.T) {
	dir := t.TempDir()
	if err := database.PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(query string) {
		t.Helper()
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO CONFIG_SITE(ID,INCLUDE,RSSURL) VALUES(2,'S','feed');
	INSERT INTO SITE_BRUSH_TASK(ID,SITE,STATE,DOWNLOADER,RSS_RULE,BRUSHTASK_FREE_DDL_DELETE,DOWNLOAD_COUNT) VALUES(7,'2','Y','8','rules','Y',5);`)
	call := func(op string, q brushDownloadRequest) (string, error) {
		b, _ := json.Marshal(q)
		var out bytes.Buffer
		err := BrushDownloadCommand(t.Context(), path, op, bytes.NewReader(b), &out)
		return out.String(), err
	}
	q := brushDownloadRequest{TaskID: 7, SiteID: 2, FeedURL: "feed", Title: "Film", Enclosure: "link", Downloader: "8", RuleSource: "rules", Size: 140, Deadline: "20300101_0010"}
	reserve := func(q brushDownloadRequest) string {
		t.Helper()
		out, err := call("reserve", q)
		if err != nil {
			t.Fatal(err)
		}
		var token string
		if err := json.Unmarshal([]byte(out), &token); err != nil {
			t.Fatal(err)
		}
		return token
	}
	stale := q
	stale.RuleSource = "old"
	if reserve(stale) != "" {
		t.Fatal("reserved a stale rule")
	}
	var wg sync.WaitGroup
	results := make(chan string, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := call("reserve", q)
			if err != nil {
				t.Error(err)
				return
			}
			var token string
			if json.Unmarshal([]byte(out), &token) != nil {
				t.Error(out)
			}
			if token != "" {
				results <- token
			}
		}()
	}
	wg.Wait()
	close(results)
	if len(results) != 1 {
		t.Fatalf("reservation winners: %d", len(results))
	}
	token := <-results
	exec(`INSERT INTO SITE_BRUSH_TASK(ID,SITE,STATE,DOWNLOADER,RSS_RULE) VALUES(8,'2','Y','8','rules')`)
	competing := q
	competing.TaskID = 8
	if reserve(competing) != "" {
		t.Fatal("two brush tasks reserved one enclosure")
	}
	assertCounts := func(history, count int) {
		t.Helper()
		var h, c int
		if err := db.QueryRow("SELECT (SELECT COUNT(*) FROM SITE_BRUSH_TORRENTS), DOWNLOAD_COUNT FROM SITE_BRUSH_TASK WHERE ID=7").Scan(&h, &c); err != nil || h != history || c != count {
			t.Fatal(h, c, err)
		}
	}
	r := brushDownloadRequest{Token: token, RemoteID: "remote1"}
	exec(`CREATE TRIGGER fixture_count_failure BEFORE UPDATE OF DOWNLOAD_COUNT ON SITE_BRUSH_TASK BEGIN SELECT RAISE(ABORT,'unavailable'); END;`)
	if _, err := call("accept", r); err != nil {
		t.Fatal(err)
	}
	if _, err := call("finish", r); err == nil {
		t.Fatal("expected accounting failure")
	}
	assertCounts(0, 5)
	if reserve(q) != "" {
		t.Fatal("resubmitted before recovery")
	}
	exec("DROP TRIGGER fixture_count_failure; UPDATE SITE_BRUSH_TASK SET STATE='S' WHERE ID=7")
	if out, err := call("recover", brushDownloadRequest{}); err != nil || out != "1\n" {
		t.Fatal(out, err)
	}
	assertCounts(1, 6)
	if out, err := call("recover", brushDownloadRequest{}); err != nil || out != "0\n" {
		t.Fatal(out, err)
	}
	var deadline string
	if err := db.QueryRow("SELECT FREE_DEADLINE FROM SITE_BRUSH_TORRENTS").Scan(&deadline); err != nil || deadline != q.Deadline {
		t.Fatal(deadline, err)
	}
	exec("UPDATE SITE_BRUSH_TASK SET STATE='Y'")
	if reserve(q) != "" {
		t.Fatal("repeated old enclosure")
	}
	q.Enclosure = "another-link"
	if _, err := call("accept", brushDownloadRequest{Token: reserve(q), RemoteID: "remote1"}); err == nil {
		t.Fatal("double counted same downloader ID")
	}
	q.Enclosure = "partial"
	q.Partial = true
	r = brushDownloadRequest{Token: reserve(q), RemoteID: "remote2"}
	if _, err := call("accept", r); err != nil {
		t.Fatal(err)
	}
	if _, err := call("recover", brushDownloadRequest{}); err != nil {
		t.Fatal(err)
	}
	assertCounts(1, 6)
	r.Size = 50
	if _, err := call("selected", r); err != nil {
		t.Fatal(err)
	}
	if _, err := call("finish", r); err != nil {
		t.Fatal(err)
	}
	assertCounts(2, 7)
	var size string
	if err := db.QueryRow("SELECT TORRENT_SIZE FROM SITE_BRUSH_TORRENTS WHERE DOWNLOAD_ID='remote2'").Scan(&size); err != nil || size != "50" {
		t.Fatal(size, err)
	}
	q.Enclosure = "orphan"
	r.Token = reserve(q)
	r.RemoteID = "remote3"
	exec("DELETE FROM SITE_BRUSH_TASK; INSERT INTO SITE_BRUSH_TASK(ID,DOWNLOAD_COUNT) VALUES(7,0)")
	if _, err := call("accept", r); err == nil {
		t.Fatal("attached receipt to reused task ID")
	}
	assertCounts(2, 0)
}
