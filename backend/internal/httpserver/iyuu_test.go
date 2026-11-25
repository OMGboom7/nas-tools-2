package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/iyuu"
)

const iyuuBase = "/api/v1/plugins/IYUUAutoSeed/"
const iyuuTestToken = "fixture-service-token"

var iyuuSource = strings.Repeat("a", 40)
var iyuuExcluded = strings.Repeat("e", 40)

const iyuuFixtureInfo = "d6:lengthi8e4:name9:Movie.mkv6:pieces20:xxxxxxxxxxxxxxxxxxxxe"
const iyuuFixtureTorrent = "d8:announce23:https://tracker.fixture4:info" + iyuuFixtureInfo + "e"

var iyuuTarget = iyuu.Digest(iyuuFixtureInfo)
var iyuuExisting = strings.Repeat("c", 40)

type iyuuFixture struct {
	t              *testing.T
	config         string
	handler        http.Handler
	db             *sql.DB
	token          string
	binds, lookups int
	bindReply      string
	seed           *iyuuSeedFixture
	api            *iyuuAPI
}

func (f *iyuuFixture) upstream(r *http.Request) (*http.Response, error) {
	if f.seed != nil {
		if res, err, ok := f.seed.route(f.t, r); ok {
			return res, err
		}
	}
	if r.URL.Hostname() == "qb.fixture" {
		body := ""
		q := r.URL.Query()
		switch r.URL.Path {
		case "/api/v2/auth/login":
			body = "Ok."
		case "/api/v2/app/version":
			body = "v5.0.0"
		case "/api/v2/app/webapiVersion":
			body = "2.11.0"
		case "/api/v2/app/buildInfo":
			body = `{"libtorrent":"2.0.11.0"}`
		case "/api/v2/torrents/properties":
			body = fmt.Sprintf(`{"infohash_v1":%q,"infohash_v2":""}`, q.Get("hash"))
		case "/api/v2/torrents/files":
			body = `[{"index":0,"name":"Movie.mkv","size":8,"priority":1,"progress":1}]`
		case "/api/v2/torrents/info":
			id := q.Get("hashes")
			switch {
			case q.Get("filter") == "completed":
				body = fmt.Sprintf(`[{"hash":%q},{"hash":%q}]`, iyuuSource, iyuuExcluded)
			case id == iyuuTarget:
				body = "[]"
			case id == iyuuExisting:
				body = fmt.Sprintf(`[{"hash":%q}]`, id)
			case id == iyuuSource || id == iyuuExcluded:
				tags := ""
				if id == iyuuExcluded {
					tags = "keep-private"
				}
				body = fmt.Sprintf(`[{"hash":%q,"name":"Fixture Movie","state":"stoppedUP","added_on":123,"save_path":"/library","tags":%q,"auto_tmm":false,"progress":1,"amount_left":0}]`, id, tags)
			}
		}
		if body != "" {
			return jsonResponse(r, body), nil
		}
	}
	if r.URL.Host != "2025.iyuu.cn" || r.Header.Get("token") != iyuuTestToken {
		f.t.Errorf("unexpected destination %s", r.URL.Host)
		return nil, context.Canceled
	}
	var data string
	switch r.URL.Path {
	case "/reseed/sites/recommend":
		data = `{"list":[{"id":2,"site":"fixture","nickname":"Fixture partner"}]}`
	case "/reseed/users/bind":
		f.binds++
		var in map[string]string
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["passkey"] != iyuu.Digest("mock-passkey") || in["id"] != "45" || in["sid"] != "2" || in["site"] != "fixture" || in["token"] != iyuuTestToken {
			f.t.Error("incorrect binding body")
		}
		if f.bindReply == "lost" {
			return nil, io.ErrUnexpectedEOF
		}
		return jsonResponse(r, f.bindReply), nil
	case "/reseed/sites/index":
		data = `{"sites":[{"id":2,"base_url":"site.fixture","download_page":"download.php?id={}"},{"id":3,"base_url":"other.site.fixture"}]}`
	case "/reseed/sites/reportExisting":
		var in struct {
			IDs []int `json:"sid_list"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || fmt.Sprint(in.IDs) != "[2 3]" {
			f.t.Error(in)
		}
		data = fmt.Sprintf(`{"sid_sha1":%q}`, iyuuSource)
	case "/reseed/index/index":
		var in struct{ Hash string }
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Hash != fmt.Sprintf(`[%q]`, iyuuSource) {
			f.t.Error("incorrect hash scope", in)
		}
		f.lookups++
		data = fmt.Sprintf(`{%q:{"torrent":[{"sid":2,"torrent_id":7,"info_hash":%q},{"sid":2,"torrent_id":8,"info_hash":%q},{"sid":3,"torrent_id":9,"info_hash":%q}]}}`, iyuuSource, iyuuTarget, iyuuExisting, iyuuExcluded)
	default:
		f.t.Errorf("unexpected path %s", r.URL.Path)
		return nil, context.Canceled
	}
	return jsonResponse(r, `{"code":0,"data":`+data+`}`), nil
}
func (f *iyuuFixture) start() {
	var runner *rssRunAPI
	h, err := buildHandler(config.Config{ApplicationConfigPath: f.config, DisableLegacy: true}, roundTripFunc(f.upstream), &runner)
	if err != nil {
		f.t.Fatal(err)
	}
	f.api = runner.archivePlugin.iyuu
	f.handler = h
	f.token = loginForTest(f.t, h, "admin", "password")
}
func newIyuuFixture(t *testing.T) *iyuuFixture {
	f := &iyuuFixture{t: t, config: filepath.Join(t.TempDir(), "config.yaml"), bindReply: `{"code":0,"data":[]}`}
	if err := os.WriteFile(f.config, []byte("app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.start()
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(f.config), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`INSERT INTO DOWNLOADER(ID,NAME,ENABLED,TYPE,CONFIG) VALUES(1,'Fixture qB',1,'qbittorrent','{"host":"qb.fixture"}');
	INSERT INTO CONFIG_SITE(ID,NAME,SIGNURL) VALUES(10,'Fixture Site','https://site.fixture/login');
	INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','UserInstalledPlugins','["IYUUAutoSeed"]'),('SystemConfig','plugin.IYUUAutoSeed','{"token":"fixture-service-token","sites":["10"],"downloaders":[1],"nolabels":"private","future":"keep-me"}');`)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *iyuuFixture) request(method, path string, v any, status int) *httptest.ResponseRecorder {
	body := ""
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			f.t.Fatal(err)
		}
		body = string(b)
	}
	w := nativeJSONRequest(f.handler, method, path, f.token, body)
	if w.Code != status {
		f.t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), iyuuTestToken) || strings.Contains(w.Body.String(), "mock-passkey") {
		f.t.Fatal("secret in response")
	}
	return w
}
func (f *iyuuFixture) overview() (iyuuBinding, string) {
	w := f.request("GET", iyuuBase+"overview", nil, 200)
	var v struct {
		Data struct{ Fingerprint, RequestID, MatchFingerprint string }
	}
	if json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Data.Fingerprint == "" {
		f.t.Fatal(w.Body.String())
	}
	return iyuuBinding{Fingerprint: v.Data.Fingerprint, RequestID: v.Data.RequestID, Site: "fixture", SID: "2", UID: "45", Passkey: "mock-passkey"}, v.Data.MatchFingerprint
}

