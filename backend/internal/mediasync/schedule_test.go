package mediasync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

func scheduleFixture(t *testing.T) (*Store, ScheduleDefinition, time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ScheduleDefinition{Server: "emby", ConfigDigest: strings.Repeat("a", 64), AuthorityDigest: strings.Repeat("b", 64), Actor: "admin", Libraries: []string{"films"}, IntervalSeconds: 3600}, time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
}
func TestMediaScheduleDisabledByDefaultDurableCadenceAndAtomicSuccess(t *testing.T) {
	s, d, now := scheduleFixture(t)
	ctx := context.Background()
	state, err := s.Schedule(ctx)
	if err != nil || state.Enabled || state.Revision != "" {
		t.Fatal(state, err)
	}
	state, err = s.ConfigureSchedule(ctx, "", d, now)
	if err != nil || !state.Enabled || !state.NextRun.Equal(now.Add(time.Hour)) {
		t.Fatal(state, err)
	}
	if _, claimed, err := s.ClaimSchedule(ctx, state.Revision, now); err != nil || claimed {
		t.Fatal("startup executed early", err)
	}
	late := now.Add(10*time.Hour + 30*time.Minute)
	run, claimed, err := s.ClaimSchedule(ctx, state.Revision, late)
	if err != nil || !claimed {
		t.Fatal(run, claimed, err)
	}
	after, err := s.Schedule(ctx)
	if err != nil || !after.NextRun.Equal(now.Add(11*time.Hour)) || after.RunID != run.ID {
		t.Fatal(after, err)
	}
	if _, claimed, err := s.ClaimSchedule(ctx, state.Revision, late); err != nil || claimed {
		t.Fatal("claimed same interval", err)
	}
	items := []mediaserver.SyncItem{{ID: "m", Library: "films", Type: "movie", Title: "Movie", Episodes: []mediaserver.SyncEpisode{}}}
	view, err := s.PublishScheduled(ctx, run, items, late.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.Schedule(ctx)
	if err != nil || state.LastState != "completed" || state.RunID != "" {
		t.Fatal(state, err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].SnapshotGeneration != view.Generation || runs[0].State != "completed" {
		t.Fatal(runs, err)
	}
	if _, err := s.PublishScheduled(ctx, run, nil, late); err == nil {
		t.Fatal("replayed consumed run")
	}
}
func TestMediaScheduleStopAndReplacementRevokeInflightPublication(t *testing.T) {
	for _, mode := range []string{"stop", "replace"} {
		t.Run(mode, func(t *testing.T) {
			s, d, now := scheduleFixture(t)
			ctx := context.Background()
			first, err := s.Publish(ctx, "emby", d.ConfigDigest, "admin", d.Libraries, []mediaserver.SyncItem{{ID: "old", Library: "films", Title: "Old", Type: "movie"}})
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.ConfigureSchedule(ctx, "", d, now)
			if err != nil {
				t.Fatal(err)
			}
			run, claimed, err := s.ClaimSchedule(ctx, state.Revision, now.Add(time.Hour))
			if err != nil || !claimed {
				t.Fatal(err)
			}
			if mode == "stop" {
				state, err = s.DisableSchedule(ctx, state.Revision, "admin", "stopped", "operator stopped", now.Add(time.Hour))
			} else {
				state, err = s.ConfigureSchedule(ctx, state.Revision, d, now.Add(time.Hour))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.PublishScheduled(ctx, run, nil, now.Add(time.Hour)); err == nil {
				t.Fatal("revoked run published")
			}
			view, err := s.View(ctx, "emby", 1)
			if err != nil || view.Generation != first.Generation {
				t.Fatal(view, err)
			}
			runs, err := s.Runs(ctx)
			if err != nil || len(runs) != 1 || runs[0].State == "collecting" || runs[0].State == "completed" {
				t.Fatal(runs, err)
			}
			if _, err := s.ConfigureSchedule(ctx, run.Revision, d, now); err == nil {
				t.Fatal("stale activation resurrected stopped generation")
			}
		})
	}
}
func TestMediaScheduleRestartRecoversWithoutPublishingOrImmediateReplay(t *testing.T) {
	s, d, now := scheduleFixture(t)
	ctx := context.Background()
	state, err := s.ConfigureSchedule(ctx, "", d, now)
	if err != nil {
		t.Fatal(err)
	}
	run, claimed, err := s.ClaimSchedule(ctx, state.Revision, now.Add(time.Hour))
	if err != nil || !claimed {
		t.Fatal(err)
	}
	peer, err := Open(s.dbPathForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.RecoverInterrupted(ctx, now.Add(90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	state, err = peer.Schedule(ctx)
	if err != nil || !state.Enabled || state.RunID != "" || state.LastState != "interrupted" {
		t.Fatal(state, err)
	}
	if _, claimed, err := peer.ClaimSchedule(ctx, state.Revision, now.Add(90*time.Minute)); err != nil || claimed {
		t.Fatal("interrupted run immediately retried", err)
	}
	if _, err := s.PublishScheduled(ctx, run, nil, now.Add(90*time.Minute)); err == nil {
		t.Fatal("interrupted run still authorized")
	}
	if next, claimed, err := peer.ClaimSchedule(ctx, state.Revision, now.Add(2*time.Hour)); err != nil || !claimed || next.ID == run.ID {
		t.Fatal(next, claimed, err)
	}
}
func (s *Store) dbPathForTest(t *testing.T) string {
	t.Helper()
	rows, err := s.db.Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sequence int
		var name, path string
		if err := rows.Scan(&sequence, &name, &path); err != nil {
			t.Fatal(err)
		}
		if name == "main" {
			return path
		}
	}
	t.Fatal("missing main database")
	return ""
}
func TestMediaScheduleCommitFailureCannotProduceSuccessOrClearOldCache(t *testing.T) {
	for _, target := range []string{"GO_MEDIASYNC_SCHEDULE", "GO_MEDIASYNC_RUNS"} {
		t.Run(target, func(t *testing.T) {
			s, d, now := scheduleFixture(t)
			ctx := context.Background()
			first, err := s.Publish(ctx, "emby", d.ConfigDigest, "admin", d.Libraries, []mediaserver.SyncItem{{ID: "old", Library: "films", Title: "Old", Type: "movie"}})
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.ConfigureSchedule(ctx, "", d, now)
			if err != nil {
				t.Fatal(err)
			}
			run, claimed, err := s.ClaimSchedule(ctx, state.Revision, now.Add(time.Hour))
			if err != nil || !claimed {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`CREATE TRIGGER ignore_finish BEFORE UPDATE ON ` + target + ` BEGIN SELECT RAISE(IGNORE); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PublishScheduled(ctx, run, nil, now.Add(time.Hour)); err == nil {
				t.Fatal("success without receipt")
			}
			view, err := s.View(ctx, "emby", 1)
			if err != nil || view.Generation != first.Generation || view.Total != 1 {
				t.Fatal(view, err)
			}
			state, err = s.Schedule(ctx)
			if err != nil || state.RunID != run.ID || state.LastState != "collecting" {
				t.Fatal(state, err)
			}
		})
	}
}
