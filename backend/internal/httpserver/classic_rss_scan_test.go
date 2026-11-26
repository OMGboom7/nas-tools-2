package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/batchdownload"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/database"
)

func TestClassicRSSIdentity(t *testing.T) {
	for _, tc := range []struct {
		kind, name, tmdb, year, season string
		fuzzy                          bool
		item                           classicRSSIdentity
		want                           bool
	}{
		{"MOV", "Film", "DB:5", "2024", "", false, classicRSSIdentity{Kind: "MOV", Title: "Film", Year: "2025"}, true},
		{"TV", "Film", "DB:5", "2024", "", false, classicRSSIdentity{Kind: "TV", Title: "Film", Year: "2025"}, false},
		{"TV", "Film", "51", "2024", "S00", false, classicRSSIdentity{Kind: "TV", TMDBID: "51", Season: "S01"}, false},
		{"TV", "Film", "51", "2024", "S00", false, classicRSSIdentity{Kind: "TV", TMDBID: "51", Season: "S00"}, true},
		{"TV", "Film", "51", "2024", "S00", false, classicRSSIdentity{Kind: "MOV", TMDBID: "51", Season: "S00"}, false},
		{"TV", "(?<=flag )film", "", "2024", "S00", true, classicRSSIdentity{Kind: "TV", Title: "FILM", Year: "2024", Revised: "flag", Season: "S02"}, true},
		{"TV", "film", "", "2024", "S00", true, classicRSSIdentity{Kind: "TV", Title: "FILM", Year: "2023", Season: "S02"}, false},
	} {
		src := classicSubscriptionSource{Row: map[string]any{"ID": 1, "NAME": tc.name, "TMDBID": tc.tmdb, "YEAR": tc.year, "SEASON": tc.season, "FUZZY_MATCH": tc.fuzzy}}
		r, err := newClassicRSSRule(tc.kind, src)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := r.matches(tc.item); err != nil || got != tc.want {
			t.Errorf("%+v: %v %v", tc, got, err)
		}
	}
}

// Mutate the actual database when the last candidate is offered. A pending
// download, replaced subscription, edited site and registered URL must all be
// excluded from the single hand-off, without eliminating the unaffected site.
type classicScanObserver struct {
	bytes.Buffer
	onCandidate func()
}

func (o *classicScanObserver) Write(b []byte) (int, error) {
	var e struct {
		Op    string
		Index int
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return 0, err
	}
	if e.Op == "candidate" && e.Index == 6 {
		o.onCandidate()
	}
	return o.Buffer.Write(b)
}

