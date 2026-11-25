package httpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/iyuu"
)

type iyuuSeedFixture struct {
	targetReads, failRead int
	mu                    sync.Mutex
	state, tags, body     string
	adds, checks, starts  int
	lost                  bool
}

func (f *iyuuSeedFixture) route(t *testing.T, r *http.Request) (*http.Response, error, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := ""
	switch r.URL.Hostname() {
	case "site.fixture":
		if r.URL.RequestURI() != "/download.php?id=7&https=1" && r.URL.RequestURI() != "/download.php?https=1&id=7" {
			t.Errorf("unexpected site request %s", r.URL.RequestURI())
			return nil, iyuu.ErrInput, true
		}
		body = iyuuFixtureTorrent
		if f.body != "" {
			body = f.body
		}
	case "qb.fixture":
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/api/v2/torrents/add":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			defer r.MultipartForm.RemoveAll()
			file, _, err := r.FormFile("torrents")
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(file)
			file.Close()
			if err != nil || string(b) != iyuuFixtureTorrent || r.FormValue("stopped") != "true" || r.FormValue("skip_checking") != "false" || r.FormValue("savepath") != "/library" {
				t.Error("wrong paused add")
			}
			f.tags = r.FormValue("tags")
			if !strings.HasPrefix(f.tags, "已整理,辅种,iyuu-") {
				t.Error("missing ownership label")
			}
			f.adds++
			f.state = "stoppedDL"
			if f.lost {
				return nil, io.ErrUnexpectedEOF, true
			}
			body = "Ok."
		case r.URL.Path == "/api/v2/torrents/info" && q.Get("hashes") == iyuuTarget:
			f.targetReads++
			if f.failRead == f.targetReads {
				return nil, io.ErrUnexpectedEOF, true
			}
			body = "[]"
			if f.state != "" {
				done, left := 0, 8
				if f.state == "stoppedUP" || f.state == "uploading" {
					done, left = 1, 0
				}
				body = fmt.Sprintf(`[{"hash":%q,"name":"Fixture Movie","state":%q,"added_on":125,"save_path":"/library","tags":%q,"tracker":"https://tracker.fixture","auto_tmm":false,"progress":%d,"amount_left":%d}]`, iyuuTarget, f.state, f.tags, done, left)
			}
		case r.URL.Path == "/api/v2/torrents/recheck" || r.URL.Path == "/api/v2/torrents/start":
			if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("hashes") != iyuuTarget {
				t.Error("wrong remote mutation")
			}
			if strings.HasSuffix(r.URL.Path, "recheck") {
				f.checks++
				f.state = "checkingUP"
			} else {
				f.starts++
				if f.state != "stoppedUP" {
					t.Fatal("started incomplete task")
				}
				f.state = "uploading"
			}
			body = "Ok."
		default:
			return nil, nil, false
		}
	default:
		return nil, nil, false
	}
	return jsonResponse(r, body), nil, true
}
func (f *iyuuFixture) firstSeed(t *testing.T) (map[string]any, iyuuMatch) {
	_, proof := f.overview()
	w := f.request("POST", iyuuBase+"matches", map[string]string{"fingerprint": proof}, 200)
	var list struct{ Data struct{ Items []iyuuMatch } }
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Data.Items) != 1 {
		t.Fatal(w.Body.String())
	}
	m := list.Data.Items[0]
	return map[string]any{"fingerprint": proof, "candidate": m}, m
}
func TestIYUUSeedSubmitsChecksStartsAndRestarts(t *testing.T) {
	f := newIyuuFixture(t)
	f.seed = &iyuuSeedFixture{}
	in, _ := f.firstSeed(t)
	w := f.request("POST", iyuuBase+"seeds", in, 200)
	var res struct{ Data iyuuSeedView }
	if json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Data.State != "added" || f.seed.adds != 1 {
		t.Fatal(w.Body.String())
	}
	f.start()
	f.request("POST", iyuuBase+"seeds", in, 200)
	for range 3 {
		f.api.seedTick(t.Context())
	}
	if f.seed.checks != 1 || f.seed.starts != 0 {
		t.Fatal("premature start", f.seed)
	}
	f.seed.state = "stoppedUP"
	for range 5 {
		f.api.seedTick(t.Context())
	}
	if f.seed.adds != 1 || f.seed.starts != 1 || f.seed.checks != 1 {
		t.Fatal("action counts", f.seed)
	}
	f.start()
	w = f.request("GET", iyuuBase+"seeds", nil, 200)
	if !strings.Contains(w.Body.String(), `"state":"seeding"`) || strings.Contains(w.Body.String(), "/library") || strings.Contains(w.Body.String(), "tracker.fixture") {
		t.Fatal(w.Body.String())
	}
	var history, cache string
	err := f.db.QueryRow("SELECT VALUE FROM PLUGIN_HISTORY WHERE PLUGIN_ID='IYUUAutoSeed' AND KEY=?", iyuuSource).Scan(&history)
	if err != nil || !strings.Contains(history, iyuuTarget) {
		t.Fatal(history, err)
	}
	if err = f.db.QueryRow("SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.IYUUAutoSeed'").Scan(&cache); err != nil || !strings.Contains(cache, iyuuTarget) || !strings.Contains(cache, "keep-me") {
		t.Fatal("original config or cache lost", err)
	}
	f.request("POST", iyuuBase+"seeds", in, 200)
}
func TestIYUUSeedRejectsWrongMetadataChangedSourceAndLostAdd(t *testing.T) {
	f := newIyuuFixture(t)
	f.seed = &iyuuSeedFixture{body: "wrong metainfo"}
	in, m := f.firstSeed(t)
	f.request("POST", iyuuBase+"seeds", in, 409)
	f.seed.body = ""
	m.SourceProof = strings.Repeat("d", 64)
	f.request("POST", iyuuBase+"seeds", map[string]any{"fingerprint": in["fingerprint"], "candidate": m}, 409)
	if f.seed.adds != 0 {
		t.Fatal("added an unverified target")
	}
	f.seed.lost = true
	for range 2 {
		w := f.request("POST", iyuuBase+"seeds", in, 200)
		if !strings.Contains(w.Body.String(), `"state":"unknown"`) {
			t.Fatal(w.Body.String())
		}
	}
	f.start()
	f.api.seedTick(t.Context())
	if f.seed.adds != 1 || f.seed.checks != 0 || f.seed.starts != 0 {
		t.Fatal("replayed unknown receipt")
	}
}
func TestIYUUSeedPathsAndDetailSelectors(t *testing.T) {
	origin := "https://site.fixture/"
	for _, raw := range []string{"https://foreign.fixture/dl/7", "//foreign.fixture/download.php?id=7", "http://site.fixture/download.php?id=7", "https://user@site.fixture/dl/7"} {
		if _, err := iyuuDownloadLink(origin, raw); err == nil {
			t.Fatal(raw)
		}
	}
	raw, detail, err := iyuuTemplate(origin, "download.php?id={}&passkey={passkey}&uid={uid}", "7")
	if err != nil || detail || raw != origin+"download.php?id=7&https=1" {
		t.Fatal(raw, detail, err)
	}
	for _, body := range []string{`<form action="download.php?id=7"></form>`, `<a href="/download.php?hash=7">get</a>`, `<a href="/download.php?id=7">get</a>`, `<a class="index" href="/dl/7">get</a>`, `<input title="DirectLink" value="/dl/7">`} {
		v, err := iyuuExtract(t.Context(), []byte(body), origin)
		if err != nil || !strings.HasPrefix(v, origin) || !strings.Contains(v, "https=1") {
			t.Fatal(v, err)
		}
	}
}

