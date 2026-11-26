package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/database"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/downloadhistory"
)

func TestOrdinaryRSSAcknowledgementRecovery(t *testing.T) {
	var calls atomic.Int32
	var lose atomic.Bool
	const task = "1123456789abcdef"
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID, Method string }
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Method != "aria2.addUri" {
			t.Error("unexpected request")
			return
		}
		calls.Add(1)
		if lose.Load() {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"jsonrpc": "2.0", "id": req.ID, "result": task})
	}))
	defer remote.Close()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "user.db")
	if err := database.PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	db, err := (subscriptionService{databasePath: dbPath}).openNativeSubscriptionWriteDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := downloaderconfig.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	endpoint, _ := url.Parse(remote.URL)
	cfg, _ := json.Marshal(map[string]string{"host": "http://" + endpoint.Hostname(), "port": endpoint.Port()})
	dl, err := store.Upsert(ctx, downloaderconfig.Downloader{Name: "local", Enabled: 1, Type: "aria2", Config: string(cfg), DownloadDir: "[]"})
	if err != nil {
		t.Fatal(err)
	}
	sqlExec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	check := func(query, want string) {
		t.Helper()
		var got string
		if err := db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %q, want %q, err %v", query, got, want, err)
		}
	}
	sqlExec(`INSERT INTO RSS_TVS(ID,NAME,STATE,SEASON,TOTAL,LACK,TMDBID) VALUES(1,'Series','R','S00',8,3,'42');
	INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES('1','2,4,8');
	INSERT INTO RSS_MOVIES(ID,NAME,STATE,TMDBID) VALUES(1,'Film','R','24')`)
	token := func(kind string) string {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		rows, err := listClassicSubscriptions(ctx, tx, classicSubscriptionRequest{Kind: kind, ID: 1})
		if err != nil || len(rows) != 1 {
			t.Fatalf("list: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return rows[0].Token
	}
	tv, movie := token("TV"), token("MOV")
	send := func(kind, digest, hash string, units ...int) map[string]any {
		t.Helper()
		h := downloaderconfig.OrdinaryRSS{Kind: kind, ID: 1, Token: digest, Mode: "finish", Notice: downloaderconfig.SubscriptionNotice{Title: "original", Type: kind}, Marker: classicRSSMarker{Name: "release", Enclosure: "fixture/" + hash}}
		if kind == "TV" {
			h.Mode = "episodes"
			h.Missing = []int{2, 4, 8}
			h.Covered = units
			if hash == "d" {
				h.Missing = []int{8}
			}
		}
		q := classicRequest{Downloader: dl.ID, Config: cfg, Directories: json.RawMessage("[]"), Channel: "RSS", Source: "RSS", Sender: "rss:1", Title: "release", Magnet: "magnet:?xt=urn:btih:" + strings.Repeat(hash, 40), Notice: downloadhistory.Notice{Title: "release", Source: "RSS"}, History: downloaderconfig.ClassicAcknowledgement{Subscription: &h}}
		b, _ := json.Marshal(q)
		var out bytes.Buffer
		if err := ClassicDownloadCommand(ctx, path, "submit", bytes.NewReader(b), &out); err != nil {
			t.Fatal(err)
		}
		var reply map[string]any
		if json.Unmarshal(out.Bytes(), &reply) != nil {
			t.Fatal("bad response")
		}
		return reply
	}
	drain := func() map[string]string {
		t.Helper()
		done, err := recoverOrdinaryRSS(ctx, db, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		return done
	}
	// The acknowledgement survives a rollback in download history. An uncertain
	// second item cannot reduce missing episodes until its original task is verified.
	sqlExec(`CREATE TRIGGER break_history BEFORE INSERT ON DOWNLOAD_HISTORY BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	if send("TV", tv, "a", 2)["pending"] != true {
		t.Fatal("expected pending history")
	}
	check("SELECT ACK FROM GO_ORDINARY_RSS_RECEIPTS", task)
	lose.Store(true)
	if send("TV", tv, "b", 4)["pending"] != true {
		t.Fatal("missing acknowledgement")
	}
	lose.Store(false)
	sqlExec("DROP TRIGGER break_history")
	if send("MOV", movie, "c")["subscriptionRecorded"] != true {
		t.Fatal("movie not accepted")
	}
	// This newly inserted movie must not inherit the completed original's ID.
	sqlExec(`DELETE FROM RSS_MOVIES WHERE ID=1;INSERT INTO RSS_MOVIES(ID,NAME,STATE,TMDBID) VALUES(1,'Film','R','24')`)
	if done := drain(); len(done) != 0 {
		t.Fatal(done)
	}
	check("SELECT EPISODES FROM RSS_TV_EPISODES", "4,8")
	check("SELECT COUNT(*) FROM RSS_MOVIES", "1")
	check("SELECT COUNT(*) FROM RSS_HISTORY", "0")
	check("SELECT COUNT(*) FROM RSS_TORRENTS", "2")
	check("SELECT COUNT(*) FROM DOWNLOAD_HISTORY", "2")
	// Manual exact-task verification and the next scan use the same frozen
	// document, and continue only from the token produced by our own update.
	var id string
	if err := db.QueryRow("SELECT ID FROM GO_ORDINARY_RSS_RECEIPTS WHERE ACK=''").Scan(&id); err != nil {
		t.Fatal(err)
	}
	r, err := store.ManualSubmission(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	h, err := store.ClassicPendingHistory(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteWithHistory(ctx, r, task, "verified_present", "operator", &h); err != nil {
		t.Fatal(err)
	}
	drain()
	check("SELECT EPISODES FROM RSS_TV_EPISODES", "8")
	tv = token("TV")
	if send("TV", tv, "d", 8)["task"] != task {
		t.Fatal("final episode not accepted")
	}
	// A failed progress transaction keeps the marker, event and history together.
	sqlExec(`CREATE TRIGGER break_progress BEFORE INSERT ON RSS_HISTORY BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	if _, err = recoverOrdinaryRSS(ctx, db, dbPath); err == nil {
		t.Fatal("failed archive reported success")
	}
	check("SELECT COUNT(*) FROM RSS_TORRENTS", "3")
	sqlExec("DROP TRIGGER break_progress")
	// The fixture's original inventory now only lacks E08.
	// This check uses the frozen inventory registered at submission.
	done := drain()
	if done[tv] != "fixture/d" {
		t.Fatal("final archive", done)
	}
	check("SELECT COUNT(*) FROM RSS_TV_EPISODES", "0")
	check("SELECT COUNT(*) FROM RSS_HISTORY", "1")
	check("SELECT COUNT(*) FROM RSS_TORRENTS", "4")
	check("SELECT COUNT(*) FROM GO_ORDINARY_RSS_RECEIPTS", "0")
	if len(drain()) != 0 {
		t.Fatal("duplicate completion")
	}
	sqlExec(`CREATE TABLE IF NOT EXISTS GO_SUBSCRIPTION_DOWNLOAD_CLAIMS(RESOURCE_KEY TEXT PRIMARY KEY,OWNER TEXT NOT NULL,KIND TEXT NOT NULL,SUB_ID INTEGER NOT NULL,MEDIA_ID TEXT NOT NULL,SEASON INTEGER NOT NULL,EPISODES TEXT NOT NULL)`)
	if _, err := db.Exec(`INSERT INTO GO_SUBSCRIPTION_DOWNLOAD_CLAIMS VALUES(?,'older-native','MOV',9,'24',0,'')`, subscriptionResourceKey("fixture/e")); err != nil {
		t.Fatal(err)
	}
	if send("MOV", token("MOV"), "e")["pending"] != true {
		t.Fatal("reused another subscription's resource proof")
	}
	if calls.Load() != 4 {
		t.Fatal("replayed a download", calls.Load())
	}
}

func TestOrdinaryRSSQualityAndPartialCoverage(t *testing.T) {
	source := classicSubscriptionSource{Token: "current"}
	row := func(mode string, rank, highest int, enclosure string) ordinaryResult {
		return ordinaryResult{h: downloaderconfig.OrdinaryRSS{Kind: "MOV", ID: 2, Mode: mode, Rank: rank, Highest: highest, Marker: classicRSSMarker{Enclosure: enclosure}}}
	}
	for _, tc := range []struct {
		items         []ordinaryResult
		op, enclosure string
	}{
		{[]ordinaryResult{row("record", 0, 0, "fuzzy")}, "", "fuzzy"},
		{[]ordinaryResult{row("record", 0, 0, "episode"), row("edition", 80, 90, "second"), row("edition", 92, 92, "best")}, "edition", "best"},
		{[]ordinaryResult{row("state", 98, 0, "group-removed"), row("edition", 80, 80, "lower")}, "state", "group-removed"},
	} {
		op, _, enclosure, err := ordinaryRSSChange(tc.items, source, false)
		if err != nil || op != tc.op || enclosure != tc.enclosure {
			t.Fatalf("quality selection: %s %s %v", op, enclosure, err)
		}
	}
	a := ordinaryResult{h: downloaderconfig.OrdinaryRSS{Kind: "TV", ID: 2, Mode: "episodes", Missing: []int{2, 4, 8, 10}, Covered: []int{2, 4}}}
	b := a
	b.h.Covered = []int{4, 10}
	op, q, _, err := ordinaryRSSChange([]ordinaryResult{a, b}, source, false)
	if err != nil || op != "remaining" || len(q.Episodes) != 1 || q.Episodes[0] != 8 {
		t.Fatalf("partial union %v %v %v", op, q.Episodes, err)
	}
}
