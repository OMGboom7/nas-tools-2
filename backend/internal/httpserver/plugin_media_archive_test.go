package httpserver

import (
	"encoding/json"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMediaArchivePageAndConfirmedRemovalUseNativeRoutes(t *testing.T) {
	h, admin, path := nativeServicesFixture(t, "", nil, nil)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const id = "MediaLibraryArchive"
	const file = "归档_20261125120000.md"
	const api = "/api/v1/plugins/MediaLibraryArchive/page"
	const body = `{"recordId":"归档_20261125120000.md","confirmation":"归档_20261125120000.md"}`
	if w := nativeJSONRequest(h, "GET", api, admin, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["MediaLibraryArchive"]`); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(path), "archive_file", "media_library")
	if w := nativeJSONRequest(h, "GET", api, admin, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatal("empty listing created a directory", err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, file)
	if err := os.WriteFile(name, []byte("private-archive-text"), 0600); err != nil {
		t.Fatal(err)
	}
	if w := nativeJSONRequest(h, "DELETE", api+"/records", "", body); w.Code != 401 {
		t.Fatal(w.Code)
	}
	read := func() string {
		t.Helper()
		w := nativeJSONRequest(h, "GET", api, admin, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), root) || strings.Contains(w.Body.String(), "private-archive-text") {
			t.Fatal(w.Code, w.Body.String())
		}
		var out struct{ Data pluginPageData }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		a := out.Data.Tables[0].RowActions[0]
		if a.RecordID != file || len(a.Proof) != 64 || a.Confirmation != "typeRecordId" || out.Data.Tables[0].Rows[0][2] != "2026-11-25 12:00:00" {
			t.Fatal(out.Data)
		}
		return a.Proof
	}
	proof := read()
	old := performFormRequest(h, "/api/v1/plugin/page", admin, url.Values{"id": {id}})
	if old.Code != 200 || !strings.Contains(old.Body.String(), `"record_id":"`+file+`"`) || !strings.Contains(old.Body.String(), proof) {
		t.Fatal(old.Code, old.Body.String())
	}
	if err := os.WriteFile(name, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"id": {id}, "record_id": {file}, "action": {"delete"}, "confirmation": {file}, "proof": {proof}}
	if w := performFormRequest(h, "/api/v1/plugin/page/action", admin, form); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	form.Set("confirmation", "wrong")
	if w := performFormRequest(h, "/api/v1/plugin/page/action", admin, form); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	proof = read()
	if w := nativeJSONRequest(h, "DELETE", api+"/records", admin, strings.TrimSuffix(body, "}")+`,"proof":"`+proof+`"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := nativeJSONRequest(h, "DELETE", api+"/records", admin, body); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if err := os.WriteFile(name, []byte("legacy fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	form.Set("confirmation", file)
	form.Del("proof")
	if w := performFormRequest(h, "/api/v1/plugin/page/action", admin, form); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Lstat(name); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
