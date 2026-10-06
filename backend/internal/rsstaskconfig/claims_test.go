package rsstaskconfig

import (
	"context"
	"path/filepath"
	"testing"
)

func TestReservationsSurviveRestartAndCompleteAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	id, err := store.Upsert(ctx, Task{Name: "Test", Uses: "D"})
	if err != nil {
		t.Fatal(err)
	}
	enclosure := "https://tracker.local/download?passkey=private"
	if reserved, err := store.ReserveDownload(ctx, id, enclosure); err != nil || !reserved {
		t.Fatal(reserved, err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if reserved, err := other.ReserveDownload(ctx, id, enclosure); err != nil || reserved {
		t.Fatal(reserved, err)
	}
	if pending, err := other.HasPendingDownload(ctx, enclosure); err != nil || !pending {
		t.Fatal(pending, err)
	}
	if _, err := store.database.Exec(`CREATE TRIGGER block_history BEFORE INSERT ON USERRSS_TASK_HISTORY BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteReservedDownload(ctx, id, "Movie", "2026", enclosure, "qB", nil); err == nil {
		t.Fatal("expected rollback")
	}
	if processed, err := store.IsProcessed(ctx, "D", "Movie", "2026", enclosure); err != nil || processed {
		t.Fatal(processed, err)
	}
	task, err := store.Get(ctx, id)
	if err != nil || task.ProcessCount != "0" {
		t.Fatal(task, err)
	}
	if pending, err := store.HasPendingDownload(ctx, enclosure); err != nil || !pending {
		t.Fatal(pending, err)
	}
	if _, err := store.database.Exec(`DROP TRIGGER block_history`); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteReservedDownload(ctx, id, "Show", "2026", enclosure, "qB", &TVIdentity{ID: "200", Name: "Show", Season: 1}); err != nil {
		t.Fatal(err)
	}
	task, err = store.Get(ctx, id)
	if err != nil || task.ProcessCount != "1" || task.UpdateTime == "" || task.MediaInfos != `[{"id":"200","name":"Show","rssid":"","season":1}]` {
		t.Fatal(task, err)
	}
	if pending, err := store.HasPendingDownload(ctx, enclosure); err != nil || pending {
		t.Fatal(pending, err)
	}
	if err := store.CompleteReservedDownload(ctx, id, "Show", "2026", enclosure, "qB", nil); err == nil {
		t.Fatal("completion was repeated")
	}
	entries, err := store.History(ctx, id)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	if reserved, err := other.ReserveDownload(ctx, id, enclosure); err != nil || reserved {
		t.Fatal(reserved, err)
	}
	if reserved, err := store.ReserveDownload(ctx, 999, "another"); err != ErrNotFound || reserved {
		t.Fatal(reserved, err)
	}
	if err := store.SetArticles(ctx, id, "set_unfinish", []Article{{Title: "changed display title", Year: "2027", Enclosure: enclosure}}); err != nil {
		t.Fatal(err)
	}
	if reserved, err := store.ReserveDownload(ctx, id, enclosure); err != nil || !reserved {
		t.Fatal("explicit reset did not allow a new confirmed download", reserved, err)
	}
	if err := store.SetArticles(ctx, id, "set_unfinish", []Article{{Title: "Show", Enclosure: enclosure}}); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.HasPendingDownload(ctx, enclosure); err != nil || !pending {
		t.Fatal("uncertain submission must not be cleared by reset", pending, err)
	}
	if err := store.ReleaseUnsubmittedDownload(ctx, id+1, enclosure); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.HasPendingDownload(ctx, enclosure); err != nil || !pending {
		t.Fatal("wrong owner cleared reservation", pending, err)
	}
	if err := store.ReleaseUnsubmittedDownload(ctx, id, enclosure); err != nil {
		t.Fatal(err)
	}
	if reserved, err := store.ReserveDownload(ctx, id, enclosure); err != nil || !reserved {
		t.Fatal("rejected submission could not be retried", reserved, err)
	}
}
