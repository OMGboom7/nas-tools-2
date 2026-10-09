//go:build linux || darwin

package organization

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Deterministically interrupt the second content read, after one real 256 KiB
// write. No sleeps, network, process timing or real media are involved.
type partialCopyContext struct {
	context.Context
	armed atomic.Bool
	reads atomic.Int32
}

func (c *partialCopyContext) Err() error {
	if c.armed.Load() && c.reads.Add(1) > 1 {
		return context.Canceled
	}
	return c.Context.Err()
}

func stagingFixture(t *testing.T, mode string) (Definition, Entry, *Store, string, Proof) {
	t.Helper()
	d, item := copyFixture(t)
	d.Mode = mode
	path := filepath.Join(d.SourceRoot, item.Source)
	contents := bytes.Repeat([]byte("original"), 128<<10)
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	item.Size, item.Modified, item.Identity = info.Size(), strconv.FormatInt(info.ModTime().UnixNano(), 10), Identity(info)
	d.Entries = []Entry{item}
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Claim(t.Context(), id, 0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &partialCopyContext{Context: base}
	transfer := TransferJournaled
	if mode == "move" {
		transfer = PrepareMoveTargetJournaled
	}
	proof, err := transfer(ctx, d, item, TempName(id, 0), func(p Proof) error {
		if !p.Incomplete || !p.Anchor {
			t.Fatal("missing early proof", p)
		}
		stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp)
		payload, err := os.Lstat(filepath.Join(stage, "payload"))
		if err != nil {
			t.Fatal(err)
		}
		anchor, err := os.Lstat(filepath.Join(stage, "anchor"))
		if err != nil || !os.SameFile(payload, anchor) || Identity(payload) != p.Identity {
			t.Fatal("anchor did not pin original inode", err)
		}
		if (mode == "copy" || mode == "move") && payload.Size() != 0 {
			t.Fatal("content written before ledger", payload.Size())
		}
		original := sha256.Sum256(contents)
		if p.Digest != hex.EncodeToString(original[:]) {
			t.Fatal("not full original-source digest")
		}
		if err := s.Stage(ctx, id, 0, p); err != nil {
			return err
		}
		if mode == "copy" || mode == "move" {
			ctx.armed.Store(true)
		} else {
			cancel()
		}
		return nil
	}, func(Proof) error { t.Fatal("partial object was prepared"); return ErrState })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	return d, item, s, id, proof
}

func TestJournaledPartialStagesAreNeverPublishedAndCanBeExplicitlyAbandoned(t *testing.T) {
	for _, mode := range []string{"copy", "move", "link", "softlink"} {
		t.Run(mode, func(t *testing.T) {
			d, item, s, id, proof := stagingFixture(t, mode)
			job, err := s.Get(t.Context(), id)
			if err != nil || job.State != "needs_review" || job.Items[0].State != "staging" || job.Items[0].Proof != proof {
				t.Fatal(job, err)
			}
			stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), proof.Temp)
			payload, err := os.Lstat(filepath.Join(stage, "payload"))
			if err != nil {
				t.Fatal(err)
			}
			if (mode == "copy" || mode == "move") && payload.Size() != 256<<10 {
				t.Fatal("not an actual interrupted copy", payload.Size())
			}
			assertMoveAbsent(t, filepath.Join(d.TargetRoot, item.Target))
			if err := VerifyPublished(t.Context(), d, item, proof); !errors.Is(err, ErrState) {
				t.Fatal(err)
			}
			if err := PublishPrepared(t.Context(), d, item, proof, func() error { return nil }); !errors.Is(err, ErrState) {
				t.Fatal(err)
			}
			if err := s.Complete(t.Context(), job, job.Items[0]); !errors.Is(err, ErrState) {
				t.Fatal(err)
			}
			if err := Cleanup(d, item, proof); !errors.Is(err, ErrState) {
				t.Fatal("ordinary cleanup accepted incomplete proof", err)
			}
			if err := abandonSaved(t, s, id); err != nil {
				t.Fatal(err)
			}
			job, err = s.Get(t.Context(), id)
			if err != nil || job.State != "abandoned" {
				t.Fatal(job, err)
			}
			assertMoveAbsent(t, stage)
			contents, err := os.ReadFile(filepath.Join(d.SourceRoot, item.Source))
			if err != nil || !bytes.Equal(contents, bytes.Repeat([]byte("original"), 128<<10)) {
				t.Fatal("source changed", err)
			}
		})
	}
}