func TestIYUUSeedDoesNotAdoptReplacedTaskOrUninstalledScope(t *testing.T) {
	f := newIyuuFixture(t)
	f.seed = &iyuuSeedFixture{}
	in, _ := f.firstSeed(t)
	f.request("POST", iyuuBase+"seeds", in, 200)
	tags := f.seed.tags
	f.seed.tags = "已整理,辅种"
	f.api.seedTick(t.Context())
	if f.seed.checks != 0 || f.seed.starts != 0 {
		t.Fatal("foreign target was modified")
	}
	f.seed.tags = tags
	if _, err := f.db.Exec("UPDATE SYSTEM_DICT SET VALUE='[]' WHERE KEY='UserInstalledPlugins'"); err != nil {
		t.Fatal(err)
	}
	f.api.seedTick(t.Context())
	if f.seed.checks != 0 || f.seed.starts != 0 {
		t.Fatal("uninstalled plugin acted")
	}
	var n int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM GO_IYUU_SEEDS WHERE state='added'").Scan(&n); err != nil || n != 1 {
		t.Fatal("unowned target entered history", err, n)
	}
}

func TestIYUUSeedDefinitelyUnsentCanBeSubmittedAgain(t *testing.T) {
	f := newIyuuFixture(t)
	f.seed = &iyuuSeedFixture{}
	in, _ := f.firstSeed(t)
	f.seed.failRead = f.seed.targetReads + 2
	w := f.request("POST", iyuuBase+"seeds", in, 200)
	if !strings.Contains(w.Body.String(), `"state":"unsent"`) || f.seed.adds != 0 {
		t.Fatal(w.Body.String())
	}
	w = f.request("POST", iyuuBase+"seeds", in, 200)
	if !strings.Contains(w.Body.String(), `"state":"added"`) || f.seed.adds != 1 {
		t.Fatal(w.Body.String())
	}
}
