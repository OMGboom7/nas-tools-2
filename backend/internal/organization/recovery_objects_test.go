//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
	"github.com/0xforee/nas-tools/backend/internal/testutil/s3test"
)

type recoveryUnit struct {
	s                      *Store
	scope                  RecoveryScope
	c                      *remotestore.Client
	cloud                  *s3test.Server
	root, config, database string
}

func recoveryFixture(t *testing.T, driver string) *recoveryUnit {
	t.Helper()
	x := &recoveryUnit{}
	if driver == "" {
		x.root, _ = filepath.EvalSymlinks(t.TempDir())
		info, _ := os.Stat(x.root)
		x.scope = RecoveryScope{Root: LibraryRoot{ID: "root", Path: x.root, Identity: Identity(info)}, ConfigDigest: Digest("config")}
		var err error
		x.s, err = OpenStore(filepath.Join(t.TempDir(), "user.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { x.s.Close() })
	} else {
		r := remoteLibraryFixture(t, driver, "no_force", map[string]string{})
		x.s, x.c, x.cloud, x.root, x.config = r.s, r.c, r.cloud, r.root, r.cfg
		x.scope = RecoveryScope{Root: r.d.Root, Remote: r.d.Remote, ConfigDigest: Digest("config")}
	}
	var seq int
	var name string
	if err := x.s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &x.database); err != nil {
		t.Fatal(err)
	}
	return x
}
func (x *recoveryUnit) put(t *testing.T, name, value string) {
	t.Helper()
	if x.cloud != nil {
		x.cloud.Put("bucket/library/"+name, []byte(value))
	} else {
		libraryWrite(t, x.root, name, value)
	}
}
func (x *recoveryUnit) exists(t *testing.T, name string) bool {
	t.Helper()
	if x.cloud != nil {
		_, yes := x.cloud.Get("bucket/library/" + name)
		return yes
	}
	_, err := os.Lstat(filepath.Join(x.root, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}
func (x *recoveryUnit) bytes(t *testing.T, name string) string {
	t.Helper()
	if x.cloud != nil {
		b, yes := x.cloud.Get("bucket/library/" + name)
		if !yes {
			t.Fatal("missing", name)
		}
		return string(b)
	}
	b, err := os.ReadFile(filepath.Join(x.root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func (x *recoveryUnit) selected() string {
	if x.scope.Remote == nil {
		return ".nastool-copy-unrecorded.tmp"
	}
	return ".nastool-remote/" + strings.Repeat("a", 32)
}
func (x *recoveryUnit) create(t *testing.T, selected string) RecoveryDisposal {
	t.Helper()
	nodes, err := RecoverySnapshot(t.Context(), x.scope, selected, x.c)
	if err != nil {
		t.Fatal(err)
	}
	d := RecoveryDefinition{Scope: x.scope, Path: selected, Nodes: nodes, Actor: "admin", RequestID: strings.Repeat("b", 32), Decision: "independently reviewed obsolete recovery copy, including unknown children"}
	review, err := x.s.ReviewRecoveryObjects(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	id, err := x.s.CreateRecoveryDisposal(t.Context(), review.Definition, review.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	r, err := x.s.RecoveryDisposal(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (x *recoveryUnit) review(t *testing.T, id string) RecoveryDisposalReview {
	t.Helper()
	r, err := x.s.RecoveryDisposal(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	v, err := x.s.ReviewRecoveryDisposal(t.Context(), r, x.c)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func (x *recoveryUnit) execute(t *testing.T, v RecoveryDisposalReview, validate func() error) error {
	t.Helper()
	if !v.CanContinue {
		t.Fatal("not reviewed", v.Evidence)
	}
	return x.s.ExecuteRecoveryDisposal(t.Context(), v.Disposal, x.c, v.Fingerprint, validate)
}

func TestRecoveryDisposalAcrossProvidersDeletesOnlyTheWholeNewlyReviewedPrivateScope(t *testing.T) {
	for _, driver := range []string{"", "rclone", "minio"} {
		t.Run(driver, func(t *testing.T) {
			x := recoveryFixture(t, driver)
			selected := x.selected()
			x.put(t, "source/keep.mkv", "source")
			x.put(t, "published/keep.mkv", "published")
			x.put(t, path.Join(selected, "payload/BDMV/index.bdmv"), "index")
			x.put(t, path.Join(selected, "payload/BDMV/STREAM/00001.m2ts"), "readonly-disc")
			x.put(t, path.Join(selected, "unknown.txt"), "explicitly reviewed foreign child")
			if driver == "" {
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("external"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(x.root, selected, "link")); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(x.root, "published/keep.mkv"), filepath.Join(x.root, selected, "hardlink")); err != nil {
					t.Fatal(err)
				}
			}
			if x.c != nil {
				if err := x.c.Mkdir(t.Context(), path.Join(x.scope.Root.Path, selected, "empty")); err != nil {
					t.Fatal(err)
				}
			}
			r := x.create(t, selected)
			if r.State != "ready" || !x.exists(t, path.Join(selected, "unknown.txt")) {
				t.Fatal("creation deleted data", r)
			}
			v := x.review(t, r.ID)
			if err := x.execute(t, v, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			done, err := x.s.RecoveryDisposal(t.Context(), r.ID)
			if err != nil || done.State != "completed" || done.Done != done.Total {
				t.Fatal(done, err)
			}
			if x.bytes(t, "source/keep.mkv") != "source" || x.bytes(t, "published/keep.mkv") != "published" {
				t.Fatal("unselected data changed")
			}
			if driver == "" && x.exists(t, r.Capture) {
				t.Fatal("capture left after complete disposal")
			}
			var history int
			if err := x.s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&history); err != nil || history != 0 {
				t.Fatal("invented transfer history", history, err)
			}
			// Terminal state cannot become an executable old transfer or be replayed.
			if x.s.ExecuteRecoveryDisposal(t.Context(), done, x.c, v.Fingerprint, func() error { return nil }) == nil {
				t.Fatal("terminal disposal replayed")
			}
		})
	}
}
func TestRecoveryDisposalRejectsBroadPathsAndChangedOrAdditionalContent(t *testing.T) {
	for _, p := range []string{".", "..", "/", "published/keep.mkv", ".nastool-remote", ".nastool-library-remote", ".nastool-remote/" + strings.Repeat("a", 32) + "/payload", ".nastool-copy-x.tmp/../other"} {
		if RecoveryCandidate(p) {
			t.Fatal("broad or partial candidate accepted", p)
		}
	}
	for _, change := range []string{"bytes", "extra", "root-link"} {
		t.Run(change, func(t *testing.T) {
			x := recoveryFixture(t, "")
			selected := x.selected()
			x.put(t, path.Join(selected, "payload"), "old-data")
			r := x.create(t, selected)
			switch change {
			case "bytes":
				file := filepath.Join(x.root, selected, "payload")
				before, _ := os.Stat(file)
				x.put(t, path.Join(selected, "payload"), "foreign!")
				if err := os.Chtimes(file, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "extra":
				x.put(t, path.Join(selected, "late"), "never approved")
			case "root-link":
				if err := os.Rename(filepath.Join(x.root, selected), filepath.Join(x.root, "retained")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("retained", filepath.Join(x.root, selected)); err != nil {
					t.Fatal(err)
				}
			}
			v := x.review(t, r.ID)
			if v.CanContinue {
				t.Fatal("changed content approved", change)
			}
			if err := x.s.ExecuteRecoveryDisposal(t.Context(), r, nil, Digest("invalid stale attempt"), func() error { return nil }); err == nil {
				t.Fatal("stale bytes deleted")
			}
			if !x.exists(t, selected) {
				t.Fatal("changed original lost")
			}
		})
	}
}
func TestRecoveryDisposalPrivateLookingHistoryTargetIsNotMistakenForARecoveryCopy(t *testing.T) {
	x := recoveryFixture(t, "")
	selected := ".nastool-library-published.mkv"
	x.put(t, selected, "published media with a private-looking name")
	if _, err := x.s.db.Exec(`CREATE TABLE TRANSFER_HISTORY(SOURCE_PATH TEXT,SOURCE_FILENAME TEXT,DEST_PATH TEXT,DEST_FILENAME TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.db.Exec(`INSERT INTO TRANSFER_HISTORY VALUES('/download','source.mkv',?,?)`, x.root, selected); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.RecoveryObjectOwners(t.Context(), x.scope, selected); err == nil {
		t.Fatal("name replaced actual publication classification")
	}
	if x.bytes(t, selected) != "published media with a private-looking name" {
		t.Fatal("published target altered")
	}
}
func TestRecoveryReviewByteBudgetMatchesIntakeAndCannotOverflowPublicCounts(t *testing.T) {
	x := recoveryFixture(t, "")
	d := RecoveryDefinition{Scope: x.scope, Path: x.selected(), Actor: "admin", RequestID: strings.Repeat("b", 32), Decision: "arithmetic boundary only; no synthetic bytes are treated as storage evidence", Nodes: []RecoveryNode{{Path: ".", Kind: "file", Node: LibraryNode{Size: RecoveryByteLimit}}}}
	v, err := x.s.ReviewRecoveryObjects(t.Context(), d)
	if err != nil || v.Bytes != RecoveryByteLimit {
		t.Fatal("intake-sized object rejected by thumbnail-sized limit", v.Bytes, err)
	}
	d.Nodes = append(d.Nodes, RecoveryNode{Path: "overflow", Kind: "file", Node: LibraryNode{Size: 1}})
	if _, err := x.s.ReviewRecoveryObjects(t.Context(), d); !errors.Is(err, ErrLimit) {
		t.Fatal("whole-scope byte arithmetic overflowed", err)
	}
}
func TestRecoveryDisposalDurableDeleteIntentSurvivesLostJournalAndNeverReusesMissingProof(t *testing.T) {
	for _, driver := range []string{"", "rclone", "minio"} {
		t.Run(driver, func(t *testing.T) {
			x := recoveryFixture(t, driver)
			selected := x.selected()
			x.put(t, path.Join(selected, "payload"), "old-data")
			r := x.create(t, selected)
			if _, err := x.s.db.Exec(`CREATE TRIGGER fail_recovery_done BEFORE UPDATE OF STATE ON GO_RECOVERY_DISPOSAL_ITEMS WHEN NEW.STATE='done' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
				t.Fatal(err)
			}
			if err := x.execute(t, x.review(t, r.ID), func() error { return nil }); err == nil {
				t.Fatal("lost receipt hidden")
			}
			if _, err := x.s.db.Exec(`DROP TRIGGER fail_recovery_done`); err != nil {
				t.Fatal(err)
			}
			x.s.Close()
			var err error
			x.s, err = OpenStore(x.database)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { x.s.Close() })
			v := x.review(t, r.ID)
			if !v.CanContinue {
				t.Fatal(v.Evidence)
			}
			before := 0
			if x.cloud != nil {
				before = len(x.cloud.Requests())
			}
			if err := x.execute(t, v, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if x.cloud != nil {
				for _, request := range x.cloud.Requests()[before:] {
					if request.Method == "DELETE" && request.Key == "bucket/library/"+path.Join(selected, "payload") {
						t.Fatal("lost item receipt replayed delete")
					}
				}
			}
		})
	}
}
func TestRecoveryDisposalRemoteLostDeleteResponseRequiresExplicitFreshContinuation(t *testing.T) {
	x := recoveryFixture(t, "minio")
	selected := x.selected()
	x.put(t, path.Join(selected, "payload"), "old-data")
	r := x.create(t, selected)
	key := "bucket/library/" + path.Join(selected, "payload")
	x.cloud.FailNextDelete(key, 500)
	if err := x.execute(t, x.review(t, r.ID), func() error { return nil }); err == nil || strings.Contains(err.Error(), s3test.SecretKey) {
		t.Fatal(err)
	}
	deletes := 0
	for _, request := range x.cloud.Requests() {
		if request.Method == "DELETE" && request.Key == key {
			deletes++
			if request.IfMatch == "" {
				t.Fatal("missing conditional delete")
			}
		}
	}
	if deletes != 1 {
		t.Fatal("ambiguous delete replayed", deletes)
	}
	v := x.review(t, r.ID)
	before := len(x.cloud.Requests())
	if err := x.execute(t, v, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, request := range x.cloud.Requests()[before:] {
		if request.Method == "DELETE" && request.Key == key {
			t.Fatal("readonly absence reconciliation resent delete")
		}
	}
}
func TestRecoveryDisposalClosesUnprovenOwnerButRetainsItsHistoryClaimsAndCanReturnWholeCapture(t *testing.T) {
	x := recoveryFixture(t, "")
	d, e := copyFixture(t)
	d.ConfigDigest = Digest("config")
	id, err := x.s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := x.s.Claim(t.Context(), id, 0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	x.root = d.TargetRoot
	x.scope.Root.Path = d.TargetRoot
	x.scope.Root.Identity = d.TargetIdentity
	selected := path.Join(path.Dir(e.Target), TempName(id, 0))
	x.put(t, path.Join(selected, "payload"), "unrecorded original")
	x.put(t, path.Join(selected, "foreign"), "independent full-scope grant needed")
	r := x.create(t, selected)
	if len(r.Definition.Owners) != 1 || r.Definition.Owners[0].ID != id {
		t.Fatal("unproven owner not associated", r.Definition.Owners)
	}
	interrupted := errors.New("stop after capture, before first deletion")
	err = x.execute(t, x.review(t, r.ID), func() error {
		if x.exists(t, r.Capture) {
			return interrupted
		}
		return nil
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	old, err := x.s.Get(t.Context(), id)
	if err != nil || old.State != RecoveryClosed || old.Items[0].State != "running" || old.Items[0].Proof != (Proof{}) {
		t.Fatal("old proof or success invented", old, err)
	}
	var claims int
	if err := x.s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, id).Scan(&claims); err != nil || claims == 0 {
		t.Fatal("old claim released", claims, err)
	}
	v := x.review(t, r.ID)
	if !v.CanReturn {
		t.Fatal(v.Evidence)
	}
	if err := x.s.ReturnRecoveryCapture(t.Context(), v.Disposal, v, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if x.bytes(t, path.Join(selected, "foreign")) != "independent full-scope grant needed" {
		t.Fatal("return lost unknown child")
	}
	if ok, _ := x.s.Claim(t.Context(), id, 0); ok {
		t.Fatal("return restarted old task")
	}
}
func TestRecoveryDisposalCaptureMismatchIsReturnedWithoutDeletingOrOverwritingArrivals(t *testing.T) {
	x := recoveryFixture(t, "")
	selected := x.selected()
	x.put(t, path.Join(selected, "payload"), "old-data")
	r := x.create(t, selected)
	if ok, err := x.s.BeginRecoveryAttempt(t.Context(), r, Digest("explicit capture attempt")); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := os.Rename(filepath.Join(x.root, selected), filepath.Join(x.root, r.Capture)); err != nil {
		t.Fatal(err)
	}
	x.put(t, path.Join(r.Capture, "payload"), "unexpected captured bytes")
	v := x.review(t, r.ID)
	if v.CanContinue || !v.CanReturn {
		t.Fatal(v.CanContinue, v.CanReturn)
	}
	x.put(t, selected, "new arrival")
	if err := x.s.ReturnRecoveryCapture(t.Context(), v.Disposal, v, func() error { return nil }); err == nil {
		t.Fatal("return overwrote new original")
	}
	if x.bytes(t, selected) != "new arrival" || x.bytes(t, path.Join(r.Capture, "payload")) != "unexpected captured bytes" {
		t.Fatal("unknown bytes lost")
	}
}
func TestRecoveryDisposalAccountRevocationAndStopPreserveRemainingObjects(t *testing.T) {
	x := recoveryFixture(t, "minio")
	selected := x.selected()
	x.put(t, path.Join(selected, "payload"), "keep")
	r := x.create(t, selected)
	if err := os.WriteFile(x.config, []byte("revoked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.ReviewRecoveryDisposal(t.Context(), r, x.c); err == nil {
		t.Fatal("changed account accepted")
	}
	if err := x.s.CloseRecoveryDisposal(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if !x.exists(t, path.Join(selected, "payload")) {
		t.Fatal("stop deleted objects")
	}
	if x.s.CheckRecoveryOpen(context.Background(), r.ID) == nil {
		t.Fatal("closed operation stayed writable")
	}
}
func TestRecoveryDisposalLibraryBackupsCloseAllOldWriteAndRestoreAuthorityAcrossProviders(t *testing.T) {
	for _, driver := range []string{"minio", "rclone"} {
		t.Run(driver, func(t *testing.T) {
			x := remoteLibraryFixture(t, driver, "force_all", map[string]string{"Movie/Movie.2026.mkv": "media", "Movie/Movie.2026.nfo": "old-nfo"})
			id, m := x.adopt(t)
			target := "Movie/Movie.2026.nfo"
			owner := Digest("owner")
			p, state, err := PublishLibrarySidecar(x.ctx(t), x.d, m, "media", "Movie.2026.nfo", []byte("new-nfo"), func(p LibraryOutputProof) error {
				return x.s.SaveLibraryOutput(t.Context(), id, x.d.Root, target, owner, "writing", "", p)
			}, func() error { return nil })
			if err != nil || state != "replaced" {
				t.Fatal(state, err)
			}
			if err := x.s.SaveLibraryOutput(t.Context(), id, x.d.Root, target, owner, state, "", p); err != nil {
				t.Fatal(err)
			}
			r := &recoveryUnit{s: x.s, c: x.c, cloud: x.cloud, root: x.root, config: x.cfg, scope: RecoveryScope{Root: x.d.Root, Remote: x.d.Remote, ConfigDigest: Digest("config")}}
			op := r.create(t, p.Stage)
			if len(op.Definition.Owners) != 1 || op.Definition.Owners[0].Kind != "library" {
				t.Fatal(op.Definition.Owners)
			}
			if err := r.execute(t, r.review(t, op.ID), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			old, err := x.s.LibraryJob(t.Context(), id)
			if err != nil || old.State != RecoveryClosed {
				t.Fatal(old, err)
			}
			out, err := x.s.FindLibraryOutput(t.Context(), id, target)
			if err != nil {
				t.Fatal(err)
			}
			if err := x.s.LibraryRestoreIntent(t.Context(), out.ID, Digest(out.Proof)); err == nil {
				t.Fatal("closed library job regained restore authority")
			}
			if x.get(t, target) != "new-nfo" || x.get(t, m.Path) != "media" {
				t.Fatal("published output or readonly media changed")
			}
		})
	}
}
