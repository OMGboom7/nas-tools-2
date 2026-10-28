package mediaserver

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	_ "modernc.org/sqlite"
)

func webhookStoreFixture(t *testing.T) (*sql.DB, *WebhookStore, WebhookControl) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s := NewWebhookStore(db)
	c := WebhookControl{Revision: strings.Repeat("a", 32), Enabled: true, Actor: "admin", Server: "emby", Scope: strings.Repeat("b", 64), Changed: "2026-10-27T12:00:00Z"}
	return db, s, c
}
func webhookStoreReceipt(t *testing.T, id string) WebhookReceipt {
	t.Helper()
	e, _, err := ParseWebhook("emby", []byte(`{"Event":"playback.start","Title":"Movie"}`))
	if err != nil {
		t.Fatal(err)
	}
	return WebhookReceipt{ID: id, Received: "2026-10-27T12:00:00Z", Event: e}
}
func TestWebhookStoreColdReadsNeverCreateTablesOrAuthority(t *testing.T) {
	db, s, _ := webhookStoreFixture(t)
	if c, err := s.Control(t.Context()); err != nil || c.Enabled || c.Revision != "" {
		t.Fatal(c, err)
	}
	if rows, err := s.List(t.Context(), 1); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if _, err := s.Get(t.Context(), strings.Repeat("d", 32)); !errors.Is(err, ErrWebhookMissing) {
		t.Fatal(err)
	}
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatal(tables, err)
	}
}
func TestWebhookReceiptNotificationRecipientsAndReceiverAuthorityAreAtomic(t *testing.T) {
	db, s, c := webhookStoreFixture(t)
	if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE MESSAGE_CLIENT(ID INTEGER PRIMARY KEY,NAME TEXT,TYPE TEXT,CONFIG TEXT,SWITCHS TEXT,INTERACTIVE INTEGER,ENABLED INTEGER,NOTE TEXT);INSERT INTO MESSAGE_CLIENT VALUES(1,'old','webhook','{}','["mediaserver_message"]',0,1,''),(2,'other','webhook','{}','["download_start"]',0,1,'')`); err != nil {
		t.Fatal(err)
	}
	p := webhookStoreReceipt(t, strings.Repeat("c", 32))
	digest := strings.Repeat("d", 64)
	p, err := s.Receive(t.Context(), c, p, digest, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO MESSAGE_CLIENT VALUES(3,'late','webhook','{}','["mediaserver_message"]',0,1,'')`); err != nil {
		t.Fatal(err)
	}
	again, err := s.Receive(t.Context(), c, p, digest, func() error { return nil })
	if err != nil || !again.Deduplicated || again.ID != p.ID {
		t.Fatal(again, err)
	}
	e, err := bus.New(db).Get(t.Context(), p.EventID)
	if err != nil || len(e.Deliveries) != 1 || e.Deliveries[0].ChannelID != 1 || len(e.Data) == 0 {
		t.Fatal(e, err)
	}
	if _, err := s.Receive(t.Context(), c, p, strings.Repeat("e", 64), func() error { return nil }); !errors.Is(err, ErrWebhookState) {
		t.Fatal("same identity adopted changed payload", err)
	}
	next := c
	next.Revision = strings.Repeat("f", 32)
	next.Enabled = false
	if err := s.SetControl(t.Context(), c, next, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	p2 := webhookStoreReceipt(t, strings.Repeat("e", 32))
	if _, err := s.Receive(t.Context(), c, p2, digest, func() error { return nil }); !errors.Is(err, ErrWebhookState) {
		t.Fatal("stopped authority received", err)
	}
	if saved, err := s.Get(t.Context(), p.ID); err != nil || saved.ID != p.ID {
		t.Fatal(saved, err)
	}
}
func TestWebhookStoreFinalValidationRollsBackCompleteReceiptAndBusinessEvent(t *testing.T) {
	for _, target := range []string{"receipt-ignore", "event-ignore", "data-ignore", "delivery-ignore", "event-tamper", "control-tamper", "receipt-tamper"} {
		t.Run(target, func(t *testing.T) {
			db, s, c := webhookStoreFixture(t)
			if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE MESSAGE_CLIENT(ID INTEGER PRIMARY KEY,NAME TEXT,TYPE TEXT,CONFIG TEXT,SWITCHS TEXT,INTERACTIVE INTEGER,ENABLED INTEGER,NOTE TEXT);INSERT INTO MESSAGE_CLIENT VALUES(1,'one','webhook','{}','["mediaserver_message"]',0,1,'')`); err != nil {
				t.Fatal(err)
			}
			first := webhookStoreReceipt(t, strings.Repeat("c", 32))
			if _, err := s.Receive(t.Context(), c, first, strings.Repeat("d", 64), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			statements := map[string]string{
				"receipt-ignore":  `CREATE TRIGGER fault BEFORE INSERT ON GO_MEDIA_WEBHOOK_RECEIPTS BEGIN SELECT RAISE(IGNORE); END`,
				"event-ignore":    `CREATE TRIGGER fault BEFORE INSERT ON GO_BUSINESS_EVENTS BEGIN SELECT RAISE(IGNORE); END`,
				"data-ignore":     `CREATE TRIGGER fault BEFORE INSERT ON GO_BUSINESS_EVENT_DATA BEGIN SELECT RAISE(IGNORE); END`,
				"delivery-ignore": `CREATE TRIGGER fault BEFORE INSERT ON GO_NOTIFICATION_DELIVERIES BEGIN SELECT RAISE(IGNORE); END`,
				"event-tamper":    `CREATE TRIGGER fault AFTER INSERT ON GO_BUSINESS_EVENT_DATA BEGIN UPDATE GO_BUSINESS_EVENTS SET BODY='changed' WHERE ID=NEW.EVENT_ID; END`,
				"control-tamper":  `CREATE TRIGGER fault AFTER INSERT ON GO_BUSINESS_EVENT_DATA BEGIN UPDATE SYSTEM_DICT SET VALUE='{}' WHERE KEY='GoMediaWebhooks'; END`,
				"receipt-tamper":  `CREATE TRIGGER fault AFTER INSERT ON GO_BUSINESS_EVENT_DATA BEGIN UPDATE GO_MEDIA_WEBHOOK_RECEIPTS SET DOCUMENT_FP='changed' WHERE ID='` + strings.Repeat("e", 32) + `'; END`,
			}
			if _, err := db.Exec(statements[target]); err != nil {
				t.Fatal(err)
			}
			p := webhookStoreReceipt(t, strings.Repeat("e", 32))
			if _, err := s.Receive(t.Context(), c, p, strings.Repeat("f", 64), func() error { return nil }); err == nil {
				t.Fatal("partial receipt committed")
			}
			for _, table := range []string{"GO_MEDIA_WEBHOOK_RECEIPTS", "GO_BUSINESS_EVENTS", "GO_BUSINESS_EVENT_DATA", "GO_NOTIFICATION_DELIVERIES"} {
				var n int
				if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 1 {
					t.Fatal(table, n, err)
				}
			}
			if current, err := s.Control(t.Context()); err != nil || current != c {
				t.Fatal(current, err)
			}
		})
	}
}
func TestWebhookStoreLateScopeChangeAndOldControlWriteCannotCommit(t *testing.T) {
	db, s, c := webhookStoreFixture(t)
	n := 0
	if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error {
		n++
		if n == 2 {
			return ErrWebhookState
		}
		return nil
	}); err == nil {
		t.Fatal("late change")
	}
	var tables int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&tables)
	if tables != 0 {
		t.Fatal("failed authorization created tables", tables)
	}
	if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error { return nil }); err == nil {
		t.Fatal("stale control request")
	}
	n = 0
	if _, err := s.Receive(t.Context(), c, webhookStoreReceipt(t, strings.Repeat("c", 32)), strings.Repeat("d", 64), func() error {
		n++
		if n == 2 {
			return ErrWebhookState
		}
		return nil
	}); err == nil {
		t.Fatal("late incoming scope change")
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_MEDIA_WEBHOOK_RECEIPTS'`).Scan(&count)
	if count != 0 {
		t.Fatal("failed incoming scope created receipt schema")
	}
}
func TestWebhookStoreCompletePagesAndStableOldEventLookup(t *testing.T) {
	db, s, c := webhookStoreFixture(t)
	if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	first := ""
	for i := 0; i < 61; i++ {
		id := RefreshDigest(i)[:32]
		if i == 0 {
			first = id
		}
		p := webhookStoreReceipt(t, id)
		if _, err := s.Receive(context.Background(), c, p, RefreshDigest(i), func() error { return nil }); err != nil {
			t.Fatal(i, err)
		}
	}
	for page, want := range map[int]int{1: 30, 2: 30, 3: 1, 4: 0} {
		rows, err := s.List(t.Context(), page)
		if err != nil || len(rows) != want {
			t.Fatal(page, len(rows), err)
		}
	}
	p, err := s.Get(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := bus.New(db).Get(t.Context(), p.EventID); err != nil || e.ID != p.EventID {
		t.Fatal(e, err)
	}
	if _, err := db.Exec(`UPDATE GO_MEDIA_WEBHOOK_RECEIPTS SET DOCUMENT='{}' WHERE ID=?`, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(t.Context(), first); !errors.Is(err, ErrWebhookState) {
		t.Fatal("tampered receipt adopted", err)
	}
}

func TestWebhookControlSilentIgnoredUpdateDoesNotCreateAnotherAuthority(t *testing.T) {
	db, s, c := webhookStoreFixture(t)
	if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fault BEFORE UPDATE ON SYSTEM_DICT BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	next := c
	next.Enabled = false
	next.Revision = strings.Repeat("d", 32)
	if err := s.SetControl(t.Context(), c, next, func() error { return nil }); !errors.Is(err, ErrWebhookState) {
		t.Fatal(err)
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM SYSTEM_DICT WHERE KEY='GoMediaWebhooks'`).Scan(&count)
	if saved, err := s.Control(t.Context()); err != nil || saved != c || count != 1 {
		t.Fatal(saved, count, err)
	}
}
func TestWebhookDuplicateCannotClaimAcceptanceAfterItsOriginalEventProofChanges(t *testing.T) {
	db, s, c := webhookStoreFixture(t)
	if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	p := webhookStoreReceipt(t, strings.Repeat("c", 32))
	p, err := s.Receive(t.Context(), c, p, strings.Repeat("d", 64), func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE GO_BUSINESS_EVENTS SET BODY='changed' WHERE ID=?`, p.EventID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Receive(t.Context(), c, p, strings.Repeat("d", 64), func() error { return nil }); !errors.Is(err, ErrWebhookState) {
		t.Fatal(err)
	}
}
