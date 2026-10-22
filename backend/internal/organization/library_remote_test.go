//go:build linux || darwin

package organization

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
	"github.com/0xforee/nas-tools/backend/internal/testutil/s3test"
)

type remoteLibraryUnit struct {
	d     LibraryDefinition
	c     *remotestore.Client
	s     *Store
	cloud *s3test.Server
	root  string
	cfg   string
}

func remoteLibraryFixture(t *testing.T, driver, mode string, files map[string]string) *remoteLibraryUnit {
	t.Helper()
	x := &remoteLibraryUnit{}
	f := remotestore.Factory{Driver: driver, ConfigFile: filepath.Join(t.TempDir(), "account.conf"), Environment: []string{}}
	if driver == "minio" {
		x.cloud = s3test.New(t)
		f.CAFile = filepath.Join(t.TempDir(), "ca.pem")
		raw, _ := json.Marshal(map[string]any{"version": "10", "aliases": map[string]any{"NASTOOL": map[string]string{"url": x.cloud.HTTP.URL, "accessKey": s3test.AccessKey, "secretKey": s3test.SecretKey, "path": "on", "api": "S3v4"}}})
		if err := os.WriteFile(f.ConfigFile, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.CAFile, x.cloud.CA, 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		f.Binary = os.Getenv("NASTOOL_TEST_RCLONE_BINARY")
		if f.Binary == "" {
			t.Skip("explicit isolated Rclone required")
		}
		alias := t.TempDir()
		x.root = filepath.Join(alias, "bucket/library")
		if err := os.MkdirAll(x.root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.ConfigFile, []byte("[NASTOOL]\ntype=alias\nremote="+alias+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	x.cfg = f.ConfigFile
	var err error
	x.c, err = f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.c.Close() })
	for name, data := range files {
		x.put(t, name, data)
	}
	if err := x.c.Probe(t.Context(), "/bucket/library"); err != nil {
		t.Fatal(err)
	}
	binding := x.c.Binding()
	x.d = LibraryDefinition{Remote: &binding, RemoteApproval: &RemoteApproval{Actor: "admin", Origin: "library-manual", Created: time.Now().UTC().Format(time.RFC3339Nano), Exclusive: true, RecoveryCopy: true}, Root: LibraryRoot{ID: "library", Path: "/bucket/library", Identity: RemoteTargetIdentityFor(LibraryRemoteMode(driver), "/bucket/library"), Kind: "movie"}, Selection: ".", Mode: mode, Actor: "admin", RequestID: strings.Repeat("a", 32), Allowed: []LibraryRoot{}}
	x.s, err = OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.s.Close() })
	return x
}
func (x *remoteLibraryUnit) ctx(t *testing.T) context.Context {
	return LibraryRemoteContext(t.Context(), x.c, x.d)
}
func (x *remoteLibraryUnit) put(t *testing.T, name, data string) {
	t.Helper()
	if x.cloud != nil {
		x.cloud.Put("bucket/library/"+name, []byte(data))
	} else {
		libraryWrite(t, x.root, name, data)
	}
}
func (x *remoteLibraryUnit) get(t *testing.T, name string) string {
	t.Helper()
	if x.cloud != nil {
		data, ok := x.cloud.Get("bucket/library/" + name)
		if !ok {
			t.Fatal("missing", name)
		}
		return string(data)
	}
	data, err := os.ReadFile(filepath.Join(x.root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func (x *remoteLibraryUnit) adopt(t *testing.T) (string, LibraryMedia) {
	t.Helper()
	ctx := x.ctx(t)
	scan, err := ScanLibrary(ctx, x.d)
	if err != nil || len(scan.Files) != 1 || scan.Files[0].Blocked != "" {
		t.Fatal(scan, err)
	}
	x.d.Count = 1
	id, err := x.s.CreateLibraryJob(ctx, Digest([]any{x.d, scan.Files}), x.d, scan.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.s.ClaimLibraryJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := x.s.LibraryItemState(ctx, id, 0, "pending", "running", ""); err != nil {
		t.Fatal(err)
	}
	if VerifyLibraryMedia(ctx, x.d, scan.Files[0]) == nil {
		t.Fatal("inventory alone authorized metadata write")
	}
	prepared, err := PrepareLibraryRemoteMedia(ctx, x.d, scan.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := x.s.BindLibraryRemoteMedia(ctx, id, 0, scan.Files[0], prepared); err != nil {
		t.Fatal(err)
	}
	if x.s.BindLibraryRemoteMedia(ctx, id, 0, scan.Files[0], prepared) == nil {
		t.Fatal("content adoption replayed")
	}
	return id, prepared
}

func TestRemoteLibraryThreeModesPreserveBothVersionsAndRestoreWithoutChangingMedia(t *testing.T) {
	for _, driver := range []string{"minio", "rclone"} {
		for _, mode := range []string{"no_force", "force_nfo", "force_all"} {
			t.Run(driver+"/"+mode, func(t *testing.T) {
				x := remoteLibraryFixture(t, driver, mode, map[string]string{"Movie/Movie.2026.mkv": "original-media", "Movie/Movie.2026.nfo": "original-nfo", "Movie/poster.jpg": "original-image"})
				id, m := x.adopt(t)
				ctx := x.ctx(t)
				for _, spec := range []struct{ name, new string }{{"Movie.2026.nfo", "new-nfo"}, {"poster.jpg", "new-image"}} {
					target, _ := LibraryTarget(x.d, m, "media", spec.name)
					owner := Digest([]string{spec.name})
					p, state, err := PublishLibrarySidecar(ctx, x.d, m, "media", spec.name, []byte(spec.new), func(p LibraryOutputProof) error {
						return x.s.SaveLibraryOutput(ctx, id, x.d.Root, target, owner, "writing", "", p)
					}, func() error { return x.s.CheckLibraryRunning(ctx, id) })
					if err != nil {
						t.Fatal(state, err)
					}
					want := "preserved"
					if LibraryOverwrite(mode, spec.name) {
						want = "replaced"
					}
					if state != want {
						t.Fatal(state, want)
					}
					if err := x.s.SaveLibraryOutput(ctx, id, x.d.Root, target, owner, state, "", p); err != nil {
						t.Fatal(err)
					}
					if state == "replaced" {
						if actual, err := ObserveLibrarySidecar(ctx, x.d, p); err != nil || actual != "replaced" {
							t.Fatal(actual, err)
						}
						out, err := x.s.FindLibraryOutput(ctx, id, target)
						if err != nil {
							t.Fatal(err)
						}
						if x.get(t, path.Join(p.Stage, "new")) != spec.new {
							t.Fatal("replacement backup missing")
						}
						state, err = RestoreLibrarySidecar(ctx, x.d, p, func() error { return x.s.LibraryRestoreIntent(ctx, out.ID, Digest(p)) })
						if err != nil || state != "restored" {
							t.Fatal(state, err)
						}
						if err := x.s.FinishLibraryRestore(ctx, out.ID, x.d.Root); err != nil {
							t.Fatal(err)
						}
						if strings.Contains(x.get(t, target), "new-") {
							t.Fatal("original did not return")
						}
						if x.get(t, path.Join(p.Stage, "new")) != spec.new {
							t.Fatal("restore discarded replacement")
						}
					}
				}
				if x.get(t, "Movie/Movie.2026.mkv") != "original-media" {
					t.Fatal("media altered")
				}
				var history int
				if err := x.s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&history); err != nil || history != 0 {
					t.Fatal("existing library invented intake history", history, err)
				}
			})
		}
	}
}

func TestRemoteLibraryWholeDiscExpansionExclusionsAndContentMutationAreNotSubsets(t *testing.T) {
	for _, driver := range []string{"minio", "rclone"} {
		t.Run(driver, func(t *testing.T) {
			x := remoteLibraryFixture(t, driver, "force_all", map[string]string{"Movie.2026/BDMV/index.bdmv": "index", "Movie.2026/BDMV/STREAM/00000.m2ts": "stream", "Movie.2026/CERTIFICATE/id.bdmv": "certificate", "Movie.2026/BDMV/META/metadata.xml": "menu"})
			x.d.Selection = "Movie.2026/BDMV/STREAM/00000.m2ts"
			if _, err := ScanLibrary(x.ctx(t), x.d); err == nil {
				t.Fatal("internal clip silently expanded")
			}
			x.d.AllowDiscExpansion = true
			id, m := x.adopt(t)
			if m.Disc == nil || !m.Disc.Expanded || len(m.Disc.Files) != 4 {
				t.Fatal(m)
			}
			if err := VerifyLibraryMedia(x.ctx(t), x.d, m); err != nil {
				t.Fatal(err)
			}
			x.put(t, "Movie.2026/movie.nfo", "mutable metadata")
			if err := VerifyLibraryMedia(x.ctx(t), x.d, m); err != nil {
				t.Fatal("metadata invalidated readonly disc", err)
			}
			x.put(t, "Movie.2026/CERTIFICATE/id.bdmv", "changed-certificate")
			if VerifyLibraryMedia(x.ctx(t), x.d, m) == nil {
				t.Fatal("disc payload mutation accepted")
			}
			if _, _, err := PublishLibrarySidecar(x.ctx(t), x.d, m, "media", "poster.jpg", []byte("image"), func(LibraryOutputProof) error { t.Fatal("changed disc reached intent"); return nil }, func() error { return nil }); err == nil {
				t.Fatal("changed disc published")
			}
			_ = id
		})
	}
}

func TestRemoteLibraryLateChangesAccountRevocationAndForeignClaimsPreserveOriginal(t *testing.T) {
	for _, mode := range []string{"late-target", "late-media", "account", "claim"} {
		t.Run(mode, func(t *testing.T) {
			x := remoteLibraryFixture(t, "minio", "force_all", map[string]string{"Movie/Movie.2026.mkv": "original-media", "Movie/Movie.2026.nfo": "original-nfo"})
			id, m := x.adopt(t)
			ctx := x.ctx(t)
			target := "Movie/Movie.2026.nfo"
			owner := Digest("owner")
			_, state, err := PublishLibrarySidecar(ctx, x.d, m, "media", "Movie.2026.nfo", []byte("new-nfo"), func(p LibraryOutputProof) error {
				if err := x.s.SaveLibraryOutput(ctx, id, x.d.Root, target, owner, "writing", "", p); err != nil {
					return err
				}
				switch mode {
				case "late-target":
					x.put(t, target, "foreign-current")
				case "late-media":
					x.put(t, m.Path, "changed-media")
				case "account":
					raw, _ := os.ReadFile(x.cfg)
					if err := os.WriteFile(x.cfg, append(raw, '\n'), 0600); err != nil {
						t.Fatal(err)
					}
				case "claim":
					return ErrClaimed
				}
				return nil
			}, func() error { return nil })
			if err == nil || state == "replaced" {
				t.Fatal(mode, state, err)
			}
			want := "original-nfo"
			if mode == "late-target" {
				want = "foreign-current"
			}
			if x.get(t, target) != want {
				t.Fatal("failure replaced target")
			}
		})
	}
}

func TestRemoteLibraryLostWriteAndRestoreReceiptsRequireFreshProofAndNeverReplay(t *testing.T) {
	x := remoteLibraryFixture(t, "minio", "force_all", map[string]string{"Movie/Movie.2026.mkv": "original-media", "Movie/Movie.2026.nfo": "original-nfo"})
	id, m := x.adopt(t)
	ctx := x.ctx(t)
	target := "Movie/Movie.2026.nfo"
	owner := Digest("owner")
	x.cloud.FailNextWrite("bucket/library/"+target, false, 500)
	p, state, err := PublishLibrarySidecar(ctx, x.d, m, "media", "Movie.2026.nfo", []byte("new-nfo"), func(p LibraryOutputProof) error {
		return x.s.SaveLibraryOutput(ctx, id, x.d.Root, target, owner, "writing", "", p)
	}, func() error { return nil })
	if err == nil || state != "uncertain" {
		t.Fatal(state, err)
	}
	if err := x.s.SaveLibraryOutput(ctx, id, x.d.Root, target, owner, state, "lost response", p); err != nil {
		t.Fatal(err)
	}
	if evidence, err := ObserveLibrarySidecar(ctx, x.d, p); err != nil || evidence != "replaced" {
		t.Fatal(evidence, err)
	}
	out, err := x.s.FindLibraryOutput(ctx, id, target)
	if err != nil {
		t.Fatal(err)
	}
	x.cloud.FailNextWrite("bucket/library/"+target, false, 500)
	if _, err := RestoreLibrarySidecar(ctx, x.d, p, func() error { return x.s.LibraryRestoreIntent(ctx, out.ID, Digest(p)) }); err == nil {
		t.Fatal("lost restore reported success")
	}
	before := len(x.cloud.Requests())
	if state, err := RestoreLibrarySidecar(ctx, x.d, p, func() error { return x.s.LibraryRestoreIntent(ctx, out.ID, Digest(p)) }); err != nil || state != "restored" {
		t.Fatal(state, err)
	}
	for _, r := range x.cloud.Requests()[before:] {
		if r.Method == "PUT" || r.Method == "POST" || r.Method == "DELETE" {
			t.Fatal("positive restore receipt replayed write")
		}
	}
	if err := x.s.FinishLibraryRestore(ctx, out.ID, x.d.Root); err != nil {
		t.Fatal(err)
	}
	x.put(t, target, "later-owner")
	if _, err := RestoreLibrarySidecar(ctx, x.d, p, func() error { return nil }); !errors.Is(err, ErrState) {
		t.Fatal("late metadata change adopted", err)
	}
}

func TestRemoteLibraryAndIntakeUncertaintyShareTheSameDurableMetadataReservation(t *testing.T) {
	x := remoteLibraryFixture(t, "minio", "force_all", map[string]string{"Movie/Movie.2026.mkv": "original-media", "Movie/Movie.2026.nfo": "old metadata"})
	id, m := x.adopt(t)
	ctx := x.ctx(t)
	target := "Movie/Movie.2026.nfo"
	owner := Digest("owner")
	x.cloud.FailNextWrite("bucket/library/"+target, false, 500)
	p, state, err := PublishLibrarySidecar(ctx, x.d, m, "media", "Movie.2026.nfo", []byte("new metadata"), func(p LibraryOutputProof) error {
		return x.s.SaveLibraryOutput(ctx, id, x.d.Root, target, owner, "writing", "", p)
	}, func() error { return nil })
	if err == nil || state != "uncertain" {
		t.Fatal(state, err)
	}
	if err := x.s.SaveLibraryOutput(ctx, id, x.d.Root, target, owner, state, "uncertain", p); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.db.Exec(`CREATE TABLE IF NOT EXISTS GO_SCRAPE_JOBS(ID TEXT PRIMARY KEY,JOB_ID TEXT,ORDINAL INTEGER,NAME TEXT,ORIGIN TEXT,CONFIG TEXT,OPTIONS TEXT,STATE TEXT,REASON TEXT,OUTPUTS TEXT,UPDATED TEXT)`); err != nil {
		t.Fatal(err)
	}
	definition, _ := json.Marshal(Definition{Remote: x.d.Remote, TargetRoot: x.d.Root.Path, Mode: "miniocopy"})
	if _, err := x.s.db.Exec(`INSERT INTO GO_ORGANIZATION_JOBS(ID,FINGERPRINT,DEFINITION,CREATED,STATE) VALUES('intake','fixture',?,'now','completed')`, string(definition)); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.db.Exec(`INSERT INTO GO_SCRAPE_JOBS VALUES('scrape','intake',0,'media','origin','config','{}','running','','[]','now')`); err != nil {
		t.Fatal(err)
	}
	record := SidecarRecord{Name: "Movie.2026.nfo", Scope: "media", State: "writing", Proof: SidecarProof{Target: target, Identity: "remote:original-intent"}}
	if err := x.s.SaveScrapeOutput(ctx, "scrape", record); !errors.Is(err, ErrClaimed) {
		t.Fatal("intake bypassed library uncertainty", err)
	}
	// A separately uncertain intake output must also block a library restore,
	// even when the final bytes still match the library's previous replacement.
	data, _ := json.Marshal([]SidecarRecord{record})
	if _, err := x.s.db.Exec(`UPDATE GO_SCRAPE_JOBS SET OUTPUTS=? WHERE ID='scrape'`, string(data)); err != nil {
		t.Fatal(err)
	}
	out, err := x.s.FindLibraryOutput(ctx, id, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.s.LibraryRestoreIntent(ctx, out.ID, Digest(p)); !errors.Is(err, ErrClaimed) {
		t.Fatal("restore bypassed intake uncertainty", err)
	}
	if remoteMetadataKey("/bucket/Library", "Movie/Movie.2026.nfo") != remoteMetadataKey("/bucket/library", "movie/MOVIE.2026.NFO") {
		t.Fatal("case/Unicode alias bypassed logical reservation")
	}
}

func TestRemoteLibraryMultiVolumeExpansionAndInternalExclusionCannotAdoptOneVolume(t *testing.T) {
	for _, driver := range []string{"minio", "rclone"} {
		t.Run(driver, func(t *testing.T) {
			x := remoteLibraryFixture(t, driver, "no_force", map[string]string{"Movie.2026/DISC1/BDMV/index.bdmv": "index-one", "Movie.2026/DISC1/BDMV/STREAM/00000.m2ts": "stream-one", "Movie.2026/DISC2/BDMV/index.bdmv": "index-two", "Movie.2026/DISC2/BDMV/STREAM/00000.m2ts": "stream-two"})
			x.d.Selection = "Movie.2026/DISC1/BDMV/index.bdmv"
			x.d.AllowDiscExpansion = true
			scan, err := ScanLibrary(x.ctx(t), x.d)
			if err != nil || len(scan.Files) != 1 || scan.Files[0].Disc == nil || scan.Files[0].Path != "Movie.2026" || len(scan.Files[0].Disc.Files) != 4 {
				t.Fatal("multi-volume source was split", scan, err)
			}
			x.d.Selection = "Movie.2026"
			x.d.Excludes = []string{"Movie.2026/DISC1/BDMV"}
			scan, err = ScanLibrary(x.ctx(t), x.d)
			if err != nil || len(scan.Files) != 0 {
				t.Fatal("internal exclusion became partial success", scan, err)
			}
		})
	}
}

func TestRemoteLibraryAutomaticKindKeepsExactAccountRootAndRejectsOtherBindings(t *testing.T) {
	x := remoteLibraryFixture(t, "minio", "force_all", map[string]string{"Movie/Movie.2026.mkv": "original-media"})
	x.d.Root.Kind = "auto"
	id, m := x.adopt(t)
	effective := x.d
	effective.Root.Kind = "movie"
	target := "Movie/Movie.2026.nfo"
	owner := Digest("owner")
	_, state, err := PublishLibrarySidecar(x.ctx(t), effective, m, "media", "Movie.2026.nfo", []byte("new metadata"), func(p LibraryOutputProof) error {
		for _, change := range []string{"id", "path", "identity", "kind"} {
			wrong := effective.Root
			switch change {
			case "id":
				wrong.ID = "other-root"
			case "path":
				wrong.Path = "/bucket/other"
			case "identity":
				wrong.Identity = "changed"
			case "kind":
				wrong.Kind = "arbitrary"
			}
			if err := x.s.SaveLibraryOutput(x.ctx(t), id, wrong, target, owner, "writing", "", p); !errors.Is(err, ErrState) {
				t.Fatal("unrelated root accepted", change, err)
			}
		}
		return x.s.SaveLibraryOutput(x.ctx(t), id, effective.Root, target, owner, "writing", "", p)
	}, func() error { return nil })
	if err != nil || state != "created" {
		t.Fatal(state, err)
	}
}
