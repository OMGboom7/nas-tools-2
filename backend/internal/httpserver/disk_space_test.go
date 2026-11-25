//go:build darwin || linux

package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestManagedDiskSpaceConsumesOldRequestWithoutOtherNativeWorkers(t *testing.T) {
	_, _, path := nativeServicesFixture(t, "", nil, nil)
	cfg := config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(path), "config.yaml"), LegacyBackendURL: "http://127.0.0.1:3002", ManagedDiskSpace: true}
	noFlask := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("disk space operation reached Flask: %s", r.URL.Path)
		return nil, fmt.Errorf("unexpected backend request")
	})
	media := t.TempDir()
	for _, filename := range []string{"Movie.mkv", "Copy.mkv"} {
		if err := os.WriteFile(filepath.Join(media, filename), []byte("same contents"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	settings, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer settings.Close()
	opts, _ := json.Marshal(map[string]any{"run_now": true, "dry_run": true, "file_size": 0, "path_list": media, "ext_list": ".mkv"})
	if err := settings.Set(t.Context(), diskConfigKey, string(opts)); err != nil {
		t.Fatal(err)
	}
	if err := settings.Set(t.Context(), "UserInstalledPlugins", `["DiskSpaceSaver"]`); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
	defer stop()
	h, join, err := newRuntimeHandler(ctx, cfg, noFlask)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(); join() }()
	admin := loginForTest(t, h, "admin", "password")
	var run diskRun
	for run.Last == "" {
		raw, err := settings.Get(ctx, diskControlKey)
		if err != nil {
			t.Fatal(err)
		}
		if run, err = readDiskRun(raw); err != nil {
			t.Fatal(err)
		}
		if run.Last == "" {
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("mixed-mode worker did not finish:", ctx.Err())
			}
		}
	}
	stop()
	join()
	if run.ID == "" || !strings.Contains(run.Last, "查重候选 1 项") {
		t.Fatal(run)
	}
	first, err := os.Stat(filepath.Join(media, "Movie.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(filepath.Join(media, "Copy.mkv"))
	if err != nil || os.SameFile(first, second) {
		t.Fatal("dry run changed duplicate file", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var unrelated int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='GO_ORGANIZATION_JOBS'").Scan(&unrelated); err != nil || unrelated != 0 {
		t.Fatal("mixed-mode disk worker opened the organization journal", unrelated, err)
	}
	h, err = newHandler(cfg, noFlask)
	if err != nil {
		t.Fatal(err)
	}
	const base = "/api/v1/plugins/DiskSpaceSaver"
	for _, endpoint := range []string{base, base + "/page"} {
		w := nativeJSONRequest(h, "GET", endpoint, admin, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), media) {
			t.Fatal(endpoint, w.Code, w.Body.String())
		}
	}
	if w := performFormRequest(h, "/api/v1/user/manage", admin, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	viewer := loginForTest(t, h, "viewer", "strong-password")
	list := performFormRequest(h, "/api/v1/plugin/list", viewer, nil)
	if list.Code != 200 || !strings.Contains(list.Body.String(), diskID) || strings.Contains(list.Body.String(), media) {
		t.Fatal(list.Code, list.Body.String())
	}
	if w := nativeJSONRequest(h, "GET", base+"/page", viewer, ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestDiskSpacePluginImportsOnceAndPreservesFilesAndSettings(t *testing.T) {
	f := newIyuuFixture(t)
	base := t.TempDir()
	dir := filepath.Join(base, "media")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Movie.mkv", "Copy.mkv"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("movie bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cache := filepath.Join(base, "original-cache.json")
	if err := os.WriteFile(cache, []byte(`{"file_info":[{"filePath":"old-file","fileSize":42,"fileModifyTime":"old","fileSha1":"legacy"}],"inode_info":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"path_list": dir, "file_size": 0, "ext_list": ".mkv", "run_now": true, "dry_run": true, "fast": true, "result_path": cache, "future": json.RawMessage("9007199254740993")})
	if _, err := f.db.Exec("UPDATE SYSTEM_DICT SET VALUE='[\"DiskSpaceSaver\"]' WHERE KEY='UserInstalledPlugins'; INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','plugin.DiskSpaceSaver',?)", string(cfg)); err != nil {
		t.Fatal(err)
	}
	restart := func() *diskSpaceAPI {
		var runner *rssRunAPI
		h, err := buildHandler(config.Config{ApplicationConfigPath: f.config, DisableLegacy: true}, nil, &runner)
		if err != nil {
			t.Fatal(err)
		}
		f.handler = h
		f.token = loginForTest(t, h, "admin", "password")
		return runner.archivePlugin.disk
	}
	api := restart()
	tick := func() {
		if err := api.tick(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	tick()
	stat := func(n string) string {
		v, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		return organization.Identity(v)
	}
	if stat("Movie.mkv") == stat("Copy.mkv") {
		t.Fatal("dry run modified duplicate")
	}
	const path = "/api/v1/plugins/DiskSpaceSaver"
	if b := f.request("GET", path+"/page", nil, 200).Body.String(); !strings.Contains(b, "仅查重") || !strings.Contains(b, "查重候选 1 项") {
		t.Fatal(b)
	}
	var detail struct{ Data pluginConfigDetail }
	if err := json.Unmarshal(f.request("GET", path, nil, 200).Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	save := map[string]any{"fingerprint": detail.Data.Meta.Fingerprint, "values": map[string]any{"dry_run": false, "run_now": true}}
	f.request("PUT", path, save, 200)
	api = restart()
	tick()
	tick()
	if stat("Movie.mkv") != stat("Copy.mkv") {
		t.Fatal("real files did not share storage")
	}
	api = restart()
	if b := f.request("GET", path+"/page", nil, 200).Body.String(); !strings.Contains(b, "已替换 1 项") {
		t.Fatal(b)
	}
	raw, err := api.store.Get(t.Context(), diskConfigKey)
	if err != nil || !strings.Contains(raw, "9007199254740993") || !strings.Contains(raw, `"run_now":false`) {
		t.Fatal(raw, err)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(f.config), "plugins", diskID, "sha1.json"))
	if err != nil || !strings.Contains(string(b), "old-file") {
		t.Fatal(string(b), err)
	}
	// Use the same cursor parameter as the React viewer, tied to the active job.
	state, err := api.store.Get(t.Context(), diskControlKey)
	if err != nil {
		t.Fatal(err)
	}
	job, err := readDiskRun(state)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 101; i++ {
		if _, err = f.db.Exec("INSERT INTO GO_DISK_SPACE_PAIRS(job,seq,pair,state) SELECT job,?,pair,state FROM GO_DISK_SPACE_PAIRS WHERE job=? AND seq=0", i, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	var page struct{ Data pluginPageData }
	if err := json.Unmarshal(f.request("GET", path+"/page", nil, 200).Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Data.NextCursor != job.ID+".100" || len(page.Data.Tables[0].Rows) != 100 {
		t.Fatal(page.Data.NextCursor)
	}
	if err := json.Unmarshal(f.request("GET", path+"/page?after="+page.Data.NextCursor, nil, 200).Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data.Tables[0].Rows) != 2 {
		t.Fatal(len(page.Data.Tables[0].Rows))
	}
	f.request("GET", fmt.Sprintf("%s/page?after=other.100", path), nil, 400)

}
