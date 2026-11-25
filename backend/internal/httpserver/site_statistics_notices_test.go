package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

func TestStatisticsNoticesCommitRecipientsAndResumeUnsentTemplate(t *testing.T) {
	dir := t.TempDir()
	path, dbPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "user.db")
	if err := os.WriteFile(path, []byte("app: {user_agent: fixture}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sites, err := siteconfig.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sites.Close()
	site, err := sites.Upsert(t.Context(), siteconfig.Site{Name: "Fixture", SignURL: "https://fixture.invalid", Include: "T", Cookie: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sitestats.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sends atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"custom":"fixture"}` || r.Header.Get("Authorization") != "fixture-token" {
			t.Errorf("invalid rendered notification")
		}
		sends.Add(1)
	}))
	defer webhook.Close()
	cfg, _ := json.Marshal(map[string]string{"url": webhook.URL, "method": "POST", "token": "fixture-token", "json_tpl": `{"custom":{{text|tojson}}}`})
	_, err = db.Exec(`CREATE TABLE MESSAGE_CLIENT(ID INTEGER PRIMARY KEY,NAME,TYPE,CONFIG,SWITCHS,INTERACTIVE,ENABLED,NOTE);
    INSERT INTO MESSAGE_CLIENT VALUES(1,'fixture','webhook',?,'["site_message","download_start"]',0,1,'')`, string(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot := sitestats.Snapshot{Site: "Fixture", URL: "https://fixture.invalid", Unread: 1, Upload: 1234}
	save := func(ref string) {
		t.Helper()
		if err := store.SaveScheduled(ctx, site, snapshot, time.Now(), ref, true); err != nil {
			t.Fatal(err)
		}
	}
	save("legacy-stats:first")
	save("legacy-stats:second")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = bus.Emit(ctx, tx, "download_start", "ordinary", "legacy-stats:unrelated", "Unrelated", "must stay pending"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	run := func(reply string) (string, error) {
		var out bytes.Buffer
		err := StatisticsRunCommand(ctx, path, "refresh", strings.NewReader("[]\n"+reply), &out)
		if strings.Contains(out.String(), "fixture-token") || strings.Contains(out.String(), webhook.URL) {
			t.Fatal("recipient data leaked to template renderer")
		}
		return out.String(), err
	}
	if data, err := run(""); !errors.Is(err, io.ErrUnexpectedEOF) || sends.Load() != 0 || !strings.Contains(data, `"kind":"render"`) {
		t.Fatal(data, err)
	}
	reply := `{"body":"{\"custom\":\"fixture\"}"}` + "\n"
	if data, err := run(reply); err != nil || sends.Load() != 1 || !strings.Contains(data, `"kind":"done"`) {
		t.Fatal(data, err)
	}
	if _, err := run(""); err != nil || sends.Load() != 1 {
		t.Fatal("replayed receipt", err)
	}
	var pending, sent int
	if err = db.QueryRow("SELECT SUM(STATE='pending'),SUM(STATE='sent') FROM GO_NOTIFICATION_DELIVERIES").Scan(&pending, &sent); err != nil || pending != 1 || sent != 1 {
		t.Fatal(pending, sent, err)
	}
}
