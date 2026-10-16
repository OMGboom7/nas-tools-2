package mediasync

import (
	"context"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

func readyActivity(t *testing.T, s *Store, id string, libraries, items, episodes int) {
	t.Helper()
	if err := s.ReportProgress(context.Background(), id, mediaserver.SyncProgress{Stage: "publishing", LibrariesDone: libraries, LibrariesTotal: libraries, ItemsDone: items, EpisodesDone: episodes, PagesRead: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
}
func TestMediaActivityProgressNeverImpliesCommittedSnapshot(t *testing.T) {
	s, d, now := scheduleFixture(t)
	ctx := context.Background()
	a, err := s.BeginManual(ctx, d.Server, d.ConfigDigest, d.Actor, d.Libraries, now)
	if err != nil {
		t.Fatal(err)
	}
	items := []mediaserver.SyncItem{{ID: "m", Library: "films", Type: "movie", Title: "Movie"}}
	if _, err := s.PublishManual(ctx, a, items, now); err == nil {
		t.Fatal("uncollected activity published")
	}
	readyActivity(t, s, a.ID, 1, 1, 0)
	activities, err := s.Activities(ctx)
	if err != nil || len(activities) != 1 || activities[0].State != "collecting" || activities[0].SnapshotGeneration != "" {
		t.Fatal(activities, err)
	}
	view, err := s.View(ctx, d.Server, 1)
	if err != nil || view.Generation != "" {
		t.Fatal(view, err)
	}
	if err := s.ReportProgress(ctx, a.ID, mediaserver.SyncProgress{Stage: "items", LibrariesTotal: 1}, now); err == nil {
		t.Fatal("progress regressed")
	}
	if _, err := s.db.Exec(`CREATE TRIGGER ignore_activity BEFORE UPDATE ON GO_MEDIASYNC_ACTIVITY BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishManual(ctx, a, items, now); err == nil {
		t.Fatal("snapshot committed without activity")
	}
	view, err = s.View(ctx, d.Server, 1)
	if err != nil || view.Generation != "" {
		t.Fatal("cache changed without final receipt", view, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER ignore_activity`); err != nil {
		t.Fatal(err)
	}
	view, err = s.PublishManual(ctx, a, items, now)
	if err != nil {
		t.Fatal(err)
	}
	activities, err = s.Activities(ctx)
	if err != nil || activities[0].State != "completed" || activities[0].SnapshotGeneration != view.Generation {
		t.Fatal(activities, err)
	}
	if _, err := s.PublishManual(ctx, a, items, now); err == nil {
		t.Fatal("completed activity replayed")
	}
}
func TestMediaActivityFailureAndRecoveryRetainCountsWithoutSuccess(t *testing.T) {
	s, d, now := scheduleFixture(t)
	ctx := context.Background()
	a, err := s.BeginManual(ctx, d.Server, d.ConfigDigest, d.Actor, d.Libraries, now)
	if err != nil {
		t.Fatal(err)
	}
	readyActivity(t, s, a.ID, 1, 1, 0)
	if err := s.FailManual(ctx, a.ID, "failed", "request incomplete", now); err != nil {
		t.Fatal(err)
	}
	b, err := s.BeginManual(ctx, d.Server, d.ConfigDigest, d.Actor, d.Libraries, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverInterrupted(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	activities, err := s.Activities(ctx)
	if err != nil || len(activities) != 2 || activities[0].State != "interrupted" || activities[0].ID != b.ID || activities[1].State != "failed" || activities[1].Progress.ItemsDone != 1 {
		t.Fatal(activities, err)
	}
	for _, activity := range activities {
		if activity.SnapshotGeneration != "" {
			t.Fatal("failure has successful generation", activity)
		}
	}
	if err := s.ReportProgress(ctx, b.ID, mediaserver.SyncProgress{Stage: "items", LibrariesTotal: 1}, now); err == nil {
		t.Fatal("interrupted activity revived")
	}
}
