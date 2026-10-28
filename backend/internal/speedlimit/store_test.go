package speedlimit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	_ "modernc.org/sqlite"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type limiterFixture struct {
	db *sql.DB
	s  *Store
	p  Review
}

func limiterTestFixture(t *testing.T) *limiterFixture {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT);
 CREATE TABLE DOWNLOADER(ID INTEGER PRIMARY KEY,NAME TEXT,TYPE TEXT,CONFIG TEXT,ENABLED INTEGER);
 INSERT INTO DOWNLOADER VALUES(1,'first','qbittorrent','{"host":"https://q.test","password":"private"}',1),(2,'disabled','transmission','{}',0),(3,'third','transmission','{"host":"https://t.test"}',1);
 INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE,NOTE) VALUES('SystemConfig','UserInstalledPlugins','["SpeedLimiter"]',''),('SystemConfig','plugin.SpeedLimiter','{"downloaders":[1,2,3],"allocation_ratio":"1:9:3","upload_limit":64,"download_limit":128,"download_unlimit":0,"upload_unlimit":0,"ipv4":"192.168.0.0/16","ipv6":"fd00::/8","notify":true}','')`); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	basis, err := s.Basis(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := setting(t.Context(), db, ConfigKey)
	spec, err := DecodeSpec(raw)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := s.Targets(t.Context(), spec)
	if err != nil || len(targets) != 2 || targets[1].Weight != 3 {
		t.Fatal(targets, err)
	}
	c := Control{Epoch: strings.Repeat("a", 32), Actor: "admin", Authority: strings.Repeat("b", 64), ConfigDigest: RawConfigDigest(raw), MediaDigest: strings.Repeat("c", 64), Server: "emby", MediaID: "original-server", Created: nowString(), Baselines: []Baseline{{Target: targets[0], Rates: Rates{Unit: "B/s"}}, {Target: targets[1], Rates: Rates{Unit: "RPC KB/s", OtherUpload: 50, OtherDownload: 50}}}}
	p := Review{ID: strings.Repeat("d", 32), Basis: basis, Control: c, Spec: spec, Created: nowString()}
	return &limiterFixture{db, s, p}
}
func allowLimiter() error { return nil }
func (f *limiterFixture) activate(t *testing.T) Control {
	t.Helper()
	if err := f.s.SaveReview(t.Context(), f.p, allowLimiter); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Activate(t.Context(), f.p, allowLimiter); err != nil {
		t.Fatal(err)
	}
	c, _, on, err := f.s.Scope(t.Context())
	if err != nil || !on {
		t.Fatal(on, err)
	}
	return c
}
func (f *limiterFixture) capture(t *testing.T, id string) {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Capture(t.Context(), tx, "emby", id, json.RawMessage(`{"Event":"playback.start"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func (f *limiterFixture) job(t *testing.T, c Control, id string) Job {
	t.Helper()
	f.capture(t, id)
	triggers, err := f.s.Triggers(t.Context(), true)
	if err != nil || len(triggers) < 1 {
		t.Fatal(triggers, err)
	}
	_, spec, _, _ := f.s.Scope(t.Context())
	before := []Baseline{}
	for _, b := range c.Baselines {
		rates, err := f.s.Owner(t.Context(), c, b.Target)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, Baseline{b.Target, rates})
	}
	j, err := f.s.CreateJob(t.Context(), c, spec, triggers[0], Observation{Playing: 1, Bitrate: 8000000}, before, allowLimiter)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestLimiterDefaultOffReadOnlyAndAtomicFutureCapture(t *testing.T) {
	f := limiterTestFixture(t)
	if _, _, active, err := f.s.Scope(t.Context()); err != nil || active {
		t.Fatal(active, err)
	}
	f.capture(t, "before")
	if items, err := f.s.Triggers(t.Context(), false); err != nil || len(items) != 0 {
		t.Fatal(items, err)
	}
	var tables int
	f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'GO_SPEED_LIMITER_%'`).Scan(&tables)
	if tables != 0 {
		t.Fatal("default off wrote tables", tables)
	}
	c := f.activate(t)
	f.capture(t, "after")
	f.capture(t, "after")
	rows, _ := f.s.Triggers(t.Context(), false)
	if len(rows) != 1 || rows[0].Source != "webhook" {
		t.Fatal(rows)
	}
	tx, _ := f.db.BeginTx(t.Context(), nil)
	if err := Capture(t.Context(), tx, "emby", "rollback", json.RawMessage(`{"Event":"playback.stop"}`)); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	rows, _ = f.s.Triggers(t.Context(), false)
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	tx, _ = f.db.BeginTx(t.Context(), nil)
	for _, kind := range []string{"plex", "jellyfin"} {
		if err := Capture(t.Context(), tx, kind, "wrong", json.RawMessage(`{"Event":"playback.start","event":"media.play","NotificationType":"PlaybackStart"}`)); err != nil {
			t.Fatal(err)
		}
	}
	tx.Commit()
	if err := f.s.Timer(t.Context(), c, f.p.Spec, time.Now().Add(3000*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Timer(t.Context(), c, f.p.Spec, time.Now().Add(3000*time.Second)); err != nil {
		t.Fatal(err)
	}
	rows, _ = f.s.Triggers(t.Context(), false)
	if len(rows) != 2 {
		t.Fatal("timer backfilled or duplicated", rows)
	}
}
func TestLimiterReviewActivationConflictsAndIgnoredWrites(t *testing.T) {
	for _, query := range []string{
		`CREATE TRIGGER ignore_control BEFORE INSERT ON SYSTEM_DICT WHEN NEW.KEY='GoSpeedLimiter' BEGIN SELECT RAISE(IGNORE);END`,
		`CREATE TRIGGER ignore_owner BEFORE INSERT ON GO_SPEED_LIMITER_OWNERS BEGIN SELECT RAISE(IGNORE);END`,
		`CREATE TRIGGER ignore_review BEFORE UPDATE ON GO_SPEED_LIMITER_REVIEWS BEGIN SELECT RAISE(IGNORE);END`,
		`CREATE TRIGGER ignore_play BEFORE INSERT ON GO_SPEED_LIMITER_PLAYSTATE BEGIN SELECT RAISE(IGNORE);END`,
		`CREATE TRIGGER corrupt_review AFTER UPDATE ON GO_SPEED_LIMITER_REVIEWS BEGIN UPDATE GO_SPEED_LIMITER_REVIEWS SET STATE='open' WHERE ID=NEW.ID;END`,
	} {
		t.Run(query, func(t *testing.T) {
			f := limiterTestFixture(t)
			if err := f.s.SaveReview(t.Context(), f.p, allowLimiter); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if err := f.s.Activate(t.Context(), f.p, allowLimiter); err == nil {
				t.Fatal("ignored write enabled limiter")
			}
			_, _, on, _ := f.s.Scope(t.Context())
			if on {
				t.Fatal("partial grant committed")
			}
		})
	}
	for _, change := range []string{`UPDATE DOWNLOADER SET CONFIG='{"host":"https://new.test"}' WHERE ID=1`, `UPDATE SYSTEM_DICT SET VALUE='[]' WHERE KEY='UserInstalledPlugins'`} {
		t.Run(change, func(t *testing.T) {
			f := limiterTestFixture(t)
			if err := f.s.SaveReview(t.Context(), f.p, allowLimiter); err != nil {
				t.Fatal(err)
			}
			f.db.Exec(change)
			if err := f.s.Activate(t.Context(), f.p, allowLimiter); err == nil {
				t.Fatal("stale review")
			}
		})
	}
	f := limiterTestFixture(t)
	f.activate(t)
	if err := f.s.Activate(t.Context(), f.p, allowLimiter); err == nil {
		t.Fatal("review reused")
	}
	basis, _ := f.s.Basis(t.Context())
	if err := f.s.Stop(t.Context(), basis, func() error { return errors.New("root changed") }); err == nil {
		t.Fatal("stale root stopped limiter")
	}
	if err := f.s.Stop(t.Context(), basis, allowLimiter); err != nil {
		t.Fatal(err)
	}
	if _, _, on, _ := f.s.Scope(t.Context()); on {
		t.Fatal("still active")
	}
}
func TestLimiterPartialUnknownBlocksReplayAndVerifiesOnlyOriginalSnapshot(t *testing.T) {
	f := limiterTestFixture(t)
	c := f.activate(t)
	j := f.job(t, c, "partial")
	if len(j.Operations) != 3 || j.Operations[0].Dimension != "upload" || j.Operations[1].Dimension != "download" || j.Operations[2].Dimension != "pair" {
		t.Fatal(j)
	}
	if err := f.s.BeginOperation(t.Context(), j, 0, allowLimiter); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishOperation(t.Context(), j, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginOperation(t.Context(), j, 1, allowLimiter); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishOperation(t.Context(), j, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginOperation(t.Context(), j, 2, allowLimiter); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishOperation(t.Context(), j, 2, true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Finalize(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	j, err := New(f.db).Job(t.Context(), j.ID)
	if err != nil || j.State != "partial" || j.Operations[0].State != "confirmed" || j.Operations[1].State != "uncertain" {
		t.Fatal(j, err)
	}
	if err := New(f.db).BeginOperation(t.Context(), j, 1, allowLimiter); err == nil {
		t.Fatal("replayed uncertain operation")
	}
	if pending, _ := f.s.Unresolved(t.Context(), "1"); !pending {
		t.Fatal("unknown released")
	}
	wrong := j.Operations[1].After
	wrong.Download++
	if err := f.s.VerifyOperation(t.Context(), j, 1, wrong, "admin", allowLimiter); err == nil {
		t.Fatal("wrong current snapshot verified")
	}
	if err := f.s.VerifyOperation(t.Context(), j, 1, j.Operations[1].After, "admin", allowLimiter); err != nil {
		t.Fatal(err)
	}
	current, _ := f.s.Job(t.Context(), j.ID)
	if current.Operations[1].State != "verified_current" || current.State != "partial" {
		t.Fatal("historical delivery truth changed", current)
	}
	if pending, _ := f.s.Unresolved(t.Context(), "1"); pending {
		t.Fatal("verified setting unresolved")
	}
	raw, _ := json.Marshal(current)
	for _, secret := range []string{"private", "Authority", "digest", "definition", "https://"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("public secret", string(raw))
		}
	}
}
func TestLimiterCrashIntentIgnoreAndStaleAuthority(t *testing.T) {
	f := limiterTestFixture(t)
	c := f.activate(t)
	j := f.job(t, c, "crash")
	if err := f.s.BeginOperation(t.Context(), j, 0, allowLimiter); err != nil {
		t.Fatal(err)
	}
	if err := New(f.db).BeginOperation(t.Context(), j, 0, allowLimiter); err == nil {
		t.Fatal("restart replayed intent")
	}
	if err := f.s.IgnoreJob(t.Context(), j, "admin", "", allowLimiter); err == nil {
		t.Fatal("empty ignore")
	}
	if err := f.s.IgnoreJob(t.Context(), j, "admin", "checked fake downloader", allowLimiter); err != nil {
		t.Fatal(err)
	}
	current, _ := f.s.Job(t.Context(), j.ID)
	if current.State != "ignored" || current.Operations[0].Actor != "admin" {
		t.Fatal(current)
	}
	if _, _, on, _ := f.s.Scope(t.Context()); on {
		t.Fatal("ignore did not stop current automatic scope")
	}
	if p, _ := f.s.Unresolved(t.Context(), "1"); p {
		t.Fatal("ignored remained pending")
	}
	f = limiterTestFixture(t)
	c = f.activate(t)
	j = f.job(t, c, "revoked")
	if err := f.s.BeginOperation(t.Context(), j, 0, func() error { return errors.New("root revoked") }); err == nil {
		t.Fatal("stale root")
	}
	current, _ = f.s.Job(t.Context(), j.ID)
	if current.Operations[0].State != "blocked" {
		t.Fatal(current)
	}
}
func TestLimiterConfirmedNotificationUsesCustomMessageAndCommitsOnce(t *testing.T) {
	f := limiterTestFixture(t)
	if _, err := f.db.Exec(`CREATE TABLE MESSAGE_CLIENT(ID INTEGER PRIMARY KEY,NAME TEXT,TYPE TEXT,CONFIG TEXT,SWITCHS TEXT,INTERACTIVE INTEGER,ENABLED INTEGER,NOTE TEXT);
 INSERT INTO MESSAGE_CLIENT VALUES(1,'custom','webhook','{"url":"https://secret.test"}','["custom_message"]',0,1,''),(2,'wrong','webhook','{}','["plugin_message"]',0,1,'')`); err != nil {
		t.Fatal(err)
	}
	c := f.activate(t)
	j := f.job(t, c, "complete")
	for i := range j.Operations {
		if err := f.s.BeginOperation(t.Context(), j, i, allowLimiter); err != nil {
			t.Fatal(err)
		}
		if err := f.s.FinishOperation(t.Context(), j, i, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.Finalize(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Finalize(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	events, err := bus.New(f.db).List(t.Context())
	if err != nil || len(events) != 1 || events[0].Kind != "plugin_message" || len(events[0].Deliveries) != 1 || events[0].Deliveries[0].ChannelID != 1 {
		t.Fatal(events, err)
	}
	j = f.job(t, c, "same-playing")
	if len(j.Operations) != 0 {
		t.Fatal("unchanged rates planned writes")
	}
	if err := f.s.Finalize(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	events, _ = bus.New(f.db).List(t.Context())
	if len(events) != 1 {
		t.Fatal("duplicate static state message")
	}
}
func TestLimiterPlanningRollsBackTamperedIntent(t *testing.T) {
	f := limiterTestFixture(t)
	c := f.activate(t)
	j := f.job(t, c, "intent")
	if _, err := f.db.Exec(`CREATE TRIGGER ignore_intent BEFORE UPDATE ON GO_SPEED_LIMITER_OPERATIONS WHEN NEW.STATE='intent' BEGIN SELECT RAISE(IGNORE);END`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginOperation(context.Background(), j, 0, allowLimiter); err == nil {
		t.Fatal("unpersisted intent permitted dispatch")
	}
	current, _ := f.s.Job(t.Context(), j.ID)
	if current.Operations[0].State != "pending" {
		t.Fatal(current)
	}
}
func TestLimiterCallbackAndTimerRollBackRevokedGrantTogether(t *testing.T) {
	for _, source := range []string{"webhook", "timer"} {
		t.Run(source, func(t *testing.T) {
			f := limiterTestFixture(t)
			c := f.activate(t)
			off := c
			off.Authorized = false
			raw, _ := json.Marshal(off)
			query := `CREATE TRIGGER revoke_capture AFTER INSERT ON GO_SPEED_LIMITER_TRIGGERS BEGIN UPDATE SYSTEM_DICT SET VALUE=? WHERE KEY='GoSpeedLimiter';END`
			query = strings.Replace(query, "?", "'"+strings.ReplaceAll(string(raw), "'", "''")+"'", 1)
			if _, err := f.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			var err error
			if source == "timer" {
				err = f.s.Timer(t.Context(), c, f.p.Spec, time.Now().Add(600*time.Second))
			} else {
				tx, _ := f.db.BeginTx(t.Context(), nil)
				err = Capture(t.Context(), tx, "emby", "revoke", json.RawMessage(`{"Event":"playback.start"}`))
				tx.Rollback()
			}
			if err == nil {
				t.Fatal("revoked callback committed")
			}
			rows, _ := f.s.Triggers(t.Context(), false)
			if len(rows) != 0 {
				t.Fatal(rows)
			}
		})
	}
}
func TestLimiterReviewExpiryAndOriginalCallbackTiming(t *testing.T) {
	for _, age := range []time.Duration{-time.Minute, 11 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			f := limiterTestFixture(t)
			f.p.Created = time.Now().Add(-age).UTC().Format(time.RFC3339Nano)
			if err := f.s.SaveReview(t.Context(), f.p, allowLimiter); err != nil {
				t.Fatal(err)
			}
			if err := f.s.Activate(t.Context(), f.p, allowLimiter); err == nil {
				t.Fatal("future or expired review enabled")
			}
		})
	}
	f := limiterTestFixture(t)
	c := f.activate(t)
	c.Server = "plex"
	raw, _ := json.Marshal(c)
	if _, err := f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE=? WHERE KEY='GoSpeedLimiter'`, string(raw)); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"media.pause", "media.play", "media.play"} {
		tx, _ := f.db.BeginTx(t.Context(), nil)
		if err := Capture(t.Context(), tx, "plex", "original", json.RawMessage(`{"event":"`+action+`"}`)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.s.Triggers(t.Context(), false)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	due, _ := time.Parse(time.RFC3339Nano, rows[0].Due)
	created, _ := time.Parse(time.RFC3339Nano, rows[0].Created)
	if due.Sub(created) < 2900*time.Millisecond || due.Sub(created) > 3100*time.Millisecond {
		t.Fatal("Plex three-second boundary", due.Sub(created))
	}
	if pending, _ := f.s.Triggers(t.Context(), true); len(pending) != 0 {
		t.Fatal("Plex callback ran immediately")
	}
}
