//go:build linux || darwin

package organization

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func publicationFixture(t *testing.T, mode string) (Definition, Entry, *Store, string, Proof) {
	t.Helper()
	d, item := copyFixture(t)
	d.Mode = mode
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
		t.Fatal(won, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	transfer := Transfer
	if mode == "move" {
		transfer = PrepareMoveTarget
	}
	p, err := transfer(ctx, d, item, TempName(id, 0), func(p Proof) error {
		if err := s.Prepare(ctx, id, 0, p); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertMoveAbsent(t, filepath.Join(d.TargetRoot, item.Target))
	return d, item, s, id, p
}

func TestPublicationContinuesOriginalPreparedObjectWithLiteralModeAndNoSourceRemoval(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			d, item, s, id, p := publicationFixture(t, mode)
			stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp, "payload")
			before, err := os.Lstat(stage)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := PublishPrepared(t.Context(), d, item, p, func() error { return s.VerifyPrepared(t.Context(), id, 0, p) }); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.Lstat(filepath.Join(d.TargetRoot, item.Target))
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("re-copied staged object", err)
			}
			assertMoveBytes(t, filepath.Join(d.SourceRoot, item.Source), "original")
			assertMoveBytes(t, filepath.Join(d.TargetRoot, item.Target), "original")
			if (mode == "softlink") != (after.Mode()&os.ModeSymlink != 0) {
				t.Fatal("mode fallback", mode, after.Mode())
			}
			job, err := s.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "move" {
				if err = s.Complete(t.Context(), job, job.Items[0]); !errors.Is(err, ErrState) {
					t.Fatal("publication completed move", err)
				}
				return
			}
			if err = s.Complete(t.Context(), job, job.Items[0]); err != nil {
				t.Fatal(err)
			}
			if err = s.Complete(t.Context(), job, job.Items[0]); err != nil {
				t.Fatal(err)
			}
			var count int
			var label string
			if err = s.db.QueryRow(`SELECT COUNT(*),MODE FROM TRANSFER_HISTORY`).Scan(&count, &label); err != nil || count != 1 || label != ModeLabel(mode) {
				t.Fatal(count, label, err)
			}
		})
	}
}

