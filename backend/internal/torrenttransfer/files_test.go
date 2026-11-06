package torrenttransfer

import (
	"bytes"
	"context"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func transferFileFixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "torrents")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	// Construct the string length rather than relying on the tracker URL's width.
	tracker := "https://tracker.fixture.test/a"
	data := []byte("d8:announce" + strconv.Itoa(len(tracker)) + ":" + tracker + "4:infod6:lengthi8e4:name9:movie.mkvee")
	meta, err := torrentmeta.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, meta.InfoHash+".torrent"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return root, meta.InfoHash, data
}
func TestTransferMetainfoReadAndReverificationNeverChangeTheOriginal(t *testing.T) {
	root, hash, data := transferFileFixture(t)
	bound, err := OpenMetainfoRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	snapshot, err := bound.Read(t.Context(), hash, "", true)
	if err != nil || snapshot.Prepared.InfoHash != hash {
		t.Fatal(err)
	}
	if err := bound.Verify(t.Context(), snapshot, true); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(root, hash+".torrent"))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatal("input changed", err)
	}
	changed := bytes.Replace(data, []byte("fixture.test/a"), []byte("fixture.test/b"), 1)
	if err := os.WriteFile(filepath.Join(root, hash+".torrent"), changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := bound.Verify(t.Context(), snapshot, true); err == nil {
		t.Fatal("private outer metadata change ignored")
	}
}
func TestTransferMetainfoRejectsSymlinksForeignHashesAndCancelledReads(t *testing.T) {
	root, hash, _ := transferFileFixture(t)
	bound, err := OpenMetainfoRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if _, err := bound.Read(t.Context(), hash, "../"+hash+".torrent", false); err == nil {
		t.Fatal("escaped root")
	}
	original := filepath.Join(root, hash+".torrent")
	link := filepath.Join(root, "link.torrent")
	if err := os.Symlink(original, link); err != nil {
		t.Fatal(err)
	}
	if _, err := bound.Read(t.Context(), hash, "link.torrent", false); err == nil {
		t.Fatal("followed symlink")
	}
	if _, err := bound.Read(t.Context(), "0000000000000000000000000000000000000000", hash+".torrent", false); err == nil {
		t.Fatal("foreign hash accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := bound.Read(ctx, hash, "", false); err == nil {
		t.Fatal("cancelled read succeeded")
	}
}

func TestTransferMetainfoRootReplacementAndAliasRetargetRevokeReadAccess(t *testing.T) {
	for _, alias := range []bool{false, true} {
		root, hash, _ := transferFileFixture(t)
		configured := root
		if alias {
			configured = filepath.Join(filepath.Dir(root), "current")
			if err := os.Symlink(root, configured); err != nil {
				t.Fatal(err)
			}
		}
		bound, err := OpenMetainfoRoot(configured)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := bound.Read(t.Context(), hash, "", false)
		if err != nil {
			bound.Close()
			t.Fatal(err)
		}
		if alias {
			other := filepath.Join(filepath.Dir(root), "other")
			if err := os.Mkdir(other, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(configured); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, configured); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Rename(root, root+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := bound.Verify(t.Context(), snapshot, false); err == nil {
			t.Fatal("replacement retained old authority")
		}
		bound.Close()
	}
}