func TestPartialDisposalRejectsForeignOrLostEvidence(t *testing.T) {
	for _, kind := range []string{"different-prefix", "oversized", "source-missing", "anchor-missing", "payload-replaced", "payload-missing", "unknown", "external-link", "target", "proof-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			d, item, s, id, p := stagingFixture(t, "copy")
			stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp)
			switch kind {
			case "different-prefix":
				abandonFixtureBytes(t, filepath.Join(stage, "payload"), "foreign")
			case "oversized":
				if err := os.Truncate(filepath.Join(stage, "payload"), item.Size+1); err != nil {
					t.Fatal(err)
				}
			case "source-missing":
				if err := os.Remove(filepath.Join(d.SourceRoot, item.Source)); err != nil {
					t.Fatal(err)
				}
			case "anchor-missing":
				if err := os.Remove(filepath.Join(stage, "anchor")); err != nil {
					t.Fatal(err)
				}
			case "payload-replaced":
				if err := os.Remove(filepath.Join(stage, "payload")); err != nil {
					t.Fatal(err)
				}
				abandonFixtureBytes(t, filepath.Join(stage, "payload"), "original")
			case "payload-missing":
				if err := os.Remove(filepath.Join(stage, "payload")); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				abandonFixtureBytes(t, filepath.Join(stage, "foreign"), "foreign")
			case "external-link":
				if err := os.Link(filepath.Join(stage, "payload"), filepath.Join(t.TempDir(), "foreign-alias")); err != nil {
					t.Fatal(err)
				}
			case "target":
				abandonFixtureBytes(t, filepath.Join(d.TargetRoot, item.Target), "foreign")
			case "proof-mismatch":
				if _, err := s.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='prepared' WHERE JOB_ID=?`, id); err != nil {
					t.Fatal(err)
				}
			}
			if err := abandonSaved(t, s, id); err == nil {
				t.Fatal("adopted foreign/missing evidence", kind)
			}
			var state string
			var n int
			if err := s.db.QueryRow(`SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&state); err != nil || state != "active" {
				t.Fatal(state, err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, id).Scan(&n); err != nil || n != 1 {
				t.Fatal(n, err)
			}
		})
	}
}

func TestPartialDisposalResumesAnchorOnlyAfterDurableIntent(t *testing.T) {
	d, item, s, id, p := stagingFixture(t, "copy")
	job, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.beginAbandon(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp)
	if err = os.Remove(filepath.Join(stage, "payload")); err != nil {
		t.Fatal(err)
	}
	other, err := OpenStore(filepath.Join(s.guardRoot, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err = abandonSaved(t, other, id); err != nil {
		t.Fatal(err)
	}
	assertMoveAbsent(t, stage)
	job, err = other.Get(t.Context(), id)
	if err != nil || job.State != "abandoned" {
		t.Fatal(job, err)
	}
}

func TestJournaledTransferNeverWritesContentWhenStagingTransactionFails(t *testing.T) {
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
	if ok, err := s.Claim(t.Context(), id, 0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_stage BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='staging' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	p, err := TransferJournaled(t.Context(), d, item, TempName(id, 0), func(p Proof) error { return s.Stage(t.Context(), id, 0, p) }, func(Proof) error { t.Fatal("prepared after failed early journal"); return nil })
	if err == nil {
		t.Fatal("ignored initial transaction failure")
	}
	stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp)
	info, err := os.Stat(filepath.Join(stage, "payload"))
	if err != nil || info.Size() != 0 {
		t.Fatal("wrote content without a durable proof", info, err)
	}
	job, err := s.Get(t.Context(), id)
	if err != nil || job.Items[0].State != "running" || job.Items[0].Proof != (Proof{}) {
		t.Fatal(job, err)
	}
	if err := abandonSaved(t, s, id); err == nil {
		t.Fatal("adopted unjournaled object")
	}
	assertMoveBytes(t, filepath.Join(d.SourceRoot, item.Source), "original")
	assertMoveAbsent(t, filepath.Join(d.TargetRoot, item.Target))
}

func TestStagingCannotPromoteChangedOrIncompleteProof(t *testing.T) {
	_, _, s, id, p := stagingFixture(t, "copy")
	if err := s.Prepare(t.Context(), id, 0, p); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	p.Incomplete = false
	for _, kind := range []string{"digest", "identity", "anchor", "kind", "parent"} {
		candidate := p
		switch kind {
		case "digest":
			candidate.Digest = Digest("foreign")
		case "identity":
			candidate.Identity = "1:1"
		case "anchor":
			candidate.Anchor = false
		case "kind":
			candidate.Kind = "symlink"
		case "parent":
			candidate.ParentIdentity = "1:2"
		}
		if err := s.Prepare(t.Context(), id, 0, candidate); !errors.Is(err, ErrState) {
			t.Fatal(kind, err)
		}
	}
}

func TestJournaledCompleteProofRetainsAnchorAndSupportsPostCommitCleanup(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink"} {
		t.Run(mode, func(t *testing.T) {
			d, item := copyFixture(t)
			d.Mode = mode
			s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id, err := s.Create(t.Context(), Digest(d), d)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.Claim(t.Context(), id, 0); err != nil || !ok {
				t.Fatal(ok, err)
			}
			p, err := TransferJournaled(t.Context(), d, item, TempName(id, 0), func(p Proof) error { return s.Stage(t.Context(), id, 0, p) }, func(p Proof) error { return s.Prepare(t.Context(), id, 0, p) })
			if err != nil || p.Incomplete || !p.Anchor {
				t.Fatal(p, err)
			}
			if err = VerifyPublished(t.Context(), d, item, p); err != nil {
				t.Fatal(err)
			}
			job, err := s.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Complete(t.Context(), job, job.Items[0]); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp)
			// Simulate the post-history cleanup stopping after the first unlink.
			if err = os.Remove(filepath.Join(stage, "payload")); err != nil {
				t.Fatal(err)
			}
			if err = VerifyPublished(t.Context(), d, item, p); err != nil {
				t.Fatal("lost the independent anchor proof", err)
			}
			if err = Cleanup(d, item, p); err != nil {
				t.Fatal(err)
			}
			assertMoveAbsent(t, stage)
			assertMoveBytes(t, filepath.Join(d.SourceRoot, item.Source), "original")
			assertMoveBytes(t, filepath.Join(d.TargetRoot, item.Target), "original")
		})
	}
}