func TestPublicationRejectsUnknownOrChangedEvidenceWithoutWritingTarget(t *testing.T) {
	for _, kind := range []string{"source-changed", "source-replaced", "payload-missing", "payload-replaced", "payload-changed", "stage-permissions", "stage-replaced", "source-root", "parent-proof", "digest", "move-proof", "running", "ledger-proof", "nil-callback"} {
		t.Run(kind, func(t *testing.T) {
			d, item, s, id, p := publicationFixture(t, "copy")
			stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp)
			switch kind {
			case "source-changed":
				if err := os.WriteFile(filepath.Join(d.SourceRoot, item.Source), []byte("changed!"), 0600); err != nil {
					t.Fatal(err)
				}
			case "source-replaced":
				path := filepath.Join(d.SourceRoot, item.Source)
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				fixtureFile(t, d.SourceRoot, item.Source)
			case "payload-missing":
				if err := os.Remove(filepath.Join(stage, "payload")); err != nil {
					t.Fatal(err)
				}
			case "payload-replaced":
				path := filepath.Join(stage, "payload")
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
			case "payload-changed":
				if err := os.WriteFile(filepath.Join(stage, "payload"), []byte("changed!"), 0600); err != nil {
					t.Fatal(err)
				}
			case "stage-permissions":
				if err := os.Chmod(stage, 0755); err != nil {
					t.Fatal(err)
				}
			case "stage-replaced":
				if err := os.Rename(stage, stage+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(stage, 0700); err != nil {
					t.Fatal(err)
				}
			case "source-root":
				d.SourceIdentity = "foreign"
			case "parent-proof":
				p.ParentIdentity = "foreign"
			case "digest":
				p.Digest = "unknown"
			case "move-proof":
				p.SourceHold = ".nastool-move-unknown.hold"
			case "running":
				if _, err := s.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running' WHERE JOB_ID=?`, id); err != nil {
					t.Fatal(err)
				}
			case "ledger-proof":
				if _, err := s.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET PROOF='{}' WHERE JOB_ID=?`, id); err != nil {
					t.Fatal(err)
				}
			}
			callback := func() error { return s.VerifyPrepared(t.Context(), id, 0, p) }
			if kind == "nil-callback" {
				callback = nil
			}
			if err := PublishPrepared(t.Context(), d, item, p, callback); err == nil {
				t.Fatal("invalid recovery accepted", kind)
			}
			assertMoveAbsent(t, filepath.Join(d.TargetRoot, item.Target))
		})
	}
}

func TestPublicationRechecksSourceAndStageAfterConfirmationCallback(t *testing.T) {
	for _, kind := range []string{"source-content", "stage-content", "parent-replaced", "cancelled", "ledger-failed", "target-conflict"} {
		t.Run(kind, func(t *testing.T) {
			d, item, s, id, p := publicationFixture(t, "copy")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := PublishPrepared(ctx, d, item, p, func() error {
				if err := s.VerifyPrepared(ctx, id, 0, p); err != nil {
					return err
				}
				switch kind {
				case "source-content", "stage-content":
					path := filepath.Join(d.SourceRoot, item.Source)
					if kind == "stage-content" {
						path = filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp, "payload")
					}
					info, err := os.Stat(path)
					if err != nil {
						return err
					}
					if err = os.WriteFile(path, []byte("changed!"), 0600); err != nil {
						return err
					}
					return os.Chtimes(path, info.ModTime(), info.ModTime())
				case "parent-replaced":
					parent := filepath.Dir(filepath.Join(d.TargetRoot, item.Target))
					if err := os.Rename(parent, parent+".saved"); err != nil {
						return err
					}
					return os.Mkdir(parent, 0700)
				case "cancelled":
					cancel()
				case "ledger-failed":
					return errors.New("fixture database unavailable")
				case "target-conflict":
					return os.WriteFile(filepath.Join(d.TargetRoot, item.Target), []byte("foreign"), 0600)
				}
				return nil
			})
			if err == nil {
				t.Fatal("late mutation accepted", kind)
			}
			if kind == "target-conflict" {
				if !errors.Is(err, ErrClaimed) {
					t.Fatal(err)
				}
				assertMoveBytes(t, filepath.Join(d.TargetRoot, item.Target), "foreign")
			} else {
				assertMoveAbsent(t, filepath.Join(d.TargetRoot, item.Target))
			}
		})
	}
}

func TestPublicationSoftlinkRejectsForgedObjectOrSourceWithoutFollowingIt(t *testing.T) {
	for _, kind := range []string{"same-target-foreign-link", "different-target", "source-replaced"} {
		t.Run(kind, func(t *testing.T) {
			d, item, s, id, p := publicationFixture(t, "softlink")
			path := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp, "payload")
			if kind == "source-replaced" {
				source := filepath.Join(d.SourceRoot, item.Source)
				if err := os.Rename(source, source+".saved"); err != nil {
					t.Fatal(err)
				}
				fixtureFile(t, d.SourceRoot, item.Source)
			} else {
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				target := p.LinkTarget
				if kind == "different-target" {
					target = filepath.Join(t.TempDir(), "outside")
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := PublishPrepared(t.Context(), d, item, p, func() error { return s.VerifyPrepared(t.Context(), id, 0, p) }); err == nil {
				t.Fatal("foreign link accepted")
			}
			assertMoveAbsent(t, filepath.Join(d.TargetRoot, item.Target))
		})
	}
}

func TestPublicationConcurrentRequestsOnlyLinkTheOwnedObjectOnce(t *testing.T) {
	d, item, s, id, p := publicationFixture(t, "copy")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := PublishPrepared(t.Context(), d, item, p, func() error { return s.VerifyPrepared(t.Context(), id, 0, p) }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	assertMoveBytes(t, filepath.Join(d.TargetRoot, item.Target), "original")
	job, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(t.Context(), job, job.Items[0]); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationPreservesLegacyCopyProofWithoutObjectType(t *testing.T) {
	d, item, s, id, p := publicationFixture(t, "copy")
	p.Kind = ""
	raw, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET PROOF=? WHERE JOB_ID=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	if err = PublishPrepared(t.Context(), d, item, p, func() error { return s.VerifyPrepared(t.Context(), id, 0, p) }); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationCanRaceOriginalExecutorWithoutDuplicateHistoryOrRollback(t *testing.T) {
	d, item := copyFixture(t)
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
	_, err = Transfer(t.Context(), d, item, TempName(id, 0), func(p Proof) error {
		if err := s.Prepare(t.Context(), id, 0, p); err != nil {
			return err
		}
		if err := PublishPrepared(t.Context(), d, item, p, func() error { return s.VerifyPrepared(t.Context(), id, 0, p) }); err != nil {
			return err
		}
		job, err := s.Get(t.Context(), id)
		if err != nil {
			return err
		}
		if err = s.Complete(t.Context(), job, job.Items[0]); err != nil {
			return err
		}
		return Cleanup(d, item, p)
	})
	if err == nil {
		t.Fatal("original executor ignored its already-removed private stage")
	}
	if err = s.RecordError(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	job, err := s.Get(t.Context(), id)
	if err != nil || job.State != "completed" || job.Items[0].Reason != "" {
		t.Fatal(job, err)
	}
	assertMoveBytes(t, filepath.Join(d.SourceRoot, item.Source), "original")
	assertMoveBytes(t, filepath.Join(d.TargetRoot, item.Target), "original")
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
