//go:build linux || darwin

package organization

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sourceRestoreSaved(t *testing.T, s *Store, id, key string) error {
	t.Helper()
	g, err := s.AcquireJob(t.Context(), id)
	if err != nil {
		return err
	}
	defer g.Close()
	j, err := s.Get(t.Context(), id)
	if err != nil {
		return err
	}
	return s.RestoreSource(t.Context(), j, key, "admin", "restore original source", func() error { return nil })
}
func sourceRestoreReview(t *testing.T, s *Store, id string) SourceRestoreReview {
	t.Helper()
	j, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ReviewSourceRestore(t.Context(), j)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestSourceRestoreOrdinaryMovePhasesRetainTargetHistoryAndAllEvidence(t *testing.T) {
	for _, phase := range []string{"moving", "quarantined", "lost-quarantine-receipt", "witness-only", "lost-target"} {
		t.Run(phase, func(t *testing.T) {
			f := newMoveFixture(t)
			f.begin(t)
			if phase == "quarantined" || phase == "lost-quarantine-receipt" || phase == "lost-target" {
				f.quarantine(t)
			}
			if phase == "lost-quarantine-receipt" {
				if _, err := f.s.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='moving' WHERE JOB_ID=?`, f.id); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "witness-only" {
				if err := os.Remove(f.source()); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "lost-target" {
				if err := os.Rename(f.d.TargetRoot, f.d.TargetRoot+".unmounted"); err != nil {
					t.Fatal(err)
				}
			}
			r := sourceRestoreReview(t, f.s, f.id)
			if !r.CanRestore || len(r.Items) != 1 || len(r.Fingerprint) != 64 {
				t.Fatal(r)
			}
			if err := sourceRestoreSaved(t, f.s, f.id, r.Fingerprint); err != nil {
				t.Fatal(err)
			}
			j, err := f.s.Get(t.Context(), f.id)
			if err != nil || j.State != SourceRestored || j.SourceRestore == nil || j.Items[0].State != SourceRestored || !j.SourceRestore.TargetsRetained {
				t.Fatal(j, err)
			}
			assertMoveBytes(t, f.source(), "original")
			info, err := os.Stat(f.source())
			if err != nil || Identity(info) != f.item.Identity {
				t.Fatal("restored wrong source node", err)
			}
			assertMoveBytes(t, f.held("witness"), "original")
			if phase != "lost-target" {
				assertMoveBytes(t, f.target(), "original")
				if _, err := os.Stat(filepath.Join(filepath.Dir(f.target()), f.p.Temp)); err != nil {
					t.Fatal("removed target evidence", err)
				}
			}
			var n int
			if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&n); err != nil || n != 0 {
				t.Fatal("invented history", n, err)
			}
			if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, f.id).Scan(&n); err != nil || n != 1 {
				t.Fatal("released retained target claim", n, err)
			}
			if err := sourceRestoreSaved(t, f.s, f.id, r.Fingerprint); err != nil {
				t.Fatal("lost successful response is not a new restoration", err)
			}
			if won, err := f.s.Claim(t.Context(), f.id, 0); err != nil || won {
				t.Fatal("restarted closed transfer", won, err)
			}
			if err := f.s.Complete(t.Context(), j, j.Items[0]); !errors.Is(err, ErrState) {
				t.Fatal("completed restored transfer", err)
			}
			if err := f.s.VerifyPrepared(t.Context(), f.id, 0, f.p); err == nil {
				t.Fatal("restored transfer still allowed publication")
			}
			b, _ := json.Marshal(j)
			if strings.Contains(string(b), "sourceHoldIdentity") || strings.Contains(string(b), `"Proof"`) || strings.Contains(string(b), "InitialReview") {
				t.Fatal("private restore proof exposed", string(b))
			}
		})
	}
}
func TestSourceRestoreRejectsForeignChangedAndUnjournaledOriginalBeforeIntent(t *testing.T) {
	for _, scenario := range []string{"new-source", "new-symlink", "payload-changed", "payload-replaced", "witness-missing", "hold-replaced", "hold-permissions", "unknown-hold-entry", "source-parent", "completed-history", "running"} {
		t.Run(scenario, func(t *testing.T) {
			f := newMoveFixture(t)
			f.begin(t)
			f.quarantine(t)
			switch scenario {
			case "new-source":
				abandonFixtureBytes(t, f.source(), "new arrival")
			case "new-symlink":
				if err := os.Symlink(f.target(), f.source()); err != nil {
					t.Fatal(err)
				}
			case "payload-changed":
				abandonFixtureBytes(t, f.held("payload"), "changed!")
			case "payload-replaced":
				if err := os.Rename(f.held("payload"), f.held("saved")); err != nil {
					t.Fatal(err)
				}
				abandonFixtureBytes(t, f.held("payload"), "original")
			case "witness-missing":
				if err := os.Remove(f.held("witness")); err != nil {
					t.Fatal(err)
				}
			case "hold-replaced":
				dir := filepath.Dir(f.held("payload"))
				if err := os.Rename(dir, dir+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			case "hold-permissions":
				if err := os.Chmod(filepath.Dir(f.held("payload")), 0755); err != nil {
					t.Fatal(err)
				}
			case "unknown-hold-entry":
				abandonFixtureBytes(t, f.held("foreign"), "keep")
			case "source-parent":
				if err := os.Rename(f.d.SourceRoot, f.d.SourceRoot+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(f.d.SourceRoot, 0700); err != nil {
					t.Fatal(err)
				}
			case "completed-history":
				f.complete(t)
			case "running":
				if _, err := f.s.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running',PROOF='{}' WHERE JOB_ID=?`, f.id); err != nil {
					t.Fatal(err)
				}
			}
			r := sourceRestoreReview(t, f.s, f.id)
			if r.CanRestore {
				t.Fatal("unsafe source restore eligible", scenario, r)
			}
			if err := sourceRestoreSaved(t, f.s, f.id, r.Fingerprint); err == nil {
				t.Fatal("unsafe restoration applied", scenario)
			}
			var n int
			if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_SOURCE_RESTORE`).Scan(&n); err != nil || n != 0 {
				t.Fatal("intent saved without complete evidence", n, err)
			}
			if scenario == "new-source" {
				assertMoveBytes(t, f.source(), "new arrival")
			}
			if scenario == "unknown-hold-entry" {
				assertMoveBytes(t, f.held("foreign"), "keep")
			}
		})
	}
}
func TestSourceRestoreIntentAndReceiptsSurviveInterruptionWithoutRepeatingRename(t *testing.T) {
	for _, point := range []string{"intent", "item-receipt", "final-receipt"} {
		t.Run(point, func(t *testing.T) {
			f := newMoveFixture(t)
			f.begin(t)
			f.quarantine(t)
			r := sourceRestoreReview(t, f.s, f.id)
			trigger := `CREATE TRIGGER fail_restore BEFORE UPDATE OF STATE ON GO_ORGANIZATION_JOBS WHEN NEW.STATE='source_restoring' BEGIN SELECT RAISE(ABORT,'fixture'); END`
			if point == "item-receipt" {
				trigger = `CREATE TRIGGER fail_restore BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='source_restored' BEGIN SELECT RAISE(ABORT,'fixture'); END`
			}
			if point == "final-receipt" {
				trigger = `CREATE TRIGGER fail_restore BEFORE UPDATE OF STATE ON GO_ORGANIZATION_JOBS WHEN NEW.STATE='source_restored' BEGIN SELECT RAISE(ABORT,'fixture'); END`
			}
			if _, err := f.s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if err := sourceRestoreSaved(t, f.s, f.id, r.Fingerprint); err == nil {
				t.Fatal("lost receipt accepted")
			}
			j, err := f.s.Get(t.Context(), f.id)
			if err != nil {
				t.Fatal(err)
			}
			if point == "intent" {
				if j.State != "needs_review" {
					t.Fatal(j)
				}
				assertMoveAbsent(t, f.source())
				assertMoveBytes(t, f.held("payload"), "original")
			} else {
				if j.State != SourceRestoring {
					t.Fatal(j)
				}
				assertMoveBytes(t, f.source(), "original")
				assertMoveAbsent(t, f.held("payload"))
			}
			if _, err := f.s.db.Exec(`DROP TRIGGER fail_restore`); err != nil {
				t.Fatal(err)
			}
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := OpenStore(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			fresh := sourceRestoreReview(t, s, f.id)
			if !fresh.CanRestore {
				t.Fatal(fresh)
			}
			if err := sourceRestoreSaved(t, s, f.id, fresh.Fingerprint); err != nil {
				t.Fatal(err)
			}
			j, err = s.Get(t.Context(), f.id)
			if err != nil || j.State != SourceRestored {
				t.Fatal(j, err)
			}
			assertMoveBytes(t, f.source(), "original")
			assertMoveBytes(t, f.target(), "original")
		})
	}
}
func TestSourceRestoreCurrentCallbackRacesCannotOverwriteNewObjects(t *testing.T) {
	for _, race := range []string{"new-source", "hold-swap", "journal-revoked", "cancelled"} {
		t.Run(race, func(t *testing.T) {
			f := newMoveFixture(t)
			f.begin(t)
			f.quarantine(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, _, err := inspectSourceRestore(ctx, f.d, f.item, f.p, true, func() error {
				switch race {
				case "new-source":
					abandonFixtureBytes(t, f.source(), "new arrival")
				case "hold-swap":
					dir := filepath.Dir(f.held("payload"))
					if err := os.Rename(dir, dir+".saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
				case "journal-revoked":
					return ErrState
				case "cancelled":
					cancel()
				}
				return nil
			})
			if err == nil {
				t.Fatal("restored over callback race", race)
			}
			if race == "new-source" {
				assertMoveBytes(t, f.source(), "new arrival")
			} else {
				assertMoveAbsent(t, f.source())
			}
			assertMoveBytes(t, f.target(), "original")
		})
	}
}

func TestSourceRestoreBluRayWholeDirectoryAndEmptyHoldReceiptRecovery(t *testing.T) {
	for _, phase := range []string{"moving", "quarantined", "new-source", "changed-inner", "lost-receipt"} {
		t.Run(phase, func(t *testing.T) {
			d, e, files := discFixture(t, "move")
			s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id, err := s.Create(t.Context(), Digest(d), d)
			if err != nil {
				t.Fatal(err)
			}
			if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
				t.Fatal(won, err)
			}
			p, err := PrepareMoveTarget(t.Context(), d, e, TempName(id, 0), func(p Proof) error { return s.Prepare(t.Context(), id, 0, p) })
			if err != nil {
				t.Fatal(err)
			}
			p, err = PrepareMove(t.Context(), d, e, p, MoveHoldName(id, 0))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.BeginMove(t.Context(), id, 0, p); err != nil {
				t.Fatal(err)
			}
			if phase != "moving" {
				if err := ContinueMove(t.Context(), d, e, p, false, func() error { return s.MarkQuarantined(t.Context(), id, 0, p) }); err != nil {
					t.Fatal(err)
				}
			}
			source := filepath.Join(d.SourceRoot, e.Source)
			hold := filepath.Join(filepath.Dir(source), p.SourceHold)
			if phase == "new-source" {
				if err := os.Mkdir(source, 0700); err != nil {
					t.Fatal(err)
				}
				abandonFixtureBytes(t, filepath.Join(source, "foreign.txt"), "keep")
			}
			if phase == "changed-inner" {
				abandonFixtureBytes(t, filepath.Join(hold, "payload/BDMV/index.bdmv"), "changed")
			}
			r := sourceRestoreReview(t, s, id)
			if phase == "new-source" || phase == "changed-inner" {
				if r.CanRestore {
					t.Fatal("unsafe whole disc restored", r)
				}
				return
			}
			if !r.CanRestore {
				t.Fatal(r)
			}
			if phase == "lost-receipt" {
				if _, err := s.db.Exec(`CREATE TRIGGER lose_disc_restore BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='source_restored' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
				if err := sourceRestoreSaved(t, s, id, r.Fingerprint); err == nil {
					t.Fatal("lost whole-disc receipt ignored")
				}
				if _, err := s.db.Exec(`DROP TRIGGER lose_disc_restore`); err != nil {
					t.Fatal(err)
				}
				r = sourceRestoreReview(t, s, id)
			}
			if err := sourceRestoreSaved(t, s, id, r.Fingerprint); err != nil {
				t.Fatal(err)
			}
			discAssertTree(t, source, files)
			discAssertTree(t, filepath.Join(d.TargetRoot, e.Target), files)
			if info, err := os.Stat(source); err != nil || Identity(info) != e.Identity {
				t.Fatal("restored wrong directory", err)
			}
			if _, err := os.Stat(hold); err != nil {
				t.Fatal("empty original hold was deleted", err)
			}
			j, err := s.Get(t.Context(), id)
			if err != nil || j.State != SourceRestored {
				t.Fatal(j, err)
			}
		})
	}
}
