package cookiecloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestOriginalProtocolAndRedirectRefusal(t *testing.T) {
	var calls atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		if r.Method != "POST" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" ||
			json.NewDecoder(r.Body).Decode(&body) != nil || body["password"] != "fixture-password" {
			t.Error("original cloud request changed")
		}
		switch r.URL.Path {
		case "/cloud/get/fixture-key":
			io.WriteString(w, `{"cookie_data":{"tracker.test":[{"domain":".tracker.test","name":"SID","value":"cookie"}]},"local_storage_data":{"ignored":true}}`)
		case "/cloud/get/redirect":
			http.Redirect(w, r, "/forbidden", http.StatusTemporaryRedirect)
		default:
			t.Error("cloud redirect was followed")
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer service.Close()
	settings := Settings{Server: service.URL + "/cloud", Key: "fixture-key", Password: "fixture-password"}
	cookies, err := Fetch(context.Background(), settings, service.Client().Transport)
	if err != nil || len(cookies) != 1 || cookies[0].Name != "SID" {
		t.Fatal(cookies, err)
	}
	settings.Key = "redirect"
	if _, err := Fetch(t.Context(), settings, service.Client().Transport); err == nil || calls.Load() != 2 {
		t.Fatal("unexpected redirection", err, calls.Load())
	}
}

func TestCandidateScopeAndFiltering(t *testing.T) {
	now := time.Unix(1000, 0)
	cookies := []Cookie{
		{Domain: "first.tracker.co.uk", Name: "SID", Value: "first"},
		{Domain: "other.tracker.co.uk", Name: "SID", Value: "other"},
		{Domain: ".tracker.co.uk", Name: "shared", Value: "valid", Path: "/"},
		{Domain: ".co.uk", Name: "bad", Value: "suffix"},
		{Domain: ".tracker.co.uk", Name: "restricted", Value: "bad", Path: "/admin"},
		{Domain: ".tracker.co.uk", Name: "expired", Value: "bad", Expiration: 500},
		{Domain: ".tracker.co.uk", Name: "secure", Value: "ssl", Secure: true},
		{Domain: ".tracker.co.uk", Name: "bad", Value: "v\r\nx:y"},
		{Domain: "new.other.test", Name: "SID", Value: "added"},
		{Domain: "onlycf.example", Name: "cf_clearance", Value: "skip"},
		{Domain: "onlycf.example", Name: "CookieAutoDeleteBrowsingDataCleanup", Value: "skip"},
	}
	sites := []siteconfig.Site{{ID: 1, Name: "First", SignURL: "https://first.tracker.co.uk/"}, {ID: 2, Name: "Other", SignURL: "http://other.tracker.co.uk/"}}
	catalog := indexercatalog.Catalog{Indexers: []indexercatalog.Definition{
		{ID: "new", Name: "New", Domain: "https://new.other.test/"},
		{ID: "onlycf", Name: "CF", Domain: "https://onlycf.example/"},
	}}
	plan := Candidates(sites, catalog, cookies, Settings{Mode: "all_mode"}, now)
	if len(plan) != 3 || plan[0].Cookie != "SID=first; shared=valid; secure=ssl" ||
		plan[1].Cookie != "SID=other; shared=valid" || plan[2].Original.ID != 0 || plan[2].Original.Name != "New" {
		t.Fatal(plan)
	}
	plan = Candidates(sites, catalog, cookies, Settings{Mode: "white_mode", WhiteList: "https://other.test,\n unused.example"}, now)
	if len(plan) != 1 || plan[0].Original.Name != "New" {
		t.Fatal(plan)
	}
	plan = Candidates(sites, catalog, cookies, Settings{Mode: "black_mode", BlackList: "tracker.co.uk，other.test"}, now)
	if len(plan) != 0 {
		t.Fatal(plan)
	}
	cookies = append(cookies, Cookie{Domain: "first.tracker.co.uk", Name: "SID", Value: "different"})
	plan = Candidates(sites, catalog, cookies, Settings{Mode: "white_mode", WhiteList: "tracker.co.uk"}, now)
	if len(plan) != 1 || plan[0].Original.ID != 2 {
		t.Fatal("ambiguous target cookie was applied", plan)
	}
}

func TestSettingsMergePreservesRawUnknownData(t *testing.T) {
	raw := `{"server":"localhost:9999/base","key":"fixture","password":"original","onlyonce":true,"future":{"big":9007199254740993,"null":null}}`
	next, err := Merge(raw, map[string]json.RawMessage{"onlyonce": json.RawMessage("false")})
	if err != nil || !strings.Contains(next, `"future":{"big":9007199254740993,"null":null}`) ||
		!strings.Contains(next, `"password":"original"`) {
		t.Fatal(next, err)
	}
	s, err := Parse(next)
	if err != nil || s.OnlyOnce || s.Mode != "all_mode" || s.Server != "http://localhost:9999/base" {
		t.Fatal(s, err)
	}
}

func TestSharedSiteCookieSkipsDifferentReceivers(t *testing.T) {
	sites := []siteconfig.Site{
		{ID: 1, SignURL: "https://first.fixture.test/", RSSURL: "https://rss.fixture.test/feed"},
		{ID: 2, SignURL: "https://second.fixture.test/", RSSURL: "http://second.fixture.test/feed"},
		{ID: 3, SignURL: "https://third.fixture.test/"},
		{ID: 4, SignURL: "https://fourth.fixture.test/", RSSURL: "https://fourth.fixture.test/feed"},
		{ID: 5, SignURL: "https://front.m-team.test/"},
	}
	catalog := indexercatalog.Catalog{Indexers: []indexercatalog.Definition{
		{ID: "alias", Name: "Alias", Domain: "https://www.third.fixture.test/"},
	}}
	cookies := []Cookie{
		{Domain: ".fixture.test", Name: "SID", Value: "fixture", Secure: true},
		{Domain: "front.m-team.test", Name: "SID", Value: "fixture", Secure: true},
	}
	plan := Candidates(sites, catalog, cookies, Settings{Mode: "all_mode"}, time.Now())
	if len(plan) != 1 || plan[0].Original.ID != 4 || plan[0].Cookie != "SID=fixture" {
		t.Fatal(plan)
	}
}
