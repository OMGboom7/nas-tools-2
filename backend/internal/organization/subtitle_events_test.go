//go:build linux || darwin

package organization

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

func subtitleEventFixture(t *testing.T, metadata bool) (*Store, string, bus.EventWebhookReceipt) {
	t.Helper()
	d, e := copyFixture(t)
	d.ScraperEnabled = metadata
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	organizationHookGrant(t, s)
	if metadata {
		if _, err = s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,'{}')`, scraper.ConfigKey); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig','UserInstalledPlugins','["Webhook","ChineseSubFinder"]'),('SystemConfig','plugin.ChineseSubFinder','{"host":"https://subtitle.test","api_key":"private-key"}')`); err != nil {
		t.Fatal(err)
	}
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
		t.Fatal(won, err)
	}
	if _, err = Copy(t.Context(), d, e, TempName(id, 0), func(p Proof) error { return s.Prepare(t.Context(), id, 0, p) }); err != nil {
		t.Fatal(err)
	}
	job, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(t.Context(), job, job.Items[0]); err != nil {
		t.Fatal(err)
	}
	rows, err := s.BusinessEvents().EventWebhooks(t.Context(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		if p.Type == "subtitle.download" {
			return s, id, p
		}
	}
	t.Fatal("actual completed intake did not publish its subtitle event")
	return nil, "", bus.EventWebhookReceipt{}
}
func TestSubtitleIntakeBothConsumersWaitForTheirOriginalMetadataAndWakeWithoutReplay(t *testing.T) {
	s, job, event := subtitleEventFixture(t, true)
	child, e := s.Subtitles().Get(t.Context(), subtitles.IntakeID(job, 0))
	if e != nil {
		t.Fatal(e)
	}
	gate := func(tx *sql.Tx, r subtitles.Receipt) error {
		return SubtitleIntakeReady(t.Context(), tx, r.JobID, r.Ordinal, r.OriginDigest, nil, true)
	}
	eventGate := func(tx *sql.Tx, source bus.EventWebhookSource) error {
		return SubtitleIntakeReady(t.Context(), tx, source.Reference, source.Ordinal, source.Origin, &source)
	}
	authority := func(bus.EventWebhookControl) error { return nil }
	if _, _, e = s.Subtitles().Claim(t.Context(), child, gate); !errors.Is(e, bus.ErrEventWaiting) {
		t.Fatal("subtitle bypassed pending metadata", e)
	}
	if _, _, e = s.BusinessEvents().ClaimEventWebhook(t.Context(), event, authority, eventGate); !errors.Is(e, bus.ErrEventWaiting) {
		t.Fatal("event bypassed pending metadata", e)
	}
	if e = s.Subtitles().Wait(t.Context(), child); e != nil {
		t.Fatal(e)
	}
	if e = s.BusinessEvents().WaitEventWebhook(t.Context(), event); e != nil {
		t.Fatal(e)
	}
	if rows, e := s.Subtitles().List(t.Context(), true); e != nil || len(rows) != 0 {
		t.Fatal("waiting subtitle starves FIFO", rows, e)
	}
	for _, p := range mustHookRows(t, s) {
		if p.Type == "subtitle.download" && p.State != "pending" {
			t.Fatal("waiting made a sending intent", p)
		}
	}
	scrapes, e := s.Scrapes(t.Context(), true)
	if e != nil || len(scrapes) != 1 {
		t.Fatal(scrapes, e)
	}
	if e = s.ClaimScrape(t.Context(), scrapes[0]); e != nil {
		t.Fatal(e)
	}
	if e = s.FinishScrape(t.Context(), scrapes[0].ID, "completed", ""); e != nil {
		t.Fatal(e)
	}
	if rows, e := s.Subtitles().List(t.Context(), true); e != nil || len(rows) != 1 {
		t.Fatal("completion did not wake the original item", rows, e)
	}
	if _, _, e = s.Subtitles().Claim(t.Context(), child, gate); e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.BusinessEvents().ClaimEventWebhook(t.Context(), event, authority, eventGate); e != nil {
		t.Fatal(e)
	}
	if e = s.Subtitles().Finish(t.Context(), child.ID, "uncertain", ""); e != nil {
		t.Fatal(e)
	}
	current, e := s.BusinessEvents().EventWebhook(t.Context(), event.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.BusinessEvents().FinishEventWebhook(t.Context(), current, "uncertain", 0); e != nil {
		t.Fatal(e)
	}
	if rows, e := s.Subtitles().List(t.Context(), true); e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	current, e = s.BusinessEvents().EventWebhook(t.Context(), event.ID)
	if e != nil || current.State != "uncertain" {
		t.Fatal(current, e)
	}
}
func mustHookRows(t *testing.T, s *Store) []bus.EventWebhookReceipt {
	t.Helper()
	rows, e := s.BusinessEvents().EventWebhooks(t.Context(), 1, false)
	if e != nil {
		t.Fatal(e)
	}
	return rows
}
func TestSubtitleIntakeChangedOrMissingOriginalMetadataCannotBeAdopted(t *testing.T) {
	for _, change := range []string{"options", "source_missing", "unknown_output", "running"} {
		t.Run(change, func(t *testing.T) {
			s, job, event := subtitleEventFixture(t, true)
			query := `UPDATE GO_SCRAPE_JOBS SET OPTIONS='{"nfo":false}'`
			switch change {
			case "source_missing":
				query = `DELETE FROM GO_SUBTITLE_INTAKE_SOURCES`
			case "unknown_output":
				query = `UPDATE GO_SCRAPE_JOBS SET STATE='completed',OUTPUTS='[{"Name":"Movie.nfo","Scope":"media","State":"uncertain"}]'`
			case "running":
				query = `UPDATE GO_SCRAPE_JOBS SET STATE='running'`
			}
			if _, e := s.db.Exec(query); e != nil {
				t.Fatal(e)
			}
			child, e := s.Subtitles().Get(t.Context(), subtitles.IntakeID(job, 0))
			if e != nil {
				t.Fatal(e)
			}
			_, _, e = s.Subtitles().Claim(t.Context(), child, func(tx *sql.Tx, r subtitles.Receipt) error {
				return SubtitleIntakeReady(t.Context(), tx, r.JobID, r.Ordinal, r.OriginDigest, nil, true)
			})
			if change == "running" {
				if !errors.Is(e, bus.ErrEventWaiting) {
					t.Fatal(e)
				}
			} else if !errors.Is(e, subtitles.ErrState) {
				t.Fatal("changed source obtained an intent", e)
			}
			_, _, e = s.BusinessEvents().ClaimEventWebhook(t.Context(), event, func(bus.EventWebhookControl) error { return nil }, func(tx *sql.Tx, p bus.EventWebhookSource) error {
				return SubtitleIntakeReady(t.Context(), tx, p.Reference, p.Ordinal, p.Origin, &p)
			})
			if change == "running" {
				if !errors.Is(e, bus.ErrEventWaiting) {
					t.Fatal(e)
				}
			} else if !errors.Is(e, bus.ErrEventWebhook) {
				t.Fatal(e)
			}
		})
	}
}
func TestSubtitleEventSourceCheckedAgainInsideTheActualIntentTransaction(t *testing.T) {
	s, _, event := subtitleEventFixture(t, false)
	if _, e := s.db.Exec(`CREATE TRIGGER alter_original_subtitle_source AFTER UPDATE OF STATE ON GO_EVENT_WEBHOOK_OUTBOX WHEN NEW.STATE='intent' AND NEW.TYPE='subtitle.download' BEGIN UPDATE GO_ORGANIZATION_ITEMS SET REASON='changed during intent';END`); e != nil {
		t.Fatal(e)
	}
	_, _, e := s.BusinessEvents().ClaimEventWebhook(t.Context(), event, func(bus.EventWebhookControl) error { return nil }, func(tx *sql.Tx, p bus.EventWebhookSource) error {
		return SubtitleIntakeReady(t.Context(), tx, p.Reference, p.Ordinal, p.Origin, &p)
	})
	if !errors.Is(e, bus.ErrEventWebhook) {
		t.Fatal("post-intent source mutation was accepted", e)
	}
	current, e := s.BusinessEvents().EventWebhook(t.Context(), event.ID)
	if e != nil || current.State != "pending" {
		t.Fatal("failed intent escaped rollback", current, e)
	}
	var changed int
	if e = s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_ITEMS WHERE REASON!=''`).Scan(&changed); e != nil || changed != 0 {
		t.Fatal("domain mutation escaped same transaction", changed, e)
	}
}
