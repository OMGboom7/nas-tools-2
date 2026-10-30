package sitelogin

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

type fixture struct {
	s     *Store
	sites *siteconfig.Store
	site  siteconfig.Site
	r     Record
	b     Batch
	path  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "user.db")
	sites, err := siteconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sites.Close() })
	site, err := sites.Upsert(t.Context(), siteconfig.Site{Name: "Fixture", SignURL: "https://site.test/", Cookie: "original=private", Note: `{"ua":"original-agent","unknown":9007199254740993,"nested":{"n":9007199254740993}}`})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT)`, `ALTER TABLE CONFIG_SITE ADD COLUMN EXTRA BLOB`, `UPDATE CONFIG_SITE SET EXTRA=X'00FF01'`} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	source := Source{SiteID: site.ID, Site: Hash(site), Application: Hash("app"), Catalog: Hash("catalog"), Authority: Hash("root"), LoginURL: "https://site.test/login.php", UserAgent: "original-agent"}
	b := Batch{ID: Hash("batch"), Actor: "root", Created: now, Updated: now, Jobs: []string{Hash("job")}, RememberState: "not_requested"}
	r := Record{ID: b.Jobs[0], Batch: b.ID, Actor: b.Actor, Name: site.Name, Host: "https://site.test", State: "queued", Created: now, Updated: now, Source: source, Operations: []Operation{}}
	return &fixture{s: s, sites: sites, site: site, r: r, b: b, path: path}
}
func good(ctx context.Context) error { return ctx.Err() }
func (f *fixture) create(t *testing.T) {
	t.Helper()
	if _, repeat, err := f.s.Create(t.Context(), f.b, []Record{f.r}, good); err != nil || repeat {
		t.Fatal(err, repeat)
	}
}
func TestOriginalLoginJournalPromptPublicationAndReopen(t *testing.T) {
	f := newFixture(t)
	f.create(t)
	if _, repeat, err := f.s.Create(t.Context(), f.b, []Record{f.r}, good); err != nil || !repeat {
		t.Fatal("original batch did not read back", err)
	}
	if err := f.s.Start(t.Context(), f.r.ID, good); err != nil {
		t.Fatal(err)
	}
	binding := Hash(f.r.Source)
	action, err := f.s.Step(t.Context(), f.r.ID, binding, "action:credentials", Hash("command"), good)
	if err != nil {
		t.Fatal(err)
	}
	network, err := f.s.Step(t.Context(), f.r.ID, binding, "network:POST:Document", Hash("network"), good)
	if err != nil {
		t.Fatal("network could not follow an unacknowledged browser action", err)
	}
	if err = f.s.FinishStep(t.Context(), f.r.ID, binding, network, "received", Hash("response")); err != nil {
		t.Fatal(err)
	}
	if err = f.s.FinishStep(t.Context(), f.r.ID, binding, action, "received", Hash("click")); err != nil {
		t.Fatal(err)
	}
	nonce := Random()
	if err = f.s.Prompt(t.Context(), f.r.ID, "email_code", nonce, time.Now().Add(time.Minute), good); err != nil {
		t.Fatal(err)
	}
	if err = f.s.ConsumePrompt(t.Context(), f.r.ID, nonce, good); err != nil {
		t.Fatal(err)
	}
	if err = f.s.ConsumePrompt(t.Context(), f.r.ID, nonce, good); !errors.Is(err, ErrInput) {
		t.Fatal("input nonce reused", err)
	}
	if err = f.s.SaveCookie(t.Context(), f.r.ID, "session=verified", "new-agent", "acquired", good); err != nil {
		t.Fatal(err)
	}
	updated, err := f.sites.Get(t.Context(), f.site.ID)
	if err != nil || updated.Cookie != "session=verified" || !strings.Contains(updated.Note, "9007199254740993") || !strings.Contains(updated.Note, "new-agent") {
		t.Fatal(updated, err)
	}
	var blob string
	if err = f.s.db.QueryRow("SELECT hex(EXTRA) FROM CONFIG_SITE WHERE ID=?", f.site.ID).Scan(&blob); err != nil || blob != "00FF01" {
		t.Fatal(blob, err)
	}
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	record, err := reopened.Get(t.Context(), f.r.ID)
	if err != nil || record.State != "completed" || len(record.Operations) != 2 {
		t.Fatal(record.State, err)
	}
	if err = reopened.Start(t.Context(), f.r.ID, good); err == nil {
		t.Fatal("completed login replayed")
	}
}
func TestLoginSourceChangeAndUnknownResponseCannotAdvance(t *testing.T) {
	for _, mode := range []string{"site", "saved", "uncertain", "authority", "limit"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			if mode == "saved" {
				f.r.Source.UseSaved = true
				f.r.Source.Saved = Hash("")
			}
			f.create(t)
			if err := f.s.Start(t.Context(), f.r.ID, good); err != nil {
				t.Fatal(err)
			}
			if mode == "site" {
				f.site.Cookie = "changed=elsewhere"
				if _, err := f.sites.Upsert(t.Context(), f.site); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "saved" {
				if _, err := f.s.db.Exec("INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)", SavedKey, `{"username":"changed"}`); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "uncertain" {
				i, err := f.s.Step(t.Context(), f.r.ID, Hash(f.r.Source), "network:POST:Document", Hash("first"), good)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.s.FinishStep(t.Context(), f.r.ID, Hash(f.r.Source), i, "uncertain", ""); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "limit" {
				record, _ := f.s.Get(t.Context(), f.r.ID)
				for i := 0; i < MaxOperations; i++ {
					record.Operations = append(record.Operations, Operation{Kind: "network:GET", Signature: Hash(i), State: "received", Digest: Hash("ok")})
				}
				if _, err := f.s.db.Exec("UPDATE GO_SITE_LOGIN_JOBS SET DOCUMENT=? WHERE ID=?", mustJSON(record), record.ID); err != nil {
					t.Fatal(err)
				}
			}
			guard := good
			if mode == "authority" {
				guard = func(context.Context) error { return ErrScope }
			}
			if _, err := f.s.Step(t.Context(), f.r.ID, Hash(f.r.Source), "network:POST:Document", Hash("second"), guard); err == nil {
				t.Fatal("changed/unknown source advanced")
			}
			if mode == "limit" {
				// The budget blocks another request, while all already received
				// operations may still publish their verified result.
				if err := f.s.SaveCookie(t.Context(), f.r.ID, "new=verified", "ua", "acquired", guard); err != nil {
					t.Fatal("received work could not finish at the budget", err)
				}
				return
			}
			if err := f.s.SaveCookie(t.Context(), f.r.ID, "new=forbidden", "ua", "acquired", guard); err == nil {
				t.Fatal("changed/unknown source published cookie")
			}
		})
	}
}
func TestSilentDatabasePublicationOrReceiptReplacementRollsBack(t *testing.T) {
	for _, trigger := range []string{
		`CREATE TRIGGER ignore_cookie BEFORE UPDATE OF COOKIE ON CONFIG_SITE BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER replace_cookie AFTER UPDATE OF COOKIE ON CONFIG_SITE BEGIN UPDATE CONFIG_SITE SET COOKIE='foreign=bad' WHERE ID=NEW.ID;END`,
		`CREATE TRIGGER ignore_receipt BEFORE UPDATE ON GO_SITE_LOGIN_JOBS WHEN NEW.STATE='completed' BEGIN SELECT RAISE(IGNORE);END`,
	} {
		t.Run(Hash(trigger)[:8], func(t *testing.T) {
			f := newFixture(t)
			f.create(t)
			if err := f.s.Start(t.Context(), f.r.ID, good); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if err := f.s.SaveCookie(t.Context(), f.r.ID, "new=session", "new-agent", "acquired", good); err == nil {
				t.Fatal("silent database mutation accepted")
			}
			current, err := f.sites.Get(t.Context(), f.site.ID)
			if err != nil || Hash(current) != Hash(f.site) {
				t.Fatal("failed publication changed original site", err)
			}
			record, _ := f.s.Get(t.Context(), f.r.ID)
			if record.State != "running" {
				t.Fatal(record.State)
			}
		})
	}
}
func TestInterruptedLoginClosesWithoutReplayAndRememberRetainsUnknowns(t *testing.T) {
	f := newFixture(t)
	f.b.Remember = true
	f.b.RememberState = "pending"
	f.create(t)
	if err := f.s.Start(t.Context(), f.r.ID, good); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Step(t.Context(), f.r.ID, Hash(f.r.Source), "action:credentials", Hash("intent"), good); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	record, _ := f.s.Get(t.Context(), f.r.ID)
	if record.State != "uncertain" {
		t.Fatal(record.State)
	}
	batch, _, err := f.s.Batch(t.Context(), f.b.ID)
	if err != nil || batch.RememberState != "uncertain" {
		t.Fatal(batch, err)
	}
	raw := `{"username":"old","password":"old-private","two_step_code":"old-retained","unknown":9007199254740993}`
	if _, err = f.s.db.Exec("INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)", SavedKey, raw); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Remember(t.Context(), raw, "new-user", "new-password", good); err != nil {
		t.Fatal(err)
	}
	after, err := f.s.Saved(t.Context())
	if err != nil || !strings.Contains(after, "9007199254740993") || !strings.Contains(after, "old-retained") || !strings.Contains(after, "new-password") {
		t.Fatal(after, err)
	}
	if err = f.s.Remember(t.Context(), raw, "again", "bad", good); !errors.Is(err, ErrScope) {
		t.Fatal("concurrent credentials overwritten", err)
	}
}

func TestBatchCreationAndIndexedIdentityMustReadBackExactly(t *testing.T) {
	for _, mode := range []string{"ignore-batch", "replace-batch", "job-actor", "job-batch", "job-created"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			if mode == "ignore-batch" || mode == "replace-batch" {
				statement := `CREATE TRIGGER ignore_batch BEFORE INSERT ON GO_SITE_LOGIN_BATCHES BEGIN SELECT RAISE(IGNORE);END`
				if mode == "replace-batch" {
					statement = `CREATE TRIGGER replace_batch AFTER INSERT ON GO_SITE_LOGIN_BATCHES BEGIN UPDATE GO_SITE_LOGIN_BATCHES SET ACTOR='foreign' WHERE ID=NEW.ID;END`
				}
				if _, err := f.s.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
				if _, _, err := f.s.Create(t.Context(), f.b, []Record{f.r}, good); err == nil {
					t.Fatal("silent batch replacement accepted")
				}
				var count int
				if err := f.s.db.QueryRow("SELECT COUNT(*) FROM GO_SITE_LOGIN_JOBS").Scan(&count); err != nil || count != 0 {
					t.Fatal("batch failure left executable children", count, err)
				}
				return
			}
			f.create(t)
			column := map[string]string{"job-actor": "ACTOR", "job-batch": "BATCH_ID", "job-created": "CREATED"}[mode]
			if _, err := f.s.db.Exec("UPDATE GO_SITE_LOGIN_JOBS SET "+column+"='foreign' WHERE ID=?", f.r.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.Get(t.Context(), f.r.ID); err == nil {
				t.Fatal("indexed identity and private document diverged")
			}
		})
	}
}
