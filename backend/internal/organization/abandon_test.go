//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func abandonSaved(t *testing.T, s *Store, id string) error {
	t.Helper()
	guard, err := s.AcquireJob(t.Context(), id)
	if err != nil {
		return err
	}
	defer guard.Close()
	job, err := s.Get(t.Context(), id)
	if err != nil {
		return err
	}
	return s.AbandonUnpublished(t.Context(), job, func() error { return nil })
}

func abandonFixtureBytes(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonUnpublishedPreservesSourceAndHistoryAndReleasesOnlyAfterReceipt(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			d, item, s, id, p := publicationFixture(t, mode)
			if err := abandonSaved(t, s, id); err != nil {
				t.Fatal(err)
			}
			job, err := s.Get(t.Context(), id)
			if err != nil || job.State != "abandoned" || job.Items[0].State != "abandoned" || job.Items[0].Proof != p {
				t.Fatal(job, err)
			}
			assertMoveBytes(t, filepath.Join(d.SourceRoot, item.Source), "original")
			assertMoveAbsent(t, filepath.Join(d.TargetRoot, item.Target))
			assertMoveAbsent(t, filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp))
			var n int
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, id).Scan(&n); err != nil || n != 0 {
				t.Fatal(n, err)
			}
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&n); err != nil || n != 0 {
				t.Fatal("invented successful history", n, err)
			}
			if _, err = s.Create(t.Context(), Digest(d), d); err != nil {
				t.Fatal("claim not released", err)
			}
			if ok, err := s.Claim(t.Context(), id, 0); err != nil || ok {
				t.Fatal("restarted abandoned job", ok, err)
			}
			if err = s.Complete(t.Context(), job, job.Items[0]); !errors.Is(err, ErrState) {
				t.Fatal("completed abandoned job", err)
			}
			if err = PublishPrepared(t.Context(), d, item, p, func() error { return s.VerifyPrepared(t.Context(), id, 0, p) }); err == nil {
				t.Fatal("published after abandonment")
			}
		})
	}
}

func TestAbandonUnpublishedRejectsInsufficientEvidenceBeforeIntent(t *testing.T) {
	for _, kind := range []string{"source-changed", "source-missing", "target", "target-symlink", "payload-missing", "payload-changed", "unknown-file", "stage-permissions", "stage-missing", "running", "source-hold", "config"} {
		t.Run(kind, func(t *testing.T) {
			d, item, s, id, p := publicationFixture(t, "copy")
			stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp)
			switch kind {
			case "source-changed":
				abandonFixtureBytes(t, filepath.Join(d.SourceRoot, item.Source), "changed!")
			case "source-missing":
				if err := os.Remove(filepath.Join(d.SourceRoot, item.Source)); err != nil {
					t.Fatal(err)
				}
			case "target":
				abandonFixtureBytes(t, filepath.Join(d.TargetRoot, item.Target), "original")
			case "target-symlink":
				if err := os.Symlink(filepath.Join(d.SourceRoot, item.Source), filepath.Join(d.TargetRoot, item.Target)); err != nil {
					t.Fatal(err)
				}
			case "payload-missing":
				if err := os.Remove(filepath.Join(stage, "payload")); err != nil {
					t.Fatal(err)
				}
			case "payload-changed":
				abandonFixtureBytes(t, filepath.Join(stage, "payload"), "changed!")
			case "unknown-file":
				abandonFixtureBytes(t, filepath.Join(stage, "foreign"), "foreign")
			case "stage-permissions":
				if err := os.Chmod(stage, 0755); err != nil {
					t.Fatal(err)
				}
			case "stage-missing":
				if err := os.Rename(stage, stage+".saved"); err != nil {
					t.Fatal(err)
				}
			case "running":
				if _, err := s.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running' WHERE JOB_ID=?`, id); err != nil {
					t.Fatal(err)
				}
			case "source-hold":
				if _, err := s.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET PROOF=json_set(PROOF,'$.sourceHold','unknown') WHERE JOB_ID=?`, id); err != nil {
					t.Fatal(err)
				}
			}
			job, err := s.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			err = s.AbandonUnpublished(t.Context(), job, func() error {
				if kind == "config" {
					return ErrState
				}
				return nil
			})
			if err == nil {
				t.Fatal("accepted", kind)
			}
			var state string
			var n int
			if err = s.db.QueryRow(`SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&state); err != nil || state != "active" {
				t.Fatal(state, err)
			}
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, id).Scan(&n); err != nil || n != 1 {
				t.Fatal(n, err)
			}
			if kind == "unknown-file" {
				assertMoveBytes(t, filepath.Join(stage, "foreign"), "foreign")
				assertMoveBytes(t, filepath.Join(stage, "payload"), "original")
			}
		})
	}
}

