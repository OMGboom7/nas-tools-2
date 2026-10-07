//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func copyFixture(t *testing.T) (Definition, Entry) {
	t.Helper()
	source, _ := filepath.EvalSymlinks(t.TempDir())
	target, _ := filepath.EvalSymlinks(t.TempDir())
	fixtureFile(t, source, "Movie.mkv")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(source, "Movie.mkv"), old, old); err != nil {
		t.Fatal(err)
	}
	sourceInfo, _ := os.Stat(source)
	targetInfo, _ := os.Stat(target)
	fileInfo, _ := os.Stat(filepath.Join(source, "Movie.mkv"))
	entry := Entry{Source: "Movie.mkv", Target: "Movie (2026)/Movie.mkv", Kind: "media", Size: fileInfo.Size(), Modified: strconv.FormatInt(fileInfo.ModTime().UnixNano(), 10), Identity: Identity(fileInfo), TMDBID: "100", Title: "Movie", Year: "2026", MediaType: "电影"}
	d := Definition{SourceRoot: source, TargetRoot: target, SourceIdentity: Identity(sourceInfo), TargetIdentity: Identity(targetInfo), Mode: "copy", Entries: []Entry{entry}}
	return d, entry
}

func TestCopyPublishesVerifiedBytesWithoutChangingSourceOrLeavingStage(t *testing.T) {
	d, item := copyFixture(t)
	proof, err := Copy(t.Context(), d, item, TempName("test", 0), func(p Proof) error {
		if _, err := os.Stat(filepath.Join(d.TargetRoot, item.Target)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("published before journal", err)
		}
		if p.Identity == "" || len(p.Digest) != 64 {
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
	sourceInfo, _ := os.Stat(filepath.Join(d.SourceRoot, item.Source))
	targetInfo, _ := os.Stat(filepath.Join(d.TargetRoot, item.Target))
	if os.SameFile(sourceInfo, targetInfo) {
		t.Fatal("copy mode linked the source instead of copying its bytes")
	}
	if err = Cleanup(d, item, proof); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(d.SourceRoot, item.Source), filepath.Join(d.TargetRoot, item.Target)} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "original" {
			t.Fatal(path, string(data), err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(filepath.Join(d.TargetRoot, item.Target)))
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
}

func TestCopyNeverOverwritesTargetIncludingLateConflict(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(strconv.FormatBool(late), func(t *testing.T) {
			d, item := copyFixture(t)
			write := func() {
				fixtureFile(t, d.TargetRoot, item.Target)
				if err := os.WriteFile(filepath.Join(d.TargetRoot, item.Target), []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if !late {
				write()
			}
			_, err := Copy(t.Context(), d, item, TempName("test", 0), func(Proof) error {
				if late {
					write()
				}
				return nil
			})
			if err == nil {
				t.Fatal("overwrote conflict")
			}
			bytes, _ := os.ReadFile(filepath.Join(d.TargetRoot, item.Target))
			if string(bytes) != "existing" {
				t.Fatal(string(bytes))
			}
		})
	}
}

func TestCopyRejectsSymlinksChangedSnapshotsAndActiveFiles(t *testing.T) {
	for _, kind := range []string{"source-link", "target-link", "root-changed", "source-changed", "active"} {
		t.Run(kind, func(t *testing.T) {
			d, item := copyFixture(t)
			switch kind {
			case "source-link":
				if err := os.Rename(filepath.Join(d.SourceRoot, item.Source), filepath.Join(d.SourceRoot, "saved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("saved", filepath.Join(d.SourceRoot, item.Source)); err != nil {
					t.Fatal(err)
				}
			case "target-link":
				if err := os.Symlink(t.TempDir(), filepath.Join(d.TargetRoot, "Movie (2026)")); err != nil {
					t.Fatal(err)
				}
			case "root-changed":
				d.SourceIdentity = "different"
			case "source-changed":
				if err := os.WriteFile(filepath.Join(d.SourceRoot, item.Source), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "active":
				now := time.Now()
				os.Chtimes(filepath.Join(d.SourceRoot, item.Source), now, now)
				info, _ := os.Stat(filepath.Join(d.SourceRoot, item.Source))
				item.Modified = strconv.FormatInt(info.ModTime().UnixNano(), 10)
			}
			if _, err := Copy(t.Context(), d, item, TempName("test", 0), func(Proof) error { return nil }); err == nil {
				t.Fatal("unsafe file accepted")
			}
			if _, err := os.Stat(filepath.Join(d.TargetRoot, item.Target)); err == nil {
				t.Fatal("published unsafe file")
			}
		})
	}
}

func TestCopyPreparedInterruptionIsNotPublicationAndUnrelatedTargetsCannotRecover(t *testing.T) {
	d, item := copyFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	proof, err := Copy(ctx, d, item, TempName("test", 0), func(Proof) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := VerifyPublished(t.Context(), d, item, proof); err == nil {
		t.Fatal("staging treated as published")
	}
	fixtureFile(t, d.TargetRoot, item.Target)
	if err := VerifyPublished(t.Context(), d, item, proof); err == nil {
		t.Fatal("unrelated equal bytes treated as owned target")
	}
}

func TestCopyRejectsSourceAndDirectoryReplacementAfterPreparation(t *testing.T) {
	for _, kind := range []string{"source", "directory", "content"} {
		t.Run(kind, func(t *testing.T) {
			d, item := copyFixture(t)
			_, err := Copy(t.Context(), d, item, TempName("test", 0), func(Proof) error {
				switch kind {
				case "source":
					path := filepath.Join(d.SourceRoot, item.Source)
					os.Rename(path, path+".saved")
					fixtureFile(t, d.SourceRoot, item.Source)
				case "directory":
					path := filepath.Dir(filepath.Join(d.TargetRoot, item.Target))
					os.Rename(path, path+".saved")
					os.Mkdir(path, 0700)
				case "content":
					os.WriteFile(filepath.Join(d.SourceRoot, item.Source), []byte("changed!"), 0600)
				}
				return nil
			})
			if err == nil {
				t.Fatal("published changed snapshot")
			}
			if _, err := os.Stat(filepath.Join(d.TargetRoot, item.Target)); err == nil {
				t.Fatal("unexpected destination")
			}
		})
	}
}

func TestJournalClaimsSurviveReopenAndOnlyOneConcurrentExecutionWins(t *testing.T) {
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
	again, err := s.Create(t.Context(), Digest(d), d)
	if err != nil || again != id {
		t.Fatal(again, err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := s.Claim(t.Context(), id, 0)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Error(err)
			}
			if won {
				wins++
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatal(wins)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err := s.Get(t.Context(), id)
	if err != nil || job.Items[0].State != "running" || job.State != "needs_review" {
		t.Fatal(job, err)
	}
	if err = s.Cancel(t.Context(), id); !errors.Is(err, ErrState) {
		t.Fatal("cancel released interrupted claim", err)
	}
	d.Entries[0] = item
	d.Entries[0].Identity = "other"
	if _, err = s.Create(t.Context(), Digest(d), d); !errors.Is(err, ErrClaimed) {
		t.Fatal("target claim expired", err)
	}
}

func TestJournalCancelledDraftReleasesTargetsWithoutErasingJob(t *testing.T) {
	d, _ := copyFixture(t)
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Cancel(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(t.Context(), id, 0); err != nil || won {
		t.Fatal(won, err)
	}
	job, err := s.Get(t.Context(), id)
	if err != nil || job.State != "cancelled" {
		t.Fatal(job, err)
	}
	next, err := s.Create(t.Context(), Digest(d), d)
	if err != nil || next == id {
		t.Fatal(next, err)
	}
}

func TestJournalPublishedFileRecoversAfterHistoryFailureWithoutSecondCopy(t *testing.T) {
	d, item := copyFixture(t)
	path := filepath.Join(t.TempDir(), "user.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY, MODE TEXT, TYPE TEXT, CATEGORY TEXT, TMDBID INTEGER, TITLE TEXT, YEAR TEXT, SEASON_EPISODE TEXT, SOURCE TEXT, SOURCE_PATH TEXT, SOURCE_FILENAME TEXT, DEST TEXT, DEST_PATH TEXT, DEST_FILENAME TEXT, DATE TEXT); CREATE TRIGGER deny_history BEFORE INSERT ON TRANSFER_HISTORY BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
		t.Fatal(won, err)
	}
	proof, err := Copy(t.Context(), d, item, TempName(id, 0), func(p Proof) error { return s.Prepare(t.Context(), id, 0, p) })
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(t.Context(), job, job.Items[0]); err == nil {
		t.Fatal("fixture failure ignored")
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&n)
	if n != 0 {
		t.Fatal(n)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err = s.Get(t.Context(), id)
	if err != nil || job.Items[0].State != "prepared" {
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
	s.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE MODE='复制' AND TYPE='电影' AND TITLE='Movie' AND SOURCE='手动整理'`).Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
	if err = Cleanup(d, item, proof); err != nil {
		t.Fatal(err)
	}
	job, err = s.Get(t.Context(), id)
	if err != nil || job.State != "completed" {
		t.Fatal(job, err)
	}
}