func TestPartialDisposalReceiptFailureResumesWithoutInventingSuccessfulTransfer(t *testing.T) {
	d, item, s, id, p := stagingFixture(t, "copy")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_receipt BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='abandoned' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := abandonSaved(t, s, id); err == nil {
		t.Fatal("ignored receipt failure")
	}
	assertMoveAbsent(t, filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp))
	job, err := s.Get(t.Context(), id)
	if err != nil || job.State != "abandoning" || job.Items[0].State != "discarding" {
		t.Fatal(job, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_receipt`); err != nil {
		t.Fatal(err)
	}
	other, err := OpenStore(filepath.Join(s.guardRoot, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err = abandonSaved(t, other, id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = other.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestPartialDisposalNeverAdoptsReplacementAfterIntentOrCallback(t *testing.T) {
	for _, kind := range []string{"payload-only", "callback-replacement"} {
		t.Run(kind, func(t *testing.T) {
			d, item, s, id, p := stagingFixture(t, "copy")
			job, err := s.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.beginAbandon(t.Context(), job); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), p.Temp)
			if kind == "payload-only" {
				if err := os.Remove(filepath.Join(stage, "anchor")); err != nil {
					t.Fatal(err)
				}
				err = abandonSaved(t, s, id)
			} else {
				err = inspectUnpublished(t.Context(), d, item, p, true, func() error {
					if err := os.Remove(filepath.Join(stage, "payload")); err != nil {
						return err
					}
					// Equal bytes still do not make a replacement the saved inode;
					// the original anchor prevents the inode from being recycled.
					abandonFixtureBytes(t, filepath.Join(stage, "payload"), "original")
					return nil
				})
				assertMoveBytes(t, filepath.Join(stage, "payload"), "original")
			}
			if err == nil {
				t.Fatal("deleted unknown replacement")
			}
			var count int
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, id).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestJournaledTransferRejectsSourceContentChangeWithRestoredSnapshot(t *testing.T) {
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
	if ok, err := s.Claim(t.Context(), id, 0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	_, err = TransferJournaled(t.Context(), d, item, TempName(id, 0), func(p Proof) error {
		if err := s.Stage(t.Context(), id, 0, p); err != nil {
			return err
		}
		abandonFixtureBytes(t, filepath.Join(d.SourceRoot, item.Source), "changed!")
		nanos, err := strconv.ParseInt(item.Modified, 10, 64)
		if err != nil {
			return err
		}
		return os.Chtimes(filepath.Join(d.SourceRoot, item.Source), time.Unix(0, nanos), time.Unix(0, nanos))
	}, func(Proof) error { t.Fatal("prepared changed bytes using old source digest"); return nil })
	if !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	assertMoveAbsent(t, filepath.Join(d.TargetRoot, item.Target))
	job, err := s.Get(t.Context(), id)
	if err != nil || job.Items[0].State != "staging" || !job.Items[0].Proof.Incomplete {
		t.Fatal(job, err)
	}
	if err := abandonSaved(t, s, id); err == nil {
		t.Fatal("discarded without original source preservation")
	}
}
