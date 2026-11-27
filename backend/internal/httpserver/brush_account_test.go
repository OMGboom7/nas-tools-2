package httpserver

import (
	"database/sql"
	"github.com/0xforee/nas-tools/backend/internal/database"
	"path/filepath"
	"testing"
)

func TestBrushAccountingAtomicRecoveryAndLegacyCounters(t *testing.T) {
	dir := t.TempDir()
	if err := database.PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
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
	exec(`INSERT INTO DOWNLOADER(ID,TYPE,ENABLED,CONFIG) VALUES(7,'qbittorrent',1,'{}');
 INSERT INTO SITE_BRUSH_TASK(ID,SITE,DOWNLOADER,STATE,REMOVE_RULE,REMOVE_COUNT) VALUES(4,1,'7','Y','{"ratio":"gt#1"}',2);
 INSERT INTO SITE_BRUSH_TORRENTS(TASK_ID,DOWNLOADER,DOWNLOAD_ID,TORRENT_SIZE) VALUES('4','7','0','8'),('4','7','0','3,9'),('4','7','one','1000'),('4','7','two','100'),('4','7','missing','20');`)
	q := brushAccountingRequest{brushMaintenanceRequest: brushMaintenanceRequest{TaskID: 4, Downloader: "7"}}
	call := func(op string) any {
		t.Helper()
		r, e := accountBrushScan(t.Context(), db, op, q)
		if e != nil {
			t.Fatal(op, e)
		}
		return r
	}
	begin := func() { q.Token = call("begin").(brushScanStart).Token; q.Absent = nil }
	begin()
	old := q.Token
	begin()
	stale := q
	stale.Token = old
	if _, e := accountBrushScan(t.Context(), db, "plan", stale); e == nil {
		t.Fatal("superseded scan")
	}
	q.Items = []brushObservation{{ID: "one", Completed: true, Ratio: 2, Uploaded: 400, Downloaded: 200}, {ID: "two", Uploaded: 30, Downloaded: 10, Size: 100}}
	call("plan")
	exec("UPDATE SITE_BRUSH_TASK SET REMOVE_RULE='{}'")
	if _, e := accountBrushScan(t.Context(), db, "issue", q); e == nil {
		t.Fatal("changed rule")
	}
	exec(`UPDATE SITE_BRUSH_TASK SET REMOVE_RULE='{"ratio":"gt#1"}'`)
	begin()
	call("plan")
	approved := call("issue").([]string)
	if len(approved) != 1 || approved[0] != "one" {
		t.Fatal(approved)
	}
	begin()
	if call("plan").(map[string]brushDecision)["one"].Action != "pending" {
		t.Fatal("issued twice")
	}
	q.Absent = []string{"one", "missing"}
	exec("CREATE TRIGGER disk_full BEFORE UPDATE OF REMOVE_COUNT ON SITE_BRUSH_TASK BEGIN SELECT RAISE(ABORT,'fixture'); END")
	if _, e := accountBrushScan(t.Context(), db, "finish", q); e == nil {
		t.Fatal("expected rollback")
	}
	var count int
	if e := db.QueryRow("SELECT count(*) FROM SITE_BRUSH_TORRENTS WHERE DOWNLOAD_ID!='0'").Scan(&count); e != nil || count != 3 {
		t.Fatal(count, e)
	}
	exec("DROP TRIGGER disk_full")
	call("finish")
	call("finish")
	totals := func() {
		t.Helper()
		var n, u, d int
		if e := db.QueryRow("SELECT REMOVE_COUNT,UPLOAD_SIZE,DOWNLOAD_SIZE FROM SITE_BRUSH_TASK").Scan(&n, &u, &d); e != nil || n != 4 || u != 441 || d != 219 {
			t.Fatal(n, u, d, e)
		}
	}
	totals()
	begin()
	q.Items = nil
	call("plan")
	call("finish")
	totals()
	exec("UPDATE SITE_BRUSH_TASK SET STATE='N'")
	if _, e := accountBrushScan(t.Context(), db, "begin", q); e == nil {
		t.Fatal("disabled task")
	}
}
