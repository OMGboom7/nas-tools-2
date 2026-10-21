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
	"time"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func remoteApprovalTest() RemoteApproval {
	return RemoteApproval{Actor: "admin", Created: time.Now().UTC().Format(time.RFC3339Nano), Exclusive: true, RecoveryCopy: true}
}
func remoteUnitFixture(t *testing.T) (Definition, Entry, *Store, string, Proof) {
	t.Helper()
	d, e := copyFixture(t)
	d.Mode = "rclonecopy"
	d.TargetRoot = "/library/movies"
	d.TargetIdentity = RemoteTargetIdentity(d.TargetRoot)
	d.Remote = &remotestore.Binding{Driver: "rclone", ConfigFile: "/isolated/rclone.conf", ConfigDigest: strings.Repeat("a", 64), Binary: "/isolated/rclone", Protocol: 1}
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildRemoteProof(t.Context(), d, e, TempName(id, 0), remoteApprovalTest())
	if err != nil {
		t.Fatal(err)
	}
	return d, e, s, id, p
}
func TestRemoteJournalRequiresDurableStagesAndFrozenProofAcrossEveryTransition(t *testing.T) {
	d, e, s, id, p := remoteUnitFixture(t)
	if err := s.PrepareRemote(t.Context(), id, 0, p); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	if ok, err := s.Claim(t.Context(), id, 0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.Stage(t.Context(), id, 0, p); err != nil {
		t.Fatal(err)
	}
	prepared := p
	r := *p.Remote
	prepared.Remote = &r
	prepared.Incomplete = false
	r.StageReady = true
	mutated := prepared
	changed := r
	mutated.Remote = &changed
	changed.Approval.Actor = "another-admin"
	if err := s.PrepareRemote(t.Context(), id, 0, mutated); !errors.Is(err, ErrState) {
		t.Fatal("approval substituted", err)
	}
	if err := s.PrepareRemote(t.Context(), id, 0, prepared); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyRemotePhase(t.Context(), id, 0, p, "staging"); err == nil {
		t.Fatal("old phase accepted")
	}
	job, _ := s.Get(t.Context(), id)
	if err := s.Complete(t.Context(), job, job.Items[0]); err == nil {
		t.Fatal("completed without publication")
	}
	intent := prepared
	ri := r
	intent.Remote = &ri
	ri.PublicationIntent = true
	if err := s.BeginRemotePublish(t.Context(), id, 0, intent); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginRemotePublish(t.Context(), id, 0, intent); err == nil {
		t.Fatal("publication replay allowed")
	}
	verified := intent
	rv := ri
	verified.Remote = &rv
	rv.TargetVerified = true
	if err := s.FinishRemotePublish(t.Context(), id, 0, verified); err != nil {
		t.Fatal(err)
	}
	job, _ = s.Get(t.Context(), id)
	if err := s.Complete(t.Context(), job, job.Items[0]); err != nil {
		t.Fatal(err)
	}
	job, _ = s.Get(t.Context(), id)
	if job.State != "completed" || job.Remote == nil || job.Remote.Atomic {
		t.Fatal(job)
	}
	var mode string
	if err := s.db.QueryRow(`SELECT MODE FROM TRANSFER_HISTORY`).Scan(&mode); err != nil || mode != "Rclone复制" {
		t.Fatal(mode, err)
	}
	if remoteProofValid(d, e, verified) != nil {
		t.Fatal("legitimate frozen proof rejected")
	}
}
func TestRemoteReservationsCoverCaseUnicodeAliasesAndParentChildRoots(t *testing.T) {
	d, e, s, id, _ := remoteUnitFixture(t)
	for _, target := range []string{strings.ToUpper(e.Target), filepath.Dir(e.Target), e.Target + "/child"} {
		other := d
		other.Entries = []Entry{e}
		other.Entries[0].Target = target
		if _, err := s.Create(t.Context(), Digest([]any{"other", target}), other); !errors.Is(err, ErrClaimed) {
			t.Fatal(target, err)
		}
	}
	alias := d
	alias.TargetRoot = "/library"
	alias.TargetIdentity = RemoteTargetIdentity(alias.TargetRoot)
	alias.Entries = []Entry{e}
	alias.Entries[0].Target = "movies/" + e.Target
	if _, err := s.Create(t.Context(), Digest(alias), alias); !errors.Is(err, ErrClaimed) {
		t.Fatal("root alias bypass", err)
	}
	if err := s.Cancel(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(t.Context(), Digest(alias), alias); err != nil {
		t.Fatal("never-started reservation not released", err)
	}
	unicode := d
	unicode.Entries = []Entry{e}
	unicode.Entries[0].Target = "Café/Film.mkv"
	if _, err := s.Create(t.Context(), Digest(unicode), unicode); err != nil {
		t.Fatal(err)
	}
	unicode.Entries[0].Target = "Cafe\u0301/film.mkv"
	if _, err := s.Create(t.Context(), Digest(unicode), unicode); !errors.Is(err, ErrClaimed) {
		t.Fatal("Unicode reservation alias bypass", err)
	}
}
func TestRemoteClosureRetainsEvidenceAndClaimsWithoutSuccessOrDestructiveFallback(t *testing.T) {
	d, e, s, id, p := remoteUnitFixture(t)
	if ok, err := s.Claim(t.Context(), id, 0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.Stage(t.Context(), id, 0, p); err != nil {
		t.Fatal(err)
	}
	j, _ := s.Get(t.Context(), id)
	if err := s.AbandonUnpublished(t.Context(), j, func() error { return nil }); !errors.Is(err, ErrMode) {
		t.Fatal("local cleanup allowed", err)
	}
	if err := s.CloseRemoteJob(t.Context(), j, "admin", "stop remote transfer"); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Get(t.Context(), id)
	if j.State != "remote_closed" || j.RemoteClosure == nil || j.Items[0].State != "remote_stopped" {
		t.Fatal(j)
	}
	if err := s.CloseRemoteJob(t.Context(), j, "admin", "stop remote transfer"); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseRemoteJob(t.Context(), j, "admin", "a different reason"); err == nil {
		t.Fatal("closure audit rewritten")
	}
	if _, err := s.Create(t.Context(), Digest([]any{"again", d}), d); !errors.Is(err, ErrClaimed) {
		t.Fatal("claims released", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("invented history", n, err)
	}
	assertMoveBytes(t, filepath.Join(d.SourceRoot, e.Source), "original")
}
func realRemoteClient(t *testing.T, d *Definition) (*remotestore.Client, string) {
	t.Helper()
	binary := os.Getenv("NASTOOL_TEST_RCLONE_BINARY")
	if binary == "" {
		t.Skip("explicit temporary-alias Rclone binary required")
	}
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.MkdirAll(filepath.Join(root, "library/movies"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[NASTOOL]\ntype = alias\nremote = "+root+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := (remotestore.Factory{ConfigFile: cfg, Binary: binary, Environment: []string{}}).Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	d.TargetRoot = "/library/movies"
	d.TargetIdentity = RemoteTargetIdentity(d.TargetRoot)
	if err := c.Probe(t.Context(), d.TargetRoot); err != nil {
		t.Fatal(err)
	}
	b := c.Binding()
	d.Remote = &b
	return c, filepath.Join(root, "library/movies")
}
func TestRealRemoteCopyMoveAndWholeDiscPublicationPreserveRecoveryPayload(t *testing.T) {
	for _, disc := range []bool{false, true} {
		for _, mode := range []string{"rclonecopy", "rclone"} {
			t.Run(mode+"/disc="+map[bool]string{true: "yes", false: "no"}[disc], func(t *testing.T) {
				var d Definition
				var e Entry
				var files map[string]string
				if disc {
					d, e, files = discFixture(t, mode)
				} else {
					d, e = copyFixture(t)
					d.Mode = mode
				}
				c, target := realRemoteClient(t, &d)
				s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				id, err := s.Create(t.Context(), Digest(d), d)
				if err != nil {
					t.Fatal(err)
				}
				ok, err := s.Claim(t.Context(), id, 0)
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
				p, err := BuildRemoteProof(t.Context(), d, e, TempName(id, 0), remoteApprovalTest())
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Stage(t.Context(), id, 0, p); err != nil {
					t.Fatal(err)
				}
				if err := UploadRemoteStage(t.Context(), c, d, e, p, func() error { return s.VerifyRemotePhase(t.Context(), id, 0, p, "staging") }); err != nil {
					t.Fatal(err)
				}
				p.Incomplete = false
				p.Remote.StageReady = true
				if err := s.PrepareRemote(t.Context(), id, 0, p); err != nil {
					t.Fatal(err)
				}
				p.Remote.PublicationIntent = true
				if err := s.BeginRemotePublish(t.Context(), id, 0, p); err != nil {
					t.Fatal(err)
				}
				if err := PublishRemote(t.Context(), c, d, e, p, func() error { return s.VerifyRemotePhase(t.Context(), id, 0, p, "publishing") }); err != nil {
					t.Fatal(err)
				}
				// A lost final receipt can be explicitly rechecked. Existing
				// exact files are read, never sent through copyto again.
				if err := PublishRemote(t.Context(), c, d, e, p, func() error { return s.VerifyRemotePhase(t.Context(), id, 0, p, "publishing") }); err != nil {
					t.Fatal("identical partial publication recovery", err)
				}
				p.Remote.TargetVerified = true
				if err := s.FinishRemotePublish(t.Context(), id, 0, p); err != nil {
					t.Fatal(err)
				}
				ctx := RemoteContext(t.Context(), c, d)
				if mode == "rclone" {
					p, err = PrepareMove(ctx, d, e, p, MoveHoldName(id, 0))
					if err != nil {
						t.Fatal(err)
					}
					if err := s.BeginMove(ctx, id, 0, p); err != nil {
						t.Fatal(err)
					}
					if err := ContinueMove(ctx, d, e, p, false, func() error { return s.MarkQuarantined(ctx, id, 0, p) }); err != nil {
						t.Fatal(err)
					}
				}
				j, _ := s.Get(ctx, id)
				if err := s.Complete(ctx, j, j.Items[0]); err != nil {
					t.Fatal(err)
				}
				if mode == "rclone" {
					if err := CleanupMovedSource(ctx, d, e, p); err != nil {
						t.Fatal(err)
					}
					if err := s.AdvanceMoveCleanup(ctx, id, 0, p, true); err != nil {
						t.Fatal(err)
					}
					if err := CleanupMoveTarget(ctx, d, e, p); err != nil {
						t.Fatal(err)
					}
					if err := s.AdvanceMoveCleanup(ctx, id, 0, p, false); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Lstat(filepath.Join(d.SourceRoot, e.Source)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("source not moved", err)
					}
				} else if _, err := os.Stat(filepath.Join(d.SourceRoot, e.Source)); err != nil {
					t.Fatal("copy removed source", err)
				}
				j, _ = s.Get(ctx, id)
				if err := VerifyCompletedTarget(ctx, j, j.Items[0]); err != nil {
					t.Fatal(err)
				}
				if err := verifyRemoteManifest(ctx, c, d, e, p); err != nil {
					t.Fatal("private recovery removed", err)
				}
				if disc {
					discAssertTree(t, filepath.Join(target, e.Target), files)
				} else {
					assertMoveBytes(t, filepath.Join(target, e.Target), "original")
					prepared := 0
					intent := func(SidecarProof) error { prepared++; return nil }
					proof, state, err := PublishSidecar(ctx, j, j.Items[0], "media", "Movie.nfo", []byte("reviewed nfo"), intent, func() error { return nil })
					if err != nil || state != "created" || prepared != 1 {
						t.Fatal("remote metadata", state, err)
					}
					assertMoveBytes(t, filepath.Join(target, proof.Target), "reviewed nfo")
					assertMoveBytes(t, filepath.Join(target, proof.Temp, "payload"), "reviewed nfo")
					_, state, err = PublishSidecar(ctx, j, j.Items[0], "media", "Movie.nfo", []byte("different nfo"), intent, func() error { return nil })
					if err != nil || state != "preserved" || prepared != 1 {
						t.Fatal("existing metadata not preserved", state, err)
					}
				}
			})
		}
	}
}
func TestRemoteProofRejectsMissingConsentAndUnsafeFrozenDirectoryManifest(t *testing.T) {
	d, e, _, _, p := remoteUnitFixture(t)
	if _, err := BuildRemoteProof(t.Context(), d, e, p.Temp, RemoteApproval{}); err == nil {
		t.Fatal("implicit consent")
	}
	p.Remote.Directories = []string{"../../escape"}
	p.Remote.Manifest = Digest(json.RawMessage(remoteManifestBytes(e, d.Mode, *p.Remote)))
	p.StagingIdentity = p.Remote.Manifest
	if remoteProofValid(d, e, p) == nil {
		t.Fatal("unsafe directory accepted")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := BuildRemoteProof(cancelled, d, e, p.Temp, remoteApprovalTest()); err == nil {
		t.Fatal("cancelled source hashing")
	}
}
