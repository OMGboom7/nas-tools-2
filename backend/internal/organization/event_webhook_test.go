//go:build linux || darwin

package organization

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

func organizationHookGrant(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT)`); err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"webhook_url": "https://receiver.test/", "method": "post"}
	c := bus.EventWebhookControl{Epoch: strings.Repeat("a", 32), Actor: "admin", Authority: bus.Hash("authority"), ConfigDigest: bus.Hash(values), Created: time.Now().UTC().Format(time.RFC3339Nano), Authorized: true}
	for key, value := range map[string]any{"UserInstalledPlugins": []string{"Webhook"}, bus.EventWebhookKey: values, bus.EventWebhookControlKey: c} {
		raw, _ := json.Marshal(value)
		if _, err := s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, key, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
}
func TestEventWebhookActualTransferPublishesOriginalPathsOnceAndNoLateGrantAdoption(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "authorized", false: "enabled_after_success"}[enabled], func(t *testing.T) {
			d, e := copyFixture(t)
			s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if enabled {
				organizationHookGrant(t, s)
			}
			id, err := s.Create(t.Context(), Digest(d), d)
			if err != nil {
				t.Fatal(err)
			}
			if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
				t.Fatal(won, err)
			}
			_, err = Copy(t.Context(), d, e, TempName(id, 0), func(p Proof) error { return s.Prepare(t.Context(), id, 0, p) })
			if err != nil {
				t.Fatal(err)
			}
			job, err := s.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Complete(t.Context(), job, job.Items[0]); err != nil {
				t.Fatal(err)
			}
			if !enabled {
				organizationHookGrant(t, s)
			}
			if err := s.Complete(t.Context(), job, job.Items[0]); err != nil {
				t.Fatal(err)
			}
			rows, err := s.BusinessEvents().EventWebhooks(t.Context(), 1, false)
			if err != nil {
				t.Fatal(err)
			}
			if !enabled {
				if len(rows) != 0 {
					t.Fatal("late grant backfilled successful transfer", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].Type != "transfer.finished" || rows[0].Reference != id {
				t.Fatal(rows)
			}
			var payload map[string]any
			json.Unmarshal(rows[0].Definition.Envelope.Data, &payload)
			if payload["file"] != filepath.Join(d.SourceRoot, e.Source) || payload["dest"] != filepath.Join(d.TargetRoot, e.Target) || payload["in_path"] != d.SourceRoot || payload["target_path"] != d.TargetRoot {
				t.Fatal(payload)
			}
			if payload["media_info"].(map[string]any)["title"] != e.Title {
				t.Fatal(payload)
			}
		})
	}
}