func TestClassicRSSScanSelection(t *testing.T) {
	dir := t.TempDir()
	if err := database.PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("media:\n  movie_path: "+dir+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/")
		if slug == "excluded" {
			t.Error("fetched site outside subscription scope")
		}
		fmt.Fprintf(w, `<rss><channel><item><title>%s</title><link>fixture/%s</link></item></channel></rss>`, slug, slug)
	}))
	defer server.Close()
	for n, name := range []string{"good", "edited", "recorded", "replaced", "pending", "other", "excluded"} {
		exec(`INSERT INTO CONFIG_SITE(ID,NAME,INCLUDE,RSSURL,NOTE) VALUES(?,?,'D',?,?)`, n+1, name, server.URL+"/"+name, fmt.Sprintf(`{"rule":%d}`, 20+n))
	}
	exec(`INSERT INTO RSS_MOVIES(ID,NAME,TMDBID,STATE,RSS_SITES) VALUES
	(1,'film','5','R','["good","deleted","edited","recorded","other"]'),
	(2,'film','5','R','["replaced"]'),(3,'film','5','R','["pending"]'),
	(4,'film','5','R','["deleted"]')`)
	observer := classicScanObserver{onCandidate: func() {
		exec(`UPDATE CONFIG_SITE SET NOTE='{"rule":99}' WHERE ID=2;
		INSERT INTO RSS_TORRENTS(ENCLOSURE) VALUES('fixture/recorded');
		INSERT OR REPLACE INTO RSS_MOVIES(ID,NAME,TMDBID,STATE,RSS_SITES) VALUES(2,'film','5','R','["replaced"]')`)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := ensureSubscriptionSubmissionSchema(context.Background(), tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO GO_RSS_DOWNLOAD_CLAIMS VALUES('uncertain',0,'pending','');
		INSERT INTO GO_SUBSCRIPTION_DOWNLOAD_CLAIMS VALUES('uncertain','','MOV',3,'5',0,'')`)
	}}
	var in bytes.Buffer
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&in, "{\"op\":\"identify\",\"index\":%d,\"identity\":{\"kind\":\"MOV\",\"title\":\"film\",\"tmdbid\":\"5\"}}\n", i)
		fmt.Fprintf(&in, "{\"op\":\"candidate\",\"index\":%d,\"accepted\":true,\"inventory\":{\"kind\":\"电影\",\"title\":\"film\",\"id\":\"5\"}}\n", i)
		fmt.Fprintf(&in, "{\"op\":\"ready\",\"index\":%d,\"accepted\":true}\n", i)
	}
	in.WriteString("{\"op\":\"submit\",\"index\":0}\n")
	if err := ClassicRSSScanCommand(context.Background(), path, &in, &observer); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(observer.String()), "\n")
	if len(lines) != 20 {
		t.Fatalf("unexpected scan events: %s", observer.String())
	}
	for i := 0; i < 6; i++ {
		var e struct {
			Site   struct{ Rule int }
			Source map[string]any
		}
		if err := json.Unmarshal([]byte(lines[i*3+1]), &e); err != nil {
			t.Fatal(err)
		}
		if e.Site.Rule != 20+i || e.Source["filter_rule"] != nil && text(e.Source["filter_rule"]) != "" {
			t.Fatalf("unexpected rule %v %+v", e.Site, e.Source)
		}
	}
	var submit struct {
		Op      string
		Indices []int
	}
	if json.Unmarshal([]byte(lines[18]), &submit) != nil || submit.Op != "submit" || !reflect.DeepEqual(submit.Indices, []int{1, 6}) {
		t.Fatalf("unexpected submit %s", lines[18])
	}
}

func TestClassicRSSLibraryEligibility(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("app: {rmt_tmdbkey: test, tmdb_domain: test.invalid}\nmedia:\n  tv_path: "+dir+"\n  movie_path: "+dir+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	season := 0
	tmdb := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "test.invalid" || r.URL.Path != "/3/tv/42" {
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":42,"name":"Show","seasons":[{"season_number":0,"episode_count":4}]}`)), Header: http.Header{}}, nil
	})
	scan := classicRSSScan{inventory: subscriptionService{configStore: config.NewStore(cfg), client: &http.Client{Transport: tmdb}}}
	id := classicRSSIdentity{Kind: "TV", TMDBID: "42", Title: "Show", Year: "2026", Season: "S00"}
	query := inventoryQuery{Kind: "电视剧", ID: "42", Title: "Show", Year: "2026"}
	query.Metadata.Episodes.Season = &season
	check := func(t *testing.T, episodes []map[string]any, start int, want []int, eligible bool) {
		t.Helper()
		r, err := newClassicRSSRule("TV", classicSubscriptionSource{Row: map[string]any{"ID": 1, "SEASON": "S00", "CURRENT_EP": start}, Episodes: episodes})
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := scan.eligible(context.Background(), r, id, &query)
		if err != nil || ok != eligible || ok && (got.Number != 0 || got.Total != 4 || !reflect.DeepEqual(got.Episodes, want)) {
			t.Fatal(got, ok, err)
		}
	}
	check(t, nil, 3, []int{3, 4}, true)
	check(t, nil, 5, nil, false)
	check(t, []map[string]any{{"EPISODES": ""}}, 0, nil, false)
	check(t, []map[string]any{{"EPISODES": "2,4"}}, 0, []int{2, 4}, true)
	r := classicRSSRule{kind: "TV"}
	for _, invalid := range []*inventoryQuery{nil, {ID: "43", Title: "Show", Kind: "电视剧"}, {ID: "42", Title: "Show", Kind: "电影"}, {ID: "42", Title: "Show", Kind: "电视剧", LocalOnly: true}} {
		if _, ok, err := scan.eligible(context.Background(), &r, id, invalid); ok || err == nil {
			t.Fatal(invalid, ok, err)
		}
	}
	if err := os.WriteFile(cfg, []byte("app: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := scan.eligible(context.Background(), &r, id, &query); ok || err == nil {
		t.Fatal("unknown inventory", ok, err)
	}
}

func TestClassicRSSBatchScopes(t *testing.T) {
	a, b := &classicRSSRule{token: "a"}, &classicRSSRule{token: "b"}
	scan := classicRSSScan{accepted: []classicRSSChoice{
		{index: 1, rule: a, show: "42", gap: &batchdownload.Season{Number: 0, Total: 2}},
		{index: 2, rule: b, show: "42", gap: &batchdownload.Season{Number: 0, Total: 4, Episodes: []int{4}}},
		{index: 3, rule: a, show: "42", gap: &batchdownload.Season{Number: 0, Total: 4, Episodes: []int{3}}},
	}}
	shows, receipts := scan.batchMissing([]int{1, 2})
	got := shows["42"]
	if receipts["a"].Total != 2 || !reflect.DeepEqual(receipts["b"].Episodes, []int{4}) {
		t.Fatal(receipts)
	}
	_, receipts = scan.batchMissing([]int{1, 2, 3})
	if !reflect.DeepEqual(receipts["a"].Episodes, []int{1, 2, 3}) || !reflect.DeepEqual(receipts["b"].Episodes, []int{4}) {
		t.Fatal(receipts)
	}
	if !reflect.DeepEqual(got, []batchdownload.Season{{Number: 0, Total: 4, Episodes: []int{1, 2, 4}}}) || len(scan.accepted[0].gap.Episodes) != 0 {
		t.Fatal(got)
	}
}
