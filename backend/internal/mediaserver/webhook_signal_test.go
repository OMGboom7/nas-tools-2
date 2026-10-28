package mediaserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

func mediaSignalGrant(t *testing.T, s *WebhookStore) {
	t.Helper()
	values := map[string]any{"webhook_url": "https://receiver.test/", "method": "post"}
	c := bus.EventWebhookControl{Epoch: strings.Repeat("e", 32), Actor: "admin", Authority: bus.Hash("authority"), ConfigDigest: bus.Hash(values), Created: time.Now().UTC().Format(time.RFC3339Nano), Authorized: true}
	for key, value := range map[string]any{"UserInstalledPlugins": []string{"Webhook"}, bus.EventWebhookKey: values, bus.EventWebhookControlKey: c} {
		raw, _ := json.Marshal(value)
		if _, err := s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, key, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
}
func TestWebhookSignalLedgerOutboxAndNotificationAreOneAtomicAcceptance(t *testing.T) {
	db, s, c := webhookStoreFixture(t)
	if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	mediaSignalGrant(t, s)
	raw := json.RawMessage(`{"Event":"playback.start","Title":"Movie","Private":"retained"}`)
	digest, _ := WebhookPayloadDigest("emby", raw)
	p := webhookStoreReceipt(t, strings.Repeat("c", 32))
	if _, err := s.Receive(t.Context(), c, p, digest, func() error { return nil }, raw); err != nil {
		t.Fatal(err)
	}
	rows, err := bus.New(db).EventWebhooks(t.Context(), 1, false)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if _, err := db.Exec(`CREATE TRIGGER IGNORE_SIGNAL BEFORE INSERT ON GO_EVENT_WEBHOOK_OUTBOX BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	p.ID = strings.Repeat("d", 32)
	if _, err := s.Receive(t.Context(), c, p, digest, func() error { return nil }, raw); err == nil {
		t.Fatal("ignored subscriber queue accepted notification")
	}
	for _, table := range []string{"GO_MEDIA_WEBHOOK_RECEIPTS", "GO_MEDIA_WEBHOOK_SIGNALS", "GO_BUSINESS_EVENTS"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 1 {
			t.Fatal("partial callback acceptance", table, n, err)
		}
	}
	paused := json.RawMessage(`{"Event":"playback.pause","EventId":"unique","Private":"original"}`)
	pd, _ := WebhookPayloadDigest("emby", paused)
	if _, err := s.ReceiveSignal(t.Context(), c, p.ID, pd, paused, func() error { return nil }); err == nil {
		t.Fatal("ignored raw subscriber queue accepted")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM GO_MEDIA_WEBHOOK_SIGNALS`).Scan(&n)
	if n != 1 {
		t.Fatal("partial raw identity committed", n)
	}
}
func TestWebhookSignalIdentityAndRawDataAreStrictButNoNotificationIsInvented(t *testing.T) {
	db, s, c := webhookStoreFixture(t)
	if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"Event":"playback.pause","EventId":"one","Private":{"n":9007199254740993}}`)
	digest, _ := WebhookPayloadDigest("emby", raw)
	if id, err := WebhookSignalIdentity(raw); err != nil || id != "one" {
		t.Fatal(id, err)
	}
	id := strings.Repeat("c", 32)
	for i := 0; i < 2; i++ {
		duplicate, err := s.ReceiveSignal(t.Context(), c, id, digest, raw, func() error { return nil })
		if err != nil || duplicate != (i == 1) {
			t.Fatal(duplicate, err)
		}
	}
	for _, table := range []string{"GO_MEDIA_WEBHOOK_RECEIPTS", "GO_BUSINESS_EVENTS", "GO_EVENT_WEBHOOK_OUTBOX"} {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, table).Scan(&n)
		if n != 0 {
			t.Fatal("closed subscriber created state", table)
		}
	}
	mediaSignalGrant(t, s)
	if _, err := s.ReceiveSignal(t.Context(), c, id, digest, raw, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if rows, err := bus.New(db).EventWebhooks(t.Context(), 1, false); err != nil || len(rows) != 0 {
		t.Fatal("old raw callback backfilled", rows, err)
	}
	for _, bad := range []string{`{"EventId":true}`, `{"EventId":"a","EventId":"b"}`, `{}{}`} {
		if _, err := WebhookSignalIdentity([]byte(bad)); err == nil {
			t.Fatal("ambiguous raw callback identity", bad)
		}
	}
}

func TestWebhookSignalEarlierNotificationIdentityCannotBecomeNewOutgoingFact(t *testing.T) {
	db, s, c := webhookStoreFixture(t)
	if err := s.SetControl(t.Context(), WebhookControl{}, c, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"Event":"playback.start","Title":"Movie"}`)
	digest, _ := WebhookPayloadDigest("emby", raw)
	p := webhookStoreReceipt(t, strings.Repeat("c", 32))
	// Simulate the previous deployed receiver, before raw signal capture.
	if _, err := s.Receive(t.Context(), c, p, digest, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	mediaSignalGrant(t, s)
	if duplicate, err := s.ReceiveSignal(t.Context(), c, p.ID, digest, raw, func() error { return nil }); err != nil || !duplicate {
		t.Fatal(duplicate, err)
	}
	changed := json.RawMessage(`{"Event":"playback.pause","Title":"Movie"}`)
	changedDigest, _ := WebhookPayloadDigest("emby", changed)
	if _, err := s.ReceiveSignal(t.Context(), c, p.ID, changedDigest, changed, func() error { return nil }); err == nil {
		t.Fatal("old receiver identity adopted changed action")
	}
	if _, err := db.Exec(`UPDATE GO_BUSINESS_EVENT_DATA SET DATA='{}' WHERE EVENT_ID=?`, bus.Hash([]string{"mediaserver_message", p.ID})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReceiveSignal(t.Context(), c, p.ID, digest, raw, func() error { return nil }); err == nil {
		t.Fatal("damaged previous notification accepted as duplicate")
	}
	for _, table := range []string{"GO_MEDIA_WEBHOOK_SIGNALS", "GO_EVENT_WEBHOOK_OUTBOX"} {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, table).Scan(&n)
		if n != 0 {
			t.Fatal("earlier callback was backfilled", table)
		}
	}
}
