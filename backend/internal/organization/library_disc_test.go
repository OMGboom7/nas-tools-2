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

func libraryDiscFixture(t *testing.T, mode string) (LibraryDefinition, LibraryMedia, map[string]string) {
	t.Helper()
	native, e, files := discFixture(t, mode)
	transfer := Transfer
	if mode == "move" {
		transfer = PrepareMoveTarget
	}
	p, err := transfer(t.Context(), native, e, TempName("library-disc", 0), func(Proof) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	clean := native
	if mode == "move" {
		clean.Mode = "copy"
	}
	if err := Cleanup(clean, e, p); err != nil {
		t.Fatal(err)
	}
	d := LibraryDefinition{Root: LibraryRoot{ID: "disc-library", Path: native.TargetRoot, Identity: native.TargetIdentity, Kind: "movie"}, Allowed: []LibraryRoot{{ID: "disc-input", Path: native.SourceRoot, Identity: native.SourceIdentity}}, Selection: e.Target, Mode: "no_force", Actor: "admin", RequestID: strings.Repeat("a", 32), Count: 1}
	scan, err := ScanLibrary(t.Context(), d)
	if err != nil || len(scan.Files) != 1 || scan.Files[0].Disc == nil || scan.Files[0].Blocked != "" {
		t.Fatal(scan, err)
	}
	return d, scan.Files[0], files
}
func TestLibraryDiscAllLocalLayoutsAreOneFrozenReadonlyObject(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			d, m, files := libraryDiscFixture(t, mode)
			if err := VerifyLibraryMedia(t.Context(), d, m); err != nil {
				t.Fatal(err)
			}
			info := LibraryDiscSummary(m)
			if info.Files != len(files) || info.Directories != 5 || info.Expanded || len(info.Fingerprint) != 64 {
				t.Fatal(info)
			}
			if _, err := ReadLibraryMedia(t.Context(), d, m, func(f *os.File) ([]byte, error) { t.Fatal("decoded unverified STREAM clip"); return io.ReadAll(f) }); !errors.Is(err, ErrMode) {
				t.Fatal(err)
			}
			path, err := LibraryTarget(d, m, "media", LibraryDiscStem(d, m)+".nfo")
			if err != nil || path != filepath.Join(m.Path, LibraryDiscStem(d, m)+".nfo") {
				t.Fatal(path, err)
			}
			for _, file := range m.Disc.Files {
				if strings.Contains(file.Path, ".nastool-") {
					t.Fatal("adopted private stage", file)
				}
			}
		})
	}
}
func TestLibraryDiscMetadataModesBackupsAndRestorationKeepPayloadFrozen(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		for _, scrape := range []string{"no_force", "force_nfo", "force_all"} {
			t.Run(mode+scrape, func(t *testing.T) {
				d, m, files := libraryDiscFixture(t, mode)
				d.Mode = scrape
				name := LibraryDiscStem(d, m) + ".nfo"
				target := filepath.Join(m.Path, name)
				libraryWrite(t, d.Root.Path, target, "original-nfo")
				libraryWrite(t, d.Root.Path, filepath.Join(m.Path, "poster.png"), "original-art")
				if err := VerifyLibraryMedia(t.Context(), d, m); err != nil {
					t.Fatal("legitimate root metadata invalidated payload", err)
				}
				for _, spec := range []struct{ name, data string }{{name, "new-nfo"}, {"poster.png", "new-art"}} {
					p, state, err := PublishLibrarySidecar(t.Context(), d, m, "media", spec.name, []byte(spec.data), func(LibraryOutputProof) error { return nil }, func() error { return nil })
					expected := "replaced"
					if scrape == "no_force" || scrape == "force_nfo" && spec.name == "poster.png" {
						expected = "preserved"
					}
					if err != nil || state != expected {
						t.Fatal(state, expected, err)
					}
					if err := VerifyLibraryMedia(t.Context(), d, m); err != nil {
						t.Fatal("own output/backup changed frozen disc", err)
					}
					if state == "replaced" {
						actual, err := ObserveLibrarySidecar(t.Context(), d, p)
						if err != nil || actual != "replaced" {
							t.Fatal(actual, err)
						}
						if state, err := RestoreLibrarySidecar(t.Context(), d, p, func() error { return nil }); err != nil || state != "restored" {
							t.Fatal(state, err)
						}
					}
				}
				discAssertTree(t, filepath.Join(d.Root.Path, m.Path), files)
				if err := VerifyLibraryMedia(t.Context(), d, m); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
func TestLibraryDiscRejectsPayloadMutationReplacementAndLinkRetargeting(t *testing.T) {
	for _, kind := range []string{"bytes", "extra", "missing", "directory", "root", "retarget", "input-bytes", "outside-link", "directory-link", "temporary"} {
		t.Run(kind, func(t *testing.T) {
			mode := "copy"
			if kind == "retarget" || kind == "input-bytes" {
				mode = "softlink"
			}
			d, m, _ := libraryDiscFixture(t, mode)
			root := filepath.Join(d.Root.Path, m.Path)
			clip := filepath.Join(root, "BDMV/STREAM/00000.m2ts")
			switch kind {
			case "bytes":
				libraryWrite(t, d.Root.Path, filepath.Join(m.Path, "BDMV/STREAM/00000.m2ts"), "changed")
			case "extra":
				libraryWrite(t, d.Root.Path, filepath.Join(m.Path, "new-data.bin"), "unreviewed")
			case "missing":
				if err := os.Remove(clip); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Rename(filepath.Join(root, "CERTIFICATE/empty"), filepath.Join(root, "CERTIFICATE/old")); err != nil {
					t.Fatal(err)
				}
			case "root":
				if err := os.Rename(root, root+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			case "retarget":
				if err := os.Remove(clip); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(d.Allowed[0].Path, "Movie.2026.BLURAY/BDMV/STREAM/00001.m2ts"), clip); err != nil {
					t.Fatal(err)
				}
			case "input-bytes":
				libraryWrite(t, d.Allowed[0].Path, "Movie.2026.BLURAY/BDMV/STREAM/00000.m2ts", "changed input")
			case "outside-link":
				if err := os.Remove(clip); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(t.TempDir(), "outside.m2ts"), clip); err != nil {
					t.Fatal(err)
				}
			case "directory-link":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "foreign-dir")); err != nil {
					t.Fatal(err)
				}
			case "temporary":
				libraryWrite(t, d.Root.Path, filepath.Join(m.Path, "BDMV/STREAM/downloading.m2ts.!qb"), "part")
			}
			if err := VerifyLibraryMedia(t.Context(), d, m); err == nil {
				t.Fatal("changed whole-disc snapshot accepted", kind)
			}
			_, state, err := PublishLibrarySidecar(t.Context(), d, m, "media", LibraryDiscStem(d, m)+".nfo", []byte("nfo"), func(LibraryOutputProof) error { t.Fatal("saved intent for changed media"); return nil }, func() error { return nil })
			if err == nil || state != "blocked" {
				t.Fatal(state, err)
			}
		})
	}
}
func TestLibraryDiscSourceChangeDuringMetadataPreparationStopsPublication(t *testing.T) {
	d, m, _ := libraryDiscFixture(t, "copy")
	name := LibraryDiscStem(d, m) + ".nfo"
	_, state, err := PublishLibrarySidecar(t.Context(), d, m, "media", name, []byte("new-metadata"), func(LibraryOutputProof) error {
		libraryWrite(t, d.Root.Path, filepath.Join(m.Path, "BDMV/index.bdmv"), "changed catalog")
		return nil
	}, func() error { return nil })
	if err == nil || state != "blocked" {
		t.Fatal(state, err)
	}
	assertMoveAbsent(t, filepath.Join(d.Root.Path, m.Path, name))
}
func TestLibraryDiscExpansionExclusionsMultiVolumeAndRootBoundary(t *testing.T) {
	d, m, _ := libraryDiscFixture(t, "copy")
	inside := filepath.Join(m.Path, "BDMV/STREAM/00000.m2ts")
	d.Selection = inside
	if _, err := ScanLibrary(t.Context(), d); !errors.Is(err, ErrPath) {
		t.Fatal("silent whole-disc expansion", err)
	}
	d.AllowDiscExpansion = true
	scan, err := ScanLibrary(t.Context(), d)
	if err != nil || len(scan.Files) != 1 || scan.Files[0].Disc == nil || !scan.Files[0].Disc.Expanded || VerifyLibraryMedia(t.Context(), d, scan.Files[0]) != nil {
		t.Fatal(scan, err)
	}
	d.Excludes = []string{filepath.Join(m.Path, "CERTIFICATE/empty")}
	scan, err = ScanLibrary(t.Context(), d)
	if err != nil || len(scan.Files) != 0 || scan.Skipped != 1 {
		t.Fatal("partial excluded disc adopted", scan, err)
	}
	d.Excludes = nil
	d.Selection = m.Path
	d.AllowDiscExpansion = false
	root := filepath.Join(d.Root.Path, m.Path)
	info, _ := os.Stat(root)
	d.Root.Path, d.Root.Identity = root, Identity(info)
	d.Selection = "."
	scan, err = ScanLibrary(t.Context(), d)
	if err != nil || len(scan.Files) != 1 || scan.Files[0].Path != "." {
		t.Fatal(scan, err)
	}
	nfo := filepath.Base(root) + ".nfo"
	target, err := LibraryTarget(d, scan.Files[0], "media", nfo)
	if err != nil || target != nfo {
		t.Fatal("configured disc root output escaped", target, err)
	}
	d.Root.Path = filepath.Join(root, "BDMV")
	info, _ = os.Stat(d.Root.Path)
	d.Root.Identity = Identity(info)
	if _, err := ScanLibrary(t.Context(), d); err == nil {
		t.Fatal("configured BDMV fragment treated as separate movies")
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	discTestTree(t, base, "Movie.2026/Disc1")
	discTestTree(t, base, "Movie.2026/Disc2")
	info, _ = os.Stat(base)
	d.Root.Path, d.Root.Identity = base, Identity(info)
	d.Selection = "."
	scan, err = ScanLibrary(t.Context(), d)
	if err != nil || len(scan.Files) != 1 || scan.Files[0].Path != "Movie.2026" || LibraryDiscSummary(scan.Files[0]).Files != 16 {
		t.Fatal(scan, err)
	}
}
func TestLibraryDiscNFOReadingAndSymlinkReplacementNeverWritesInput(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "outside"}[external], func(t *testing.T) {
			d, m, _ := libraryDiscFixture(t, "softlink")
			inputRoot := d.Allowed[0].Path
			if external {
				inputRoot = t.TempDir()
			}
			libraryWrite(t, inputRoot, "original.nfo", "<movie><tmdbid>101</tmdbid></movie>")
			source := filepath.Join(inputRoot, "original.nfo")
			name := LibraryDiscStem(d, m) + ".nfo"
			target := filepath.Join(d.Root.Path, m.Path, name)
			if err := os.Symlink(source, target); err != nil {
				t.Fatal(err)
			}
			data, err := LibraryReadNFO(t.Context(), d, m, filepath.Join(m.Path, name))
			if external {
				if err == nil {
					t.Fatal("read outside allowed NFO root")
				}
			} else if err != nil || !strings.Contains(string(data), "101") {
				t.Fatal(string(data), err)
			}
			d.Mode = "force_nfo"
			p, state, err := PublishLibrarySidecar(t.Context(), d, m, "media", name, []byte("<movie><tmdbid>100</tmdbid></movie>"), func(LibraryOutputProof) error { return nil }, func() error { return nil })
			if err != nil || state != "replaced" || p.OldLink != source {
				t.Fatal(state, p, err)
			}
			libraryContents(t, inputRoot, "original.nfo", "<movie><tmdbid>101</tmdbid></movie>")
			if state, err := ObserveLibrarySidecar(t.Context(), d, p); err != nil || state != "replaced" {
				t.Fatal(state, err)
			}
			if state, err := RestoreLibrarySidecar(t.Context(), d, p, func() error { return nil }); err != nil || state != "restored" {
				t.Fatal(state, err)
			}
			if link, err := os.Readlink(target); err != nil || link != source {
				t.Fatal("original link not restored", link, err)
			}
			if state, err := RestoreLibrarySidecar(t.Context(), d, p, func() error { return nil }); err != nil || state != "restored" {
				t.Fatal("restore replay exchanged again", state, err)
			}
		})
	}
}
func TestLibraryDiscJournalStoresLargeInventoryPrivatelyAndResumesOnlyOriginalOutputs(t *testing.T) {
	d, m, _ := libraryDiscFixture(t, "copy")
	for i := 0; i < 180; i++ {
		libraryWrite(t, d.Root.Path, filepath.Join(m.Path, "BDMV/AUXDATA", Digest(i)+".bin"), "data")
	}
	scan, err := ScanLibrary(t.Context(), d)
	if err != nil || len(scan.Files) != 1 {
		t.Fatal(scan, err)
	}
	m = scan.Files[0]
	raw, _ := json.Marshal(m)
	if len(raw) <= 64<<10 {
		t.Fatal("fixture did not exercise old row limit", len(raw))
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.CreateLibraryJob(t.Context(), Digest([]any{d, m}), d, []LibraryMedia{m})
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.LibraryItems(t.Context(), id, 1)
	if err != nil || len(items) != 1 || items[0].Disc == nil || items[0].Disc.Files != 188 {
		t.Fatal(items, err)
	}
	public, _ := json.Marshal(items)
	if strings.Contains(string(public), "AUXDATA") || strings.Contains(string(public), "InputRoot") {
		t.Fatal("private inventory leaked", string(public))
	}
	if err := VerifyLibraryMedia(t.Context(), d, items[0].Media); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := VerifyLibraryMedia(ctx, d, m); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := ScanLibrary(ctx, d); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled scan became a blocked success", err)
	}
	if _, err := s.db.Exec(`UPDATE GO_LIBRARY_SCRAPE_ITEMS SET MEDIA=? WHERE JOB_ID=?`, `{"Path":"`+m.Path+`","Disc":{}}`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LibraryItems(t.Context(), id, 1); !errors.Is(err, ErrState) {
		t.Fatal("corrupt inventory emitted a plausible public summary", err)
	}
}

func TestLibraryDiscMalformedPartialAndMixedLayoutsNeverBecomeStandaloneClips(t *testing.T) {
	for _, scenario := range []string{"no-stream-media", "one-incomplete-volume", "directory-metadata", "inventory-document-limit"} {
		t.Run(scenario, func(t *testing.T) {
			d, m, _ := libraryDiscFixture(t, "copy")
			switch scenario {
			case "no-stream-media":
				for _, name := range []string{"00000.m2ts", "00001.m2ts"} {
					if err := os.Remove(filepath.Join(d.Root.Path, m.Path, "BDMV/STREAM", name)); err != nil {
						t.Fatal(err)
					}
				}
			case "one-incomplete-volume":
				if err := os.Rename(filepath.Join(d.Root.Path, m.Path), filepath.Join(d.Root.Path, "Disc1")); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(d.Root.Path, "Disc2/BDMV/STREAM"), 0700); err != nil {
					t.Fatal(err)
				}
				d.Selection = "."
			case "directory-metadata":
				if err := os.Mkdir(filepath.Join(d.Root.Path, m.Path, "poster.png"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "inventory-document-limit" {
				m.Disc.Files[0].Link = strings.Repeat("x", libraryDiscDocument)
				if _, err := LibraryInventoryBytes([]LibraryMedia{m}); !errors.Is(err, ErrLimit) {
					t.Fatal(err)
				}
				return
			}
			scan, err := ScanLibrary(t.Context(), d)
			if err != nil || len(scan.Files) != 1 {
				t.Fatal(scan, err)
			}
			if scenario == "directory-metadata" {
				if scan.Files[0].Disc == nil || VerifyLibraryMedia(t.Context(), d, m) == nil {
					t.Fatal("directory disguised as mutable art was omitted from proof", scan)
				}
			} else if scan.Files[0].Blocked == "" || scan.Files[0].Disc != nil {
				t.Fatal("incomplete disc converted into fragment work items", scan)
			}
		})
	}
}
