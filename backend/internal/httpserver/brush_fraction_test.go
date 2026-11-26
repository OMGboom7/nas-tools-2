package httpserver

import (
	"github.com/0xforee/nas-tools/backend/internal/database"
	"math"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

func TestBrushFractionBoundsAndFileOrder(t *testing.T) {
	const gb = 1 << 30
	r := brushFractionRule{Ranges: "1,3", Ratios: "0.5,0.25", After: "0,5"}
	for _, tc := range []struct{ size, quota, space, want int64 }{
		{gb, 2 * gb, 2 * gb, gb}, {2 * gb, 3 * gb, 3 * gb, gb}, {3 * gb, 5 * gb, 5 * gb, 3 * gb / 2}, {4 * gb, 4 * gb, 4 * gb, gb},
		{40 * gb, 40 * gb, 40 * gb, 5 * gb}, {40 * gb, 3 * gb, 2 * gb, 2 * gb},
	} {
		got, err := r.limit(tc.size, tc.quota, tc.space)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	for _, r := range []brushFractionRule{{}, {Ranges: "1", Ratios: "NaN"}, {Ranges: "1", Ratios: "1.2"}, {Ranges: "5,2", Ratios: ".5,.2"}, {After: "5,1"}, {After: "0,8589934592"}} {
		if _, err := r.limit(100, 100, 100); err == nil {
			t.Fatal("accepted invalid rule", r)
		}
	}
	files := []torrentfiles.File{{Index: 4, Path: "e", Size: 10}, {Index: 0, Path: "a", Size: 20}, {Index: 1, Path: "b", Size: 150}, {Index: 2, Path: "c", Size: 40}, {Index: 3, Path: "d", Size: 30}, {Index: 5, Path: ".pad/1", Size: 1, Padding: true}}
	p, err := planBrushFiles(files, 100)
	if err != nil || p.Size != 100 || p.Priorities[0] != 7 || p.Priorities[1] != 0 || p.Priorities[2] != 6 || p.Priorities[3] != 1 || p.Priorities[4] != 1 || p.Priorities[5] != 0 {
		t.Fatal(p, err)
	}
	if _, err = planBrushFiles(files, 9); err == nil {
		t.Fatal("empty selection")
	}
	p, err = planBrushFiles([]torrentfiles.File{{Index: 0, Path: "one", Size: math.MaxInt64}}, math.MaxInt64)
	if err != nil || p.Size != math.MaxInt64 || p.Priorities[0] != 1 {
		t.Fatal(p, err)
	}
}
func TestBrushFractionStartProofIncludesPrioritiesAndLayout(t *testing.T) {
	s := torrentfiles.Snapshot{Hash: "123", Directory: "/data", Files: []torrentfiles.File{{Index: 0, Path: "a", Size: 1, Selected: true}}, Priorities: map[int]int{0: 7}}
	proof := brushFractionProof(s)
	s.Paused = true
	s.State = "stopped"
	if proof != brushFractionProof(s) || brushFractionRunning(s) {
		t.Fatal("paused evidence")
	}
	s.Paused = false
	s.State = "checking"
	if brushFractionRunning(s) {
		t.Fatal("checking is not a start")
	}
	s.State = "unreported"
	if brushFractionRunning(s) {
		t.Fatal("unrecognized state")
	}
	s.State = "downloading"
	if !brushFractionRunning(s) {
		t.Fatal("running")
	}
	s.Priorities[0] = 6
	if proof == brushFractionProof(s) {
		t.Fatal("changed priority")
	}
	s.Priorities[0] = 7
	s.Files[0].Index = 1
	if proof == brushFractionProof(s) {
		t.Fatal("changed manifest index")
	}
}

func TestBrushStartReservesOutstandingBytesAndRechecksTask(t *testing.T) {
	dir := t.TempDir()
	if err := database.PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	db, err := (subscriptionService{databasePath: filepath.Join(dir, "user.db")}).openNativeSubscriptionWriteDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(brushReceiptSchema + brushFractionSchema + `
	INSERT INTO SITE_BRUSH_TASK(ID,STATE,SEED_SIZE,RSS_RULE,FRACTION_RULE) VALUES(1,'Y','1','rss','fraction');
	INSERT INTO GO_BRUSH_SUBMISSION(token,enclosure,task_id,downloader,title,size,deadline,partial,remote_id) VALUES
	('one','a',1,'1','one',900000000,'',1,'hashA'),('two','b',1,'1','two',900000000,'',1,'hashB');`)
	if err != nil {
		t.Fatal(err)
	}
	q := brushFractionRequest{Brush: brushDownloadRequest{TaskID: 1, Size: 900000000, RuleSource: "rss"}, LocalDir: dir, RuleSource: "fraction", Rule: brushFractionRule{After: "0,1"}}
	start := func(token, hash string, size int64) error {
		return reserveBrushStart(t.Context(), db, q, token, hash, "proof", "cfg", size)
	}
	// Scale amounts down so even a small test volume is sufficient.
	if _, err := db.Exec("UPDATE SITE_BRUSH_TASK SET SEED_SIZE=?", strconv.FormatFloat(1024.0/(1<<30), 'g', -1, 64)); err != nil {
		t.Fatal(err)
	}
	if err := start("one", "hashA", 800); err != nil {
		t.Fatal(err)
	}
	if err := start("two", "hashB", 225); err == nil {
		t.Fatal("both plans consumed same bytes")
	}
	if _, err := db.Exec("UPDATE SITE_BRUSH_TASK SET RSS_RULE='other'"); err != nil {
		t.Fatal(err)
	}
	if err := start("two", "hashB", 224); err == nil {
		t.Fatal("stale RSS rule")
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM GO_BRUSH_FRACTION").Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
