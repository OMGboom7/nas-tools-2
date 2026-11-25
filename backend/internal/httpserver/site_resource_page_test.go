package httpserver

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestSiteResourcePageOriginalClientsAndGoOnly(t *testing.T) {
	const path = "/api/v1/site/resources"
	const catalog = "../../../web/backend/user.sites.bin"
	const fixture = `{"code":"0","data":{"data":[{"id":77,"name":"Fixture","smallDescr":"中文","size":"9007199254740993","status":{"seeders":"5","leechers":0,"discount":"_2X_FREE"},"episode_info":{"imdb":"tt1234567"}}]}}`
	siteCalls, fallbackCalls := 0, 0
	wantKeyword := any(nil)
	siteBody := fixture
	other := url.Values{"id": {"unmigrated"}, "keyword": {"剧集 & #7"}, "page": {"2"}}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "api.m-team.info":
			siteCalls++
			if r.URL.Path != "/api/torrent/search" || r.Method != "POST" || r.UserAgent() != "fixture-agent" || r.Header.Get("x-api-key") != "saved-key" || r.Header.Get("Cookie") != "" {
				t.Fatal("unexpected M-Team request")
			}
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			k, has := input["keyword"]
			if !has || k != wantKeyword || input["pageNumber"] != float64(3) || input["pageSize"] != float64(100) {
				t.Fatal(input)
			}
			return jsonResponse(r, siteBody), nil
		case "zhuque.in":
			siteCalls++
			if r.Header.Get("Cookie") != "saved-cookie" || r.Header.Get("x-api-key") != "" {
				t.Fatal("unexpected Zhuque credentials")
			}
			if r.URL.Path == "/" {
				return jsonResponse(r, `<meta name="x-csrf-token" content="fixture-token">`), nil
			}
			var input map[string]any
			if r.URL.Path != "/api/torrent/advancedSearch" || r.Header.Get("X-CSRF-TOKEN") != "fixture-token" || json.NewDecoder(r.Body).Decode(&input) != nil || input["page"] != float64(1) || input["size"] != float64(100) {
				t.Fatal("unexpected Zhuque page")
			}
			return jsonResponse(r, `{"data":{"torrents":[{"id":7,"title":"Movie","size":1,"complete":"12","upload_time":null}]}}`), nil
		case "legacy":
			fallbackCalls++
			body, e := io.ReadAll(r.Body)
			if e != nil || string(body) != other.Encode() || r.URL.Path != path {
				t.Fatal("fallback lost original form")
			}
			return jsonResponse(r, `{"fixture":"other parser"}`), nil
		default:
			t.Fatalf("unexpected destination %s", r.URL.Hostname())
			return nil, nil
		}
	})
	dir := t.TempDir()
	mixed, store, token := nativeSiteFixtureAt(t, dir, transport, catalog)
	site := siteconfig.Site{Name: "Original name", SignURL: "https://kp.m-team.info/login", Cookie: "unused-cookie", APIKey: "saved-key", Note: `{"ua":"fixture-agent"}`}
	for _, s := range []siteconfig.Site{site, {Name: "Zhuque", SignURL: "https://zhuque.in/", Cookie: "saved-cookie"}} {
		if _, err := store.Upsert(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	pure, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(dir, "config.yaml"), SiteCatalogPath: catalog, DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []http.Handler{mixed, pure} {
		for _, selector := range []string{"mteam-kpinfo", "kp.m-team.info", "https://kp.m-team.info/"} {
			form := url.Values{"id": {selector}, "page": {"2"}, "apiKey": {"ignored-input"}, "domain": {"https://unused.example"}}
			wantKeyword = nil
			if selector != "mteam-kpinfo" {
				keyword := ""
				if selector == "kp.m-team.info" {
					keyword = "影片 & 测试"
				}
				wantKeyword = keyword
				form.Set("keyword", keyword)
			}
			response := performFormRequest(h, path, token, form)
			var reply struct {
				Success bool
				Data    struct{ Data []map[string]any }
			}
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &reply) != nil || !reply.Success || len(reply.Data.Data) != 1 {
				t.Fatal(response.Code, response.Body.String())
			}
			row := reply.Data.Data[0]
			if len(row) != 11 || row["size"] != "9007199254740993" || row["seeders"] != "5" || row["peers"] != float64(0) || row["imdbid"] != "tt1234567" || row["uploadvolumefactor"] != float64(2) || row["downloadvolumefactor"] != float64(0) || row["enclosure"] != "https://kp.m-team.info/detail/77" {
				t.Fatal(row)
			}
		}
		response := performFormRequest(h, path, token, url.Values{"site": {"zhuque"}})
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"grabs":"12"`) || !strings.Contains(response.Body.String(), `"pubdate":null`) {
			t.Fatal(response.Code, response.Body.String())
		}
		before := siteCalls
		for _, bad := range []struct {
			token, page string
			status      int
		}{{"", "2", 401}, {token, "-1", 400}, {token, "x", 400}} {
			res := performFormRequest(h, path, bad.token, url.Values{"id": {"mteam-kpinfo"}, "page": {bad.page}})
			if res.Code != bad.status || siteCalls != before {
				t.Fatal(res.Code, res.Body.String(), siteCalls)
			}
		}
	}
	if res := performFormRequest(mixed, path, token, other); res.Code != 200 || fallbackCalls != 1 {
		t.Fatal(res.Code, fallbackCalls)
	}
	if res := performFormRequest(pure, path, token, other); res.Code != 501 || fallbackCalls != 1 {
		t.Fatal(res.Code, fallbackCalls)
	}
	wantKeyword = nil
	form := url.Values{"id": {"mteam-kpinfo"}, "page": {"2"}}
	siteBody = `{"code":0,"data":{"data":[]}}`
	res := performFormRequest(pure, path, token, form)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"code":1`) {
		t.Fatal(res.Body.String())
	}
	siteBody = `{"code":1,"data":null}`
	res = performFormRequest(pure, path, token, form)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"success":false`) {
		t.Fatal(res.Body.String())
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var okCount, failCount int
	err = db.QueryRow("SELECT SUM(RESULT='Y'), SUM(RESULT='N') FROM INDEXER_STATISTICS WHERE INDEXER=? AND TYPE='builtin' AND SECONDS >= 0 AND DATE IS NOT NULL", site.Name).Scan(&okCount, &failCount)
	if err != nil || okCount != 7 || failCount != 1 {
		t.Fatal(okCount, failCount, err)
	}

	site.Name = "Duplicate"
	if _, err := store.Upsert(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	before := siteCalls
	res = performFormRequest(mixed, path, token, form)
	if !strings.Contains(res.Body.String(), `"success":false`) || siteCalls != before || fallbackCalls != 1 {
		t.Fatal(res.Body.String(), siteCalls, fallbackCalls)
	}
}
