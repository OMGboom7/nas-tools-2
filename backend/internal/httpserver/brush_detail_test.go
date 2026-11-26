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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/database"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestBrushDeadlineUnitsAndClock(t *testing.T) {
	now := time.Date(2026, 2, 28, 1, 10, 38, 0, time.FixedZone("CST", 28800))
	for input, want := range map[string]string{"2日23時11分": "20260303_0021", "剩余时间：1天23小时48分钟": "20260302_0058", "3分钟": "20260228_0113", "2026-05-04 08:12:59": "20260504_0812", "2026年12月3日": "", "-3天": "", "0分钟": "", "已过期": "", "999999999999999天": ""} {
		if got := brushDeadlineValue(input, now); got != want {
			t.Errorf("%q: %q want %q", input, got, want)
		}
	}
}

func TestBrushDetailPromotionEvidence(t *testing.T) {
	rules := subscriptionTestDetailRules()
	rules["FREE_DDL"] = json.RawMessage(`["//time"]`)
	parse := func(body string) (externalindexer.Resource, error) {
		return parseSubscriptionDetailPolicy(t.Context(), []byte("<html><h1 id='top'>Film <span>FREE</span></h1>"+body+"</html>"), "Film", rules, externalindexer.Resource{}, brushRSSDetail)
	}
	expiry := `<time title="2026-09-01 16:33:28">剩余时间</time>`
	got, err := parse(`<span class="double"></span><span class="hr"></span><span id="seeders">1,025 人</span>` + expiry)
	if err != nil || got.PromotionEnd != "20260901_1633" || got.HitAndRun == nil || !*got.HitAndRun || got.Seeders == nil || *got.Seeders != 1025 {
		t.Fatal(got, err)
	}
	got, err = parse(expiry)
	if err != nil || got.PromotionEnd != "" || got.Seeders != nil || got.Freeleech == nil || *got.Freeleech {
		t.Fatal(got, err)
	}
	if _, err = parse(`<span class="free"></span>` + expiry + `<time title="2026-09-02 16:33:28">other</time>`); err == nil {
		t.Fatal("conflicting deadlines accepted")
	}
	start := time.Now()
	got, err = parseSubscriptionDetailPolicy(t.Context(), []byte(`<h1 id="top">Film <span>1小时</span></h1><span class="free"></span>`), "Film", rules, externalindexer.Resource{}, brushRSSDetail)
	a, b := start.Add(time.Hour).Format("20060102_1504"), time.Now().Add(time.Hour).Format("20060102_1504")
	if err != nil || got.PromotionEnd != a && got.PromotionEnd != b {
		t.Fatal(got, err)
	}
}

func TestBrushDetailCommandScope(t *testing.T) {
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
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Cookie") != "sid=fixture" || r.UserAgent() != "fixture-agent" {
			t.Error("incorrect credentials")
		}
		if r.URL.Path == "/pause" {
			if _, err := db.Exec(`UPDATE SITE_BRUSH_TASK SET STATE='S' WHERE ID=5`); err != nil {
				t.Error(err)
			}
		}
		fmt.Fprint(w, `<html><h1>Film</h1><span class="free"></span><span id="seeders">0 seeders</span></html>`)
	}))
	defer srv.Close()
	if _, err := db.Exec(`INSERT INTO CONFIG_SITE(ID,NAME,INCLUDE,COOKIE,RSSURL) VALUES(2,'Tracker','S','sid=fixture',?)`, srv.URL+"/feed"); err != nil {
		t.Fatal(err)
	}
	const custom = "https://feed.invalid/override"
	if _, err := db.Exec(`INSERT INTO SITE_BRUSH_TASK(ID,SITE,STATE,RSSURL) VALUES(5,'2','Y',?)`, custom); err != nil {
		t.Fatal(err)
	}
	cat := filepath.Join(dir, "sites.bin")
	data, err := json.Marshal(indexercatalog.Catalog{Indexers: []indexercatalog.Definition{}, Conf: map[string]map[string]json.RawMessage{srv.URL: subscriptionTestDetailRules()}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cat, []byte(base64.StdEncoding.EncodeToString(data)), 0600); err != nil {
		t.Fatal(err)
	}
	q := brushDetailRequest{TaskID: 5, SiteID: 2, FeedURL: custom, Title: "Film", PageURL: srv.URL + "/page", Enclosure: "fixture"}
	run := func(want bool) brushAttributes {
		t.Helper()
		b, _ := json.Marshal(q)
		var out bytes.Buffer
		err := BrushDetailCommand(t.Context(), cfg, cat, bytes.NewReader(b), &out)
		if (err == nil) != want || !want && out.Len() != 0 || strings.Contains(out.String(), "sid=fixture") {
			t.Fatal(out.String(), err)
		}
		var result brushAttributes
		if want {
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	got := run(true)
	if got.Free == nil || !*got.Free || got.Seeders == nil || *got.Seeders != 0 || got.HitAndRun == nil || *got.HitAndRun {
		t.Fatal(got)
	}
	q.PageURL = ""
	if got = run(true); got.Seeders != nil || got.Free != nil || got.HitAndRun != nil {
		t.Fatal(got)
	}
	q.PageURL = "https://other.invalid/details"
	run(false)
	q.PageURL = srv.URL + "/page"
	q.FeedURL = srv.URL + "/feed"
	run(false)
	q.FeedURL = custom
	q.SiteID = 1
	run(false)
	q.SiteID = 2
	q.PageURL = srv.URL + "/pause"
	run(false)
	run(false)
	if hits.Load() != 2 {
		t.Fatal(hits.Load())
	}
}

func TestBrushMTeamDeadlineAndPersonalFree(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("app: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	end := "2099-04-30 23:55:35"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.m-team.cc" || r.Header.Get("x-api-key") != "fixture" || r.Header.Get("Cookie") != "" {
			t.Error("M-Team credential scope")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":"0","data":{"id":"77","name":"Film","status":{"discount":"NORMAL","seeders":13,"toppingEndTime":"2099-04-01 12:34:56","mallSingleFree":{"endDate":"` + end + `"}}}}`))}, nil
	})
	var catalog rssDetailCatalog
	r, err := catalog.read(context.Background(), siteBrowser{config: config.NewStore(cfg), transport: transport}, "", siteconfig.Site{SignURL: "https://kp.m-team.cc", APIKey: "fixture"}, nil, externalindexer.Resource{Title: "Film", PageURL: "https://kp.m-team.cc/detail/77"}, brushRSSDetail)
	deadline, _ := time.ParseInLocation(time.DateTime, end, time.FixedZone("CST", 8*60*60))
	if err != nil || r.Freeleech == nil || !*r.Freeleech || r.PromotionEnd != deadline.Local().Format(time.DateTime) || r.Seeders == nil || *r.Seeders != 13 || r.HitAndRun != nil {
		t.Fatal(r, err)
	}
}
