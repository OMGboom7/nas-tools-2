package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
	if err := os.WriteFile(path, []byte("app: {}\n"), 0600); err != nil {
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
		fmt.Fprintf(&in, "{\"op\":\"identify\",\"index\":%d,\"identity\":{\"kind\":\"MOV\",\"tmdbid\":\"5\"}}\n", i)
		fmt.Fprintf(&in, "{\"op\":\"candidate\",\"index\":%d,\"accepted\":true}\n", i)
	}
	in.WriteString("{\"op\":\"submit\",\"index\":0}\n")
	if err := ClassicRSSScanCommand(context.Background(), path, &in, &observer); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(observer.String()), "\n")
	if len(lines) != 14 {
		t.Fatalf("unexpected scan events: %s", observer.String())
	}
	for i := 0; i < 6; i++ {
		var e struct {
			Site   struct{ Rule int }
			Source map[string]any
		}
		if err := json.Unmarshal([]byte(lines[i*2+1]), &e); err != nil {
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
	if json.Unmarshal([]byte(lines[12]), &submit) != nil || submit.Op != "submit" || !reflect.DeepEqual(submit.Indices, []int{1, 6}) {
		t.Fatalf("unexpected submit %s", lines[12])
	}
}
