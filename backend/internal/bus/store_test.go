package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func eventFixture(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE MESSAGE_CLIENT(ID INTEGER PRIMARY KEY,NAME TEXT,TYPE TEXT,CONFIG TEXT,SWITCHS TEXT,INTERACTIVE INTEGER,ENABLED INTEGER,NOTE TEXT);INSERT INTO MESSAGE_CLIENT VALUES(1,'first','webhook','{"url":"https://secret.invalid/token"}','["transfer_finished"]',0,1,'private note'),(2,'disabled','webhook','{}','["transfer_finished"]',0,0,''),(3,'other event','webhook','{}','["download_start"]',0,1,'')`); err != nil {
		t.Fatal(err)
	}
	return db, New(db)
}
func emitTestEvent(t *testing.T, db *sql.DB, key string) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Emit(t.Context(), tx, "transfer_finished", key, key, "Movie 已入库", "body"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func TestEventAtomicRollbackAndReadonlyEmptyStore(t *testing.T) {
	db, s := eventFixture(t)
	if events, err := s.List(t.Context()); err != nil || len(events) != 0 {
		t.Fatal(events, err)
	}
	if items, err := s.Pending(t.Context()); err != nil || len(items) != 0 {
		t.Fatal(items, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_BUSINESS_EVENTS'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Emit(t.Context(), tx, "transfer_finished", "rolledback", "job", "title", "text"); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_BUSINESS_EVENTS'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
func TestEventsSnapshotRecipientsDeduplicateAndNeverExposeCredentials(t *testing.T) {
	db, s := eventFixture(t)
	emitTestEvent(t, db, "one")
	if _, err := db.Exec(`INSERT INTO MESSAGE_CLIENT VALUES(4,'late','webhook','{}','["transfer_finished"]',0,1,'')`); err != nil {
		t.Fatal(err)
	}
	emitTestEvent(t, db, "one")
	events, err := s.List(t.Context())
	if err != nil || len(events) != 1 || len(events[0].Deliveries) != 1 || events[0].Deliveries[0].ChannelID != 1 {
		t.Fatal(events, err)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "secret.invalid") || strings.Contains(string(raw), "private note") || strings.Contains(string(raw), "configDigest") {
		t.Fatal(string(raw))
	}
	emitTestEvent(t, db, "two")
	events, err = s.List(t.Context())
	if err != nil || len(events) != 2 || len(events[0].Deliveries) != 2 {
		t.Fatal(events, err)
	}
}
func TestDeliveryClaimsFrozenConfigAndNeverRetriesSendingAfterRestart(t *testing.T) {
	db, s := eventFixture(t)
	emitTestEvent(t, db, "one")
	pending, err := s.Pending(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	e, c, err := s.Claim(t.Context(), pending[0])
	if err != nil || c.ID != 1 || e.Kind != "transfer_finished" || c.Config == "" {
		t.Fatal(e, c, err)
	}
	if _, _, err := s.Claim(t.Context(), pending[0]); err == nil {
		t.Fatal("duplicate claim")
	}
	reopened := New(db)
	if p, err := reopened.Pending(t.Context()); err != nil || len(p) != 0 {
		t.Fatal("sending replayed", p, err)
	}
	if err := s.Finish(t.Context(), pending[0].ID, "uncertain", "lost response"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Pending(t.Context()); err != nil || len(p) != 0 {
		t.Fatal(p, err)
	}
	if err := s.Finish(t.Context(), pending[0].ID, "sent", ""); err == nil {
		t.Fatal("uncertain relabelled sent")
	}
}
func TestDeliveryConfigChangesAndRemovalBlockRatherThanRetarget(t *testing.T) {
	for _, sql := range []string{`UPDATE MESSAGE_CLIENT SET CONFIG='{"url":"https://new.invalid"}' WHERE ID=1`, `UPDATE MESSAGE_CLIENT SET ENABLED=0 WHERE ID=1`, `UPDATE MESSAGE_CLIENT SET SWITCHS='[]' WHERE ID=1`, `DELETE FROM MESSAGE_CLIENT WHERE ID=1`} {
		t.Run(sql, func(t *testing.T) {
			db, s := eventFixture(t)
			emitTestEvent(t, db, "one")
			p, _ := s.Pending(t.Context())
			if _, err := db.Exec(sql); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Claim(t.Context(), p[0]); err == nil {
				t.Fatal("changed endpoint used")
			}
			events, err := s.List(t.Context())
			if err != nil || events[0].Deliveries[0].State != "blocked" {
				t.Fatal(events, err)
			}
		})
	}
}
func TestDeliveryDismissalPreservesBusinessEventsAndNoFalseSuccess(t *testing.T) {
	db, s := eventFixture(t)
	emitTestEvent(t, db, "one")
	p, _ := s.Pending(t.Context())
	if err := s.Dismiss(t.Context(), p[0].ID, "admin", ""); err == nil {
		t.Fatal("blank reason")
	}
	if err := s.Dismiss(t.Context(), p[0].ID, "admin", "reviewed"); err != nil {
		t.Fatal(err)
	}
	events, err := s.List(t.Context())
	if err != nil || len(events) != 1 || events[0].Deliveries[0].State != "dismissed" || events[0].Deliveries[0].Actor != "admin" {
		t.Fatal(events, err)
	}
	if _, _, err := s.Claim(context.Background(), p[0]); err == nil {
		t.Fatal("dismissed replayed")
	}
}
func TestEventTextBoundedWithoutInvalidUTF8(t *testing.T) {
	text := Bounded(strings.Repeat("中", 100)+"\x00", 20)
	if !json.Valid([]byte(`"`+text+`"`)) || len(text) > 20 {
		t.Fatal(text)
	}
}

func TestEventAndDeliveryIntentsCannotBeSilentlyIgnored(t *testing.T) {
	for _, target := range []string{"GO_BUSINESS_EVENTS", "GO_NOTIFICATION_DELIVERIES"} {
		t.Run(target, func(t *testing.T) {
			db, s := eventFixture(t)
			emitTestEvent(t, db, "seed")
			if _, err := db.Exec(`CREATE TRIGGER ignore_insert BEFORE INSERT ON ` + target + ` BEGIN SELECT RAISE(IGNORE); END`); err != nil {
				t.Fatal(err)
			}
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			err = Emit(t.Context(), tx, "transfer_finished", "ignored", "ignored", "title", "text")
			tx.Rollback()
			if err == nil {
				t.Fatal("ignored insert counted as durable event")
			}
			events, err := s.List(t.Context())
			if err != nil || len(events) != 1 {
				t.Fatal(events, err)
			}
		})
	}
	db, s := eventFixture(t)
	emitTestEvent(t, db, "one")
	p, _ := s.Pending(t.Context())
	if _, err := db.Exec(`CREATE TRIGGER ignore_claim BEFORE UPDATE ON GO_NOTIFICATION_DELIVERIES BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Claim(t.Context(), p[0]); err == nil {
		t.Fatal("sending intent was not saved")
	}
	if pending, err := s.Pending(t.Context()); err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
}

func TestEventPreservesLegacyNonzeroEnabledFlag(t *testing.T) {
	db, s := eventFixture(t)
	if _, err := db.Exec(`UPDATE MESSAGE_CLIENT SET ENABLED=2 WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	emitTestEvent(t, db, "legacy-enabled")
	p, err := s.Pending(t.Context())
	if err != nil || len(p) != 1 {
		t.Fatal(p, err)
	}
	if _, _, err := s.Claim(t.Context(), p[0]); err != nil {
		t.Fatal(err)
	}
}
