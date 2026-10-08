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

	"golang.org/x/sys/unix"
)

type moveFixture struct {
	d          Definition
	item       Entry
	s          *Store
	id, dbPath string
	p          Proof
}

func newMoveFixture(t *testing.T) moveFixture {
	t.Helper()
	d, item := copyFixture(t)
	path := filepath.Join(t.TempDir(), "user.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	// Production job creation still rejects move. Seed an internal-only job
	// to verify the future ledger without opening an unconfirmed deletion API.
	d.Mode = "move"
	raw, _ := json.Marshal(d)
	if _, err = s.db.Exec(`UPDATE GO_ORGANIZATION_JOBS SET DEFINITION=? WHERE ID=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
		t.Fatal(won, err)
	}
	p, err := PrepareMoveTarget(t.Context(), d, item, TempName(id, 0), func(p Proof) error { return s.Prepare(t.Context(), id, 0, p) })
	if err != nil {
		t.Fatal(err)
	}
	p, err = PrepareMove(t.Context(), d, item, p, MoveHoldName(id, 0))
	if err != nil {
		t.Fatal(err)
	}
	return moveFixture{d, item, s, id, path, p}
}

func (f moveFixture) source() string { return filepath.Join(f.d.SourceRoot, f.item.Source) }
func (f moveFixture) target() string { return filepath.Join(f.d.TargetRoot, f.item.Target) }
func (f moveFixture) held(name string) string {
	return filepath.Join(filepath.Dir(f.source()), f.p.SourceHold, name)
}
func (f moveFixture) begin(t *testing.T) {
	t.Helper()
	if err := f.s.BeginMove(t.Context(), f.id, 0, f.p); err != nil {
		t.Fatal(err)
	}
}
func (f moveFixture) quarantine(t *testing.T) {
	t.Helper()
	if err := ContinueMove(t.Context(), f.d, f.item, f.p, false, func() error { return f.s.MarkQuarantined(t.Context(), f.id, 0, f.p) }); err != nil {
		t.Fatal(err)
	}
}
func (f moveFixture) complete(t *testing.T) {
	t.Helper()
	job, err := f.s.Get(t.Context(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.Complete(t.Context(), job, job.Items[0]); err != nil {
		t.Fatal(err)
	}
}
func assertMoveBytes(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatal(path, string(data), err)
	}
}
func assertMoveAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unexpected object", path, err)
	}
}

func TestMoveFoundationPreservesSourceUntilIntentAndCommitsOneHistory(t *testing.T) {
	f := newMoveFixture(t)
	assertMoveBytes(t, f.source(), "original")
	assertMoveBytes(t, f.target(), "original")
	assertMoveBytes(t, f.held("witness"), "original")
	sourceInfo, _ := os.Stat(f.source())
	targetInfo, _ := os.Stat(f.target())
	if os.SameFile(sourceInfo, targetInfo) {
		t.Fatal("move target must have independent bytes")
	}
	job, _ := f.s.Get(t.Context(), f.id)
	if err := f.s.Complete(t.Context(), job, job.Items[0]); !errors.Is(err, ErrState) {
		t.Fatal("completed before source quarantine", err)
	}
	if err := CleanupMovedSource(t.Context(), f.d, f.item, f.p); !errors.Is(err, ErrState) {
		t.Fatal("cleaned still-named source", err)
	}
	f.begin(t)
	f.quarantine(t)
	assertMoveAbsent(t, f.source())
	for _, name := range []string{"payload", "witness"} {
		assertMoveBytes(t, f.held(name), "original")
	}
	f.complete(t)
	f.complete(t)
	var count int
	var mode string
	if err := f.s.db.QueryRow(`SELECT COUNT(*),MODE FROM TRANSFER_HISTORY`).Scan(&count, &mode); err != nil || count != 1 || mode != "移动" {
		t.Fatal(count, mode, err)
	}
	if err := CleanupMovedSource(t.Context(), f.d, f.item, f.p); err != nil {
		t.Fatal(err)
	}
	assertMoveAbsent(t, filepath.Dir(f.held("payload")))
	if err := Cleanup(f.d, f.item, f.p); err != nil {
		t.Fatal(err)
	}
	assertMoveBytes(t, f.target(), "original")
	job, _ = f.s.Get(t.Context(), f.id)
	if job.State != "completed" {
		t.Fatal(job)
	}
}

func TestMoveInterruptedRenameRecoversFromPositiveProofAfterReopen(t *testing.T) {
	f := newMoveFixture(t)
	f.begin(t)
	interrupted := errors.New("fixture interrupted after rename")
	err := ContinueMove(t.Context(), f.d, f.item, f.p, false, func() error { return interrupted })
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	assertMoveAbsent(t, f.source())
	assertMoveBytes(t, f.held("payload"), "original")
	if err = f.s.RecordError(t.Context(), f.id, 0); err != nil {
		t.Fatal(err)
	}
	f.s.Close()
	s, err := OpenStore(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f.s = s
	job, err := s.Get(t.Context(), f.id)
	if err != nil || job.Items[0].State != "moving" || job.Items[0].Reason == "" {
		t.Fatal(job, err)
	}
	if err = s.Complete(t.Context(), job, job.Items[0]); !errors.Is(err, ErrState) {
		t.Fatal("missing quarantine state accepted", err)
	}
	f.quarantine(t)
	f.complete(t)
	if err = CleanupMovedSource(t.Context(), f.d, f.item, f.p); err != nil {
		t.Fatal(err)
	}
	assertMoveBytes(t, f.target(), "original")
}

func TestMoveHistoryFailureKeepsRecoveryBytesAndReservedProof(t *testing.T) {
	f := newMoveFixture(t)
	f.begin(t)
	f.quarantine(t)
	if _, err := f.s.db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY, MODE TEXT, TYPE TEXT, CATEGORY TEXT, TMDBID INTEGER, TITLE TEXT, YEAR TEXT, SEASON_EPISODE TEXT, SOURCE TEXT, SOURCE_PATH TEXT, SOURCE_FILENAME TEXT, DEST TEXT, DEST_PATH TEXT, DEST_FILENAME TEXT, DATE TEXT); CREATE TRIGGER deny_history BEFORE INSERT ON TRANSFER_HISTORY BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	job, _ := f.s.Get(t.Context(), f.id)
	if err := f.s.Complete(t.Context(), job, job.Items[0]); err == nil {
		t.Fatal("history failure ignored")
	}
	job, _ = f.s.Get(t.Context(), f.id)
	if job.Items[0].State != "quarantined" || job.State != "needs_review" {
		t.Fatal(job)
	}
	for _, name := range []string{"payload", "witness"} {
		assertMoveBytes(t, f.held(name), "original")
	}
	if err := f.s.Cancel(t.Context(), f.id); !errors.Is(err, ErrState) {
		t.Fatal("uncertain claim released", err)
	}
	if _, err := f.s.db.Exec(`DROP TRIGGER deny_history`); err != nil {
		t.Fatal(err)
	}
	if err := ContinueMove(t.Context(), f.d, f.item, f.p, true, nil); err != nil {
		t.Fatal(err)
	}
	f.complete(t)
	f.complete(t)
	var count int
	f.s.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
}

func TestMoveNeverDeletesNewArrivalAtOriginalSourceName(t *testing.T) {
	f := newMoveFixture(t)
	f.begin(t)
	f.quarantine(t)
	if err := os.WriteFile(f.source(), []byte("new arrival"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyQuarantined(t.Context(), f.d, f.item, f.p); err != nil {
		t.Fatal(err)
	}
	f.complete(t)
	if err := CleanupMovedSource(t.Context(), f.d, f.item, f.p); err != nil {
		t.Fatal(err)
	}
	assertMoveBytes(t, f.source(), "new arrival")
	assertMoveBytes(t, f.target(), "original")
}

func TestMoveRenameRaceRestoresCapturedForeignObjectWithoutDeletingIt(t *testing.T) {
	f := newMoveFixture(t)
	f.begin(t)
	err := continueMove(t.Context(), f.d, f.item, f.p, false, func() error { t.Fatal("foreign source committed"); return nil }, func() error {
		if err := os.Rename(f.source(), f.source()+".saved"); err != nil {
			return err
		}
		return os.WriteFile(f.source(), []byte("foreign"), 0600)
	})
	if !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	assertMoveBytes(t, f.source(), "foreign")
	assertMoveBytes(t, f.source()+".saved", "original")
	assertMoveBytes(t, f.held("witness"), "original")
	assertMoveAbsent(t, f.held("payload"))
}

func TestMoveRejectsForeignQuarantineObjectsAndDoesNotRestoreThem(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			f := newMoveFixture(t)
			f.begin(t)
			switch kind {
			case "regular":
				if err := os.WriteFile(f.held("payload"), []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(f.target(), f.held("payload")); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(f.held("payload"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.Lstat(f.held("payload"))
			if err := ContinueMove(t.Context(), f.d, f.item, f.p, false, func() error { t.Fatal("foreign object committed"); return nil }); !errors.Is(err, ErrState) {
				t.Fatal(err)
			}
			after, err := os.Lstat(f.held("payload"))
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("foreign object moved/deleted", err)
			}
			assertMoveBytes(t, f.source(), "original")
		})
	}
}

func TestMoveRejectsMissingOrChangedProofBeforeAnySourceDisposition(t *testing.T) {
	for _, kind := range []string{"cancelled", "witness-missing", "target-replaced", "hold-replaced", "hold-permissions", "source-replaced", "payload-missing"} {
		t.Run(kind, func(t *testing.T) {
			f := newMoveFixture(t)
			f.begin(t)
			ctx := t.Context()
			switch kind {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "witness-missing":
				if err := os.Remove(f.held("witness")); err != nil {
					t.Fatal(err)
				}
			case "target-replaced":
				if err := os.Rename(f.target(), f.target()+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.target(), []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
			case "hold-replaced":
				hold := filepath.Dir(f.held("payload"))
				if err := os.Rename(hold, hold+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(hold, 0700); err != nil {
					t.Fatal(err)
				}
			case "hold-permissions":
				if err := os.Chmod(filepath.Dir(f.held("payload")), 0755); err != nil {
					t.Fatal(err)
				}
			case "source-replaced":
				if err := os.Rename(f.source(), f.source()+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.source(), []byte("new arrival"), 0600); err != nil {
					t.Fatal(err)
				}
			case "payload-missing":
				if err := os.Rename(f.source(), f.source()+".saved"); err != nil {
					t.Fatal(err)
				}
			}
			if err := ContinueMove(ctx, f.d, f.item, f.p, false, func() error { t.Fatal("invalid proof committed"); return nil }); err == nil {
				t.Fatal("invalid proof accepted")
			}
			if kind == "source-replaced" {
				assertMoveBytes(t, f.source(), "new arrival")
			} else if kind != "payload-missing" {
				assertMoveBytes(t, f.source(), "original")
			}
			job, _ := f.s.Get(t.Context(), f.id)
			if job.Items[0].State != "moving" {
				t.Fatal(job)
			}
		})
	}
}

func TestMoveCleanupPreservesBackupWhenTargetProofFailsAndUnknownFiles(t *testing.T) {
	for _, kind := range []string{"target-lost", "original-restored", "unknown-file"} {
		t.Run(kind, func(t *testing.T) {
			f := newMoveFixture(t)
			f.begin(t)
			f.quarantine(t)
			f.complete(t)
			switch kind {
			case "target-lost":
				if err := os.Remove(f.target()); err != nil {
					t.Fatal(err)
				}
			case "original-restored":
				if err := os.Link(f.held("payload"), f.source()); err != nil {
					t.Fatal(err)
				}
			case "unknown-file":
				if err := os.WriteFile(f.held("unrelated"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := CleanupMovedSource(t.Context(), f.d, f.item, f.p); err == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			if kind == "unknown-file" {
				assertMoveBytes(t, f.held("unrelated"), "keep")
				if err := os.Remove(f.held("unrelated")); err != nil {
					t.Fatal(err)
				}
				if err := CleanupMovedSource(t.Context(), f.d, f.item, f.p); err != nil {
					t.Fatal("partial cleanup could not resume", err)
				}
			} else {
				for _, name := range []string{"payload", "witness"} {
					assertMoveBytes(t, f.held(name), "original")
				}
			}
		})
	}
}

func TestMoveLedgerRequiresMatchingPreparedProofAndOneIntentWinner(t *testing.T) {
	f := newMoveFixture(t)
	tampered := f.p
	tampered.Digest = "changed"
	if err := f.s.BeginMove(t.Context(), f.id, 0, tampered); !errors.Is(err, ErrState) {
		t.Fatal("changed copy proof accepted", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.s.BeginMove(t.Context(), f.id, 0, f.p)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if !errors.Is(err, ErrState) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatal(wins)
	}
	tampered = f.p
	tampered.SourceHoldIdentity = "foreign"
	if err := f.s.MarkQuarantined(t.Context(), f.id, 0, tampered); !errors.Is(err, ErrState) {
		t.Fatal("quarantined with different proof", err)
	}
	f.quarantine(t)
}

func TestMoveRemainsUnavailableToOrdinaryTransferAndJobCreation(t *testing.T) {
	d, item := copyFixture(t)
	d.Mode = "move"
	if SupportedMode("move") {
		t.Fatal("move exposed before separate deletion confirmation")
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Create(t.Context(), Digest(d), d); !errors.Is(err, ErrPath) {
		t.Fatal(err)
	}
	if _, err = Transfer(t.Context(), d, item, TempName("test", 0), func(Proof) error { t.Fatal("unconfirmed move prepared"); return nil }); !errors.Is(err, ErrMode) {
		t.Fatal(err)
	}
	assertMoveBytes(t, filepath.Join(d.SourceRoot, item.Source), "original")
	assertMoveAbsent(t, filepath.Join(d.TargetRoot, item.Target))
}

func TestMoveDoesNotRenameWithoutDurableTransitionCallback(t *testing.T) {
	f := newMoveFixture(t)
	f.begin(t)
	if err := ContinueMove(t.Context(), f.d, f.item, f.p, false, nil); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	assertMoveBytes(t, f.source(), "original")
	assertMoveAbsent(t, f.held("payload"))
}

func TestMoveRenameNeverOverwritesExistingDestination(t *testing.T) {
	f := newMoveFixture(t)
	parent, hold, leaf, err := openMoveHold(f.d, f.item, f.p)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	defer hold.Close()
	if err := os.WriteFile(f.held("payload"), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(int(parent.Fd()), leaf, int(hold.Fd()), "payload"); !errors.Is(err, unix.EEXIST) {
		t.Fatal(err)
	}
	assertMoveBytes(t, f.source(), "original")
	assertMoveBytes(t, f.held("payload"), "foreign")
}
