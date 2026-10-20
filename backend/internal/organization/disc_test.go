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
	"time"
)

func discTestTree(t *testing.T, root, path string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, name := range []string{"BDMV/index.bdmv", "BDMV/MovieObject.bdmv", "BDMV/STREAM/00000.m2ts", "BDMV/STREAM/00001.m2ts", "BDMV/PLAYLIST/00000.mpls", "CERTIFICATE/id.bdmv", "readme.txt", ".release-info"} {
		full := filepath.Join(root, path, name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		data := "original " + name
		files[name] = data
		if err := os.WriteFile(full, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, path, "CERTIFICATE/empty"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, path, ".release-info"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for name := range files {
		if err := os.Chtimes(filepath.Join(root, path, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return files
}
func discFixture(t *testing.T, mode string) (Definition, Entry, map[string]string) {
	t.Helper()
	source, _ := filepath.EvalSymlinks(t.TempDir())
	target, _ := filepath.EvalSymlinks(t.TempDir())
	path := "Movie.2026.BLURAY"
	files := discTestTree(t, source, path)
	scan, err := Scan(t.Context(), source, path)
	if err != nil || len(scan.Files) != 1 || scan.Files[0].Disc == nil {
		t.Fatal(scan, err)
	}
	f := scan.Files[0]
	sourceInfo, _ := os.Stat(source)
	targetInfo, _ := os.Stat(target)
	e := Entry{Source: path, Target: "Movie (2026)", Kind: "media", Size: f.Size, Modified: strconv.FormatInt(f.Modified, 10), Identity: f.Identity, Disc: f.DiscInfo, TMDBID: "100", Title: "Movie", Year: "2026", MediaType: "电影"}
	d := Definition{SourceRoot: source, TargetRoot: target, SourceIdentity: Identity(sourceInfo), TargetIdentity: Identity(targetInfo), Mode: mode, Entries: []Entry{e}, Discs: map[string]DiscManifest{path: *f.Disc}}
	return d, e, files
}
func discAssertTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, want := range files {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(b) != want {
			t.Fatal(name, string(b), err)
		}
	}
	if info, err := os.Stat(filepath.Join(root, "CERTIFICATE/empty")); err != nil || !info.IsDir() {
		t.Fatal("empty directory not preserved", err)
	}
}
func discPrepared(t *testing.T, mode string, incomplete bool) (Definition, Entry, *Store, string, Proof) {
	t.Helper()
	d, e, _ := discFixture(t, mode)
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
	transfer := TransferJournaled
	if mode == "move" {
		transfer = PrepareMoveTargetJournaled
	}
	_, err = transfer(ctx, d, e, TempName(id, 0), func(p Proof) error {
		err := s.Stage(ctx, id, 0, p)
		if err == nil && incomplete {
			cancel()
		}
		return err
	}, func(p Proof) error {
		err := s.Prepare(ctx, id, 0, p)
		if err == nil {
			cancel()
		}
		return err
	})
	if err == nil {
		t.Fatal("did not stop before publication")
	}
	job, err := s.Get(t.Context(), id)
	if err != nil || len(job.Items) != 1 {
		t.Fatal(job, err)
	}
	state := "prepared"
	if incomplete {
		state = "staging"
	}
	if job.Items[0].State != state {
		t.Fatal(job.Items[0])
	}
	return d, e, s, id, job.Items[0].Proof
}

func TestDiscScanCollapsesCatalogAndPreservesWholeFrozenInventory(t *testing.T) {
	d, e, files := discFixture(t, "copy")
	for _, selected := range []string{e.Source, filepath.Join(e.Source, "BDMV"), filepath.Join(e.Source, "BDMV/STREAM/00000.m2ts")} {
		scan, err := Scan(t.Context(), d.SourceRoot, selected)
		if err != nil || len(scan.Files) != 1 {
			t.Fatal(scan, err)
		}
		f := scan.Files[0]
		if f.Path != e.Source || f.DiscInfo.Files != len(files) || f.DiscInfo.Expanded != (selected != e.Source) || f.Size != e.Size || len(f.DiscInfo.Fingerprint) != 64 {
			t.Fatal(f)
		}
	}
	rootScan, err := Scan(t.Context(), d.SourceRoot, e.Source)
	if err != nil {
		t.Fatal(err)
	}
	before := rootScan.Files[0].DiscInfo.Fingerprint
	if err := os.WriteFile(filepath.Join(d.SourceRoot, e.Source, "readme.txt"), []byte("new inventory"), 0644); err != nil {
		t.Fatal(err)
	}
	after, err := Scan(t.Context(), d.SourceRoot, e.Source)
	if err != nil || before == after.Files[0].DiscInfo.Fingerprint {
		t.Fatal("inner file change lost", after, err)
	}
}
func TestDiscScanRejectsLinksTemporaryFilesAndIncompleteCatalogs(t *testing.T) {
	for _, kind := range []string{"symlink", "temporary", "no-index", "no-clips", "reserved"} {
		t.Run(kind, func(t *testing.T) {
			d, e, _ := discFixture(t, "copy")
			root := filepath.Join(d.SourceRoot, e.Source)
			switch kind {
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "foreign")); err != nil {
					t.Fatal(err)
				}
			case "temporary":
				os.WriteFile(filepath.Join(root, "BDMV/STREAM/downloading.m2ts.!qb"), []byte("part"), 0644)
			case "no-index":
				os.Remove(filepath.Join(root, "BDMV/index.bdmv"))
			case "no-clips":
				os.Remove(filepath.Join(root, "BDMV/STREAM/00000.m2ts"))
				os.Remove(filepath.Join(root, "BDMV/STREAM/00001.m2ts"))
			case "reserved":
				os.WriteFile(filepath.Join(root, ".nastool-foreign"), []byte("keep"), 0644)
			}
			scan, err := Scan(t.Context(), d.SourceRoot, e.Source)
			if err == nil && len(scan.Files) == 1 && scan.Files[0].Disc != nil {
				t.Fatal("adopted unsafe disc", scan)
			}
			if _, err := Transfer(t.Context(), d, e, TempName("unsafe", 0), func(Proof) error { return nil }); err == nil {
				t.Fatal("transferred changed inventory")
			}
		})
	}
}
func TestDiscMultiVolumeNeverAggregatesUnrelatedReleases(t *testing.T) {
	root := t.TempDir()
	discTestTree(t, root, "Collection/Disc1")
	discTestTree(t, root, "Collection/Disc2")
	scan, err := Scan(t.Context(), root, "Collection")
	if err != nil || len(scan.Files) != 1 || scan.Files[0].Path != "Collection" || scan.Files[0].DiscInfo.Files != 16 {
		t.Fatal(scan, err)
	}
	root = t.TempDir()
	discTestTree(t, root, "Downloads/MovieA.2026")
	discTestTree(t, root, "Downloads/MovieB.2026")
	scan, err = Scan(t.Context(), root, "Downloads")
	if err != nil || len(scan.Files) != 2 {
		t.Fatal("shared directory treated as one release", scan, err)
	}
}
func TestDiscDownloadSelectionRequiresEveryInventoryFile(t *testing.T) {
	d, e, _ := discFixture(t, "copy")
	all := ScanResult{Files: []File{}}
	for _, node := range d.Discs[e.Source].Nodes {
		if node.Directory {
			continue
		}
		file, err := SnapshotSelected(t.Context(), d.SourceRoot, filepath.Join(e.Source, node.Path))
		if err != nil {
			t.Fatal(err)
		}
		all.Files = append(all.Files, file)
	}
	scan, err := ScanSelected(t.Context(), d.SourceRoot, all.Files)
	if err != nil || len(scan.Files) != 1 || scan.Files[0].Disc == nil || scan.Files[0].DiscInfo.Expanded {
		t.Fatal(scan, err)
	}
	if _, err := ScanSelected(t.Context(), d.SourceRoot, all.Files[:len(all.Files)-1]); err == nil {
		t.Fatal("absorbed file outside download selection")
	}
	if err := os.WriteFile(filepath.Join(d.SourceRoot, e.Source, "new-file.txt"), []byte("foreign"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanSelected(t.Context(), d.SourceRoot, all.Files); err == nil {
		t.Fatal("absorbed unadvertised new file")
	}
}
func TestDiscLiteralModesPublishWholeTreeAndCleanOnlyOwnedWitnesses(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink"} {
		t.Run(mode, func(t *testing.T) {
			d, e, files := discFixture(t, mode)
			staged, prepared := false, false
			p, err := TransferJournaled(t.Context(), d, e, TempName("disc", 0), func(p Proof) error {
				staged = true
				if !p.Incomplete || p.Disc == nil {
					t.Fatal(p)
				}
				if _, err := os.Stat(filepath.Join(d.TargetRoot, e.Target)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("published before intent")
				}
				return nil
			}, func(p Proof) error {
				prepared = true
				if p.Incomplete || p.Kind != "directory" {
					t.Fatal(p)
				}
				return nil
			})
			if err != nil || !staged || !prepared {
				t.Fatal(p, err)
			}
			if err := VerifyPublished(t.Context(), d, e, p); err != nil {
				t.Fatal(err)
			}
			discAssertTree(t, filepath.Join(d.TargetRoot, e.Target), files)
			discAssertTree(t, filepath.Join(d.SourceRoot, e.Source), files)
			for name := range files {
				src, _ := os.Lstat(filepath.Join(d.SourceRoot, e.Source, name))
				dst, err := os.Lstat(filepath.Join(d.TargetRoot, e.Target, name))
				if err != nil || (mode == "link") != os.SameFile(src, dst) || (mode == "softlink") != (dst.Mode()&os.ModeSymlink != 0) {
					t.Fatal("mode fallback", mode, name, err)
				}
				if mode == "copy" && (dst.Mode().Perm() != src.Mode().Perm() || !dst.ModTime().Equal(src.ModTime())) {
					t.Fatal("disc copy broadened file permissions or discarded modification time", name)
				}
			}
			if err := Cleanup(d, e, p); err != nil {
				t.Fatal(err)
			}
			if err := Cleanup(d, e, p); err != nil {
				t.Fatal("cleanup not reentrant", err)
			}
			job := Job{State: "completed", Definition: d}
			item := JobItem{Entry: e, State: "completed", Proof: p}
			if err := VerifyCompletedTarget(t.Context(), job, item); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(d.TargetRoot, p.Temp)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("stage retained", err)
			}
		})
	}
}
func TestDiscHardlinksSharingOneOriginalInodeHaveCorrectWitnessAccounting(t *testing.T) {
	d, e, _ := discFixture(t, "link")
	a := filepath.Join(d.SourceRoot, e.Source, "BDMV/index.bdmv")
	b := filepath.Join(d.SourceRoot, e.Source, "BDMV/backup.bdmv")
	if err := os.Link(a, b); err != nil {
		t.Fatal(err)
	}
	scan, err := Scan(t.Context(), d.SourceRoot, e.Source)
	if err != nil {
		t.Fatal(err)
	}
	f := scan.Files[0]
	e.Disc, e.Size, e.Modified = f.DiscInfo, f.Size, strconv.FormatInt(f.Modified, 10)
	d.Discs[e.Source] = *f.Disc
	d.Entries = []Entry{e}
	p, err := Transfer(t.Context(), d, e, TempName("shared", 0), func(Proof) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(d, e, p); err != nil {
		t.Fatal(err)
	}
	if _, err := discSourceStamp(t.Context(), d, e, &p, true, true); err != nil {
		t.Fatal("own links mistaken for source corruption", err)
	}
}
func TestDiscPreparedRecoveryReusesDirectoryAndRefusesChangedOrExtraObjects(t *testing.T) {
	for _, kind := range []string{"valid", "extra-payload", "source-added", "same-bytes-replacement", "late-target", "bad-witness", "lost-journal"} {
		t.Run(kind, func(t *testing.T) {
			d, e, s, id, p := discPrepared(t, "copy", false)
			stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, e.Target)), p.Temp)
			payload := filepath.Join(stage, "payload")
			before, _ := os.Stat(payload)
			switch kind {
			case "extra-payload":
				os.WriteFile(filepath.Join(payload, "unknown.txt"), []byte("keep"), 0644)
			case "source-added":
				os.WriteFile(filepath.Join(d.SourceRoot, e.Source, "unknown.txt"), []byte("new"), 0644)
			case "same-bytes-replacement":
				path := filepath.Join(payload, "BDMV/index.bdmv")
				data, _ := os.ReadFile(path)
				os.Rename(path, path+".saved")
				os.WriteFile(path, data, 0644)
			case "late-target":
				os.Mkdir(filepath.Join(d.TargetRoot, e.Target), 0755)
			case "bad-witness":
				os.Rename(filepath.Join(stage, "witness/BDMV/index.bdmv"), filepath.Join(stage, "witness/BDMV/index.saved"))
			case "lost-journal":
				s.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running' WHERE JOB_ID=?`, id)
			}
			err := PublishPrepared(t.Context(), d, e, p, func() error { return s.VerifyPrepared(t.Context(), id, 0, p) })
			if kind != "valid" {
				if err == nil {
					t.Fatal("published changed evidence", kind)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(filepath.Join(d.TargetRoot, e.Target))
			if !os.SameFile(before, after) {
				t.Fatal("recopied directory")
			}
			if err := PublishPrepared(t.Context(), d, e, p, func() error { return s.VerifyPrepared(t.Context(), id, 0, p) }); err != nil {
				t.Fatal("positive recovery not reentrant", err)
			}
		})
	}
}
func TestDiscAbandonPreparedAndJournaledPrefixesHasNoSuccessHistory(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		for _, incomplete := range []bool{false, true} {
			t.Run(mode+strconv.FormatBool(incomplete), func(t *testing.T) {
				d, e, s, id, p := discPrepared(t, mode, incomplete)
				if incomplete && (mode == "copy" || mode == "move") {
					path := filepath.Join(d.TargetRoot, p.Temp, "payload/BDMV/index.bdmv")
					if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if err := abandonSaved(t, s, id); err != nil {
					t.Fatal(err)
				}
				job, err := s.Get(t.Context(), id)
				if err != nil || job.State != "abandoned" {
					t.Fatal(job, err)
				}
				if _, err := os.Stat(filepath.Join(d.SourceRoot, e.Source, "BDMV/index.bdmv")); err != nil {
					t.Fatal("lost original", err)
				}
				var count int
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_DISC_TARGETS WHERE JOB_ID=?`, id).Scan(&count); err != nil || count != 0 {
					t.Fatal("reservation not released", count, err)
				}
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&count); err != nil || count != 0 {
					t.Fatal("invented history", count, err)
				}
			})
		}
	}
}
func TestDiscAbandonRefusesForeignIncompleteBytesAndExtraNodes(t *testing.T) {
	for _, kind := range []string{"bad-prefix", "extra", "missing-witness", "source-replaced"} {
		t.Run(kind, func(t *testing.T) {
			d, e, s, id, p := discPrepared(t, "copy", true)
			stage := filepath.Join(d.TargetRoot, p.Temp)
			switch kind {
			case "bad-prefix":
				os.WriteFile(filepath.Join(stage, "payload/BDMV/index.bdmv"), []byte("foreign"), 0644)
			case "extra":
				os.WriteFile(filepath.Join(stage, "payload/keep.txt"), []byte("foreign"), 0644)
			case "missing-witness":
				os.Rename(filepath.Join(stage, "witness"), filepath.Join(stage, "saved"))
			case "source-replaced":
				os.Rename(filepath.Join(d.SourceRoot, e.Source), filepath.Join(d.SourceRoot, e.Source+".saved"))
				discTestTree(t, d.SourceRoot, e.Source)
			}
			if err := abandonSaved(t, s, id); err == nil {
				t.Fatal("abandoned unsafe directory", kind)
			}
			job, err := s.Get(t.Context(), id)
			if err != nil || job.State == "abandoned" || job.State == "abandoning" {
				t.Fatal("intent saved without evidence", job, err)
			}
			if _, err := os.Stat(stage); err != nil {
				t.Fatal("deleted unknown evidence", err)
			}
		})
	}
}
func TestDiscSubtreeReservationRejectsChildParentAndCaseAliasesUntilCancel(t *testing.T) {
	d, e, _ := discFixture(t, "copy")
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{e.Target + "/child.mkv", strings.ToUpper(e.Target) + "/child.mkv", e.Target} {
		ordinary := e
		ordinary.Source = "other.mkv"
		ordinary.Target = target
		ordinary.Disc = nil
		other := d
		other.Entries = []Entry{ordinary}
		if _, err := s.Create(t.Context(), Digest(other), other); !errors.Is(err, ErrClaimed) {
			t.Fatal("subtree not reserved", target, err)
		}
	}
	if err := s.Cancel(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(t.Context(), Digest(d), d); err != nil {
		t.Fatal("cancel did not release subtree", err)
	}
	d2 := d
	ordinary := e
	ordinary.Source = "outside.mkv"
	ordinary.Target = e.Target + "/child.mkv"
	ordinary.Disc = nil
	d2.Entries = []Entry{e, ordinary}
	if _, err := s.Create(t.Context(), Digest(d2), d2); err == nil {
		t.Fatal("same-job tree overlaps accepted")
	}
}
func TestDiscCompletedSidecarsAreInsideDiscAndNeverAlterPayload(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink"} {
		t.Run(mode, func(t *testing.T) {
			d, e, files := discFixture(t, mode)
			p, err := Transfer(t.Context(), d, e, TempName("nfo", 0), func(Proof) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := Cleanup(d, e, p); err != nil {
				t.Fatal(err)
			}
			job := Job{State: "completed", Definition: d}
			item := JobItem{State: "completed", Entry: e, Proof: p}
			name := filepath.Base(e.Target) + ".nfo"
			_, state, err := PublishSidecar(t.Context(), job, item, "media", name, []byte("<movie/>"), func(SidecarProof) error { return nil }, func() error { return nil })
			if err != nil || state != "created" {
				t.Fatal(state, err)
			}
			if _, err := os.Stat(filepath.Join(d.TargetRoot, e.Target, name)); err != nil {
				t.Fatal("NFO outside disc", err)
			}
			if err := VerifyCompletedTarget(t.Context(), job, item); err != nil {
				t.Fatal("native NFO invalidated disc", err)
			}
			discAssertTree(t, filepath.Join(d.TargetRoot, e.Target), files)
			if err := os.WriteFile(filepath.Join(d.TargetRoot, e.Target, "unknown-file"), []byte("foreign"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := VerifyCompletedTarget(t.Context(), job, item); err == nil {
				t.Fatal("unapproved payload accepted")
			}
			b, _ := json.Marshal(item)
			if strings.Contains(string(b), "WitnessDirectories") || strings.Contains(string(b), "StagingIdentity") {
				t.Fatal("private proof exposed", string(b))
			}
		})
	}
}

func TestDiscMoveQuarantineRecoveryAndCleanupPreservesNewArrival(t *testing.T) {
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
	p, err := PrepareMoveTargetJournaled(t.Context(), d, e, TempName(id, 0), func(p Proof) error { return s.Stage(t.Context(), id, 0, p) }, func(p Proof) error { return s.Prepare(t.Context(), id, 0, p) })
	if err != nil {
		t.Fatal(err)
	}
	discAssertTree(t, filepath.Join(d.SourceRoot, e.Source), files)
	p, err = PrepareMove(t.Context(), d, e, p, MoveHoldName(id, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := CleanupMovedSource(t.Context(), d, e, p); err == nil {
		t.Fatal("source disposition without intent")
	}
	if err := s.BeginMove(t.Context(), id, 0, p); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("lost quarantine acknowledgement")
	if err := ContinueMove(t.Context(), d, e, p, false, func() error { return interrupted }); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	assertMoveAbsent(t, filepath.Join(d.SourceRoot, e.Source))
	hold := filepath.Join(d.SourceRoot, p.SourceHold)
	discAssertTree(t, filepath.Join(hold, "payload"), files)
	if err := ContinueMove(t.Context(), d, e, p, false, func() error { return s.MarkQuarantined(t.Context(), id, 0, p) }); err != nil {
		t.Fatal(err)
	}
	job, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(t.Context(), job, job.Items[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(d.SourceRoot, e.Source), 0755); err != nil {
		t.Fatal(err)
	}
	arrival := filepath.Join(d.SourceRoot, e.Source, "new.txt")
	if err := os.WriteFile(arrival, []byte("new arrival"), 0644); err != nil {
		t.Fatal(err)
	}
	// Reenter after a subset of known original leaves was removed, but before
	// the durable source-cleaned receipt. Remaining ownership is still proven.
	if err := os.Remove(filepath.Join(hold, "payload/BDMV/STREAM/00001.m2ts")); err != nil {
		t.Fatal(err)
	}
	if err := CleanupMovedSource(t.Context(), d, e, p); err != nil {
		t.Fatal(err)
	}
	if err := CleanupMovedSource(t.Context(), d, e, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hold); err != nil {
		t.Fatal("positive source cleanup witness removed before receipt", err)
	}
	if err := s.AdvanceMoveCleanup(t.Context(), id, 0, p, true); err != nil {
		t.Fatal(err)
	}
	if err := CleanupMoveTarget(t.Context(), d, e, p); err != nil {
		t.Fatal(err)
	}
	if err := CleanupMoveTarget(t.Context(), d, e, p); err != nil {
		t.Fatal("target cleanup not reentrant", err)
	}
	if err := s.AdvanceMoveCleanup(t.Context(), id, 0, p, false); err != nil {
		t.Fatal(err)
	}
	job, err = s.Get(t.Context(), id)
	if err != nil || job.State != "completed" {
		t.Fatal(job, err)
	}
	discAssertTree(t, filepath.Join(d.TargetRoot, e.Target), files)
	assertMoveBytes(t, arrival, "new arrival")
	assertMoveAbsent(t, hold)
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
func TestDiscAbandonIntentRecoveryHandlesOnlyMissingKnownLeaves(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(strconv.FormatBool(extra), func(t *testing.T) {
			d, _, s, id, p := discPrepared(t, "copy", true)
			job, err := s.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.beginAbandon(t.Context(), job); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(d.TargetRoot, p.Temp)
			if err := os.Remove(filepath.Join(stage, "payload/BDMV/index.bdmv")); err != nil {
				t.Fatal(err)
			}
			if extra {
				if err := os.WriteFile(filepath.Join(stage, "witness/foreign.txt"), []byte("keep"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			err = abandonSaved(t, s, id)
			if extra {
				if err == nil {
					t.Fatal("unknown file removed")
				}
				assertMoveBytes(t, filepath.Join(stage, "witness/foreign.txt"), "keep")
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestDiscStreamClipsAreNotStandaloneLibraryScraperMovies(t *testing.T) {
	d, e, _ := discFixture(t, "copy")
	info, _ := os.Stat(d.SourceRoot)
	library := LibraryDefinition{Root: LibraryRoot{ID: "test", Path: d.SourceRoot, Identity: Identity(info), Kind: "movie"}, Selection: e.Source, Mode: "no_force"}
	scan, err := ScanLibrary(t.Context(), library)
	if err != nil || len(scan.Files) != 2 {
		t.Fatal(scan, err)
	}
	for _, m := range scan.Files {
		if m.Blocked == "" || VerifyLibraryMedia(t.Context(), library, m) == nil {
			t.Fatal("STREAM clip considered a standalone movie", m)
		}
	}
}
func TestDiscMetadataStampObservesRequestCancellation(t *testing.T) {
	d, e, _ := discFixture(t, "copy")
	p, err := Transfer(t.Context(), d, e, TempName("cancel-stamp", 0), func(Proof) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sidecarMediaStamp(ctx, Job{State: "completed", Definition: d}, JobItem{State: "completed", Entry: e, Proof: p}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled directory traversal continued", err)
	}
}
