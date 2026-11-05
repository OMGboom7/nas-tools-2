//go:build darwin || linux

package httpserver

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestBackupRestoreStagesNativeHTTPWithoutApplying(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := []byte("app:\n  login_user: admin\n  login_password: original-password\nsecurity:\n  api_key: original-key\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	handler, err := newHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: path}, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	admin := loginForTest(t, handler, "admin", "original-password")
	create := performFormRequest(handler, "/api/v1/user/manage", admin, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"viewer-password"}, "pris": {""}})
	if create.Code != 200 {
		t.Fatal(create.Code, create.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "viewer-password")
	dbPath := filepath.Join(t.TempDir(), "fixture.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT);
	CREATE TABLE CONFIG_USERS(ID INTEGER PRIMARY KEY,NAME TEXT,PASSWORD TEXT,PRIS TEXT);`)
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	database, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	z := zip.NewWriter(&buffer)
	for name, data := range map[string][]byte{
		"user.db":               database,
		"default-category.yaml": []byte("movie: {}\n"),
		"config.yaml":           []byte("app:\n  login_user: from-backup\n  login_password: backup-password\nsecurity:\n  api_key: backup-key\n"),
	} {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	stage := func(route, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", route, bytes.NewReader(buffer.Bytes()))
		r.Header.Set("Content-Type", "application/zip")
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if r := stage("/api/v1/backup/restore", ""); r.Code != 401 {
		t.Fatal(r.Code)
	}
	if r := stage("/api/v1/backup/restore", viewer); r.Code != 403 {
		t.Fatal(r.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, ".nastool-restore")); !os.IsNotExist(err) {
		t.Fatal("unapproved upload wrote a stage", err)
	}
	for _, route := range []string{"/api/v1/backup/restore", "/api/v1/config/restore"} {
		r := stage(route, admin)
		var out struct{ Data backuprestore.Stage }
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil || out.Data.ID == "" || len(out.Data.Files) != 3 ||
			r.Header().Get("Cache-Control") != "private, no-store" || strings.Contains(r.Body.String(), "backup-password") {
			t.Fatal(r.Code, r.Body.String())
		}
		get := nativeJSONRequest(handler, "GET", "/api/v1/backup/restore/"+out.Data.ID, admin, "")
		if get.Code != 200 || !strings.Contains(get.Body.String(), out.Data.ID) {
			t.Fatal(get.Code, get.Body.String())
		}
	}
	if r := performFormRequest(handler, "/api/v1/config/restore", admin, url.Values{"file_name": {"../config.yaml"}}); r.Code != 415 {
		t.Fatal(r.Code)
	}
	if r := nativeJSONRequest(handler, "POST", "/do", admin, `{"cmd":"restory_backup","data":{"file_name":"x.zip"}}`); r.Code != 410 {
		t.Fatal(r.Code)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, original) {
		t.Fatal("staging overwrote live configuration", err)
	}
}
