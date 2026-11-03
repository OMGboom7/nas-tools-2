package sitestats

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
)

func TestLegacyLayoutTablesCannotDuplicateOrTruncateSeeding(t *testing.T) {
	for _, schema := range []string{"NexusPhp", "FileList", "SmallHorse", "Gazelle", "Unit3d", "TorrentLeech", "Discuz"} {
		t.Run(schema, func(t *testing.T) {
			table := seedTable(10, 6, 8)
			wrapped := `<table id="layout"><tr><td>` + table + `</td></tr></table>`
			rows, ok := seedRows(wrapped, schema)
			if !ok || len(rows) != 1 || rows[0] != [2]int64{3, 1610612736} {
				t.Fatal(schema, rows, ok)
			}
		})
	}
	for _, bad := range []string{`<tr><td>Fixture</td><td>broken</td><td>not a size</td><td>3</td></tr>`, `<tr><td colspan="4">Database error</td></tr>`, `<tr><td>truncated</td></tr>`} {
		p := `<table class="torrents"><tr><td>Name</td><td>Other</td><td>Size</td><td>Seeders</td></tr>` + bad + `</table>`
		if rows, ok := seedRows(p, "NexusPhp"); ok {
			t.Fatal("malformed table accepted", rows, p)
		}
	}
	p := `<table class="torrents"><tr><td>Name</td><td>Other</td><td>Size</td><td>Seeders</td></tr><tr><td colspan="4">No torrents</td></tr></table>`
	if rows, ok := seedRows(p, "NexusPhp"); !ok || len(rows) != 0 {
		t.Fatal("explicit empty state rejected", rows, ok)
	}
	if _, ok := seedRows(`<table><tr><th>Database error</th></tr></table>`, "NexusPhp"); ok {
		t.Fatal("unrelated error table became an empty seed list")
	}
}

func TestNexusRelativePaginationKeepsTheOriginalSeedEndpoint(t *testing.T) {
	page := 0
	f := fetchFunc(func(_ context.Context, r Request) ([]byte, error) {
		u, e := url.Parse(r.Path)
		if e != nil || strings.TrimPrefix(u.Path, "/") != "getusertorrentlistajax.php" || u.Query().Get("userid") != "42" {
			t.Fatalf("relative page escaped seed endpoint: %s", r.Path)
		}
		page++
		if page == 1 {
			return []byte(seedTable(5, 3, 4) + `<a href="?page=2">下一页</a>`), nil
		}
		if u.Query().Get("page") != "2" {
			t.Fatal(r.Path)
		}
		return []byte(seedTable(5, 3, 4)), nil
	})
	v := Snapshot{Schema: "NexusPhp", UserID: "42"}
	if e := seedPages(t.Context(), f, &v, Request{Path: "getusertorrentlistajax.php?userid=42&type=seeding"}); e != nil || v.Seeding != 2 || v.SeedingSize != 3221225472 {
		t.Fatal(v, e)
	}
}
func TestNexusDetailOnlyRetainsSeedFallbackAndUserTraffic(t *testing.T) {
	home := []byte(`<title>Fixture</title><a href="userdetails">fixture</a>`)
	for _, summary := range []bool{false, true} {
		t.Run(stringInt(map[bool]int64{false: 0, true: 1}[summary]), func(t *testing.T) {
			detail := `<div>总上传量：100 TB</div><div>上传量：2 GiB</div><div>下载量：1 GiB</div>`
			if summary {
				detail += `<table><tr><td>做种统计</td><td>总做种数: 1 总做种体积: 1.5 GiB</td></tr></table>`
			} else {
				detail += `<table><tr><td>当前上传</td><td>` + seedTable(6, 4, 5) + `</td></tr></table>`
			}
			f := fetchFunc(func(_ context.Context, r Request) ([]byte, error) {
				if r.Path == "userdetails" {
					return []byte(detail), nil
				}
				return nil, ErrResponse
			})
			v, e := Parse(t.Context(), f, home, Options{})
			if e != nil || v.Upload != 2<<30 || v.Download != 1<<30 || v.Seeding != 1 || v.SeedingSize != 1610612736 || v.SeedDetailsKnown == summary {
				t.Fatalf("%+v %v", v, e)
			}
		})
	}
}
func TestBrowserSessionFlowsIntoRawJSONAndCSRFAndHonorsDeletion(t *testing.T) {
	renders := 0
	rawRequests := 0
	c := Client{Origin: "https://fixture.invalid", Cookie: "old=fixture", Emulate: true, Guard: func(context.Context) error { return nil }}
	c.Renderer = rendererFunc(func(_ context.Context, r browserruntime.Request) (browserruntime.Result, error) {
		renders++
		if renders == 1 && r.Cookie != "old=fixture" {
			t.Fatal("initial Cookie lost", r.Cookie)
		}
		cookie := "session=acquired"
		if renders == 2 {
			if r.Cookie != "session=acquired" {
				t.Fatal("acquired session lost", r.Cookie)
			}
			cookie = ""
		}
		return browserruntime.Result{URL: r.URL, HTML: []byte(`<title>Fixture</title>`), Cookie: cookie}, nil
	})
	c.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		rawRequests++
		want := "session=acquired"
		if rawRequests > 1 {
			want = ""
		}
		if r.Header.Get("Cookie") != want || r.Header.Get("X-CSRF-TOKEN") != "fixture-token" {
			t.Fatal("raw request lost session/header", r.Header)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":200}`))}, nil
	})
	if _, e := c.Fetch(t.Context(), Request{Path: "/"}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Fetch(t.Context(), Request{Path: "/api/user/getMainInfo", Raw: true, Headers: map[string]string{"X-CSRF-TOKEN": "fixture-token"}}); e != nil {
		t.Fatal(e)
	}
	if renders != 1 {
		t.Fatal("API rendered as DOM")
	}
	if _, e := c.Fetch(t.Context(), Request{Path: "/profile"}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Fetch(t.Context(), Request{Path: "/api/user/getMainInfo", Raw: true, Headers: map[string]string{"X-CSRF-TOKEN": "fixture-token"}}); e != nil {
		t.Fatal(e)
	}
}
func TestPublicByteCountersRemainExactAndMissingSeedDetailDoesNotClearOld(t *testing.T) {
	v := fixtureSnapshot()
	b, e := json.Marshal(v)
	if e != nil || !strings.Contains(string(b), `"upload":"9007199254740993"`) {
		t.Fatal(string(b), e)
	}
	var restored Snapshot
	if e = json.Unmarshal(b, &restored); e != nil || restored.Upload != v.Upload {
		t.Fatal(restored, e)
	}
	s := statsStore(t)
	j := runningJob(t, s, "seed-original")
	if e = s.Save(t.Context(), j, v, "2026-11-03", "original", false); e != nil {
		t.Fatal(e)
	}
	v.SeedDetailsKnown = false
	v.Seeds = nil
	v.SeedingSize = 5 << 30
	if e = s.Save(t.Context(), j, v, "2026-11-03", "summary-only", false); e != nil {
		t.Fatal(e)
	}
	seeds, e := s.Seeds(t.Context(), v.URL)
	if e != nil || len(seeds) != 1 || seeds[0][1] != 1610612736 {
		t.Fatal("summary erased old details", seeds, e)
	}
}
