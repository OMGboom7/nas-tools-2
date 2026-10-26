package mediaserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func refreshQueueFixture(t *testing.T) (*RefreshStore, RefreshControl) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT)`); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"enable": true, "delay": "3", "private_cache": "retain-private-value"}
	c := RefreshControl{Epoch: strings.Repeat("a", 32), Actor: "admin", Server: "emby", ServerID: "server-1", ServerDigest: strings.Repeat("b", 64), AuthorityDigest: strings.Repeat("c", 64), PluginDigest: RefreshDigest(config), Created: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), Authorized: true, Delay: 3}
	for key, v := range map[string]any{"UserInstalledPlugins": []string{"LibraryRefresh"}, RefreshPluginKey: config, RefreshControlKey: c} {
		b, _ := json.Marshal(v)
		if _, err := db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, key, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	return NewRefreshStore(db), c
}
func refreshQueueDefinition(c RefreshControl) RefreshDefinition {
	p := RefreshPlan{Server: "emby", ServerID: "server-1", Targets: []RefreshTarget{{Kind: "item", ID: "first", Label: "First"}, {Kind: "item", ID: "second", Label: "Second"}}}
	return RefreshDefinition{Actor: "admin", RequestID: strings.Repeat("d", 32), Source: "refresh_media_server", Whole: true, Control: c, Items: []RefreshItem{}, Reviewed: &p}
}
func TestRefreshQueueReadonlyAbsentStoreDoesNotCreateTables(t *testing.T) {
	s, _ := refreshQueueFixture(t)
	if rows, err := s.List(t.Context(), false, time.Now()); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if _, err := s.Get(t.Context(), strings.Repeat("f", 64)); !errors.Is(err, ErrRefreshMissing) {
		t.Fatal(err)
	}
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'GO_MEDIA_REFRESH_%'`).Scan(&count)
	if count != 0 {
		t.Fatal("readonly lookup created runtime tables", count)
	}
}
func TestRefreshQueuePersistsExactIntentsAndNeverReclaimsInterruptedOrUncertainWork(t *testing.T) {
	s, c := refreshQueueFixture(t)
	d := refreshQueueDefinition(c)
	j, err := s.Create(t.Context(), d)
	if err != nil || j.State != "pending" {
		t.Fatal(j, err)
	}
	repeated, err := s.Create(t.Context(), d)
	if err != nil || repeated.ID != j.ID {
		t.Fatal(repeated, err)
	}
	j, err = s.Start(t.Context(), j.ID)
	if err != nil || j.State != "planning" {
		t.Fatal(j, err)
	}
	if rows, err := NewRefreshStore(s.db).List(t.Context(), true, time.Now().Add(time.Hour)); err != nil || len(rows) != 0 {
		t.Fatal("planning was reclaimed", rows, err)
	}
	if err := s.Plan(t.Context(), j.ID, *d.Reviewed); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(t.Context(), j.ID, "accepted", "not actually accepted"); err == nil {
		t.Fatal("pending actions reported accepted")
	}
	if err := s.Intent(t.Context(), j.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Intent(t.Context(), j.ID, 0); err == nil {
		t.Fatal("intent repeated")
	}
	if err := s.ActionResult(t.Context(), j.ID, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Intent(t.Context(), j.ID, 1); err != nil {
		t.Fatal(err)
	}
	if rows, err := NewRefreshStore(s.db).List(t.Context(), true, time.Now().Add(time.Hour)); err != nil || len(rows) != 0 {
		t.Fatal("running intent was reclaimed", rows, err)
	}
	if err := s.ActionResult(t.Context(), j.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(t.Context(), j.ID, "uncertain", "retained"); err != nil {
		t.Fatal(err)
	}
	j, err = NewRefreshStore(s.db).Get(t.Context(), j.ID)
	if err != nil || j.State != "uncertain" || j.Actions[0].State != "accepted" || j.Actions[1].State != "uncertain" {
		t.Fatal(j, err)
	}
	if rows, err := s.List(t.Context(), true, time.Now().Add(time.Hour)); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	b, _ := json.Marshal(j)
	for _, private := range []string{"private_cache", "AuthorityDigest", "ServerDigest", "PluginDigest", "retain-private-value"} {
		if strings.Contains(string(b), private) {
			t.Fatal("private definition leaked", private)
		}
	}
}
func TestRefreshQueueEveryIgnoredOrFailedWriteRollsBackTheWholeBoundary(t *testing.T) {
	for _, point := range []string{"job", "plan", "action", "intent", "finish"} {
		t.Run(point, func(t *testing.T) {
			s, c := refreshQueueFixture(t)
			d := refreshQueueDefinition(c)
			if err := func() error {
				tx, e := s.db.Begin()
				if e != nil {
					return e
				}
				defer tx.Rollback()
				if e = refreshSchema(t.Context(), tx); e != nil {
					return e
				}
				return tx.Commit()
			}(); err != nil {
				t.Fatal(err)
			}
			if point == "job" {
				s.db.Exec(`CREATE TRIGGER reject_job BEFORE INSERT ON GO_MEDIA_REFRESH_JOBS BEGIN SELECT RAISE(IGNORE);END`)
				if _, err := s.Create(t.Context(), d); err == nil {
					t.Fatal("ignored job accepted")
				}
				return
			}
			j, err := s.Create(t.Context(), d)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Start(t.Context(), j.ID); err != nil {
				t.Fatal(err)
			}
			switch point {
			case "plan":
				s.db.Exec(`CREATE TRIGGER reject_plan BEFORE UPDATE ON GO_MEDIA_REFRESH_JOBS WHEN NEW.STATE='running' BEGIN SELECT RAISE(IGNORE);END`)
			case "action":
				s.db.Exec(`CREATE TRIGGER reject_action BEFORE INSERT ON GO_MEDIA_REFRESH_ACTIONS WHEN NEW.ORDINAL=1 BEGIN SELECT RAISE(ABORT,'fixture');END`)
			}
			err = s.Plan(t.Context(), j.ID, *d.Reviewed)
			if point == "plan" || point == "action" {
				if err == nil {
					t.Fatal("plan boundary accepted")
				}
				old, e := s.Get(t.Context(), j.ID)
				if e != nil || old.State != "planning" || old.Plan != nil || len(old.Actions) != 0 {
					t.Fatal("partial plan persisted", old, e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if point == "intent" {
				s.db.Exec(`CREATE TRIGGER reject_intent BEFORE UPDATE ON GO_MEDIA_REFRESH_ACTIONS WHEN NEW.STATE='intent' BEGIN SELECT RAISE(IGNORE);END`)
				if err := s.Intent(t.Context(), j.ID, 0); err == nil {
					t.Fatal("ignored intent accepted")
				}
				return
			}
			for i := 0; i < 2; i++ {
				if err := s.Intent(t.Context(), j.ID, i); err != nil {
					t.Fatal(err)
				}
				if err := s.ActionResult(t.Context(), j.ID, i, true); err != nil {
					t.Fatal(err)
				}
			}
			s.db.Exec(`CREATE TRIGGER reject_finish BEFORE UPDATE ON GO_MEDIA_REFRESH_JOBS WHEN NEW.STATE='accepted' BEGIN SELECT RAISE(IGNORE);END`)
			if err := s.Finish(t.Context(), j.ID, "accepted", ""); err == nil {
				t.Fatal("ignored completion accepted")
			}
		})
	}
}
func TestRefreshQueueChangedAuthorityAndServerIdentityCannotAdoptOldJobs(t *testing.T) {
	s, c := refreshQueueFixture(t)
	d := refreshQueueDefinition(c)
	j, err := s.Create(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	c.Epoch = strings.Repeat("e", 32)
	b, _ := json.Marshal(c)
	s.db.Exec(`UPDATE SYSTEM_DICT SET VALUE=? WHERE KEY=?`, string(b), RefreshControlKey)
	if _, err := s.Start(t.Context(), j.ID); err == nil {
		t.Fatal("new grant adopted old job")
	}
	c = d.Control
	b, _ = json.Marshal(c)
	s.db.Exec(`UPDATE SYSTEM_DICT SET VALUE=? WHERE KEY=?`, string(b), RefreshControlKey)
	if _, err := s.Start(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	p := *d.Reviewed
	p.ServerID = "different-server"
	if err := s.Plan(t.Context(), j.ID, p); err == nil {
		t.Fatal("server identity changed")
	}
	if err := s.Plan(t.Context(), j.ID, *d.Reviewed); err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE SYSTEM_DICT SET VALUE='{"enable":true,"delay":"3","new_cache":true}' WHERE KEY=?`, RefreshPluginKey)
	if err := s.Intent(t.Context(), j.ID, 0); err == nil {
		t.Fatal("changed plugin passed final intent")
	}
}
func TestRefreshQueueAutomaticDelayIsPersistentAndOldEventsAreNotBackfilled(t *testing.T) {
	s, c := refreshQueueFixture(t)
	s.db.Exec(`CREATE TABLE GO_BUSINESS_EVENTS(ID TEXT PRIMARY KEY,KIND TEXT,CREATED TEXT)`)
	if _, err := s.db.Exec(`CREATE TABLE GO_ORGANIZATION_JOBS(ID TEXT PRIMARY KEY,DEFINITION TEXT,STATE TEXT)`); err != nil {
		t.Fatal(err)
	}
	eventTime := time.Now().UTC().Truncate(time.Second)
	items := []RefreshItem{refreshTestItem()}
	for i, created := range []time.Time{eventTime.Add(-2 * time.Hour), eventTime} {
		job := strings.Repeat([]string{"1", "2"}[i], 32)
		tx, _ := s.db.Begin()
		if _, err := tx.Exec(`INSERT INTO GO_ORGANIZATION_JOBS VALUES(?,'{}','active')`, job); err != nil {
			t.Fatal(err)
		}
		tx.Exec(`INSERT INTO GO_BUSINESS_EVENTS VALUES(?,'transfer_finished',?)`, RefreshDigest([]string{"transfer_finished", job}), created.Format(time.RFC3339Nano))
		if err := EnqueueTransferRefresh(t.Context(), tx, job, items); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.List(t.Context(), false, eventTime)
	if err != nil || len(all) != 1 || all[0].Source != "transfer_finished" {
		t.Fatal(all, err)
	}
	due, _ := time.Parse(time.RFC3339Nano, all[0].Due)
	if !due.Equal(eventTime.Add(time.Duration(c.Delay) * time.Second)) {
		t.Fatal(due)
	}
	for _, tc := range []struct {
		at time.Time
		n  int
	}{{due.Add(-time.Nanosecond), 0}, {due, 1}, {due.Add(time.Second), 1}} {
		rows, err := NewRefreshStore(s.db).List(t.Context(), true, tc.at)
		if err != nil || len(rows) != tc.n {
			t.Fatal(rows, err, tc)
		}
	}
}
func TestRefreshQueueStoppingPreservesAnInflightReceiptAndOriginalDefinition(t *testing.T) {
	s, c := refreshQueueFixture(t)
	d := refreshQueueDefinition(c)
	j, err := s.Create(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	s.Start(t.Context(), j.ID)
	s.Plan(t.Context(), j.ID, *d.Reviewed)
	s.Intent(t.Context(), j.ID, 0)
	if err := s.Finish(t.Context(), j.ID, "stopped", "explicit stop"); err != nil {
		t.Fatal(err)
	}
	if err := s.ActionResult(t.Context(), j.ID, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(t.Context(), j.ID, "accepted", "late result"); err != nil {
		t.Fatal(err)
	}
	out, err := s.Get(t.Context(), j.ID)
	if err != nil || out.State != "stopped" || out.Actions[0].State != "accepted" || out.Fingerprint != j.Fingerprint {
		t.Fatal(out, err)
	}
	if err := s.Intent(t.Context(), j.ID, 1); err == nil {
		t.Fatal("stop allowed next request")
	}
}
