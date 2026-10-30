//go:build linux || darwin

package organization

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

func manualSubtitleFixture(t *testing.T, hook bool) (*Store, SubtitleRequestDefinition) {
	t.Helper()
	s, e := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	if hook {
		organizationHookGrant(t, s)
	}
	if _, e = s.db.Exec(`CREATE TABLE IF NOT EXISTS SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT);CREATE TABLE TRANSFER_HISTORY(ID INTEGER PRIMARY KEY,DEST_PATH TEXT,EXTRA BLOB,NOTE TEXT);INSERT INTO TRANSFER_HISTORY VALUES(9007199254740993,'unchanged original path',X'00FF01',NULL)`); e != nil {
		t.Fatal(e)
	}
	installed := "[\"ChineseSubFinder\"]"
	if hook {
		installed = "[\"ChineseSubFinder\",\"Webhook\"]"
	}
	if _, e = s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig','UserInstalledPlugins',?),('SystemConfig','plugin.ChineseSubFinder','{"host":"https://subtitle.test","api_key":"private-key","unknown":9007199254740993}')`, installed); e != nil {
		t.Fatal(e)
	}
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	relative := "Films/Movie.2026.mkv"
	if e = os.MkdirAll(filepath.Dir(filepath.Join(root, relative)), 0755); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, relative), []byte("original existing video, never transferred by this request"), 0640); e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(root)
	if e != nil {
		t.Fatal(e)
	}
	d := SubtitleRequestDefinition{Version: 1, Actor: "admin", Authority: Digest("original admin"), RequestID: strings.Repeat("b", 32), Created: time.Now().UTC().Format(time.RFC3339Nano), Scope: RecoveryScope{Root: LibraryRoot{ID: "original-library", Path: root, Identity: Identity(info), Kind: "movie"}, ConfigDigest: Digest("original config")}, Selection: relative, InputPath: relative, Name: "Movie.2026.mkv", Title: "Movie", Year: "2026", MediaType: "电影", TMDBID: "100", MediaInfo: map[string]any{"title": "Movie", "year": "2026", "type": "电影", "tmdb_id": "100"}, Allowed: []LibraryRoot{}, RemoteNodes: []RecoveryNode{}}
	m, e := CaptureLibraryMedia(t.Context(), SubtitleRequestLibrary(d), relative)
	if e != nil {
		t.Fatal(e)
	}
	d.Local = &m
	d.ContentDigest, e = SubtitleRequestLocalDigest(t.Context(), SubtitleRequestLibrary(d), m)
	if e != nil {
		t.Fatal(e)
	}
	d.Plugin, e = s.Subtitles().Snapshot(t.Context(), filepath.Join(root, relative), d.MediaType, false)
	if e != nil {
		t.Fatal(e)
	}
	if !subtitleRequestValid(d) {
		raw, _ := json.Marshal(d)
		t.Fatalf("invalid original fixture root=%t path=%t payload=%+v: %s", recoveryScopeValid(d.Scope), SubtitleRequestPath(d.Selection), d.Plugin.Payload, raw)
	}
	return s, d
}
func manualSubtitleReview(t *testing.T, s *Store, d SubtitleRequestDefinition) SubtitleRequest {
	t.Helper()
	p, e := s.SaveSubtitleReview(t.Context(), d, func() error { return VerifySubtitleRequestSource(t.Context(), d, nil) })
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestManualSubtitleRequestQueuesOriginalEventAndChildWithoutIntake(t *testing.T) {
	s, d := manualSubtitleFixture(t, true)
	p := manualSubtitleReview(t, s, d)
	if p.Queued || p.ServiceState != "not_queued" {
		t.Fatal(p)
	}
	rows, e := s.Subtitles().List(t.Context(), false)
	if e != nil || len(rows) != 0 {
		t.Fatal("review created remote work", rows, e)
	}
	queued, e := s.QueueSubtitleRequest(t.Context(), p.ID, d.Actor, p.Fingerprint, func() error { return VerifySubtitleRequestSource(t.Context(), d, nil) })
	if e != nil || queued.State != "queued" || queued.ServiceState != "pending" {
		t.Fatal(queued, e)
	}
	again, e := s.QueueSubtitleRequest(t.Context(), p.ID, d.Actor, p.Fingerprint, func() error { t.Fatal("duplicate request revalidated a replaced path"); return nil })
	if e != nil || again.ID != queued.ID {
		t.Fatal(again, e)
	}
	hooks, e := s.BusinessEvents().EventWebhooks(t.Context(), 1, false)
	if e != nil || len(hooks) != 1 || hooks[0].Type != "subtitle.download" || hooks[0].Definition.Source != nil {
		t.Fatal(hooks, e)
	}
	var payload map[string]any
	if json.Unmarshal(hooks[0].Definition.Envelope.Data, &payload) != nil || payload["file"] != filepath.Join(d.Scope.Root.Path, d.Selection) || payload["file_ext"] != ".mkv" || payload["bluray"] != false {
		t.Fatal(payload)
	}
	var count int
	if e = s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); e != nil || count != 0 {
		t.Fatal("manual file adopted as fabricated intake", count, e)
	}
	var id, path, blob string
	var null bool
	if e = s.db.QueryRow(`SELECT CAST(ID AS TEXT),DEST_PATH,hex(EXTRA),NOTE IS NULL FROM TRANSFER_HISTORY`).Scan(&id, &path, &blob, &null); e != nil || id != "9007199254740993" || path != "unchanged original path" || blob != "00FF01" || !null {
		t.Fatal(id, path, blob, null, e)
	}
}
func TestManualSubtitleRequestExpiryConfigChangesAndCancellationPreserveObjects(t *testing.T) {
	for _, change := range []string{"expired", "configuration", "content", "stop_review", "stop_pending"} {
		t.Run(change, func(t *testing.T) {
			s, d := manualSubtitleFixture(t, false)
			if change == "expired" {
				d.Created = time.Now().Add(-11 * time.Minute).UTC().Format(time.RFC3339Nano)
			}
			p := manualSubtitleReview(t, s, d)
			switch change {
			case "configuration":
				if _, e := s.db.Exec(`UPDATE SYSTEM_DICT SET VALUE=REPLACE(VALUE,'9007199254740993','9007199254740992') WHERE KEY='plugin.ChineseSubFinder'`); e != nil {
					t.Fatal(e)
				}
			case "content":
				if e := os.WriteFile(filepath.Join(d.Scope.Root.Path, d.Selection), []byte("changed source"), 0640); e != nil {
					t.Fatal(e)
				}
			case "stop_review":
				stopped, e := s.CancelSubtitleRequest(t.Context(), p.ID, "new-current-admin", "stop this original request")
				if e != nil || stopped.State != "cancelled" || stopped.StoppedBy != "new-current-admin" || stopped.Queued {
					t.Fatal(stopped, e)
				}
				if _, e = s.QueueSubtitleRequest(t.Context(), p.ID, d.Actor, p.Fingerprint, func() error { return nil }); !errors.Is(e, ErrState) {
					t.Fatal(e)
				}
				return
			case "stop_pending":
				if _, e := s.QueueSubtitleRequest(t.Context(), p.ID, d.Actor, p.Fingerprint, func() error { return nil }); e != nil {
					t.Fatal(e)
				}
				stopped, e := s.CancelSubtitleRequest(t.Context(), p.ID, d.Actor, "stop before submission")
				if e != nil || stopped.ServiceState != "blocked" || stopped.State != "cancelled" {
					t.Fatal(stopped, e)
				}
				if _, e = os.Stat(filepath.Join(d.Scope.Root.Path, d.Selection)); e != nil {
					t.Fatal(e)
				}
				return
			}
			if _, e := s.QueueSubtitleRequest(t.Context(), p.ID, d.Actor, p.Fingerprint, func() error { return VerifySubtitleRequestSource(t.Context(), d, nil) }); e == nil {
				t.Fatal("stale original was queued", change)
			}
			if p, e := s.SubtitleRequest(t.Context(), p.ID); e != nil || p.Queued || p.State != "review" {
				t.Fatal(p, e)
			}
		})
	}
}
func TestManualSubtitleRequestRollbackKeepsOriginalReviewAndNoPartialEvent(t *testing.T) {
	for _, where := range []string{"child", "parent", "outbox"} {
		t.Run(where, func(t *testing.T) {
			s, d := manualSubtitleFixture(t, true)
			p := manualSubtitleReview(t, s, d)
			if where == "child" {
				tx, e := s.db.BeginTx(t.Context(), nil)
				if e != nil {
					t.Fatal(e)
				}
				if e = subtitles.Emit(t.Context(), tx, "unused", 0, "unused", "/unused.mkv", "电影", Digest("unused")); e != nil {
					t.Fatal(e)
				}
				if e = tx.Commit(); e != nil {
					t.Fatal(e)
				}
				if _, e = s.db.Exec(`CREATE TRIGGER refuse_manual_child BEFORE INSERT ON GO_SUBTITLE_JOBS WHEN NEW.JOB_ID LIKE 'manual-subtitle:%' BEGIN SELECT RAISE(IGNORE);END`); e != nil {
					t.Fatal(e)
				}
			} else if where == "parent" {
				if _, e := s.db.Exec(`CREATE TRIGGER refuse_manual_parent BEFORE UPDATE ON GO_MANUAL_SUBTITLE_REQUESTS WHEN NEW.STATE='queued' BEGIN SELECT RAISE(IGNORE);END`); e != nil {
					t.Fatal(e)
				}
			} else {
				tx, e := s.db.BeginTx(t.Context(), nil)
				if e != nil {
					t.Fatal(e)
				}
				if e = bus.PublishSignal(t.Context(), tx, "search.start", "create-outbox", "fixture", map[string]any{}); e != nil {
					t.Fatal(e)
				}
				if e = tx.Commit(); e != nil {
					t.Fatal(e)
				}
				if _, e = s.db.Exec(`CREATE TRIGGER refuse_manual_event BEFORE INSERT ON GO_EVENT_WEBHOOK_OUTBOX WHEN NEW.TYPE='subtitle.download' BEGIN SELECT RAISE(IGNORE);END`); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := s.QueueSubtitleRequest(t.Context(), p.ID, d.Actor, p.Fingerprint, func() error { return nil }); e == nil {
				t.Fatal("partial request reported queued")
			}
			got, e := s.SubtitleRequest(t.Context(), p.ID)
			if e != nil || got.State != "review" || got.Queued {
				t.Fatal(got, e)
			}
			var count int
			exists, e := recoveryTable(t.Context(), s.db, "GO_SUBTITLE_JOBS")
			if e != nil {
				t.Fatal(e)
			}
			if exists {
				e = s.db.QueryRow(`SELECT COUNT(*) FROM GO_SUBTITLE_JOBS WHERE JOB_ID=?`, SubtitleRequestSource(p.ID)).Scan(&count)
			}
			if e != nil || count != 0 {
				t.Fatal("child escaped domain rollback", count, e)
			}
		})
	}
}
func TestManualSubtitleRequestDamagedParentOrChildDoesNotBecomeAValidOriginal(t *testing.T) {
	for _, change := range []string{"definition", "child"} {
		t.Run(change, func(t *testing.T) {
			s, d := manualSubtitleFixture(t, false)
			p := manualSubtitleReview(t, s, d)
			if _, e := s.QueueSubtitleRequest(t.Context(), p.ID, d.Actor, p.Fingerprint, func() error { return nil }); e != nil {
				t.Fatal(e)
			}
			query := `UPDATE GO_MANUAL_SUBTITLE_REQUESTS SET DOCUMENT=REPLACE(DOCUMENT,'Movie.2026.mkv','Other.2026.mkv')`
			if change == "child" {
				query = `UPDATE GO_SUBTITLE_JOBS SET LOCAL_PATH='/foreign/Other.mkv'`
			}
			if _, e := s.db.Exec(query); e != nil {
				t.Fatal(e)
			}
			if _, e := s.SubtitleRequest(t.Context(), p.ID); !errors.Is(e, ErrState) {
				t.Fatal("damaged proof adopted", e)
			}
		})
	}
}
