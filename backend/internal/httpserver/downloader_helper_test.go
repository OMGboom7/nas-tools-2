package httpserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

type helperFixture struct {
	*iyuuFixture
	helper                *downloaderHelperAPI
	rows                  map[string]map[string]any
	dir                   string
	starts, tags, deletes int
	loseStart             bool
}

func (f *helperFixture) transport(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() != "qb.fixture" {
		return nil, fmt.Errorf("unexpected destination")
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v2/")
	body := "Ok."
	switch path {
	case "auth/login":
	case "app/version":
		body = "v5.2.3"
	case "app/webapiVersion":
		body = "2.11.0"
	case "app/buildInfo":
		body = `{"libtorrent":"2.0.11.0"}`
	case "torrents/properties":
		b, _ := json.Marshal(map[string]string{"infohash_v1": r.URL.Query().Get("hash"), "infohash_v2": ""})
		body = string(b)
	case "torrents/files":
		row := f.rows[r.URL.Query().Get("hash")]
		v := row["file"]
		if v == nil {
			v = row["name"]
		}
		b, _ := json.Marshal([]any{map[string]any{"name": v, "index": 0, "priority": 1, "size": 8, "progress": row["progress"]}})
		body = string(b)
	case "torrents/info":
		list := []map[string]any{}
		for h, row := range f.rows {
			if id := r.URL.Query().Get("hashes"); id == "" || id == h {
				list = append(list, row)
			}
		}
		b, _ := json.Marshal(list)
		body = string(b)
	case "torrents/start", "torrents/addTags", "torrents/removeTags", "torrents/delete":
		if r.Method != "POST" || r.ParseForm() != nil {
			f.t.Fatal("wrong mutation")
		}
		h := r.Form.Get("hashes")
		row := f.rows[h]
		if row == nil {
			f.t.Fatal("unexpected target")
		}
		switch path {
		case "torrents/start":
			f.starts++
			if f.loseStart {
				return nil, errors.New("connection lost")
			}
			row["state"] = "stalledUP"
		case "torrents/addTags", "torrents/removeTags":
			tags := []string{}
			for _, v := range strings.Split(row["tags"].(string), ",") {
				if v = strings.TrimSpace(v); v != "" {
					tags = append(tags, v)
				}
			}
			v := r.Form.Get("tags")
			if strings.HasSuffix(path, "addTags") {
				tags = append(tags, v)
			} else {
				tags = slices.DeleteFunc(tags, func(s string) bool { return s == v })
			}
			row["tags"] = strings.Join(tags, ", ")
			f.tags++
		case "torrents/delete":
			if r.Form.Get("deleteFiles") != "true" {
				f.t.Fatal("wrong delete mode")
			}
			f.deletes++
			delete(f.rows, h)
		}
	default:
		f.t.Fatalf("unexpected qB path %s", path)
	}
	return jsonResponse(r, body), nil
}
func (f *helperFixture) restart() {
	var runner *rssRunAPI
	handler, err := buildHandler(config.Config{ApplicationConfigPath: f.config, DisableLegacy: true}, roundTripFunc(f.transport), &runner)
	if err != nil {
		f.t.Fatal(err)
	}
	f.helper = runner.archivePlugin.helper
	f.handler = handler
	f.token = loginForTest(f.t, handler, "admin", "password")
}
func newHelperFixture(t *testing.T) *helperFixture {
	f := &helperFixture{iyuuFixture: newIyuuFixture(t), rows: map[string]map[string]any{}}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.dir = dir
	mapping, _ := json.Marshal([]map[string]string{{"save_path": dir, "container_path": dir}})
	if _, err = f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE='["DownloaderHelper"]' WHERE KEY='UserInstalledPlugins';
	UPDATE CONFIG_SITE SET SIGNURL='https://pt.fixture.test/login';
	INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','plugin.DownloaderHelper','{"future":9007199254740993,"downloader.2.enable_tagging":true}')`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec("UPDATE DOWNLOADER SET DOWNLOAD_DIR=? WHERE ID=1", string(mapping)); err != nil {
		t.Fatal(err)
	}
	f.restart()
	return f
}
func (f *helperFixture) task(name, state, tags string) string {
	h := fmt.Sprintf("%040x", len(f.rows)+1)
	f.rows[h] = map[string]any{"hash": h, "name": name, "state": state, "progress": 1, "amount_left": 0, "added_on": 100, "tags": tags, "save_path": f.dir, "tracker": "https://tracker.fixture.test/announce?passkey=fixture-private-token", "auto_tmm": false}
	return h
}
func (f *helperFixture) save(v map[string]any) {
	const path = "/api/v1/plugins/DownloaderHelper"
	reply := f.request("GET", path, nil, 200)
	var detail struct{ Data pluginConfigDetail }
	if json.Unmarshal(reply.Body.Bytes(), &detail) != nil {
		f.t.Fatal("invalid config")
	}
	f.request("PUT", path, map[string]any{"fingerprint": detail.Data.Meta.Fingerprint, "values": v}, 200)
}
func (f *helperFixture) state() helperSchedule {
	f.t.Helper()
	raw, err := f.helper.transfer.system.Get(f.t.Context(), helperControlKey)
	if err != nil {
		f.t.Fatal(err)
	}
	s, err := helperState(raw)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f *helperFixture) tick(now time.Time) {
	f.t.Helper()
	if err := f.helper.tick(f.t.Context(), now); err != nil {
		f.t.Fatal(err)
	}
}
func (f *helperFixture) event(kind, key string, data map[string]any) {
	f.t.Helper()
	err := f.helper.transfer.system.Transaction(f.t.Context(), func(tx *sql.Tx) error {
		return bus.EmitData(f.t.Context(), tx, kind, key, key, "fixture", "fixture", data)
	})
	if err != nil {
		f.t.Fatal(err)
	}
}
func TestDownloaderHelperOnceUsesExactTagsAndProtectsSharedFiles(t *testing.T) {
	f := newHelperFixture(t)
	seed := f.task("A.mkv", "stoppedUP", "站点/fixture, keep")
	hold := f.task("B.mkv", "stoppedUP", "skip")
	f.task("Gone.mkv", "missingFiles", "")
	shared := f.task("Shared.mkv", "missingFiles", "")
	f.rows[shared]["file"] = "A.mkv"
	f.save(map[string]any{"run_once": true, "enable_notify": true, "exclude_tags": "skip", "use_site_config": true, "downloader.1.enable": true, "downloader.1.enable_seeding": true, "downloader.1.enable_tagging": true, "downloader.1.enable_delete": true})
	s := f.state()
	f.tick(s.Once.Add(-time.Second))
	if f.starts != 0 {
		t.Fatal("early execution")
	}
	f.tick(s.Once)
	f.restart()
	f.tick(s.Once.Add(time.Minute))
	if f.starts != 1 || f.deletes != 1 || f.tags != 2 || f.rows[seed]["tags"] != "keep, 站点/Fixture Site" || f.rows[hold]["state"] != "stoppedUP" || f.rows[shared] == nil {
		t.Fatalf("wrong actions: %+v %d %d %d, %s", f.rows, f.starts, f.deletes, f.tags, f.state().Last)
	}
	w := f.request("GET", "/api/v1/plugins/DownloaderHelper/page", nil, 200)
	if !strings.Contains(w.Body.String(), "做种 1 个，打标 1 个，删除 1 个") || strings.Contains(w.Body.String(), "fixture-private-token") {
		t.Fatal(w.Body.String())
	}
	var n int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='plugin_message'").Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	raw, err := f.helper.transfer.system.Get(t.Context(), helperConfigKey)
	if err != nil || !strings.Contains(raw, "9007199254740993") || !strings.Contains(raw, "downloader.2.enable_tagging") {
		t.Fatal("legacy config lost")
	}
}
func TestDownloaderHelperEventsRestrictModeAndMatchActualSource(t *testing.T) {
	f := newHelperFixture(t)
	h := f.task("Renamed task", "stoppedUP", "")
	f.rows[h]["file"] = "Film/Movie.mkv"
	f.save(map[string]any{"enable": true, "listen_download_event": true, "listen_source_file_event": true, "downloader.1.enable": true, "downloader.1.enable_seeding": true, "downloader.1.enable_tagging": true, "downloader.1.enable_delete": true})
	f.event("download_start", "foreign", map[string]any{"downloader_id": 9})
	f.event("download_start", "mine", map[string]any{"downloader_id": 1})
	f.tick(time.Now().Add(time.Minute))
	if f.tags != 0 {
		t.Fatal("foreign downloader")
	}
	f.tick(time.Now().Add(time.Minute))
	if f.tags != 1 || f.starts != 0 || f.deletes != 0 {
		t.Fatal("download event ran other modes")
	}
	path := filepath.Join(f.dir, "Film")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	evt := map[string]any{"driver": "", "path": path, "filename": "Movie.mkv"}
	f.event("source_file_deleted", "part", evt)
	f.tick(time.Now().Add(time.Minute))
	if f.deletes != 0 {
		t.Fatal("deleted existing top-level directory")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f.event("source_file_deleted", "all", evt)
	f.tick(time.Now().Add(time.Minute))
	if f.deletes != 1 || f.starts != 0 {
		t.Fatal("wrong source deletion", f.state())
	}
}
func TestDownloaderHelperLostReplyAndReplacement(t *testing.T) {
	f := newHelperFixture(t)
	h := f.task("A", "stoppedUP", "")
	f.loseStart = true
	f.save(map[string]any{"enable": true, "run_once": true, "cron": "* * * * *", "downloader.1.enable": true, "downloader.1.enable_seeding": true})
	s := f.state()
	f.tick(s.Once)
	f.restart()
	f.tick(f.state().Next)
	if f.starts != 1 || !strings.Contains(f.state().Last, "待核实 1 项") {
		t.Fatal("replayed lost reply", f.state())
	}
	f.rows[h]["state"] = "uploading"
	f.tick(f.state().Next)
	f.rows[h]["state"] = "stoppedUP"
	f.loseStart = false
	f.tick(f.state().Next)
	if f.starts != 2 {
		t.Fatal("new pause after positive reconciliation was not handled")
	}
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET CONFIG='{"host":"other.fixture"}' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	f.tick(f.state().Next)
	if !f.state().Next.IsZero() {
		t.Fatal("replaced endpoint inherited old schedule")
	}
}

func TestDownloaderHelperUnselectedPeerSharesStorage(t *testing.T) {
	f := newHelperFixture(t)
	h := f.task("Missing.mkv", "missingFiles", "")
	mapping, _ := json.Marshal([]map[string]string{{"save_path": "/remote", "container_path": f.dir}})
	if _, err := f.db.Exec("INSERT INTO DOWNLOADER(ID,NAME,ENABLED,TYPE,CONFIG,DOWNLOAD_DIR) VALUES(2,'NAS peer',1,'transmission','{}',?)", string(mapping)); err != nil {
		t.Fatal(err)
	}
	f.save(map[string]any{"run_once": true, "downloader.1.enable": true, "downloader.1.enable_delete": true})
	f.tick(f.state().Once)
	if f.deletes != 0 || f.rows[h] == nil {
		t.Fatal("shared directory was deleted")
	}
	mapping, _ = json.Marshal([]map[string]string{{"save_path": "/remote", "container_path": f.dir + "-other"}})
	if _, err := f.db.Exec("UPDATE DOWNLOADER SET DOWNLOAD_DIR=? WHERE ID=2", string(mapping)); err != nil {
		t.Fatal(err)
	}
	f.save(map[string]any{"run_once": true})
	f.tick(f.state().Once)
	if f.deletes != 1 || f.rows[h] != nil {
		t.Fatal("unrelated peer prevented removal", f.state().Last)
	}
}

func TestDownloaderHelperWaitsForTransferCompletion(t *testing.T) {
	f := newHelperFixture(t)
	h := f.task("Transfer.mkv", "stalledUP", "")
	ctx := t.Context()
	f.save(map[string]any{"enable": true, "run_once": true, "listen_download_event": true, "downloader.1.enable": true, "downloader.1.enable_tagging": true})
	d, err := f.helper.transfer.downloaders.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	c, err := (&helperRun{api: f.helper}).client(d)
	if err != nil {
		t.Fatal(err)
	}
	l, err := torrenttransfer.NewLedger(ctx, f.db)
	if err != nil {
		t.Fatal(err)
	}
	id := torrenttransfer.Hash("transfer fixture")
	k := torrenttransfer.ActionKey{Receiver: torrenttransfer.Hash(c.Address()), WireID: h, Action: "add"}
	proof := torrenttransfer.Hash("scope")
	if _, _, err = l.Claim(ctx, k, id, proof); err != nil {
		t.Fatal(err)
	}
	if err = l.Finish(ctx, k, id, proof, "acknowledged"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec("INSERT INTO GO_TORRENT_TRANSFER_PLANS VALUES(?, '{}')", id); err != nil {
		t.Fatal(err)
	}
	f.tick(f.state().Once)
	if f.tags != 0 {
		t.Fatal("modified transfer during verification")
	}
	for range 2 {
		if err := l.CompleteTarget(ctx, id, 1); err != nil {
			t.Fatal(err)
		}
	}
	f.tick(time.Now().Add(time.Minute))
	var n int
	if err = f.db.QueryRow("SELECT COUNT(*) FROM GO_DOWNLOADER_HELPER_EVENTS").Scan(&n); err != nil || n != 1 || f.tags != 1 {
		t.Fatal(n, f.tags, err, f.state().Last)
	}
}