func TestAbandonUnpublishedDurableIntentAndInterruptedReceipts(t *testing.T) {
	for _, point := range []string{"intent", "item-receipt", "claim-release"} {
		t.Run(point, func(t *testing.T) {
			d, item, s, id, p := publicationFixture(t, "copy")
			trigger := `CREATE TRIGGER fail_abandon BEFORE UPDATE OF STATE ON GO_ORGANIZATION_JOBS WHEN NEW.STATE='abandoning' BEGIN SELECT RAISE(ABORT,'fixture'); END`
			if point == "item-receipt" {
				trigger = `CREATE TRIGGER fail_abandon BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='abandoned' BEGIN SELECT RAISE(ABORT,'fixture'); END`
			}
			if point == "claim-release" {
				trigger = `CREATE TRIGGER fail_abandon BEFORE DELETE ON GO_ORGANIZATION_TARGETS BEGIN SELECT RAISE(ABORT,'fixture'); END`
			}
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if err := abandonSaved(t, s, id); err == nil {
				t.Fatal("ignored durable receipt failure")
			}
			job, err := s.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp)
			if point == "intent" {
				assertMoveBytes(t, filepath.Join(stage, "payload"), "original")
				if job.State != "needs_review" {
					t.Fatal(job.State)
				}
			} else {
				assertMoveAbsent(t, stage)
				if job.State != "abandoning" {
					t.Fatal(job.State)
				}
			}
			var count int
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, id).Scan(&count); err != nil || count != 1 {
				t.Fatal("early reservation release", count, err)
			}
			if _, err = s.db.Exec(`DROP TRIGGER fail_abandon`); err != nil {
				t.Fatal(err)
			}
			// Reopen from disk; no in-memory cleanup receipt can complete recovery.
			other, err := OpenStore(filepath.Join(s.guardRoot, "user.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			if err = abandonSaved(t, other, id); err != nil {
				t.Fatal(err)
			}
			job, err = other.Get(t.Context(), id)
			if err != nil || job.State != "abandoned" {
				t.Fatal(job.State, err)
			}
			assertMoveBytes(t, filepath.Join(d.SourceRoot, item.Source), "original")
		})
	}
}

func TestAbandonRevalidatesAfterCallbackAndNeverDeletesUnknownReplacement(t *testing.T) {
	d, item, s, id, p := publicationFixture(t, "copy")
	job, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.beginAbandon(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp)
	err = inspectUnpublished(t.Context(), d, item, p, true, func() error {
		if err := os.Rename(stage, stage+".original"); err != nil {
			return err
		}
		if err := os.Mkdir(stage, 0700); err != nil {
			return err
		}
		abandonFixtureBytes(t, filepath.Join(stage, "payload"), "foreign")
		return nil
	})
	if err == nil {
		t.Fatal("deleted unknown replacement")
	}
	assertMoveBytes(t, filepath.Join(stage, "payload"), "foreign")
	assertMoveBytes(t, filepath.Join(stage+".original", "payload"), "original")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = s.AbandonUnpublished(ctx, job, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestAbandonResumeNeverUsesMissingStageAsSourcePreservationEvidence(t *testing.T) {
	d, item, s, id, p := publicationFixture(t, "copy")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_abandon BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='abandoned' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := abandonSaved(t, s, id); err == nil {
		t.Fatal("missing receipt failure")
	}
	assertMoveAbsent(t, filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp))
	if _, err := s.db.Exec(`DROP TRIGGER fail_abandon`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(d.SourceRoot, item.Source)); err != nil {
		t.Fatal(err)
	}
	if err := abandonSaved(t, s, id); err == nil {
		t.Fatal("released with missing source")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, id).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	job, err := s.Get(t.Context(), id)
	if err != nil || job.State != "abandoning" || job.Items[0].State != "discarding" {
		t.Fatal(job, err)
	}
}
