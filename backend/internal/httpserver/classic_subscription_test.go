package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/database"
)

func TestClassicSubscriptionProgress(t *testing.T) {
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
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	check := func(q, want string) {
		t.Helper()
		var got string
		if err := db.QueryRow(q).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %q, want %q (%v)", q, got, want, err)
		}
	}
	call := func(op string, q classicSubscriptionRequest) ([]byte, error) {
		b, err := json.Marshal(q)
		if err != nil {
			return nil, err
		}
		var out bytes.Buffer
		err = ClassicSubscriptionCommand(context.Background(), filepath.Join(dir, "config.yaml"), op, bytes.NewReader(b), &out)
		return out.Bytes(), err
	}
	source := func(kind string) classicSubscriptionRequest {
		t.Helper()
		q := classicSubscriptionRequest{Kind: kind, ID: 1}
		b, err := call("list", q)
		var all []classicSubscriptionSource
		if err != nil || json.Unmarshal(b, &all) != nil || len(all) != 1 {
			t.Fatalf("list %s: %v %s", kind, err, b)
		}
		q.Token = all[0].Token
		q.Notice.Title = "Fixture"
		q.Notice.Type = kind
		return q
	}
	apply := func(op string, q classicSubscriptionRequest, want string) classicProgressResult {
		t.Helper()
		b, err := call(op, q)
		var r classicProgressResult
		if err != nil || json.Unmarshal(b, &r) != nil || r.Status != want {
			t.Fatalf("%s: %s %v", op, b, err)
		}
		return r
	}
	exec(`INSERT INTO RSS_MOVIES(ID,STATE,NAME,TMDBID) VALUES(1,'D','Film','50');
	INSERT INTO RSS_TVS(ID,STATE,NAME,SEASON,TOTAL,LACK,TMDBID) VALUES(1,'R','Show','S00',8,4,'60');
	INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES('1','1,2,6,8');
    INSERT INTO MESSAGE_CLIENT(ID,NAME,TYPE,CONFIG,SWITCHS,ENABLED) VALUES(51,'fixture','webhook','{}','["rss_finished"]',1)`)
	tv := source("TV")
	movie := source("MOV")
	if err := database.PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE RSS_TV_EPISODES SET EPISODES='2,4,6,8'`)
	apply("finish", tv, "stale")
	tv = source("TV")
	movie.State = "S"
	start := apply("state", movie, "updated")
	apply("finish", movie, "stale")
	movie.Token = start.Token
	tv.Episodes = []int{8, 2, 2}
	tv.Markers = []classicRSSMarker{{Enclosure: "first"}}
	tv.Token = apply("remaining", tv, "updated").Token
	check(`SELECT STATE||':'||TOTAL||':'||LACK FROM RSS_TVS`, "R:8:2")
	check(`SELECT EPISODES FROM RSS_TV_EPISODES`, "2,8")
	apply("finish", movie, "finished")
	check(`SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='rss_finished'`, "1")
	check(`SELECT COUNT(*) FROM GO_NOTIFICATION_DELIVERIES`, "0")
	exec(`CREATE TRIGGER fail_record BEFORE INSERT ON RSS_TORRENTS BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	tv.Markers = []classicRSSMarker{{Enclosure: "second"}}
	if _, err := call("finish", tv); err == nil {
		t.Fatal("marker failure should roll back")
	}
	check(`SELECT COUNT(*) FROM RSS_TVS`, "1")
	check(`SELECT COUNT(*) FROM RSS_HISTORY`, "1")
	check(`SELECT COUNT(*) FROM GO_BUSINESS_EVENTS`, "1")
	exec(`DROP TRIGGER fail_record`)
	var wg sync.WaitGroup
	outcomes := make(chan string, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := call("finish", tv)
			var r classicProgressResult
			if err != nil || json.Unmarshal(b, &r) != nil {
				t.Errorf("finish: %v", err)
			}
			outcomes <- r.Status
		}()
	}
	wg.Wait()
	close(outcomes)
	counts := map[string]int{}
	for status := range outcomes {
		counts[status]++
	}
	if counts["finished"] != 1 || counts["stale"] != 1 {
		t.Fatal(counts)
	}
	check(`SELECT COUNT(DISTINCT TYPE) FROM RSS_HISTORY WHERE RSSID='1'`, "2")
	check(`SELECT COUNT(*) FROM RSS_TV_EPISODES`, "0")
	check(`SELECT COUNT(*) FROM GO_BUSINESS_EVENTS`, "2")
	check(`SELECT COUNT(*) FROM RSS_TORRENTS`, "2")
	check(`SELECT COUNT(*) FROM GO_MESSAGE_CENTER`, "1")
	check(`SELECT COUNT(*) FROM GO_NOTIFICATION_DELIVERIES WHERE STATE='pending'`, "1")

	const insertMovie = `INSERT INTO RSS_MOVIES(ID,STATE,NAME,TMDBID,OVER_EDITION,FILTER_ORDER) VALUES(1,'R','Film','50',1,70)`
	exec(insertMovie)
	old := source("MOV")
	exec(`DELETE FROM RSS_MOVIES`)
	exec(insertMovie)
	old.Rank = 100
	old.Highest = 100
	apply("edition", old, "stale")
	current := source("MOV")
	current.Rank = 60
	current.Highest = 100
	current.Token = apply("edition", current, "updated").Token
	check(`SELECT FILTER_ORDER FROM RSS_MOVIES`, "70")
	current.Rank = 85
	current.Token = apply("edition", current, "updated").Token
	check(`SELECT FILTER_ORDER FROM RSS_MOVIES`, "85")
	current.Rank = 100
	apply("edition", current, "finished")
	apply("edition", current, "stale")
	check(`SELECT COUNT(*) FROM GO_SUBSCRIPTION_EDITION_ARCHIVES`, "1")
	check(`SELECT COUNT(*) FROM RSS_HISTORY WHERE TYPE='MOV' AND RSSID='1'`, "2")
}
