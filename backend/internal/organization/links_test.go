//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestTransferLinkModesPreserveSourceAndPublishActualRequestedObject(t *testing.T) {
	for _, mode := range []string{"link", "softlink"} {
		t.Run(mode, func(t *testing.T) {
			d, item := copyFixture(t)
			d.Mode = mode
			before, _ := os.Stat(filepath.Join(d.SourceRoot, item.Source))
			proof, err := Transfer(t.Context(), d, item, TempName("test", 0), func(p Proof) error {
				if _, err := os.Lstat(filepath.Join(d.TargetRoot, item.Target)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("published before journal", err)
				}
				if p.Digest == "" || p.Identity == "" {
					t.Fatal(p)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = VerifyPublished(t.Context(), d, item, proof); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(d.TargetRoot, item.Target)
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "link" {
				if !info.Mode().IsRegular() || !os.SameFile(before, info) || proof.Identity != item.Identity {
					t.Fatal("hard link silently became a copy", info, proof)
				}
			} else {
				link, err := os.Readlink(path)
				if err != nil || link != filepath.Join(d.SourceRoot, item.Source) || info.Mode()&os.ModeSymlink == 0 || proof.Kind != "symlink" {
					t.Fatal("soft link silently became a regular file", link, proof, err)
				}
			}
			if err = Cleanup(d, item, proof); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(filepath.Join(d.SourceRoot, item.Source))
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("linking changed/deleted source", err)
			}
			bytes, err := os.ReadFile(path)
			if err != nil || string(bytes) != "original" {
				t.Fatal(string(bytes), err)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(entries) != 1 {
				t.Fatal("staging cleanup failed", entries, err)
			}
		})
	}
}

func TestTransferLinkModesNeverReplaceExistingOrLateTargetLinks(t *testing.T) {
	for _, mode := range []string{"link", "softlink"} {
		for _, late := range []bool{false, true} {
			name := mode + "/early"
			if late {
				name = mode + "/late"
			}
			t.Run(name, func(t *testing.T) {
				d, item := copyFixture(t)
				d.Mode = mode
				target := filepath.Join(d.TargetRoot, item.Target)
				var original os.FileInfo
				create := func() {
					os.MkdirAll(filepath.Dir(target), 0700)
					if err := os.Symlink(filepath.Join(d.SourceRoot, item.Source), target); err != nil {
						t.Fatal(err)
					}
					original, _ = os.Lstat(target)
				}
				if !late {
					create()
				}
				_, err := Transfer(t.Context(), d, item, TempName("test", 0), func(Proof) error {
					if late {
						create()
					}
					return nil
				})
				if !errors.Is(err, ErrClaimed) {
					t.Fatal(err)
				}
				after, err := os.Lstat(target)
				if err != nil || !os.SameFile(original, after) {
					t.Fatal("replaced existing target", err)
				}
			})
		}
	}
}

func TestTransferLinkModesRejectSourceReplacementAndCancellationAfterPreparation(t *testing.T) {
	for _, mode := range []string{"link", "softlink"} {
		for _, change := range []string{"file", "source-root", "cancel"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				d, item := copyFixture(t)
				d.Mode = mode
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				before, _ := os.Stat(filepath.Join(d.SourceRoot, item.Source))
				_, err := Transfer(ctx, d, item, TempName("test", 0), func(Proof) error {
					path := filepath.Join(d.SourceRoot, item.Source)
					switch change {
					case "file":
						if err := os.Rename(path, path+".saved"); err != nil {
							t.Fatal(err)
						}
						fixtureFile(t, d.SourceRoot, item.Source)
						os.Chtimes(path, before.ModTime(), before.ModTime())
					case "source-root":
						if err := os.Rename(d.SourceRoot, d.SourceRoot+".saved"); err != nil {
							t.Fatal(err)
						}
						os.Mkdir(d.SourceRoot, 0700)
						fixtureFile(t, d.SourceRoot, item.Source)
						os.Chtimes(path, before.ModTime(), before.ModTime())
					case "cancel":
						cancel()
					}
					return nil
				})
				if err == nil {
					t.Fatal("published changed source")
				}
				if _, err := os.Lstat(filepath.Join(d.TargetRoot, item.Target)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unexpected publication", err)
				}
			})
		}
	}
}

