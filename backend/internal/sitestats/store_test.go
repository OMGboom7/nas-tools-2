package sitestats

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

func statsStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "user.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func runningJob(t *testing.T, s *Store, origin string) Job {
	t.Helper()
	j, _, e := s.Create(t.Context(), Job{ID: Hash(origin), Actor: "fixture-admin", Origin: origin, Fingerprint: "fixture", Authority: "private-authority", SiteIDs: []string{}})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Change(t.Context(), j.ID, func(v *Job) error { v.State = "running"; return nil }); e != nil {
		t.Fatal(e)
	}
	j.State = "running"
	return j
}
func fixtureSnapshot() Snapshot {
	return Snapshot{SeedDetailsKnown: true, Site: "fixture", URL: "https://fixture.invalid", Schema: "NexusPhp", Username: "fixture-user", Level: "VIP", Upload: 9007199254740993, Download: 1 << 30, Ratio: 2.5, Bonus: 1.25, Seeding: 1, SeedingSize: 1610612736, Seeds: [][2]int64{{3, 1610612736}}, Unread: 1, Messages: []Message{{Title: "Fixture notice", Content: "Fixture content", Date: "2026-11-03"}}}
}
func TestLegacyStatisticsTransactionPreservesUnknownDataAndPrecision(t *testing.T) {
	s := statsStore(t)
	ctx := t.Context()
	if _, e := s.db.Exec(`ALTER TABLE SITE_USER_INFO_STATS ADD COLUMN FUTURE BLOB`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(`INSERT INTO SITE_USER_INFO_STATS(URL,EXT_INFO,FUTURE)VALUES('https://fixture.invalid','{"big":9007199254740993,"null":null}',X'0001FF')`); e != nil {
		t.Fatal(e)
	}
	j := runningJob(t, s, "fixture-first")
	v := fixtureSnapshot()
	if e := s.Save(ctx, j, v, "2026-11-03", "2026-11-03 12:00:00", true); e != nil {
		t.Fatal(e)
	}
	rows, e := s.List(ctx, []string{v.URL})
	if e != nil || len(rows) != 1 || rows[0].Upload != 9007199254740993 {
		t.Fatal(rows, e)
	}
	var raw, blob string
	if e = s.db.QueryRow(`SELECT EXT_INFO,hex(FUTURE)FROM SITE_USER_INFO_STATS WHERE URL=?`, v.URL).Scan(&raw, &blob); e != nil || raw != `{"big":9007199254740993,"null":null}` || blob != "0001FF" {
		t.Fatal(raw, blob, e)
	}
	h, e := s.History(ctx, v.URL, 7)
	if e != nil || len(h) != 1 || h[0].Upload != v.Upload {
		t.Fatal(h, e)
	}
	seeds, e := s.Seeds(ctx, v.URL)
	if e != nil || len(seeds) != 1 || seeds[0][1] != 1610612736 {
		t.Fatal(seeds, e)
	}
	// A late failure must roll back current data, history, seeds and messages.
	if _, e = s.db.Exec(`CREATE TRIGGER fixture_abort BEFORE UPDATE ON SITE_USER_SEEDING_INFO BEGIN SELECT RAISE(ABORT,'fixture');END`); e != nil {
		t.Fatal(e)
	}
	v.Upload++
	v.SeedingSize++
	if e = s.Save(ctx, j, v, "2026-11-04", "changed", true); e == nil {
		t.Fatal("trigger ignored")
	}
	rows, _ = s.List(ctx, []string{v.URL})
	h, _ = s.History(ctx, v.URL, 7)
	if rows[0].Upload != 9007199254740993 || len(h) != 1 {
		t.Fatal("partial transaction", rows, h)
	}
}
func TestUnreadNotificationUsesOriginalSwitchAndIsAtomic(t *testing.T) {
	s := statsStore(t)
	ctx := t.Context()
	_, e := s.db.Exec(`CREATE TABLE MESSAGE_CLIENT(ID INTEGER PRIMARY KEY,NAME TEXT,TYPE TEXT,CONFIG TEXT,SWITCHS TEXT,INTERACTIVE INTEGER,ENABLED INTEGER,NOTE TEXT);INSERT INTO MESSAGE_CLIENT VALUES(9007199254740993,'fixture','webhook','{}','["site_message"]',0,1,'')`)
	if e != nil {
		t.Fatal(e)
	}
	j := runningJob(t, s, "notify")
	v := fixtureSnapshot()
	if e = s.Save(ctx, j, v, "2026-11-03", "fixture", true); e != nil {
		t.Fatal(e)
	}
	events, e := bus.New(s.db).List(ctx)
	if e != nil || len(events) != 1 || len(events[0].Deliveries) != 1 || events[0].Deliveries[0].ChannelID != 9007199254740993 || events[0].Kind != "site_message" {
		t.Fatal(events, e)
	}
	if e = s.Save(ctx, j, v, "2026-11-03", "fixture again", true); e != nil {
		t.Fatal(e)
	}
	events, _ = bus.New(s.db).List(ctx)
	if len(events) != 1 {
		t.Fatal("repeated unread event")
	}
}
func TestControlRevocationDedupAndCrashRecovery(t *testing.T) {
	s := statsStore(t)
	ctx := context.Background()
	c := Control{Enabled: true, Epoch: Hash("grant"), Authority: "fixture-authority", Fingerprint: "fixture", Actor: "fixture-admin"}
	if e := s.SetControl(ctx, c, ""); e != nil {
		t.Fatal(e)
	}
	read, e := s.Control(ctx)
	if e != nil || read.Authority != c.Authority {
		t.Fatal("private grant lost", read, e)
	}
	if e = s.SetControl(ctx, c, "stale"); e == nil {
		t.Fatal("stale grant accepted")
	}
	j := runningJob(t, s, "crash")
	same, old, e := s.Create(ctx, j)
	if e != nil || !old || same.ID != j.ID {
		t.Fatal("duplicate job", same, e)
	}
	if e = s.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	same, e = s.Get(ctx, j.ID)
	if e != nil || same.State != "unknown" || same.Authority != j.Authority {
		t.Fatal("crash not retained", same, e)
	}
	if e = s.Save(ctx, j, fixtureSnapshot(), "2026-11-03", "fixture", false); e == nil {
		t.Fatal("recovered job republished")
	}
	c.Enabled = false
	if e = s.SetControl(ctx, c, c.Epoch); e != nil {
		t.Fatal(e)
	}
	if e = s.Valid(ctx, read); e == nil {
		t.Fatal("revoked control valid")
	}
}
func TestHistoryUsesRecentDatesAndOriginalMinMaxDelta(t *testing.T) {
	s := statsStore(t)
	j := runningJob(t, s, "history")
	v := fixtureSnapshot()
	v.Upload = 100
	v.Download = 50
	for _, day := range []string{"2026-11-01", "2026-11-02", "2026-11-03"} {
		if e := s.Save(t.Context(), j, v, day, day, false); e != nil {
			t.Fatal(e)
		}
		v.Upload += 20
		v.Download += 10
	}
	rows, e := s.History(t.Context(), v.URL, 2)
	if e != nil || len(rows) != 2 || rows[0].Date != "2026-11-02" {
		t.Fatal(rows, e)
	}
	end, _ := time.Parse("2006-01-02", "2026-11-03")
	delta, e := s.Deltas(t.Context(), []string{v.URL}, 3, end)
	if e != nil || len(delta) != 1 || delta[0].Upload != 40 || delta[0].Download != 20 {
		t.Fatal(delta, e)
	}
	empty, e := s.Deltas(t.Context(), nil, 3, end)
	if e != nil || len(empty) != 0 {
		t.Fatal("unconfigured site leaked", empty, e)
	}
}
