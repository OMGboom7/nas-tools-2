package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

type subscriptionWriteFixture struct {
	path string
	db   *sql.DB
}

func newSubscriptionWriteFixture(t *testing.T) subscriptionWriteFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("pt: {search_no_result_rss: true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := subscriptionWriteFixture{path, db}
	f.exec(t, "CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY, TYPE TEXT, KEY TEXT, VALUE TEXT)")
	return f
}

func (f subscriptionWriteFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f subscriptionWriteFixture) call(req map[string]any) (subscriptionWriteReceipt, error) {
	var receipt subscriptionWriteReceipt
	data, err := json.Marshal(req)
	if err != nil {
		return receipt, err
	}
	var out bytes.Buffer
	err = ResolvedSubscriptionCommand(context.Background(), f.path, bytes.NewReader(data), &out)
	if err == nil {
		err = json.Unmarshal(out.Bytes(), &receipt)
	}
	return receipt, err
}

func (f subscriptionWriteFixture) expect(t *testing.T, req map[string]any, code int) subscriptionWriteReceipt {
	t.Helper()
	r, err := f.call(req)
	if err != nil || r.Code != code {
		t.Fatalf("expected %d, got %+v, %v", code, r, err)
	}
	return r
}

func TestResolvedSubscriptionSeasonDefaultsAndAtomicEdits(t *testing.T) {
	f := newSubscriptionWriteFixture(t)
	f.exec(t, "INSERT INTO SYSTEM_DICT VALUES(1,'SystemConfig','DefaultRssSettingTV',?)",
		`{"rule":9007199254740993,"download_setting":-1,"pix":"4K","rss_sites":["default"],"search_sites":["search"]}`)
	fields := map[string]any{"NAME": "Series", "YEAR": "2026", "RSS_SITES": []string{"chosen"}, "FILTER_PIX": "1080p", "TOTAL_EP": 12, "CURRENT_EP": 3, "STATE": "D"}
	req := map[string]any{"kind": "TV", "fields": fields, "defaultKind": "TV", "chooseSeason": true, "seasons": []map[string]int{
		{"season_number": 2, "episode_count": 10}, {"season_number": 0, "episode_count": 2}, {"season_number": 1, "episode_count": 6},
	}}
	r := f.expect(t, req, 0)
	if r.Season == nil || *r.Season != 2 || r.Episodes != 10 || r.Event["download_setting"] != -1.0 || r.Event["filter_pix"] != "1080p" {
		t.Fatal(r)
	}
	var snapshot string
	err := f.db.QueryRow("SELECT SEASON||'/'||TOTAL||'/'||LACK||'/'||RSS_SITES||'/'||SEARCH_SITES||'/'||FILTER_RULE FROM RSS_TVS WHERE ID=?", r.ID).Scan(&snapshot)
	if err != nil || snapshot != `S02/12/8/["chosen"]/["search"]/9007199254740993` {
		t.Fatal(snapshot, err)
	}
	if dup := f.expect(t, req, 9); dup.Event != nil {
		t.Fatal(dup)
	}
	req["season"] = 5
	f.expect(t, req, 3)
	delete(req, "season")
	req["douban"] = true
	other := f.expect(t, req, 0)
	if *other.Season != 1 || other.Episodes != 6 {
		t.Fatal(other)
	}
	req["editID"] = strconv.FormatInt(r.ID, 10)
	f.exec(t, "CREATE TABLE RSS_TV_EPISODES(RSSID INTEGER, EPISODE INTEGER)")
	f.exec(t, "INSERT INTO RSS_TV_EPISODES VALUES(?,4)", r.ID)
	f.expect(t, req, 9)
	fields["NAME"] = "changed"
	f.exec(t, "CREATE TRIGGER reject_edit BEFORE UPDATE ON RSS_TVS BEGIN SELECT RAISE(IGNORE); END")
	if _, err := f.call(req); err == nil {
		t.Fatal("failed edit returned success")
	}
	var count int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM RSS_TVS JOIN RSS_TV_EPISODES ON RSSID=ID WHERE NAME='Series' AND SEASON='S02' AND EPISODE=4").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	f.exec(t, "DROP TRIGGER reject_edit")
	req["endSeason"] = 3
	if edited := f.expect(t, req, 0); edited.ID != r.ID {
		t.Fatal(edited)
	}
	if err := f.db.QueryRow("SELECT SEASON,(SELECT COUNT(*) FROM RSS_TV_EPISODES) FROM RSS_TVS WHERE ID=?", r.ID).Scan(&snapshot, &count); err != nil || snapshot != "S01-S03" || count != 0 {
		t.Fatal(snapshot, count, err)
	}
	req["editID"] = "999"
	f.expect(t, req, -1)
	f.expect(t, map[string]any{"kind": "TV", "fields": map[string]any{"NAME": "Series", "YEAR": "2026", "SEASON": "", "FUZZY_MATCH": 1}}, 9)
}

func TestResolvedSubscriptionConcurrentCreate(t *testing.T) {
	f := newSubscriptionWriteFixture(t)
	req := map[string]any{"kind": "MOV", "fields": map[string]any{"NAME": "movie", "YEAR": nil, "STATE": "R"}}
	replies := make(chan int, 6)
	for range 6 {
		go func() {
			r, err := f.call(req)
			if err != nil {
				t.Errorf("save: %v", err)
				replies <- -100
				return
			}
			replies <- r.Code
		}()
	}
	counts := map[int]int{}
	for range 6 {
		counts[<-replies]++
	}
	if counts[0] != 1 || counts[9] != 5 {
		t.Fatal(counts)
	}
}

func TestResolvedSubscriptionRequiresCurrentChatChoice(t *testing.T) {
	f := newSubscriptionWriteFixture(t)
	f.exec(t, "CREATE TABLE GO_CHAT_CHOICE(CHANNEL TEXT,SENDER TEXT,REVISION TEXT,ACTION TEXT,CHOICES BLOB)")
	f.exec(t, "INSERT INTO GO_CHAT_CHOICE VALUES('WEB/','sender','rev','search',?)", `[{"kind":"电影","douban_id":"123","rss_sites":["site"]}]`)
	choice := map[string]any{"channel": "WEB/", "sender": "sender", "revision": "rev", "selector": "DB:123", "index": 1}
	fields := map[string]any{"NAME": "Movie", "STATE": "R", "TMDBID": 99, "RSS_SITES": []string{"site"}}
	req := map[string]any{"kind": "MOV", "fields": fields, "sourceID": "DB:123", "defaultKind": "MOV", "choice": choice}
	for _, change := range []struct {
		m     map[string]any
		key   string
		value any
	}{
		{choice, "sender", "other"}, {choice, "revision", "old"}, {choice, "index", 2}, {req, "sourceID", "DB:999"},
		{fields, "DOWNLOAD_SETTING", 8}, {fields, "STATE", "D"}, {fields, "RSS_SITES", []string{"extra"}},
	} {
		t.Run(fmt.Sprint(change.key), func(t *testing.T) {
			old, exists := change.m[change.key]
			change.m[change.key] = change.value
			f.expect(t, req, 4)
			if exists {
				change.m[change.key] = old
			} else {
				delete(change.m, change.key)
			}
		})
	}
	if err := os.WriteFile(f.path, []byte("pt: {search_no_result_rss: false}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.expect(t, req, 4)
	f.exec(t, "UPDATE GO_CHAT_CHOICE SET ACTION='subscribe'")
	fields["STATE"] = "D"
	f.expect(t, req, 0)
	f.expect(t, req, 9)
}