func TestSoftlinkProofRejectsDanglingForeignAndTamperedObjects(t *testing.T) {
	for _, kind := range []string{"dangling", "source-content", "source-replaced", "foreign-link", "kind", "link-target", "stage-path", "source-link"} {
		t.Run(kind, func(t *testing.T) {
			d, item := copyFixture(t)
			d.Mode = "softlink"
			proof, err := Transfer(t.Context(), d, item, TempName("test", 0), func(Proof) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			source, target := filepath.Join(d.SourceRoot, item.Source), filepath.Join(d.TargetRoot, item.Target)
			switch kind {
			case "dangling":
				os.Remove(source)
			case "source-content":
				os.WriteFile(source, []byte("changed!"), 0600)
			case "source-replaced":
				info, _ := os.Stat(source)
				os.Rename(source, source+".saved")
				fixtureFile(t, d.SourceRoot, item.Source)
				os.Chtimes(source, info.ModTime(), info.ModTime())
			case "foreign-link":
				os.Remove(target)
				os.Symlink(source, target)
			case "kind":
				proof.Kind = "regular"
			case "link-target":
				proof.LinkTarget = filepath.Join(t.TempDir(), "outside")
			case "stage-path":
				proof.Temp = "/outside"
			case "source-link":
				os.Rename(source, source+".saved")
				os.Symlink(source+".saved", source)
			}
			if err = VerifyPublished(t.Context(), d, item, proof); err == nil {
				t.Fatal("accepted invalid softlink proof")
			}
		})
	}
}

func TestHardlinkProofUsesOwnedInodeAndDigestNotSourcePathExistence(t *testing.T) {
	d, item := copyFixture(t)
	d.Mode = "link"
	proof, err := Transfer(t.Context(), d, item, TempName("test", 0), func(Proof) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(d.SourceRoot, item.Source)); err != nil {
		t.Fatal(err)
	}
	if err = VerifyPublished(t.Context(), d, item, proof); err != nil {
		t.Fatal("removing source name destroyed valid hardlink", err)
	}
	if err = os.WriteFile(filepath.Join(d.TargetRoot, item.Target), []byte("changed!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = VerifyPublished(t.Context(), d, item, proof); err == nil {
		t.Fatal("changed hardlink accepted")
	}
}

func TestModeValidationRejectsCrossDeviceWithoutFallbackOrWrites(t *testing.T) {
	d, item := copyFixture(t)
	d.Mode = "link"
	item.Identity = "other-device:1"
	if err := ValidateMode(t.Context(), d, item); !errors.Is(err, ErrMode) {
		t.Fatal(err)
	}
	if _, err := Transfer(t.Context(), d, item, TempName("test", 0), func(Proof) error { t.Fatal("prepared invalid device"); return nil }); !errors.Is(err, ErrMode) {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(d.TargetRoot); err != nil || len(entries) != 0 {
		t.Fatal("device rejection wrote target", entries, err)
	}
	for _, mode := range []string{"move", "rclonecopy", "miniocopy", "invalid"} {
		d.Mode = mode
		if _, err := Transfer(t.Context(), d, item, TempName("test", 0), func(Proof) error { return nil }); !errors.Is(err, ErrMode) {
			t.Fatal(mode, err)
		}
	}
}

func TestLinkJobsRecoverAfterRestartAndHistoryFailureWithExactMode(t *testing.T) {
	for _, mode := range []string{"link", "softlink"} {
		t.Run(mode, func(t *testing.T) {
			d, item := copyFixture(t)
			d.Mode = mode
			path := filepath.Join(t.TempDir(), "user.db")
			s, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY, MODE TEXT, TYPE TEXT, CATEGORY TEXT, TMDBID INTEGER, TITLE TEXT, YEAR TEXT, SEASON_EPISODE TEXT, SOURCE TEXT, SOURCE_PATH TEXT, SOURCE_FILENAME TEXT, DEST TEXT, DEST_PATH TEXT, DEST_FILENAME TEXT, DATE TEXT); INSERT INTO TRANSFER_HISTORY(TITLE) VALUES ('legacy'); CREATE TRIGGER deny_history BEFORE INSERT ON TRANSFER_HISTORY BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
				t.Fatal(err)
			}
			id, err := s.Create(t.Context(), Digest(d), d)
			if err != nil {
				t.Fatal(err)
			}
			if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
				t.Fatal(won, err)
			}
			proof, err := Transfer(t.Context(), d, item, TempName(id, 0), func(p Proof) error { return s.Prepare(t.Context(), id, 0, p) })
			if err != nil {
				t.Fatal(err)
			}
			job, err := s.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Complete(t.Context(), job, job.Items[0]); err == nil {
				t.Fatal("history failure ignored")
			}
			s.Close()
			s, err = OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			job, err = s.Get(t.Context(), id)
			if err != nil || job.Items[0].State != "prepared" || job.Mode != mode {
				t.Fatal(job, err)
			}
			if err = VerifyPublished(t.Context(), job.Definition, job.Items[0].Entry, job.Items[0].Proof); err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec(`DROP TRIGGER deny_history`); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err = s.Complete(t.Context(), job, job.Items[0]); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE MODE=? AND TITLE='Movie'`, ModeLabel(mode)).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
			s.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE TITLE='legacy'`).Scan(&count)
			if count != 1 {
				t.Fatal("legacy history modified")
			}
			if err = Cleanup(d, item, proof); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSoftlinkPublicationKeepsPrivateAnchorOnInterruptionBeforePublish(t *testing.T) {
	d, item := copyFixture(t)
	d.Mode = "softlink"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	proof, err := Transfer(ctx, d, item, TempName("test", 0), func(Proof) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = VerifyPublished(t.Context(), d, item, proof); err == nil {
		t.Fatal("unpublished stage recognized as success")
	}
	stage := filepath.Join(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)), proof.Temp, "payload")
	info, err := os.Lstat(stage)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("lost interruption evidence", err)
	}
}

func TestStatIdentityAgreesWithRegularAndSymlinkFileInfo(t *testing.T) {
	d, item := copyFixture(t)
	root, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := unix.Symlinkat(item.Source, int(root.Fd()), "alias"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{item.Source, "alias"} {
		info, err := os.Lstat(filepath.Join(d.SourceRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		stat, err := lstatAt(root, name)
		if err != nil || statIdentity(stat) != Identity(info) {
			t.Fatal(name, statIdentity(stat), Identity(info), err)
		}
	}
}

func TestLegacyCopyProofWithoutObjectTypeRemainsRecoverable(t *testing.T) {
	d, item := copyFixture(t)
	path := filepath.Join(t.TempDir(), "user.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
		t.Fatal(won, err)
	}
	_, err = Copy(t.Context(), d, item, TempName(id, 0), func(p Proof) error { p.Kind = ""; p.LinkTarget = ""; return s.Prepare(t.Context(), id, 0, p) })
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err := s.Get(t.Context(), id)
	if err != nil || job.Items[0].Proof.Kind != "" {
		t.Fatal(job, err)
	}
	if err = VerifyPublished(t.Context(), job.Definition, job.Items[0].Entry, job.Items[0].Proof); err != nil {
		t.Fatal("legacy copy proof invalidated", err)
	}
	if err = s.Complete(t.Context(), job, job.Items[0]); err != nil {
		t.Fatal(err)
	}
	if err = Cleanup(d, item, job.Items[0].Proof); err != nil {
		t.Fatal(err)
	}
}

func TestModeOperationErrorsAreExplicitAndDoNotTriggerFallback(t *testing.T) {
	for _, err := range []error{unix.EXDEV, unix.EOPNOTSUPP, unix.ENOSYS, unix.EPERM} {
		if !errors.Is(modeOperationError(err), ErrMode) {
			t.Fatal(err)
		}
	}
	for _, err := range []error{context.Canceled, unix.ENOSPC, unix.EIO} {
		if !errors.Is(modeOperationError(err), err) {
			t.Fatal("lost underlying cancellation/failure", err)
		}
	}
}
