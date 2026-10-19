//go:build linux || darwin

package organization

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sidecarFixture(t *testing.T) (Job, JobItem) {
	t.Helper()
	d, e := copyFixture(t)
	p, err := Copy(t.Context(), d, e, TempName("sidecar", 0), func(Proof) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(d, e, p); err != nil {
		t.Fatal(err)
	}
	return Job{State: "completed", Definition: d}, JobItem{Entry: e, State: "completed", Proof: p}
}
func TestSidecarPublicationPinsCompletedMediaAndNeverOverwrites(t *testing.T) {
	for _, mode := range []string{"new", "early", "late", "symlink", "movie-nfo", "late-movie-nfo", "no-intent", "parent-replaced", "changed-media", "late-changed-media", "temp-replaced"} {
		t.Run(mode, func(t *testing.T) {
			job, item := sidecarFixture(t)
			dir := filepath.Dir(filepath.Join(job.Definition.TargetRoot, item.Target))
			name := "Movie.nfo"
			target := filepath.Join(dir, name)
			writeExisting := func() {
				if err := os.WriteFile(target, []byte("keep-existing"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "early" {
				writeExisting()
			}
			if mode == "symlink" {
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), target); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "movie-nfo" {
				os.WriteFile(filepath.Join(dir, "movie.nfo"), []byte("existing movie metadata"), 0600)
			}
			if mode == "changed-media" {
				os.WriteFile(filepath.Join(job.Definition.TargetRoot, item.Target), []byte("replaced content"), 0600)
			}
			prepared := false
			proof, state, err := PublishSidecar(t.Context(), job, item, "media", name, []byte("<movie/>"), func(p SidecarProof) error {
				prepared = true
				f, err := os.Stat(filepath.Join(dir, p.Temp, "payload"))
				if err != nil || f.Size() != 0 || p.Identity == "" {
					t.Fatal("bytes written before intent", p, err)
				}
				if mode == "no-intent" {
					return errors.New("journal unavailable")
				}
				if mode == "late" {
					writeExisting()
				}
				if mode == "late-movie-nfo" {
					if err := os.WriteFile(filepath.Join(dir, "movie.nfo"), []byte("late canonical NFO"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "late-changed-media" {
					media := filepath.Join(job.Definition.TargetRoot, item.Target)
					info, _ := os.Stat(media)
					if err := os.WriteFile(media, bytes.Repeat([]byte("x"), int(info.Size())), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(media, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "temp-replaced" {
					temp := filepath.Join(dir, p.Temp)
					if err := os.Rename(temp, temp+"-saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(temp, []byte("foreign node"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "parent-replaced" {
					if err := os.Rename(dir, dir+"-saved"); err != nil {
						t.Fatal(err)
					}
					os.Mkdir(dir, 0700)
				}
				return nil
			}, func() error { return nil })
			switch mode {
			case "new":
				if err != nil || state != "created" || !prepared || len(proof.Digest) != 64 {
					t.Fatal(state, err, proof)
				}
				b, _ := os.ReadFile(target)
				if string(b) != "<movie/>" {
					t.Fatal(string(b))
				}
			case "early", "late":
				if err != nil || state != "preserved" {
					t.Fatal(state, err)
				}
				b, _ := os.ReadFile(target)
				if string(b) != "keep-existing" {
					t.Fatal(string(b))
				}
			case "symlink", "movie-nfo", "late-movie-nfo":
				if err != nil || state != "preserved" {
					t.Fatal(state, err)
				}
			case "no-intent", "parent-replaced", "changed-media", "late-changed-media", "temp-replaced":
				if err == nil || state == "created" {
					t.Fatal(mode, state, err)
				}
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unexpected publication", err)
				}
			}
		})
	}
}
func TestSidecarPathsCannotEscapeApprovedDirectoryOrWriteMedia(t *testing.T) {
	job, item := sidecarFixture(t)
	for _, name := range []string{"../../elsewhere.nfo", "Movie.mkv", "other.nfo", ".nastool-sidecar-evil", "poster.svg"} {
		if _, _, err := PublishSidecar(t.Context(), job, item, "media", name, []byte("x"), func(SidecarProof) error { t.Fatal("invalid name reached intent"); return nil }, func() error { return nil }); err == nil {
			t.Fatal(name)
		}
	}
	if _, _, err := PublishSidecar(t.Context(), job, item, "show", "tvshow.nfo", []byte("x"), func(SidecarProof) error { return nil }, func() error { return nil }); err == nil {
		t.Fatal("movie metadata escaped into show directory")
	}
}
func TestScrapeQueueAtomicIntentConfigurationScopeAndNoReplay(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.Scrapes(t.Context(), false)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	var exists bool
	s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_SCRAPE_JOBS')`).Scan(&exists)
	if exists {
		t.Fatal("readonly created scraper journal")
	}
	s.db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT)`)
	s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','UserScraperConf','{"scraper_nfo":{"movie":{"basic":true}}}')`)
	emit := func(id string) {
		t.Helper()
		tx, err := s.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := emitScrape(t.Context(), tx, id, 0, "Movie", "frozen-proof"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	emit("job-one")
	emit("job-one")
	rows, err = s.Scrapes(t.Context(), true)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if err := s.ClaimScrape(t.Context(), rows[0]); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.ScrapePending(t.Context(), "job-one", 0); err != nil || pending {
		t.Fatal("running replayed", pending, err)
	}
	if err := s.SaveScrapeOutput(t.Context(), rows[0].ID, SidecarRecord{Name: "Movie.nfo", Scope: "media", State: "writing", Proof: SidecarProof{Temp: "private-temp", Digest: strings.Repeat("a", 64)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimScrape(t.Context(), rows[0]); !errors.Is(err, ErrState) {
		t.Fatal("running task reclaimed", err)
	}
	public, _ := s.Scrapes(t.Context(), false)
	b, _ := json.Marshal(public)
	if strings.Contains(string(b), "private-temp") || strings.Contains(string(b), "frozen-proof") || strings.Contains(string(b), "scraper_nfo") {
		t.Fatal("private journal escaped", string(b))
	}
	emit("job-two")
	rows, _ = s.Scrapes(t.Context(), true)
	s.db.Exec(`UPDATE SYSTEM_DICT SET VALUE='{"scraper_nfo":{"movie":{"basic":false}}}'`)
	if err := s.ClaimScrape(t.Context(), rows[0]); !errors.Is(err, ErrState) {
		t.Fatal("changed config adopted", err)
	}
	if rows, _ := s.Scrapes(t.Context(), true); len(rows) != 0 {
		t.Fatal(rows)
	}
}
