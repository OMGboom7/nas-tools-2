//go:build linux || darwin

package organization

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func reservationTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func reservationTestReview(t *testing.T, s *Store, kind, id string) ReservationRelease {
	t.Helper()
	v, err := s.ReviewReservationRelease(t.Context(), ReservationOwnerRef{kind, id}, "admin", Digest([]string{kind, id})[:32], "independent complete reservation release; no content proof adopted")
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func reservationTestJob(t *testing.T, s *Store, n int, target string) string {
	t.Helper()
	d := Definition{SourceRoot: "/offline/source-" + strconv.Itoa(n), TargetRoot: "/offline/library", SourceIdentity: "source-" + strconv.Itoa(n), TargetIdentity: "target-root", Mode: "copy", Entries: []Entry{{Source: "source.mkv", Target: target, Identity: "file-" + strconv.Itoa(n), Kind: "media"}}}
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func TestReservationReleaseEveryIntakeModeIsLedgerOnlyAndOldAuthorityNeverReturns(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move", "rclonecopy", "miniocopy"} {
		t.Run(mode, func(t *testing.T) {
			d, e := copyFixture(t)
			d.Mode = mode
			if RemoteMode(mode) {
				d.TargetRoot = "/offline/library"
				d.TargetIdentity = RemoteTargetIdentityFor(mode, d.TargetRoot)
				d.Remote = &remotestore.Binding{Driver: RemoteDriver(mode), ConfigFile: "/does-not-exist/fixture-only.conf", ConfigDigest: Digest("offline account"), Protocol: 1}
			}
			s := reservationTestStore(t)
			id, err := s.Create(t.Context(), Digest(d), d)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.Claim(t.Context(), id, 0); err != nil || !ok {
				t.Fatal(ok, err)
			}
			old, err := s.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			before, err := reservationOwnerRows(t.Context(), s.db, "organization", id)
			if err != nil {
				t.Fatal(err)
			}
			v := reservationTestReview(t, s, "organization", id)
			var schema bool
			s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_RESERVATION_RELEASES')`).Scan(&schema)
			if schema {
				t.Fatal("review created write tables")
			}
			out, err := s.ReleaseReservations(t.Context(), v)
			if err != nil || out.State != "released" || out.Fingerprint != v.Fingerprint {
				t.Fatal(out, err)
			}
			if Digest(out.Document.Rows) != Digest(before) {
				t.Fatal("original typed rows not retained")
			}
			closed, err := s.Get(t.Context(), id)
			if err != nil || closed.State != RecoveryClosed || closed.Items[0].State != old.Items[0].State || Digest(closed.Items[0].Proof) != Digest(old.Items[0].Proof) {
				t.Fatal(closed, err)
			}
			if ok, err := s.Claim(t.Context(), id, 0); ok {
				t.Fatal("old writer reopened", ok, err)
			}
			if err := s.Cancel(t.Context(), id); err == nil {
				t.Fatal("old job cancellation adopted authority")
			}
			if _, err := s.Create(t.Context(), Digest([]any{"new independently reviewed job", d}), d); err != nil {
				t.Fatal("exact target remained reserved", err)
			}
			libraryContents(t, d.SourceRoot, e.Source, "original")
			stored, err := s.ReservationRelease(t.Context(), v.ID)
			if err != nil || stored.Fingerprint != v.Fingerprint {
				t.Fatal(stored, err)
			}
			if repeated, err := s.ReleaseReservations(t.Context(), v); err != nil || repeated.ID != v.ID {
				t.Fatal("lost-response receipt not idempotent", repeated, err)
			}
		})
	}
}
func TestReservationReleaseWholeDiscClaimAndEveryUnmodifiedObjectRemain(t *testing.T) {
	d, e, files := discFixture(t, "copy")
	s := reservationTestStore(t)
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	v := reservationTestReview(t, s, "organization", id)
	hasDisc := false
	for _, c := range v.Owners[0].Claims {
		hasDisc = hasDisc || c.Kind == "disc-target"
	}
	if !hasDisc {
		t.Fatal(v)
	}
	if _, err := s.ReleaseReservations(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_DISC_TARGETS`).Scan(&count)
	if count != 0 {
		t.Fatal("subtree claim retained", count)
	}
	discAssertTree(t, filepath.Join(d.SourceRoot, e.Source), files)
	if entries, err := os.ReadDir(d.TargetRoot); err != nil || len(entries) != 0 {
		t.Fatal("release wrote target media", entries, err)
	}
}
func TestReservationReleaseClosureHandlesHyphenInterleavingUnicodeAndCompleteLegacySiblings(t *testing.T) {
	s := reservationTestStore(t)
	parent := reservationTestJob(t, s, 1, "Café")
	unrelated := reservationTestJob(t, s, 2, "Cafe\u0301-other/file.mkv")
	child := reservationTestJob(t, s, 3, "CAFE\u0301/child.mkv")
	sibling := reservationTestJob(t, s, 4, "Other/movie.mkv")
	batch := strings.Repeat("b", 64)
	doc := legacyBatchDocument{Jobs: []string{child, sibling}, Definitions: []Definition{}}
	for i, id := range doc.Jobs {
		j, _ := s.Get(t.Context(), id)
		j.Definition.LegacyBatch = &LegacyBatchRef{ID: batch, Index: i}
		doc.Definitions = append(doc.Definitions, j.Definition)
		raw, _ := json.Marshal(j.Definition)
		if _, err := s.db.Exec(`UPDATE GO_ORGANIZATION_JOBS SET DEFINITION=? WHERE ID=?`, string(raw), id); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(doc)
	if _, err := s.db.Exec(`INSERT INTO GO_LEGACY_ORGANIZATION_BATCHES VALUES(?,?,?,'needs_review','','admin','fixture')`, batch, string(raw), Digest(doc)); err != nil {
		t.Fatal(err)
	}
	v := reservationTestReview(t, s, "organization", parent)
	if len(v.Owners) != 3 {
		t.Fatal("not the complete connected closure", v.Owners)
	}
	if _, err := s.ReleaseReservations(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{parent, child, sibling} {
		j, err := s.Get(t.Context(), id)
		if err != nil || j.State != RecoveryClosed {
			t.Fatal(j, err)
		}
	}
	var state string
	s.db.QueryRow(`SELECT STATE FROM GO_LEGACY_ORGANIZATION_BATCHES WHERE ID=?`, batch).Scan(&state)
	if state != "closed" {
		t.Fatal(state)
	}
	j, _ := s.Get(t.Context(), unrelated)
	if j.State == RecoveryClosed {
		t.Fatal("hyphen sibling incorrectly joined")
	}
	var claims int
	s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, unrelated).Scan(&claims)
	if claims != 1 {
		t.Fatal("unselected claim changed", claims)
	}
}
func TestReservationReleaseStaleSnapshotsAndAnyTransactionFailureKeepAllClaims(t *testing.T) {
	for _, point := range []string{"stale", "delete", "marker", "receipt"} {
		t.Run(point, func(t *testing.T) {
			s := reservationTestStore(t)
			id := reservationTestJob(t, s, 1, "Movie/file.mkv")
			v := reservationTestReview(t, s, "organization", id)
			if point == "stale" {
				s.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET REASON='concurrent change' WHERE JOB_ID=?`, id)
			} else {
				tx, _ := s.db.Begin()
				if err := reservationReleaseSchema(t.Context(), tx); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				q := map[string]string{"delete": `CREATE TRIGGER reject_release BEFORE DELETE ON GO_ORGANIZATION_TARGETS BEGIN SELECT RAISE(ABORT,'fixture'); END`, "marker": `CREATE TRIGGER reject_release BEFORE INSERT ON GO_RESERVATION_RELEASE_MARKERS BEGIN SELECT RAISE(ABORT,'fixture'); END`, "receipt": `CREATE TRIGGER reject_release BEFORE INSERT ON GO_RESERVATION_RELEASES BEGIN SELECT RAISE(ABORT,'fixture'); END`}[point]
				if _, err := s.db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ReleaseReservations(t.Context(), v); err == nil {
				t.Fatal("stale/failed transaction released reservations")
			}
			var count int
			s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, id).Scan(&count)
			if count != 1 {
				t.Fatal(count)
			}
			var state string
			s.db.QueryRow(`SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&state)
			if state != "active" {
				t.Fatal("partial task closure committed", state)
			}
			if _, err := s.ReservationRelease(t.Context(), v.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
		})
	}
}
func TestReservationReleaseTypedArchiveUnknownAssociationAndReopenPreserveOriginalRows(t *testing.T) {
	s := reservationTestStore(t)
	id := reservationTestJob(t, s, 1, "Movie/file.mkv")
	for _, q := range []string{`ALTER TABLE GO_ORGANIZATION_JOBS ADD COLUMN NULLABLE TEXT`, `ALTER TABLE GO_ORGANIZATION_JOBS ADD COLUMN HUGE INTEGER`, `ALTER TABLE GO_ORGANIZATION_JOBS ADD COLUMN BINARY BLOB`, `CREATE TABLE TRANSFER_UNKNOWN(ID INTEGER PRIMARY KEY,STATE TEXT,PATH TEXT)`} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE GO_ORGANIZATION_JOBS SET HUGE=9007199254740993,BINARY=? WHERE ID=?`, []byte{0, 255, 3}, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO GO_ORGANIZATION_UNKNOWN_LINKS VALUES(?,9007199254740993,'pending')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO TRANSFER_UNKNOWN VALUES(9007199254740993,'N','/offline/source/source.mkv')`); err != nil {
		t.Fatal(err)
	}
	v := reservationTestReview(t, s, "organization", id)
	if _, err := s.ReleaseReservations(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, dbpath string
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &dbpath); err != nil {
		t.Fatal(err)
	}
	other, err := OpenStore(dbpath)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	receipt, err := other.ReservationRelease(t.Context(), v.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]MediaRemovalValue{}
	for _, row := range receipt.Document.Rows {
		if row.Table == "GO_ORGANIZATION_JOBS" {
			for i, col := range row.Columns {
				found[col] = row.Values[i]
			}
		}
	}
	if found["NULLABLE"].Kind != "null" || found["HUGE"].Int != 9007199254740993 || string(found["BINARY"].Blob) != string([]byte{0, 255, 3}) {
		t.Fatal(found)
	}
	var state string
	other.db.QueryRow(`SELECT STATE FROM TRANSFER_UNKNOWN`).Scan(&state)
	if state != "N" {
		t.Fatal("unknown record falsely succeeded", state)
	}
	other.db.QueryRow(`SELECT STATE FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE JOB_ID=?`, id).Scan(&state)
	if state != "cancelled" {
		t.Fatal(state)
	}
	public, _ := json.Marshal(receipt)
	for _, secret := range []string{"NULLABLE", "BINARY", "definition", "sourceIdentity"} {
		if strings.Contains(string(public), secret) {
			t.Fatal("private archive leaked", secret)
		}
	}
}
func TestReservationReleaseLibraryUncertaintyPreservesMediaMetadataAndBackup(t *testing.T) {
	d, m := libraryFixture(t)
	d.Mode = "force_all"
	libraryWrite(t, d.Root.Path, "Movie/poster.png", "old metadata")
	s := reservationTestStore(t)
	id := libraryStoredJob(t, s, d, m)
	p, _, err := PublishLibrarySidecar(t.Context(), d, m, "media", "poster.png", []byte("replacement"), func(p LibraryOutputProof) error {
		return s.SaveLibraryOutput(t.Context(), id, d.Root, p.Target, Digest("old owner"), "writing", "", p)
	}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	v := reservationTestReview(t, s, "library", id)
	if _, err := s.ReleaseReservations(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	libraryContents(t, d.Root.Path, m.Path, "original-media")
	libraryContents(t, d.Root.Path, p.Target, "replacement")
	outputs, err := s.LibraryOutputs(t.Context(), id, 1)
	if err != nil || len(outputs) != 1 || outputs[0].State != "writing" || Digest(outputs[0].Proof) != Digest(p) {
		t.Fatal(outputs, err)
	}
	if err := s.LibraryRestoreIntent(t.Context(), outputs[0].ID, Digest(p)); err == nil {
		t.Fatal("closed restore authority reused")
	}
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM GO_LIBRARY_SCRAPE_CLAIMS`).Scan(&count)
	if count != 0 {
		t.Fatal(count)
	}
	d.RequestID = strings.Repeat("c", 32)
	fresh := libraryStoredJob(t, s, d, m)
	_, _, err = PublishLibrarySidecar(t.Context(), d, m, "media", "poster.png", []byte("fresh independently reviewed replacement"), func(p LibraryOutputProof) error {
		return s.SaveLibraryOutput(t.Context(), fresh, d.Root, p.Target, Digest("new owner"), "writing", "", p)
	}, func() error { return nil })
	if err != nil {
		t.Fatal("fresh publication did not acquire its own claim", err)
	}
}
func TestReservationReleaseImplicitRemoteMetadataClaimRequiresExactClosedSnapshot(t *testing.T) {
	d, _, s, id, _ := remoteUnitFixture(t)
	if _, err := s.db.Exec(`CREATE TABLE GO_SCRAPE_JOBS(ID TEXT PRIMARY KEY,JOB_ID TEXT,ORDINAL INTEGER,NAME TEXT,ORIGIN TEXT,CONFIG TEXT,OPTIONS TEXT,STATE TEXT,REASON TEXT,OUTPUTS TEXT,UPDATED TEXT)`); err != nil {
		t.Fatal(err)
	}
	target := "Movie (2026)/poster.png"
	scrape := Digest("scrape")
	outputs, _ := json.Marshal([]SidecarRecord{{Name: "poster.png", State: "uncertain", Proof: SidecarProof{Target: target, Temp: ".nastool-sidecar-old.tmp", Digest: Digest("unknown outcome")}}})
	if _, err := s.db.Exec(`INSERT INTO GO_SCRAPE_JOBS VALUES(?,?,0,'media','origin','config','{}','needs_review','lost response',?,'fixture')`, scrape, id, string(outputs)); err != nil {
		t.Fatal(err)
	}
	check := func() error {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		return checkRemoteMetadataClaims(t.Context(), tx, d.TargetRoot, target, "new-output", "")
	}
	if err := check(); !errors.Is(err, ErrClaimed) {
		t.Fatal(err)
	}
	v := reservationTestReview(t, s, "organization", id)
	if _, err := s.ReleaseReservations(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal("exact released uncertainty still blocked", err)
	}
	if _, err := s.db.Exec(`UPDATE GO_SCRAPE_JOBS SET OUTPUTS=replace(OUTPUTS,'unknown outcome','changed') WHERE ID=?`, scrape); err != nil {
		t.Fatal(err)
	}
	// Change an actual stored proof byte, rather than assuming an unhashed label
	// is present in OUTPUTS. A changed old receipt must reinstate the barrier.
	if _, err := s.db.Exec(`UPDATE GO_SCRAPE_JOBS SET REASON='externally changed old receipt' WHERE ID=?`, scrape); err != nil {
		t.Fatal(err)
	}
	if err := check(); !errors.Is(err, ErrClaimed) {
		t.Fatal("stale marker bypassed changed uncertainty", err)
	}
}
func TestReservationReleaseStoppedMediaScopeAndRecoveryChildrenCannotDeleteLater(t *testing.T) {
	x := recoveryFixture(t, "")
	mediaRemovalHistoryFixture(t, x, 1)
	x.put(t, "Movie/movie.mkv", "keep selected media")
	removal := mediaRemovalTestSave(t, x, mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie/movie.mkv", "library", 1)}, []int64{1}))
	if err := mediaRemovalClaimed(t.Context(), x.s.db, []string{filepath.Join(x.root, "Movie/movie.mkv")}, []int64{1}, ""); !errors.Is(err, ErrClaimed) {
		t.Fatal(err)
	}
	v := reservationTestReview(t, x.s, "media-removal", removal.ID)
	if len(v.Owners) != 2 {
		t.Fatal("deletion child omitted", v.Owners)
	}
	if _, err := x.s.ReleaseReservations(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	if err := mediaRemovalClaimed(t.Context(), x.s.db, []string{filepath.Join(x.root, "Movie/movie.mkv")}, []int64{1}, ""); err != nil {
		t.Fatal("stopped deletion scope not independently released", err)
	}
	current, err := x.s.MediaRemoval(t.Context(), removal.ID)
	if err != nil || current.State != "closed" || current.Units[0].Disposal.State != "closed" {
		t.Fatal(current, err)
	}
	if err := x.s.CheckRecoveryOpen(t.Context(), current.Units[0].Disposal.ID); err == nil {
		t.Fatal("old child reopened")
	}
	if !x.exists(t, "Movie/movie.mkv") || x.bytes(t, "Movie/movie.mkv") != "keep selected media" {
		t.Fatal("ledger release touched media")
	}
	var count int
	x.s.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count)
	if count != 1 {
		t.Fatal("history deleted", count)
	}
	// A new independent deletion review, not the old parent, may reserve it.
	next := mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie/movie.mkv", "library", 1)}, []int64{1})
	next.Definition.RequestID = Digest("fresh removal")[:32]
	next, err = x.s.PrepareMediaRemoval(t.Context(), next.Definition, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.CreateMediaRemoval(t.Context(), next.Definition, next.Fingerprint); err != nil {
		t.Fatal(err)
	}
}
func TestReservationReleaseIndependentPrivateDisposalStopsWithoutDeletingOrReturningCapture(t *testing.T) {
	x := recoveryFixture(t, "")
	x.put(t, filepath.Join(x.selected(), "payload"), "retain recovery copy")
	disposal := x.create(t, x.selected())
	v := reservationTestReview(t, x.s, "recovery", disposal.ID)
	if _, err := x.s.ReleaseReservations(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	if !x.exists(t, filepath.Join(x.selected(), "payload")) {
		t.Fatal("private copy was deleted")
	}
	if err := x.s.CheckRecoveryOpen(t.Context(), disposal.ID); err == nil {
		t.Fatal("old destructive operation reopened")
	}
}
func TestReservationReleasePaginationCancellationAndChangedOwnerListNeverMutate(t *testing.T) {
	s := reservationTestStore(t)
	for i := 0; i < 33; i++ {
		reservationTestJob(t, s, i, "Movie/"+strconv.Itoa(i)+".mkv")
	}
	a, err := s.ReservationReleases(t.Context(), "")
	if err != nil || len(a.Owners) != 30 || a.Next == "" {
		t.Fatal(a, err)
	}
	b, err := s.ReservationReleases(t.Context(), a.Next)
	if err != nil || len(b.Owners) != 3 || b.Next != "" {
		t.Fatal(b, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.ReviewReservationRelease(ctx, a.Owners[0].ReservationOwnerRef, "admin", strings.Repeat("a", 32), "cancelled"); err == nil {
		t.Fatal("cancelled review succeeded")
	}
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS`).Scan(&count)
	if count != 33 {
		t.Fatal(count)
	}
}

func TestReservationReleaseLinkedLibraryInputUsesItsFrozenRootIDAcrossDeletionAndRelease(t *testing.T) {
	x := recoveryFixture(t, "")
	x.put(t, "Movie/original.mkv", "outside linked input")
	d, _ := libraryFixture(t)
	d.Allowed = []LibraryRoot{x.scope.Root}
	if err := os.Symlink(filepath.Join(x.root, "Movie/original.mkv"), filepath.Join(d.Root.Path, "linked.mkv")); err != nil {
		t.Fatal(err)
	}
	scan, err := ScanLibrary(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	var linked LibraryMedia
	for _, m := range scan.Files {
		if m.Path == "linked.mkv" {
			linked = m
		}
	}
	if linked.InputRoot != x.scope.Root.ID || filepath.IsAbs(linked.InputRoot) {
		t.Fatal(linked)
	}
	id := libraryStoredJob(t, x.s, d, linked)
	removal := mediaRemovalTestSave(t, x, mediaRemovalTestPlan(t, x, []MediaRemovalUnit{mediaRemovalTestUnit(t, x, "Movie/original.mkv", "file", 0)}, nil))
	if len(removal.Owners) != 1 || removal.Owners[0].ID != id {
		t.Fatal("linked input owner omitted", removal.Owners)
	}
	d.RequestID = strings.Repeat("d", 32)
	if _, err := x.s.CreateLibraryJob(t.Context(), Digest(d), d, []LibraryMedia{linked}); !errors.Is(err, ErrClaimed) {
		t.Fatal("new linked reader bypassed active deletion", err)
	}
	v := reservationTestReview(t, x.s, "library", id)
	if len(v.Owners) != 3 {
		t.Fatal("linked input not included in complete closure", v.Owners)
	}
	if _, err := x.s.ReleaseReservations(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	if x.bytes(t, "Movie/original.mkv") != "outside linked input" {
		t.Fatal("input changed")
	}
	if target, err := os.Readlink(filepath.Join(d.Root.Path, "linked.mkv")); err != nil || target != filepath.Join(x.root, "Movie/original.mkv") {
		t.Fatal("link changed", target, err)
	}
}

func TestReservationReleaseFrozenRootIdentityAliasesIncludePendingWriters(t *testing.T) {
	s := reservationTestStore(t)
	id := reservationTestJob(t, s, 1, "Movie/file.mkv")
	var raw string
	if err := s.db.QueryRow(`SELECT DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var original Definition
	if json.Unmarshal([]byte(raw), &original) != nil {
		t.Fatal("fixture definition")
	}
	d := LibraryDefinition{Root: LibraryRoot{ID: "alias-root", Path: "/offline/another-bind-name", Identity: original.TargetIdentity}, Selection: ".", Mode: "force_all", Actor: "admin", RequestID: strings.Repeat("a", 32), Count: 1}
	m := LibraryMedia{Path: "Movie/file.mkv", InputRoot: d.Root.ID, InputPath: "Movie/file.mkv"}
	other, err := s.CreateLibraryJob(t.Context(), Digest(d), d, []LibraryMedia{m})
	if err != nil {
		t.Fatal(err)
	}
	v := reservationTestReview(t, s, "organization", id)
	if len(v.Owners) != 2 {
		t.Fatal("frozen identity alias writer omitted", v.Owners)
	}
	if _, err := s.ReleaseReservations(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	job, err := s.LibraryJob(t.Context(), other)
	if err != nil || job.State != RecoveryClosed {
		t.Fatal(job, err)
	}
	if err := s.ClaimLibraryJob(t.Context(), other); err == nil {
		t.Fatal("old alias writer reopened")
	}
}

func TestReservationReleaseWholeCurrentLibraryCyclePreservesFutureAuthorization(t *testing.T) {
	s := reservationTestStore(t)
	batch := Digest("current cycle")
	ids := []string{}
	for i := 0; i < 2; i++ {
		d := LibraryDefinition{Root: LibraryRoot{ID: "root-" + strconv.Itoa(i), Path: "/offline/library-" + strconv.Itoa(i), Identity: "root-identity-" + strconv.Itoa(i)}, Selection: ".", Mode: "no_force", Actor: "admin", RequestID: Digest(i)[:32], Count: 1, Schedule: &LibraryScheduleOrigin{Epoch: strings.Repeat("a", 32), Batch: batch}}
		id, err := s.CreateLibraryJob(t.Context(), Digest(d), d, []LibraryMedia{{Path: "Movie/file.mkv", InputRoot: d.Root.ID, InputPath: "Movie/file.mkv"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := s.db.Exec(`CREATE TABLE GO_LIBRARY_SCRAPE_RUNS(ID TEXT PRIMARY KEY,EPOCH TEXT,DOCUMENT TEXT)`); err != nil {
		t.Fatal(err)
	}
	run := LibraryPluginRun{ID: batch, Epoch: strings.Repeat("a", 32), State: "queued", Jobs: ids}
	doc, _ := json.Marshal(run)
	if _, err := s.db.Exec(`INSERT INTO GO_LIBRARY_SCRAPE_RUNS VALUES(?,?,?)`, batch, run.Epoch, string(doc)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO SYSTEM_DICT VALUES(1,'SystemConfig','GoLibraryScraper','future authorization must remain unchanged')`); err != nil {
		t.Fatal(err)
	}
	v := reservationTestReview(t, s, "library", ids[0])
	if len(v.Owners) != 2 {
		t.Fatal("current cycle siblings omitted", v.Owners)
	}
	if _, err := s.ReleaseReservations(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		job, err := s.LibraryJob(t.Context(), id)
		if err != nil || job.State != RecoveryClosed {
			t.Fatal(job, err)
		}
	}
	var setting string
	s.db.QueryRow(`SELECT VALUE FROM SYSTEM_DICT WHERE KEY='GoLibraryScraper'`).Scan(&setting)
	if setting != "future authorization must remain unchanged" {
		t.Fatal("historical release revoked or expanded future authorization", setting)
	}
}

func TestReservationReleaseAliasIndexIsBoundedDeduplicatedAndCancellable(t *testing.T) {
	root := LibraryRoot{Path: "/offline/root", Identity: "same-frozen-root"}
	paths := []string{"/offline/root/Movie/file.mkv", "/offline/root/Movie/file.mkv"}
	aliases, err := reservationRootAliases(t.Context(), paths, []LibraryRoot{root, root})
	if err != nil || len(aliases) != 1 {
		t.Fatal(aliases, err)
	}
	if _, err := reservationRootAliases(t.Context(), paths, make([]LibraryRoot, 2049)); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reservationRootAliases(ctx, paths, []LibraryRoot{root}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
