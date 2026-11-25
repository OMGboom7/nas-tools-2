package systemconfig

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSubscriptionHistoryRollsBackIncompleteGroupDeletion(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	if err := store.Set(ctx, "UserInstalledPlugins", `["DoubanSync"]`); err != nil {
		t.Fatal(err)
	}
	_, err = store.database.ExecContext(ctx, `
		CREATE TABLE PLUGIN_HISTORY(ID INTEGER PRIMARY KEY, PLUGIN_ID TEXT, KEY TEXT, VALUE TEXT, DATE TEXT);
		INSERT INTO PLUGIN_HISTORY VALUES
			(1,'DoubanSync','123','{"id":123}','first'),
			(2,'DoubanSync','123','{"id":"123"}','second');
		CREATE TRIGGER keep_second BEFORE DELETE ON PLUGIN_HISTORY
			WHEN OLD.ID=2 BEGIN SELECT RAISE(IGNORE); END;`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSubscriptionHistory(ctx, "DoubanSync", "123"); !errors.Is(err, ErrHistoryConflict) {
		t.Fatal(err)
	}
	rows, _, err := store.SubscriptionHistory(ctx, "DoubanSync", 0)
	if err != nil || len(rows) != 2 || rows[0].RowID != 1 || rows[1].RowID != 2 {
		t.Fatal(rows, err)
	}
}
