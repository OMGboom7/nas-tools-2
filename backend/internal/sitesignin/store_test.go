package sitesignin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func signinRecordFixture(t *testing.T) (*Store, Scope, func(context.Context) error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "user.db")
	sites, e := siteconfig.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer sites.Close()
	system, e := systemconfig.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer system.Close()
	if e = system.Set(context.Background(), "plugin.AutoSignIn", `{"future":{"big":9007199254740993,"nil":null},"enabled":true,"queue_cnt":2}`); e != nil {
		t.Fatal(e)
	}
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	if _, e = s.db.Exec(`INSERT INTO CONFIG_SITE(ID,NAME,PRI,SIGNURL,COOKIE,NOTE) VALUES(9007199254740993,'fixture-site','1','https://carpt.net/','private-cookie','{"future":9007199254740993}')`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`ALTER TABLE CONFIG_SITE ADD COLUMN OLD_BLOB BLOB`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`ALTER TABLE CONFIG_SITE ADD COLUMN OLD_NULL TEXT`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`UPDATE CONFIG_SITE SET OLD_BLOB=x'00FF01',OLD_NULL=NULL`); e != nil {
		t.Fatal(e)
	}
	site, e := sites.Get(context.Background(), 9007199254740993)
	if e != nil {
		t.Fatal(e)
	}
	settings, e := ParseSettings(`{"enabled":true,"queue_cnt":2,"sign_sites":["9007199254740993"]}`)
	if e != nil {
		t.Fatal(e)
	}
	scope := Scope{ID: sitelogin.Random(), Actor: "fixture-admin", Authority: sitelogin.Hash("authority"), ConfigDigest: sitelogin.Hash("original-config"), PluginDigest: sitelogin.Hash("original-plugin"), Created: time.Now().UTC().Format(time.RFC3339Nano), Location: "Asia/Shanghai", Settings: settings, Sources: []Source{{SiteID: site.ID, Name: site.Name, URL: site.SignURL, UserAgent: "fixture-agent", SiteDigest: sitelogin.Hash(site)}}}
	guard := func(ctx context.Context) error { return ctx.Err() }
	signinSaveReview(t, s, scope, guard)
	return s, scope, guard
}
func signinSaveReview(t *testing.T, s *Store, scope Scope, guard func(context.Context) error) {
	t.Helper()
	created, _ := time.Parse(time.RFC3339Nano, scope.Created)
	if e := s.SaveReview(context.Background(), Review{ID: scope.ID, Mode: "manual", Scope: scope, Expires: created.Add(10 * time.Minute).Format(time.RFC3339Nano), BeforePluginDigest: scope.PluginDigest, PreviousControlDigest: sitelogin.Hash(Control{})}, guard); e != nil {
		t.Fatal(e)
	}
}
func signinCreateFixture(t *testing.T, s *Store, scope Scope, guard func(context.Context) error) (Batch, Job) {
	t.Helper()
	r := Job{ID: sitelogin.Hash([]string{scope.ID, "job"}), Batch: scope.ID, Actor: scope.Actor, ScopeID: scope.ID, Day: "2026-11-02", Created: scope.Created, Updated: scope.Created, Source: scope.Sources[0], State: "queued", Operations: []Operation{}}
	b := Batch{ID: scope.ID, Trigger: "manual", Created: scope.Created, Scope: scope, Jobs: []string{r.ID}}
	rows, old, e := s.Create(context.Background(), b, []Job{r}, guard)
	if e != nil || old || len(rows) != 1 {
		t.Fatalf("create: %v %v", old, e)
	}
	return b, r
}
func TestDurableSignInReceiptAndOriginalData(t *testing.T) {
	s, scope, guard := signinRecordFixture(t)
	b, r := signinCreateFixture(t, s, scope, guard)
	ctx := context.Background()
	if e := s.Start(ctx, r.ID, guard); e != nil {
		t.Fatal(e)
	}
	index, e := s.Step(ctx, r.ID, Operation{Kind: "site:POST", Signature: sitelogin.Hash("request"), State: "intent", Action: true}, guard)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Acknowledge(ctx, r.ID, index, "received", sitelogin.Hash("response")); e != nil {
		t.Fatal(e)
	}
	if e = s.Finish(ctx, r.ID, signed("站点确认奖励")); e != nil {
		t.Fatal(e)
	}
	before, e := s.Get(ctx, r.ID)
	if e != nil || before.Outcome.State != "signed" {
		t.Fatalf("%+v %v", before, e)
	}
	rows, old, e := s.Create(ctx, b, []Job{r}, guard)
	if e != nil || !old || len(rows) != 1 || sitelogin.Hash(rows[0]) != sitelogin.Hash(before) {
		t.Fatal("same original batch did not return original receipt")
	}
	if e = s.Notify(ctx, b.ID, "fixture summary"); e != nil {
		t.Fatal(e)
	}
	if e = s.Notify(ctx, b.ID, "do not replace summary"); e != nil {
		t.Fatal(e)
	}
	saved, _, e := s.Batch(ctx, b.ID)
	if e != nil || saved.Summary != "fixture summary" || !saved.Notified {
		t.Fatal("summary changed on replay")
	}
	var cookie, note, blob string
	var value any
	if e = s.db.QueryRow(`SELECT COOKIE,NOTE,hex(OLD_BLOB),OLD_NULL FROM CONFIG_SITE WHERE ID=9007199254740993`).Scan(&cookie, &note, &blob, &value); e != nil {
		t.Fatal(e)
	}
	if cookie != "private-cookie" || note != `{"future":9007199254740993}` || blob != "00FF01" || value != nil {
		t.Fatal("original site fields changed")
	}
}
func TestUnknownOrInterruptedRequestNeverBecomesRetryableSuccess(t *testing.T) {
	for _, ack := range []string{"intent", "unknown"} {
		t.Run(ack, func(t *testing.T) {
			s, scope, guard := signinRecordFixture(t)
			_, r := signinCreateFixture(t, s, scope, guard)
			ctx := context.Background()
			if e := s.Start(ctx, r.ID, guard); e != nil {
				t.Fatal(e)
			}
			index, e := s.Step(ctx, r.ID, Operation{Kind: "site:GET", Signature: sitelogin.Hash("request"), State: "intent", Action: true}, guard)
			if e != nil {
				t.Fatal(e)
			}
			if ack == "unknown" {
				if e = s.Acknowledge(ctx, r.ID, index, "uncertain", ""); e != nil {
					t.Fatal(e)
				}
			}
			if e = s.Recover(ctx); e != nil {
				t.Fatal(e)
			}
			saved, e := s.Get(ctx, r.ID)
			if e != nil || saved.State != "finished" || saved.Outcome.State != "unknown" || saved.Outcome.Retryable {
				t.Fatalf("%+v %v", saved, e)
			}
			daily, e := s.Daily(ctx, r.Day)
			if e != nil {
				t.Fatal(e)
			}
			scope.Settings.Special = []string{"9007199254740993"}
			if len(scope.Settings.Select(scope.Sources, daily, false)) != 0 {
				t.Fatal("unknown action selected again")
			}
		})
	}
}
func TestChallengeConsumedOnceWithoutPersistingInput(t *testing.T) {
	s, scope, guard := signinRecordFixture(t)
	_, r := signinCreateFixture(t, s, scope, guard)
	ctx := context.Background()
	if e := s.Start(ctx, r.ID, guard); e != nil {
		t.Fatal(e)
	}
	nonce := sitelogin.Random()
	if e := s.Prompt(ctx, r.ID, "captcha", nonce, time.Now().Add(time.Minute), guard); e != nil {
		t.Fatal(e)
	}
	if e := s.Consume(ctx, r.ID, sitelogin.Random(), guard); !errors.Is(e, ErrInput) {
		t.Fatalf("foreign challenge accepted %v", e)
	}
	if e := s.Consume(ctx, r.ID, nonce, guard); e != nil {
		t.Fatal(e)
	}
	if e := s.Consume(ctx, r.ID, nonce, guard); !errors.Is(e, ErrInput) {
		t.Fatal("consumed challenge accepted")
	}
	var raw string
	if e := s.db.QueryRow(`SELECT DOCUMENT FROM GO_SITE_SIGNIN_JOBS WHERE ID=?`, r.ID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(raw, nonce) || strings.Contains(raw, "captcha") {
		t.Fatal("spent challenge retained in public/private document")
	}
}
func TestSourceEditsAndNewReviewsRevokeUnstartedWork(t *testing.T) {
	s, scope, guard := signinRecordFixture(t)
	newScope := scope
	newScope.ID = sitelogin.Random()
	signinSaveReview(t, s, newScope, guard)
	created := scope.Created
	r := Job{ID: sitelogin.Hash("late"), Batch: scope.ID, Actor: scope.Actor, ScopeID: scope.ID, Day: "2026-11-02", Created: created, Updated: created, Source: scope.Sources[0], State: "queued"}
	b := Batch{ID: scope.ID, Trigger: "manual", Scope: scope, Created: created, Jobs: []string{r.ID}}
	if _, _, e := s.Create(context.Background(), b, []Job{r}, guard); !errors.Is(e, ErrScope) {
		t.Fatalf("superseded review allowed %v", e)
	}
	_, r = signinCreateFixture(t, s, newScope, guard)
	if e := s.Start(context.Background(), r.ID, guard); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(`UPDATE CONFIG_SITE SET COOKIE='concurrent edit' WHERE ID=?`, r.Source.SiteID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Step(context.Background(), r.ID, Operation{Kind: "site:POST", State: "intent", Signature: sitelogin.Hash("new action"), Action: true}, guard); !errors.Is(e, ErrScope) {
		t.Fatalf("edited credentials allowed %v", e)
	}
}
func TestControlConsumesOriginalFlagsAndPreservesUnknownJSON(t *testing.T) {
	s, scope, guard := signinRecordFixture(t)
	ctx := context.Background()
	before := `{"enabled":true,"onlyonce":true,"clean":true,"future":{"big":9007199254740993,"null":null}}`
	if _, e := s.db.Exec(`UPDATE SYSTEM_DICT SET VALUE=? WHERE KEY='plugin.AutoSignIn'`, before); e != nil {
		t.Fatal(e)
	}
	after, e := MergeSettings(before, map[string]json.RawMessage{"onlyonce": json.RawMessage("false"), "clean": json.RawMessage("false")})
	if e != nil {
		t.Fatal(e)
	}
	scope.PluginDigest = sitelogin.Hash(after)
	scope.ID = sitelogin.Random()
	signinSaveReview(t, s, scope, guard)
	c := Control{Scope: scope, Enabled: true, NextRun: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)}
	if e = s.Activate(ctx, Control{}, c, before, after, guard); e != nil {
		t.Fatal(e)
	}
	actual, e := setting(ctx, s.db, "plugin.AutoSignIn")
	if e != nil || !strings.Contains(actual, `9007199254740993`) || !strings.Contains(actual, `"onlyonce":false`) || !strings.Contains(actual, `"null":null`) {
		t.Fatalf("original settings lost %s %v", actual, e)
	}
	if e = s.StopControl(ctx, c.Scope.ID, "explicit stop"); e != nil {
		t.Fatal(e)
	}
	saved, e := s.Control(ctx)
	if e != nil || saved.Enabled {
		t.Fatal("control did not stop")
	}
	if e = s.Advance(ctx, c, c); !errors.Is(e, ErrState) {
		t.Fatal("stale scheduler revived stopped control")
	}
}
