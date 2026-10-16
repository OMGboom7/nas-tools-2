package mediasync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

func TestSnapshotAtomicLegacyCompatibilityIsolationAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "media.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	items := []mediaserver.SyncItem{{ID: "a", Library: "films", Type: "movie", Title: "Movie", Year: "2026", TMDBID: "100", Episodes: []mediaserver.SyncEpisode{}}, {ID: "b", Library: "tv", Type: "show", Title: "Series", Episodes: []mediaserver.SyncEpisode{{Season: 1, Episode: 2}}}}
	first, err := s.Publish(ctx, "emby", strings.Repeat("a", 64), "admin", []string{"films", "tv"}, items)
	if err != nil {
		t.Fatal(err)
	}
	var kind, episodes string
	if err := s.db.QueryRow(`SELECT ITEM_TYPE,JSON FROM MEDIASYNC_ITEMS WHERE ITEM_ID='b'`).Scan(&kind, &episodes); err != nil || kind != "Series" || !strings.Contains(episodes, `"season_num":1`) {
		t.Fatal(kind, episodes, err)
	}
	if _, err := s.Publish(ctx, "plex", strings.Repeat("b", 64), "admin", []string{"films"}, items[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_media BEFORE INSERT ON MEDIASYNC_ITEMS WHEN NEW.TITLE='broken' BEGIN SELECT RAISE(ABORT,'failed'); END`); err != nil {
		t.Fatal(err)
	}
	bad := []mediaserver.SyncItem{{ID: "c", Library: "films", Type: "movie", Title: "broken"}}
	if _, err := s.Publish(ctx, "emby", strings.Repeat("c", 64), "admin", []string{"films"}, bad); err == nil {
		t.Fatal("accepted failed insert")
	}
	view, err := s.View(ctx, "emby", 1)
	if err != nil || view.Generation != first.Generation || view.Total != 2 || len(view.Items) != 2 || len(view.Items[1].Episodes) != 1 {
		t.Fatal(view, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Publish(cancelled, "emby", strings.Repeat("a", 64), "admin", []string{"films"}, nil); err == nil {
		t.Fatal("cancelled publish")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(path + ".pre-go.bak"); err != nil {
		t.Fatal(err)
	}
	view, err = s.View(ctx, "emby", 1)
	if err != nil || view.Generation != first.Generation {
		t.Fatal(view, err)
	}
	if _, err := s.Publish(ctx, "emby", strings.Repeat("a", 64), "admin", []string{"films"}, nil); err != nil {
		t.Fatal(err)
	}
	view, err = s.View(ctx, "plex", 1)
	if err != nil || len(view.Items) != 1 {
		t.Fatal("other server erased", view, err)
	}
	view, err = s.View(ctx, "emby", 1)
	if err != nil || view.Total != 0 || len(view.Items) != 0 {
		t.Fatal(view, err)
	}
}
func TestSnapshotViewPaginationAndRejectedInput(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	items := []mediaserver.SyncItem{}
	for i := 0; i < 101; i++ {
		items = append(items, mediaserver.SyncItem{ID: strings.Repeat("a", i+1), Library: "films", Type: "movie", Title: "Movie"})
	}
	if _, err := s.Publish(ctx, "emby", strings.Repeat("a", 64), "admin", []string{"films"}, items); err != nil {
		t.Fatal(err)
	}
	view, err := s.View(ctx, "emby", 2)
	if err != nil || len(view.Items) != 1 || view.Page != 2 || view.Total != 101 {
		t.Fatal(view, err)
	}
	if _, err := s.View(ctx, "emby", 0); err == nil {
		t.Fatal("accepted invalid page")
	}
	if _, err := s.Publish(ctx, "emby", strings.Repeat("a", 64), "admin", []string{"other"}, items); err == nil {
		t.Fatal("unselected library")
	}
	if _, err := s.Publish(ctx, "emby", strings.Repeat("a", 64), "admin", []string{"films"}, append(items, items[0])); err == nil {
		t.Fatal("duplicate id")
	}
}
func TestSnapshotPreservesPythonDatabaseBeforeFirstWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE PYTHON_DATA (VALUE TEXT);INSERT INTO PYTHON_DATA VALUES('original')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	backup, err := sql.Open("sqlite", path+".pre-go.bak")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var value string
	if err := backup.QueryRow(`SELECT VALUE FROM PYTHON_DATA`).Scan(&value); err != nil || value != "original" {
		t.Fatal(value, err)
	}
}
