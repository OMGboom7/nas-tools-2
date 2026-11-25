package notificationconfig

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestCenterRetentionCursorAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now().Truncate(time.Second)
	ctx := t.Context()
	for i := 1; i <= 53; i++ {
		tx, err := s.database.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = AppendSystemMessage(ctx, tx, fmt.Sprint(i), "message\nbody\n\n", at); err != nil {
			t.Fatal(err)
		}
		if i == 53 {
			err = tx.Rollback()
		} else {
			err = tx.Commit()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	reader, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	all, err := reader.SystemMessages(ctx, 100, 0, "")
	if err != nil || len(all) != 50 || all[0].Title != "52" || all[49].Title != "3" {
		t.Fatal(all, err)
	}
	first, err := reader.SystemMessages(ctx, 20, 0, "")
	if err != nil || len(first) != 20 || first[19].Title != "33" {
		t.Fatal(first, err)
	}
	poll, err := reader.SystemMessages(ctx, 20, all[2].ID, at.Format(time.DateTime))
	if err != nil || len(poll) != 2 || poll[0].Title != "52" || poll[1].Content != "message\nbody\n\n" {
		t.Fatal(poll, err)
	}
	same, err := reader.SystemMessages(ctx, 20, 0, at.Format(time.DateTime))
	if err != nil || len(same) != 0 {
		t.Fatal(same, err)
	}
}