func TestIYUUBindingAcrossRestartAndLegacyMethod(t *testing.T) {
	f := newIyuuFixture(t)
	in, _ := f.overview()
	f.request("GET", iyuuBase+"partners", nil, 200)
	f.request("GET", iyuuBase+"page", nil, 200)
	w := performFormRequest(f.handler, "/api/v1/plugin/page", f.token, url.Values{"id": {"IYUUAutoSeed"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Fixture partner") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = f.request("POST", iyuuBase+"bind", in, 200)
	if !strings.Contains(w.Body.String(), `"state":"bound"`) {
		t.Fatal(w.Body.String())
	}
	f.start()
	f.request("POST", iyuuBase+"bind", in, 200)
	if f.binds != 1 {
		t.Fatal("binding was replayed")
	}
	in.UID = "46"
	f.request("POST", iyuuBase+"bind", in, 409)
	if !strings.Contains(f.request("GET", iyuuBase+"overview", nil, 200).Body.String(), `"state":"bound"`) {
		t.Fatal("missing receipt")
	}
	legacy := map[string]any{"cmd": "run_plugin_method", "data": map[string]string{"plugin_id": "IYUUAutoSeed", "method": "iyuu_bind_site", "sid": "2", "site": "fixture", "uid": "45", "passkey": "mock-passkey"}}
	for range 2 {
		w = f.request("POST", "/do", legacy, 200)
		if !strings.Contains(w.Body.String(), `"result":{"code":0`) {
			t.Fatal(w.Body.String())
		}
	}
	if f.binds != 2 {
		t.Fatal(f.binds)
	}
	f.token = "invalid"
	f.request("GET", iyuuBase+"overview", nil, 401)
}
func TestIYUUUnknownBindingAndStaleSelection(t *testing.T) {
	f := newIyuuFixture(t)
	in, proof := f.overview()
	f.bindReply = "lost"
	for range 2 {
		w := f.request("POST", iyuuBase+"bind", in, 200)
		if !strings.Contains(w.Body.String(), `"state":"unknown"`) {
			t.Fatal(w.Body.String())
		}
	}
	if f.binds != 1 {
		t.Fatal(f.binds)
	}
	if _, err := f.db.Exec("UPDATE DOWNLOADER SET NAME='Changed' WHERE ID=1"); err != nil {
		t.Fatal(err)
	}
	f.request("POST", iyuuBase+"matches", map[string]string{"fingerprint": proof}, 409)
	in, _ = f.overview()
	in.Site = "foreign"
	f.request("POST", iyuuBase+"bind", in, 409)
	if f.binds != 1 || f.lookups != 0 {
		t.Fatal("unexpected outbound action")
	}
}
func TestIYUUMatchesOnlyEligibleSourcesAndLocalTargets(t *testing.T) {
	f := newIyuuFixture(t)
	_, proof := f.overview()
	w := f.request("POST", iyuuBase+"matches", map[string]string{"fingerprint": proof}, 200)
	var out struct {
		Data struct {
			Scanned int
			Items   []iyuuMatch
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Data.Scanned != 1 || len(out.Data.Items) != 1 || out.Data.Items[0].Hash != iyuuTarget || out.Data.Items[0].SiteID != "10" || f.lookups != 1 {
		t.Fatal(w.Body.String())
	}
}

func TestIYUUSettingsAndAdministratorScope(t *testing.T) {
	f := newIyuuFixture(t)
	path := "/api/v1/plugins/IYUUAutoSeed"
	get := f.request("GET", path, nil, 200)
	var detail struct{ Data pluginConfigDetail }
	if json.Unmarshal(get.Body.Bytes(), &detail) != nil || len(detail.Data.Fields) != 9 || detail.Data.Meta.Fingerprint == "" {
		t.Fatal(get.Body.String())
	}
	in := map[string]any{"fingerprint": detail.Data.Meta.Fingerprint, "values": map[string]any{"nolabels": "private, films", "sites": []string{"10"}}}
	f.request("PUT", path, in, 200)
	f.request("PUT", path, in, 409)
	var raw string
	if err := f.db.QueryRow("SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.IYUUAutoSeed'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if json.Unmarshal([]byte(raw), &saved) != nil || saved["future"] != "keep-me" || saved["token"] != iyuuTestToken || saved["nolabels"] != "private, films" {
		t.Fatal("old settings were lost")
	}
	w := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	f.token = loginForTest(t, f.handler, "viewer", "strong-password")
	f.request("GET", path, nil, 403)
	f.request("GET", iyuuBase+"partners", nil, 403)
	f.request("POST", iyuuBase+"bind", map[string]string{}, 403)
	f.request("POST", iyuuBase+"matches", map[string]string{}, 403)
	if f.binds != 0 || f.lookups != 0 {
		t.Fatal("unauthorized provider call")
	}
}
