package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/batchdownload"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/database"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"golang.org/x/text/encoding/simplifiedchinese"
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

func TestClassicRSSDetailAndFinalSiteAllowance(t *testing.T) {
	dir := t.TempDir()
	if err := database.PrepareApplicationDatabases(dir); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("app: {user_agent: fixture-agent}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const title = "测试影片"
	page, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(`<html><h1>测试影片</h1><span class="double"></span><span class="hr"></span></html>`))
	if err != nil {
		t.Fatal(err)
	}
	var visits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed" {
			if r.Header.Get("Cookie") != "" {
				t.Error("cookie sent with feed")
			}
			fmt.Fprintf(w, `<rss><channel><item><title>%s</title><link>http://%s/detail</link><enclosure url="fixture/torrent"/></item></channel></rss>`, title, r.Host)
			return
		}
		if r.URL.Path != "/detail" || r.Header.Get("Cookie") != "session=fixture" || r.UserAgent() != "fixture-agent" {
			t.Error("wrong detail request")
		}
		visits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=gbk")
		w.Write(page)
	}))
	defer server.Close()
	rules := subscriptionTestDetailRules()
	rules["RENDER"] = json.RawMessage("true")
	t.Setenv("NASTOOL_BROWSER_BINARY", "disabled")
	catalog := indexercatalog.Catalog{Indexers: []indexercatalog.Definition{}, Conf: map[string]map[string]json.RawMessage{server.URL: rules}}
	b, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	cat := filepath.Join(dir, "sites.bin")
	if err := os.WriteFile(cat, []byte(base64.StdEncoding.EncodeToString(b)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NASTOOL_SITE_CATALOG", cat)
	if _, err := db.Exec(`INSERT INTO CONFIG_SITE(ID,NAME,INCLUDE,RSSURL,COOKIE,NOTE) VALUES(1,'Tracker','D',?,'session=fixture','{"parse":"Y","limit_interval":1,"limit_count":1}')`, server.URL+"/feed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO RSS_MOVIES(ID,NAME,STATE,FUZZY_MATCH) VALUES(1,?,'R',1)`, title); err != nil {
		t.Fatal(err)
	}
	run := func(candidate, ready bool) {
		t.Helper()
		input := "{\"op\":\"identify\",\"index\":1,\"identity\":{\"kind\":\"MOV\",\"title\":\"" + title + "\"}}\n"
		if candidate {
			input += "{\"op\":\"candidate\",\"index\":1,\"accepted\":true}\n"
		}
		input += "{\"op\":\"submit\",\"index\":0}\n"
		var output bytes.Buffer
		if err := ClassicRSSScanCommand(t.Context(), cfg, strings.NewReader(input), &output); err != nil {
			t.Fatal(err, output.String())
		}
		if strings.Contains(output.String(), "session=fixture") {
			t.Fatal("credential present in scanner protocol")
		}
		var gotCandidate, gotReady, submitted bool
		for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
			var f struct {
				Op      string
				Site    json.RawMessage
				Indices []int
			}
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				t.Fatal(err)
			}
			switch f.Op {
			case "candidate":
				gotCandidate = true
				var site struct{ Attributes classicRSSAttributes }
				if err := json.Unmarshal(f.Site, &site); err != nil {
					t.Fatal(err)
				}
				if a := site.Attributes; a.Upload == nil || *a.Upload != 2 || a.Download == nil || *a.Download != 0 || !a.HR {
					t.Fatal(string(f.Site))
				}
			case "ready":
				gotReady = true
			case "submit":
				submitted = reflect.DeepEqual(f.Indices, []int{1})
			}
		}
		if gotCandidate != candidate || gotReady != ready || submitted != ready {
			t.Fatal(output.String())
		}
	}
	run(true, false) // Detail reads the single available allowance.
	run(false, false)
	if visits.Load() != 1 {
		t.Fatal(visits.Load())
	}
	if _, err := db.Exec(`UPDATE CONFIG_SITE SET NOTE='{"parse":"Y","limit_interval":1,"limit_count":3}' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	run(true, true)
	var count int
	if err := db.QueryRow(`SELECT VISITS FROM GO_SITE_REQUEST_LIMIT WHERE SITE_ID=1`).Scan(&count); err != nil || count != 3 || visits.Load() != 2 {
		t.Fatal(count, visits.Load(), err)
	}
}

func TestClassicRSSMTeamDetailBoundToSavedAccount(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("app: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sites, err := siteconfig.Open(filepath.Join(filepath.Dir(cfg), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sites.Close()
	site, err := sites.Upsert(t.Context(), siteconfig.Site{Name: "Tracker", SignURL: "https://kp.m-team.cc", APIKey: "fixture-key", Cookie: "fixture-cookie", Note: `{"ua":"fixture-agent"}`})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://api.m-team.cc/api/torrent/detail?id=77" || r.Header.Get("x-api-key") != "fixture-key" || r.Header.Get("Cookie") != "" || r.UserAgent() != "fixture-agent" {
			t.Fatal("unexpected M-Team request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":"0","data":{"id":"77","name":"Film","status":{"discount":"PERCENT_50","seeders":3}}}`))}, nil
	})
	scan := classicRSSScan{reader: siteBrowser{config: config.NewStore(cfg), transport: tr, sites: sites}}
	page := site.SignURL + "/detail/77"
	article := siteFeedArticle{Title: "Film", Link: &page, Enclosure: page}
	attrs, err := scan.siteAttributes(t.Context(), site, nil, article)
	if err != nil || attrs.Download == nil || *attrs.Download != 0.5 {
		t.Fatal(attrs, err)
	}
	for _, invalid := range []string{"https://other.test/detail/77", "https://kp.m-team.cc/detail/76"} {
		article.Link = &invalid
		if _, err = scan.siteAttributes(t.Context(), site, nil, article); err == nil {
			t.Fatal(invalid)
		}
	}
	article.Link = &page
	site.APIKey = "stale"
	if _, err = scan.siteAttributes(t.Context(), site, nil, article); err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
