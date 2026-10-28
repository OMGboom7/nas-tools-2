package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func hookFixture(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT)`); err != nil {
		t.Fatal(err)
	}
	return db, New(db)
}
func hookSetting(t *testing.T, db *sql.DB, key string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM SYSTEM_DICT WHERE KEY=?`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig',?,?)`, key, string(raw)); err != nil {
		t.Fatal(err)
	}
}
func hookGrant(t *testing.T, db *sql.DB, address, method, encoding, epoch string) {
	t.Helper()
	values := map[string]any{"webhook_url": address, "method": method, "get_encoding": encoding, "enable": true, "extension": "retained"}
	hookSetting(t, db, "UserInstalledPlugins", []string{"Other", EventWebhookPlugin})
	hookSetting(t, db, EventWebhookKey, values)
	hookSetting(t, db, EventWebhookControlKey, EventWebhookControl{Epoch: epoch, Actor: "admin", Authority: Hash("test authority"), ConfigDigest: Hash(values), Created: time.Now().UTC().Format(time.RFC3339Nano), Authorized: true, HTTP: strings.HasPrefix(address, "http://")})
}
func hookPublish(t *testing.T, db *sql.DB, kind, key string, data any) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := PublishSignal(t.Context(), tx, kind, key, "domain:"+key, data); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func TestEventWebhookClosedGrantHasNoQueueOrHistoryBackfill(t *testing.T) {
	db, s := hookFixture(t)
	hookSetting(t, db, "UserInstalledPlugins", []string{EventWebhookPlugin})
	hookSetting(t, db, EventWebhookKey, map[string]any{"webhook_url": "https://old.test/secret", "method": "post"})
	hookPublish(t, db, "subscribe.add", "old", map[string]any{"rssid": 1})
	if rows, err := s.EventWebhooks(t.Context(), 1, false); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_EVENT_WEBHOOK_OUTBOX'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatal(tables, err)
	}
	hookGrant(t, db, "https://receiver.test/private?token=hidden", "post", "legacy", strings.Repeat("a", 32))
	hookPublish(t, db, "subscribe.add", "new", map[string]any{"rssid": 2})
	rows, err := s.EventWebhooks(t.Context(), 1, false)
	if err != nil || len(rows) != 1 || rows[0].Reference != "domain:new" {
		t.Fatal(rows, err)
	}
	raw, _ := json.Marshal(rows)
	for _, secret := range []string{"receiver.test", "hidden", "extension", "definition", "configDigest", "targetDigest", "fingerprint"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("receipt exposed sender context", string(raw))
		}
	}
}
func TestEventWebhookAllLegacySignalTypesKeepDataAndDeduplicate(t *testing.T) {
	db, s := hookFixture(t)
	hookGrant(t, db, "https://receiver.test/", "post", "legacy", strings.Repeat("a", 32))
	kinds := []string{"emby.webhook", "jellyfin.webhook", "plex.webhook", "download.add", "download.fail", "transfer.finished", "transfer.fail", "subtitle.download", "subscribe.add", "subscribe.finished", "message.incoming", "search.start", "sourcefile.deleted", "libraryfile.deleted", "media.scrap.start", "plugin.reload", "douban.sync", "autoseed.start", "refresh.mediaserver", "site.signin"}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			data := json.RawMessage(`{"integer":9007199254740993,"nested":{"path":"/library/电影.mkv"},"keys":["a","b"]}`)
			hookPublish(t, db, kind, kind, data)
			hookPublish(t, db, kind, kind, data)
			id := Hash([]string{strings.Repeat("a", 32), Hash([]string{kind, kind})})
			p, err := s.EventWebhook(t.Context(), id)
			if err != nil || p.State != "pending" || !strings.Contains(string(p.Definition.Envelope.Data), "9007199254740993") {
				t.Fatal(p, err)
			}
		})
	}
	rows, err := s.EventWebhooks(t.Context(), 1, false)
	if err != nil || len(rows) != 20 {
		t.Fatal(len(rows), err)
	}
	hookPublish(t, db, "download.add", "new-after-full", map[string]any{"new": true})
	if _, err := s.EventWebhook(t.Context(), rows[19].ID); err != nil {
		t.Fatal("stable lookup lost older receipt", err)
	}
}
func TestEventWebhookDomainRollbackAndSilentQueueInsertAreAtomic(t *testing.T) {
	db, s := hookFixture(t)
	hookGrant(t, db, "https://receiver.test/", "post", "legacy", strings.Repeat("a", 32))
	if _, err := db.Exec(`CREATE TABLE DOMAIN_FACT(ID TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO DOMAIN_FACT VALUES('rollback')`); err != nil {
		t.Fatal(err)
	}
	if err := PublishSignal(t.Context(), tx, "transfer.finished", "rollback", "job", map[string]any{"file": "one"}); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if rows, err := s.EventWebhooks(t.Context(), 1, false); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	hookPublish(t, db, "subscribe.add", "schema", map[string]any{})
	if _, err := db.Exec(`CREATE TRIGGER DROP_HOOK BEFORE INSERT ON GO_EVENT_WEBHOOK_OUTBOX WHEN NEW.REFERENCE='job' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tx.Exec(`INSERT INTO DOMAIN_FACT VALUES('ignored')`)
	if err := PublishSignal(t.Context(), tx, "transfer.finished", "ignored", "job", map[string]any{"file": "one"}); err == nil {
		t.Fatal("ignored queue write reported accepted")
	}
	tx.Rollback()
	var facts int
	db.QueryRow(`SELECT COUNT(*) FROM DOMAIN_FACT`).Scan(&facts)
	if facts != 0 {
		t.Fatal("domain escaped rollback", facts)
	}
}
func TestEventWebhookBusinessEventsSnapshotTypedDataOnce(t *testing.T) {
	db, s := hookFixture(t)
	hookGrant(t, db, "https://receiver.test/", "post", "legacy", strings.Repeat("a", 32))
	tx, _ := db.BeginTx(t.Context(), nil)
	if err := EmitData(t.Context(), tx, "download_start", "accepted", "download:1", "accepted", "not finished", map[string]any{"media_info": map[string]any{"title": "Movie"}, "downloader_id": 3}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := s.EventWebhooks(t.Context(), 1, false)
	if err != nil || len(rows) != 1 || rows[0].Type != "download.add" {
		t.Fatal(rows, err)
	}
	var data map[string]any
	json.Unmarshal(rows[0].Definition.Envelope.Data, &data)
	if data["downloader_id"] != float64(3) || data["native_event"].(map[string]any)["stage"] != "committed" {
		t.Fatal(data)
	}
	tx, _ = db.BeginTx(t.Context(), nil)
	if err := EmitData(t.Context(), tx, "download_start", "accepted", "download:1", "accepted", "not finished", map[string]any{"media_info": map[string]any{"title": "Movie"}, "downloader_id": 3}); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	rows, _ = s.EventWebhooks(t.Context(), 1, false)
	if len(rows) != 1 {
		t.Fatal("domain replay created event")
	}
}
func TestEventWebhookScopeChangesBlockOriginalPending(t *testing.T) {
	for _, change := range []string{"stop", "target", "method", "uninstall", "reinstall", "authority"} {
		t.Run(change, func(t *testing.T) {
			db, s := hookFixture(t)
			hookGrant(t, db, "https://receiver.test/original", "post", "legacy", strings.Repeat("a", 32))
			hookPublish(t, db, "subscribe.add", "source", map[string]any{"rssid": 1})
			rows, _ := s.EventWebhooks(t.Context(), 1, true)
			authority := func(EventWebhookControl) error { return nil }
			switch change {
			case "stop":
				c, _, _, _ := s.EventWebhookScope(t.Context())
				c.Authorized = false
				hookSetting(t, db, EventWebhookControlKey, c)
			case "target":
				hookSetting(t, db, EventWebhookKey, map[string]any{"webhook_url": "https://new.test/", "method": "post"})
			case "method":
				hookSetting(t, db, EventWebhookKey, map[string]any{"webhook_url": "https://receiver.test/original", "method": "get"})
			case "uninstall":
				hookSetting(t, db, "UserInstalledPlugins", []string{"Other"})
			case "reinstall":
				hookGrant(t, db, "https://receiver.test/original", "post", "legacy", strings.Repeat("b", 32))
			case "authority":
				authority = func(EventWebhookControl) error { return errors.New("revoked") }
			}
			if _, _, err := s.ClaimEventWebhook(t.Context(), rows[0], authority); !errors.Is(err, ErrEventWebhook) {
				t.Fatal(err)
			}
			p, err := s.EventWebhook(t.Context(), rows[0].ID)
			if err != nil || p.State != "blocked" {
				t.Fatal(p, err)
			}
		})
	}
}
func TestEventWebhookClaimIsExclusiveAndRestartNeverReplaysIntent(t *testing.T) {
	db, s := hookFixture(t)
	hookGrant(t, db, "https://receiver.test/", "post", "legacy", strings.Repeat("a", 32))
	hookPublish(t, db, "download.add", "one", map[string]any{"title": "one"})
	rows, _ := s.EventWebhooks(t.Context(), 1, true)
	var claims atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.ClaimEventWebhook(t.Context(), rows[0], func(EventWebhookControl) error { return nil }); err == nil {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatal("duplicate claim", claims.Load())
	}
	reopened := New(db)
	pending, err := reopened.EventWebhooks(t.Context(), 1, true)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	p, err := reopened.EventWebhook(t.Context(), rows[0].ID)
	if err != nil || p.State != "intent" {
		t.Fatal(p, err)
	}
	if err := s.FinishEventWebhook(t.Context(), p, "uncertain", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishEventWebhook(t.Context(), p, "sent", 200); err == nil {
		t.Fatal("uncertain relabelled sent")
	}
	if err := s.DismissEventWebhook(t.Context(), p.ID, "admin", "verified with original receiver"); err != nil {
		t.Fatal(err)
	}
	p, _ = s.EventWebhook(t.Context(), p.ID)
	if p.State != "dismissed" || p.Actor != "admin" {
		t.Fatal(p)
	}
}
func TestEventWebhookIgnoredAndMutatedIntentWritesNeverPermitSend(t *testing.T) {
	for _, trigger := range []string{
		`CREATE TRIGGER BAD BEFORE UPDATE OF STATE ON GO_EVENT_WEBHOOK_OUTBOX BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER BAD AFTER UPDATE OF STATE ON GO_EVENT_WEBHOOK_OUTBOX BEGIN UPDATE GO_EVENT_WEBHOOK_OUTBOX SET STATE='pending' WHERE ID=NEW.ID; END`,
		`CREATE TRIGGER BAD AFTER UPDATE OF STATE ON GO_EVENT_WEBHOOK_OUTBOX BEGIN DELETE FROM SYSTEM_DICT WHERE KEY='GoEventWebhook'; END`,
	} {
		t.Run(trigger, func(t *testing.T) {
			db, s := hookFixture(t)
			hookGrant(t, db, "https://receiver.test/", "post", "legacy", strings.Repeat("a", 32))
			hookPublish(t, db, "subscribe.add", "one", map[string]any{})
			p, _ := s.EventWebhooks(t.Context(), 1, true)
			if _, err := db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.ClaimEventWebhook(t.Context(), p[0], func(EventWebhookControl) error { return nil }); err == nil {
				t.Fatal("invalid durable intent accepted")
			}
			original, err := s.EventWebhook(t.Context(), p[0].ID)
			if err != nil || original.State != "pending" {
				t.Fatal(original, err)
			}
		})
	}
}
func TestEventWebhookWirePOSTLegacyGETAndOptInJSON(t *testing.T) {
	for _, variant := range []struct{ method, encoding string }{{"post", "legacy"}, {"get", "legacy"}, {"get", "json"}} {
		t.Run(variant.method+variant.encoding, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("X-NASTool-Event-ID") == "" {
					t.Error("stable event identity missing")
				}
				user, pass, ok := r.BasicAuth()
				if !ok || user != "user" || pass != "secret" {
					t.Error("original URL credentials lost")
				}
				q := r.URL.Query()
				if q.Get("token") != "original" || q.Get("same") != "retained" {
					t.Error("original query changed")
				}
				if variant.method == "post" {
					if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
						t.Error(r.Method, r.Header)
					}
					var out struct {
						Type string
						Data map[string]json.RawMessage
					}
					raw, _ := io.ReadAll(r.Body)
					if json.Unmarshal(raw, &out) != nil || out.Type != "subscribe.add" || string(out.Data["rssid"]) != "9007199254740993" {
						t.Error("wire data lost", string(raw))
					}
				} else {
					if r.Method != "GET" || q.Get("type") != "subscribe.add" {
						t.Error(r.URL)
					}
					if variant.encoding == "legacy" {
						if strings.Join(q["data"], ",") != "media,rssid" {
							t.Error("Requests dictionary-key wire changed", q["data"])
						}
					} else {
						if !strings.Contains(q.Get("data"), "9007199254740993") {
							t.Error("JSON values lost", q)
						}
					}
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			db, s := hookFixture(t)
			address := strings.Replace(server.URL, "://", "://user:secret@", 1) + "/receive?token=original&same=retained"
			hookGrant(t, db, address, variant.method, variant.encoding, strings.Repeat("a", 32))
			hookPublish(t, db, "subscribe.add", "one", json.RawMessage(`{"rssid":9007199254740993,"media":{"title":"电影"}}`))
			rows, _ := s.EventWebhooks(t.Context(), 1, true)
			p, spec, err := s.ClaimEventWebhook(t.Context(), rows[0], func(EventWebhookControl) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			status, ok := SendEventWebhook(t.Context(), nil, spec, p)
			if !ok || status != 204 || calls.Load() != 1 {
				t.Fatal(status, ok, calls.Load())
			}
			if err := s.FinishEventWebhook(t.Context(), p, "sent", status); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestEventWebhookRedirectAndLostResponseNeverRetryOrFallback(t *testing.T) {
	for _, redirect := range []bool{true, false} {
		t.Run(map[bool]string{true: "redirect", false: "accepted_then_lost"}[redirect], func(t *testing.T) {
			var calls, other atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other.Add(1); w.WriteHeader(204) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if redirect {
					w.Header().Set("Location", target.URL)
					w.WriteHeader(307)
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
			}))
			defer server.Close()
			db, s := hookFixture(t)
			hookGrant(t, db, server.URL, "get", "legacy", strings.Repeat("a", 32))
			hookPublish(t, db, "download.add", "one", map[string]any{"media_info": map[string]any{}})
			rows, _ := s.EventWebhooks(t.Context(), 1, true)
			p, spec, err := s.ClaimEventWebhook(t.Context(), rows[0], func(EventWebhookControl) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			status, sent := SendEventWebhook(t.Context(), nil, spec, p)
			if sent || calls.Load() != 1 || other.Load() != 0 {
				t.Fatal(status, sent, calls.Load(), other.Load())
			}
			if err := s.FinishEventWebhook(t.Context(), p, "uncertain", status); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				pending, err := New(db).EventWebhooks(t.Context(), 1, true)
				if err != nil || len(pending) != 0 {
					t.Fatal(pending, err)
				}
			}
		})
	}
}
func TestEventWebhookConfigStrictTLSAndMalformedReceipt(t *testing.T) {
	for _, raw := range []string{`{"webhook_url":"file:///etc/passwd","method":"post"}`, `{"webhook_url":"https://receiver.test/#secret","method":"post"}`, `{"webhook_url":"https://receiver.test/?x=%ZZ","method":"get"}`, `{"method":"DELETE"}`, `{"method":true}`, `{"get_encoding":"unknown"}`} {
		if _, err := DecodeEventWebhookSpec(raw); err == nil {
			t.Fatal("invalid wire config", raw)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS accepted") }))
	defer server.Close()
	db, s := hookFixture(t)
	hookGrant(t, db, server.URL, "post", "legacy", strings.Repeat("a", 32))
	hookPublish(t, db, "subscribe.add", "one", map[string]any{})
	rows, _ := s.EventWebhooks(t.Context(), 1, true)
	p, spec, err := s.ClaimEventWebhook(t.Context(), rows[0], func(EventWebhookControl) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := SendEventWebhook(context.Background(), nil, spec, p); ok {
		t.Fatal("TLS verification disabled")
	}
	if _, err := db.Exec(`UPDATE GO_EVENT_WEBHOOK_OUTBOX SET DOCUMENT='{}' WHERE ID=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EventWebhook(t.Context(), p.ID); err == nil {
		t.Fatal("tampered receipt accepted")
	}
}
