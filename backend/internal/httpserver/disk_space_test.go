//go:build darwin || linux

package httpserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

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
