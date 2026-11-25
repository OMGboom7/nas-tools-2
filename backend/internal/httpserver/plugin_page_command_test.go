package httpserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestPluginPageCommandsReadEscapedHTMLAndDeleteExactHistory(t *testing.T) {
	h, admin, path := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected upstream %s", r.URL.Path)
		return nil, nil
	}))
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["DoubanRank","DoubanSync","MovieRandom","CustomReleaseGroups"]`); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS PLUGIN_HISTORY(ID INTEGER PRIMARY KEY, PLUGIN_ID TEXT, KEY TEXT, VALUE TEXT, DATE TEXT)`); err != nil {
		t.Fatal(err)
	}
	const key = "9007199254740993"
	const row = `{"id":9007199254740993,"name":"<b>Fixture & title</b>","state":"RSS","extras":"private-fixture"}`
	for _, id := range []string{"DoubanSync", "DoubanRank", "MovieRandom"} {
		if _, err = db.Exec(`INSERT INTO PLUGIN_HISTORY(PLUGIN_ID,KEY,VALUE) VALUES(?,?,?)`, id, key, row); err != nil {
			t.Fatal(err)
		}
	}
	run := func(token, cmd, data string, want int) map[string]any {
		t.Helper()
		w := nativeJSONRequest(h, "POST", "/do", token, `{"cmd":"`+cmd+`","data":`+data+`}`)
		var out map[string]any
		if w.Code != want || json.Unmarshal(w.Body.Bytes(), &out) != nil || strings.Contains(w.Body.String(), "private-fixture") {
			t.Fatal(w.Code, w.Body.String())
		}
		return out
	}
	const id = `{"id":"DoubanSync"}`
	run("", "get_plugin_page", id, 401)
	if w := performFormRequest(h, "/api/v1/user/manage", admin, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	viewer := loginForTest(t, h, "viewer", "strong-password")
	run(viewer, "get_plugin_page", id, 404)
	page := run(admin, "get_plugin_page", id, 200)
	html := text(page["content"])
	if !strings.Contains(html, `href="/plugins/DoubanSync"`) || !strings.Contains(html, "&lt;b&gt;Fixture &amp; title&lt;/b&gt;") || page["func"] != nil || strings.Contains(html, "<b>Fixture") {
		t.Fatal(html)
	}
	if none := run(admin, "get_plugin_page", `{"id":"CustomReleaseGroups"}`, 200); none["content"] != nil {
		t.Fatal(none)
	}
	run(admin, "get_plugin_page", `{"id":"DoubanSync","after":"bad"}`, 400)
	for _, tc := range []struct{ id, method, field, encoded string }{
		{"DoubanSync", "delete_sync_history", "douban_id", key},
		{"DoubanRank", "delete_rank_history", "tmdb_id", `"` + key + `"`},
		{"MovieRandom", "delete_random_history", "tmdb_id", key},
	} {
		input := `{"plugin_id":"` + tc.id + `","method":"` + tc.method + `","` + tc.field + `":` + tc.encoded + `}`
		run(viewer, "run_plugin_method", input, 403)
		run(admin, "run_plugin_method", strings.Replace(input, tc.method, "other", 1), 400)
		run(admin, "run_plugin_method", strings.Replace(input, tc.encoded, "1e4", 1), 400)
		out := run(admin, "run_plugin_method", input, 200)
		if result, ok := out["result"]; !ok || result != nil {
			t.Fatal(out)
		}
		run(admin, "run_plugin_method", input, 404)
	}
}

func TestArchivePageCommandUsesReviewedFileAndOriginalResultShape(t *testing.T) {
	h, admin, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Set(t.Context(), "UserInstalledPlugins", `["MediaLibraryArchive"]`); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(path), "archive_file", "media_library")
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	const name = "归档_20261125120000.md"
	file := filepath.Join(root, name)
	if err = os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		w := nativeJSONRequest(h, "POST", "/do", admin, `{"cmd":"get_plugin_page","data":{"id":"MediaLibraryArchive"}}`)
		var out struct{ Data pluginPageData }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Data.Tables) != 1 || len(out.Data.Tables[0].RowActions) != 1 {
			t.Fatal(w.Body.String())
		}
		return out.Data.Tables[0].RowActions[0].Proof
	}
	remove := func(proof, nameInput string, want int) {
		t.Helper()
		payload := map[string]any{"cmd": "run_plugin_method", "data": map[string]string{"plugin_id": "MediaLibraryArchive", "method": "remove_archive_file_for_api", "file_name": name, "confirmation": nameInput, "proof": proof}}
		b, _ := json.Marshal(payload)
		w := nativeJSONRequest(h, "POST", "/do", admin, string(b))
		if w.Code != want || want == 200 && !strings.Contains(w.Body.String(), `"result":{"code":0`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	proof := read()
	remove(proof, "", 400)
	remove("", name, 409)
	if err = os.WriteFile(file, []byte("replacement contents"), 0600); err != nil {
		t.Fatal(err)
	}
	remove(proof, name, 409)
	if b, err := os.ReadFile(file); err != nil || string(b) != "replacement contents" {
		t.Fatal(err, string(b))
	}
	remove(read(), name, 200)
	if _, err = os.Lstat(file); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, []byte("original client fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	old := `{"cmd":"run_plugin_method","data":{"plugin_id":"MediaLibraryArchive","method":"remove_archive_file_for_api","file_name":"` + name + `"}}`
	if w := nativeJSONRequest(h, "POST", "/do", "", old); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := nativeJSONRequest(h, "POST", "/do", admin, old); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = os.Lstat(file); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
