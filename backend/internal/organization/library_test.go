//go:build linux || darwin

package organization

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func libraryFixture(t *testing.T) (LibraryDefinition, LibraryMedia) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	d := LibraryDefinition{Root: LibraryRoot{ID: "library", Path: root, Identity: Identity(info), Kind: "movie"}, Selection: ".", Mode: "no_force", Actor: "admin", RequestID: strings.Repeat("a", 32), Count: 1}
	libraryWrite(t, root, "Movie/Movie.2026.mkv", "original-media")
	scan, err := ScanLibrary(t.Context(), d)
	if err != nil || len(scan.Files) != 1 {
		t.Fatal(scan, err)
	}
	return d, scan.Files[0]
}
func libraryWrite(t *testing.T, root, name, data string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func libraryContents(t *testing.T, root, name, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || string(data) != want {
		t.Fatal(name, string(data), want, err)
	}
}
func TestLibraryScanFrozenSourcesExclusionsAndNoFollow(t *testing.T) {
	d, m := libraryFixture(t)
	outside := t.TempDir()
	libraryWrite(t, outside, "outside.mkv", "outside")
	libraryWrite(t, d.Root.Path, "skip/a.mkv", "skip")
	libraryWrite(t, d.Root.Path, "skip-too/keep.mkv", "keep")
	libraryWrite(t, d.Root.Path, ".private/no.mkv", "hidden")
	libraryWrite(t, d.Root.Path, "@eaDir/no.mkv", "hidden")
	if err := os.Symlink(outside, filepath.Join(d.Root.Path, "directory-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside.mkv"), filepath.Join(d.Root.Path, "outside.mkv")); err != nil {
		t.Fatal(err)
	}
	allowed, _ := filepath.EvalSymlinks(t.TempDir())
	libraryWrite(t, allowed, "source.mkv", "source")
	info, _ := os.Stat(allowed)
	d.Allowed = []LibraryRoot{{ID: "input", Path: allowed, Identity: Identity(info)}}
	if err := os.Symlink(filepath.Join(allowed, "source.mkv"), filepath.Join(d.Root.Path, "inside.mkv")); err != nil {
		t.Fatal(err)
	}
	d.Excludes = []string{"skip"}
	scan, err := ScanLibrary(t.Context(), d)
	if err != nil || len(scan.Files) != 4 {
		t.Fatal(scan, err)
	}
	for _, file := range scan.Files {
		if file.Path == "outside.mkv" {
			if file.Blocked == "" {
				t.Fatal("outside link accepted")
			}
			continue
		}
		if err := VerifyLibraryMedia(t.Context(), d, file); err != nil {
			t.Fatal(file, err)
		}
		if file.Path == "inside.mkv" {
			data, err := ReadLibraryMedia(t.Context(), d, file, func(f *os.File) ([]byte, error) {
				if _, err := f.Write([]byte("forbidden")); err == nil {
					t.Fatal("writable media descriptor")
				}
				return io.ReadAll(f)
			})
			if err != nil || string(data) != "source" {
				t.Fatal(string(data), err)
			}
		}
	}
	libraryWrite(t, d.Root.Path, m.Path, "changed-media")
	if VerifyLibraryMedia(t.Context(), d, m) == nil {
		t.Fatal("stale file accepted")
	}
	for _, bad := range []string{"../escape", "/absolute", "a/../b"} {
		d.Selection = bad
		if _, err := ScanLibrary(t.Context(), d); err == nil {
			t.Fatal(bad)
		}
	}
}
func TestLibraryScanIsNotLimitedToIntakeThousandFiles(t *testing.T) {
	d, _ := libraryFixture(t)
	for i := 0; i < 1001; i++ {
		libraryWrite(t, d.Root.Path, filepath.Join("Many", Digest(i)+".mkv"), "x")
	}
	scan, err := ScanLibrary(t.Context(), d)
	if err != nil || len(scan.Files) != 1002 {
		t.Fatal(len(scan.Files), err)
	}
}
func TestLibraryMetadataModesBackupsAndExplicitRestoration(t *testing.T) {
	for _, tc := range []struct{ mode, name, state string }{{"no_force", "Movie.2026.nfo", "preserved"}, {"force_nfo", "poster.png", "preserved"}, {"force_nfo", "Movie.2026.nfo", "replaced"}, {"force_all", "poster.png", "replaced"}} {
		t.Run(tc.mode+tc.name, func(t *testing.T) {
			d, m := libraryFixture(t)
			d.Mode = tc.mode
			target := filepath.Join("Movie", tc.name)
			libraryWrite(t, d.Root.Path, target, "old-metadata")
			var intent LibraryOutputProof
			p, state, err := PublishLibrarySidecar(t.Context(), d, m, "media", tc.name, []byte("new-metadata"), func(p LibraryOutputProof) error {
				intent = p
				libraryContents(t, d.Root.Path, target, "old-metadata")
				return nil
			}, func() error { return nil })
			if err != nil || state != tc.state {
				t.Fatal(p, state, err)
			}
			if state == "preserved" {
				if intent.NewIdentity != "" {
					t.Fatal("preserved file had write intent")
				}
				libraryContents(t, d.Root.Path, target, "old-metadata")
				return
			}
			libraryContents(t, d.Root.Path, target, "new-metadata")
			libraryContents(t, filepath.Join(d.Root.Path, "Movie", p.Stage), "payload", "old-metadata")
			if outcome, err := ObserveLibrarySidecar(t.Context(), d, p); err != nil || outcome != "replaced" {
				t.Fatal(outcome, err)
			}
			called := 0
			outcome, err := RestoreLibrarySidecar(t.Context(), d, p, func() error { called++; return nil })
			if err != nil || outcome != "restored" || called != 1 {
				t.Fatal(outcome, err, called)
			}
			libraryContents(t, d.Root.Path, target, "old-metadata")
			libraryContents(t, filepath.Join(d.Root.Path, "Movie", p.Stage), "payload", "new-metadata")
			if _, err := RestoreLibrarySidecar(t.Context(), d, p, func() error { called++; return nil }); err != nil || called != 2 {
				t.Fatal("restored receipt cannot be recovered", err)
			}
			libraryContents(t, d.Root.Path, m.Path, "original-media")
		})
	}
}
func TestLibraryPublicationNoIntentLateRacesAndChangedRecovery(t *testing.T) {
	for _, scenario := range []string{"intent-failed", "late-target", "late-source", "late-force-target", "force-uncertain", "foreign-stage", "changed-after-publication", "restore-intent-failed"} {
		t.Run(scenario, func(t *testing.T) {
			d, m := libraryFixture(t)
			name := "Movie.2026.nfo"
			target := filepath.Join("Movie", name)
			if scenario != "intent-failed" && scenario != "late-target" && scenario != "late-source" {
				d.Mode = "force_nfo"
				libraryWrite(t, d.Root.Path, target, "old")
			}
			checks := 0
			p, state, err := PublishLibrarySidecar(t.Context(), d, m, "media", name, []byte("new"), func(p LibraryOutputProof) error {
				switch scenario {
				case "intent-failed":
					return ErrState
				case "late-target", "late-force-target":
					libraryWrite(t, d.Root.Path, target, "foreign")
				case "late-source":
					libraryWrite(t, d.Root.Path, m.Path, "changed")
				}
				return nil
			}, func() error {
				checks++
				if scenario == "force-uncertain" && checks >= 3 {
					return ErrState
				}
				return nil
			})
			switch scenario {
			case "intent-failed", "late-source":
				if err == nil {
					t.Fatal(state)
				}
				if _, e := os.Stat(filepath.Join(d.Root.Path, target)); !os.IsNotExist(e) {
					t.Fatal("unexpected publication", e)
				}
			case "late-target":
				if err != nil || state != "preserved" {
					t.Fatal(state, err)
				}
				libraryContents(t, d.Root.Path, target, "foreign")
			case "late-force-target":
				if err == nil {
					t.Fatal(state)
				}
				libraryContents(t, d.Root.Path, target, "foreign")
			case "force-uncertain":
				if state != "uncertain" || err == nil {
					t.Fatal(state, err)
				}
				if outcome, e := ObserveLibrarySidecar(t.Context(), d, p); e != nil || outcome != "replaced" {
					t.Fatal(outcome, e)
				}
				libraryContents(t, filepath.Join(d.Root.Path, "Movie", p.Stage), "payload", "old")
			default:
				if err != nil || state != "replaced" {
					t.Fatal(state, err)
				}
				if scenario == "foreign-stage" {
					stage := filepath.Join(d.Root.Path, "Movie", p.Stage)
					if err := os.Rename(stage, stage+"-held"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(t.TempDir(), stage); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "changed-after-publication" {
					libraryWrite(t, d.Root.Path, target, "later-edit")
				}
				if _, e := RestoreLibrarySidecar(t.Context(), d, p, func() error {
					if scenario == "restore-intent-failed" {
						return ErrState
					}
					return nil
				}); e == nil {
					t.Fatal("unsafe recovery succeeded")
				}
				want := "new"
				if scenario == "changed-after-publication" {
					want = "later-edit"
				}
				libraryContents(t, d.Root.Path, target, want)
			}
		})
	}
}
func TestLibraryNoForceCanonicalMovieNFOAndMetadataCannotEscape(t *testing.T) {
	d, m := libraryFixture(t)
	libraryWrite(t, d.Root.Path, "Movie/movie.nfo", "canonical")
	p, state, err := PublishLibrarySidecar(t.Context(), d, m, "media", "Movie.2026.nfo", []byte("new"), func(LibraryOutputProof) error { t.Fatal("canonical NFO overwritten"); return nil }, func() error { return nil })
	if err != nil || state != "preserved" {
		t.Fatal(p, state, err)
	}
	for _, name := range []string{"Movie.2026.mkv", "../../escape.nfo", "other.nfo", "poster.svg"} {
		if _, _, err := PublishLibrarySidecar(t.Context(), d, m, "media", name, []byte("bad"), func(LibraryOutputProof) error { t.Fatal("unsafe intent"); return nil }, func() error { return nil }); err == nil {
			t.Fatal(name)
		}
	}
}
func libraryStoredJob(t *testing.T, s *Store, d LibraryDefinition, m LibraryMedia) string {
	t.Helper()
	id, err := s.CreateLibraryJob(t.Context(), Digest([]any{d, m}), d, []LibraryMedia{m})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimLibraryJob(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return id
}
func TestLibraryJournalDurableIntentsClaimsReconciliationAndRestore(t *testing.T) {
	d, m := libraryFixture(t)
	d.Mode = "force_all"
	libraryWrite(t, d.Root.Path, "Movie/poster.png", "old")
	path := filepath.Join(t.TempDir(), "user.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if jobs, err := s.LibraryJobs(t.Context(), false); err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	var exists bool
	s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_LIBRARY_SCRAPE_JOBS')`).Scan(&exists)
	if exists {
		t.Fatal("read created library tables")
	}
	id := libraryStoredJob(t, s, d, m)
	owner := Digest("media-owner")
	p, state, err := PublishLibrarySidecar(t.Context(), d, m, "media", "poster.png", []byte("new"), func(p LibraryOutputProof) error {
		return s.SaveLibraryOutput(t.Context(), id, d.Root, p.Target, owner, "writing", "", p)
	}, func() error { return nil })
	if err != nil || state != "replaced" {
		t.Fatal(p, state, err)
	}
	outputs, err := s.LibraryOutputs(t.Context(), id, 1)
	if err != nil || len(outputs) != 1 || outputs[0].State != "writing" {
		t.Fatal(outputs, err)
	}
	data, _ := json.Marshal(outputs)
	if strings.Contains(string(data), "payload") || strings.Contains(string(data), p.NewDigest) || strings.Contains(string(data), p.Stage) {
		t.Fatal("private proof leaked", string(data))
	}
	d2 := d
	d2.RequestID = strings.Repeat("b", 32)
	other := libraryStoredJob(t, s, d2, m)
	if err := s.SaveLibraryOutput(t.Context(), other, d.Root, p.Target, owner, "writing", "", p); !errors.Is(err, ErrClaimed) {
		t.Fatal("uncertain target reused", err)
	}
	if jobs, err := s.LibraryJobs(t.Context(), true); err != nil || len(jobs) != 0 {
		t.Fatal("running replay eligible", jobs, err)
	}
	out := outputs[0]
	observed, err := ObserveLibrarySidecar(t.Context(), d, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileLibraryOutput(t.Context(), out.ID, LibraryOutputDigest(out), observed, d.Root); err != nil {
		t.Fatal(err)
	}
	if err := s.LibraryRestoreIntent(t.Context(), out.ID, Digest(p)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveLibraryOutput(t.Context(), other, d.Root, p.Target, owner, "writing", "", p); !errors.Is(err, ErrClaimed) {
		t.Fatal("restore intent did not reserve target", err)
	}
	if _, err := RestoreLibrarySidecar(t.Context(), d, p, func() error { return s.LibraryRestoreIntent(t.Context(), out.ID, Digest(p)) }); err != nil {
		t.Fatal(err)
	}
	// Simulate receipt loss and reopen: no publication replay, explicit restore
	// sees the already-restored original and can finish the same intent.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if jobs, err := s.LibraryJobs(t.Context(), true); err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	if _, err := RestoreLibrarySidecar(t.Context(), d, p, func() error { return s.LibraryRestoreIntent(t.Context(), out.ID, Digest(p)) }); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishLibraryRestore(t.Context(), out.ID, d.Root); err != nil {
		t.Fatal(err)
	}
	libraryContents(t, d.Root.Path, p.Target, "old")
	if err := s.SaveLibraryOutput(t.Context(), other, d.Root, p.Target, owner, "writing", "", p); err != nil {
		t.Fatal("finished restore held reservation", err)
	}
}
func TestLibraryJobCreationReadbackRollbackAndIdempotence(t *testing.T) {
	d, m := libraryFixture(t)
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fp := Digest([]any{d, m})
	id, err := s.CreateLibraryJob(t.Context(), fp, d, []LibraryMedia{m})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.CreateLibraryJob(t.Context(), fp, d, []LibraryMedia{m}); err != nil || again != id {
		t.Fatal(again, err)
	}
	if _, err := s.ExistingLibraryRequest(t.Context(), d.Actor, d.RequestID, Digest("stale")); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER corrupt_library_insert AFTER INSERT ON GO_LIBRARY_SCRAPE_ITEMS BEGIN UPDATE GO_LIBRARY_SCRAPE_ITEMS SET PATH='corrupt' WHERE JOB_ID=NEW.JOB_ID; END`); err != nil {
		t.Fatal(err)
	}
	d.RequestID = strings.Repeat("b", 32)
	if _, err := s.CreateLibraryJob(t.Context(), Digest(d), d, []LibraryMedia{m}); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	jobs, err := s.LibraryJobs(t.Context(), false)
	if err != nil || len(jobs) != 1 {
		t.Fatal("partial creation survived", jobs, err)
	}
	if err := s.CancelLibraryJob(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimLibraryJob(t.Context(), id); !errors.Is(err, ErrState) {
		t.Fatal("cancelled job ran", err)
	}
}

func TestLibraryUnpublishedIntentEvidenceAndClaimAliasProtection(t *testing.T) {
	d, m := libraryFixture(t)
	d.Mode = "force_all"
	libraryWrite(t, d.Root.Path, "Movie/poster.png", "old")
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := libraryStoredJob(t, s, d, m)
	owner := Digest("owner")
	r, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Save an intent, then observe the pre-publication bytes while the stage is
	// still owned. A cancelled/crashed writer must not be restarted from them.
	var saved LibraryOutputProof
	_, _, err = PublishLibrarySidecar(t.Context(), d, m, "media", "poster.png", []byte("new"), func(p LibraryOutputProof) error {
		saved = p
		return s.SaveLibraryOutput(t.Context(), id, d.Root, p.Target, owner, "writing", "", p)
	}, func() error {
		state, err := ObserveLibrarySidecar(t.Context(), d, saved)
		if err != nil || state != "not_published" {
			t.Fatal("unpublished intent evidence", state, err)
		}
		return ErrState
	})
	if err == nil {
		t.Fatal("intent resumed unexpectedly")
	}
	libraryContents(t, d.Root.Path, "Movie/poster.png", "old")
	d2 := d
	d2.RequestID = strings.Repeat("b", 32)
	other := libraryStoredJob(t, s, d2, m)
	alias := saved
	alias.Target = "ALIAS/POSTER.PNG"
	rootAlias := d.Root
	rootAlias.Path = filepath.Join(d.Root.Path, "Movie")
	rootAlias.ID = "overlapping-root"
	if err := s.SaveLibraryOutput(t.Context(), other, rootAlias, alias.Target, owner, "writing", "", alias); !errors.Is(err, ErrClaimed) {
		t.Fatal("physical target claim bypassed through root/leaf alias", err)
	}
}

func TestLibraryMetadataHardlinksAreExchangedNotTruncated(t *testing.T) {
	d, m := libraryFixture(t)
	d.Mode = "force_nfo"
	target := "Movie/Movie.2026.nfo"
	libraryWrite(t, d.Root.Path, target, "old")
	if err := os.Link(filepath.Join(d.Root.Path, target), filepath.Join(d.Root.Path, "other.nfo")); err != nil {
		t.Fatal(err)
	}
	p, state, err := PublishLibrarySidecar(t.Context(), d, m, "media", "Movie.2026.nfo", []byte("new"), func(LibraryOutputProof) error { return nil }, func() error { return nil })
	if err != nil || state != "replaced" {
		t.Fatal(state, err)
	}
	libraryContents(t, d.Root.Path, "other.nfo", "old")
	if _, err := RestoreLibrarySidecar(t.Context(), d, p, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	libraryContents(t, d.Root.Path, target, "old")
	libraryContents(t, d.Root.Path, "other.nfo", "old")
}

func TestLibraryClaimReadbackFailureRollsBackBeforePublication(t *testing.T) {
	d, m := libraryFixture(t)
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := libraryStoredJob(t, s, d, m)
	if _, err := s.db.Exec(`CREATE TRIGGER erase_library_claim AFTER INSERT ON GO_LIBRARY_SCRAPE_CLAIMS BEGIN DELETE FROM GO_LIBRARY_SCRAPE_CLAIMS WHERE TARGET_KEY=NEW.TARGET_KEY; END`); err != nil {
		t.Fatal(err)
	}
	_, _, err = PublishLibrarySidecar(t.Context(), d, m, "media", "Movie.2026.nfo", []byte("new"), func(p LibraryOutputProof) error {
		return s.SaveLibraryOutput(t.Context(), id, d.Root, p.Target, Digest("owner"), "writing", "", p)
	}, func() error { return nil })
	if !errors.Is(err, ErrClaimed) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.Root.Path, "Movie/Movie.2026.nfo")); !os.IsNotExist(err) {
		t.Fatal("lost claim still published", err)
	}
	outputs, err := s.LibraryOutputs(t.Context(), id, 1)
	if err != nil || len(outputs) != 0 {
		t.Fatal("partial intent survived", outputs, err)
	}
}
