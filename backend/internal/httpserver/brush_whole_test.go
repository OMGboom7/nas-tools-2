package httpserver

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/database"
	"github.com/0xforee/nas-tools/backend/internal/downloadhistory"
)

func TestBrushWholeAcknowledgementAndRecovery(t *testing.T) {
	for _, kind := range []string{"aria2", "pan115", "pikpak"} {
		t.Run(kind, func(t *testing.T) {
			f := manualHTTPFixture(t, kind, false)
			if err := database.PrepareApplicationDatabases(filepath.Dir(f.path)); err != nil {
				t.Fatal(err)
			}
			exec := func(q string) {
				t.Helper()
				if _, err := f.db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO CONFIG_SITE(ID,INCLUDE,RSSURL) VALUES(33,'S','feed');
   INSERT INTO SITE_BRUSH_TASK(ID,SITE,DOWNLOADER,STATE,RSS_RULE,DOWNLOAD_COUNT) VALUES(7,33,1,'Y','{}',0);`)
			f.intentQuery = `SELECT COUNT(*) FROM GO_BRUSH_WHOLE w JOIN GO_BRUSH_SUBMISSION s USING(token) WHERE w.v1=? AND s.remote_id IS NULL AND s.partial=0`
			item, err := f.store.Get(t.Context(), 1)
			if err != nil {
				t.Fatal(err)
			}
			s := downloadService{client: &http.Client{Transport: f.transport}, downloaders: f.store}
			q := brushSubmitRequest{Magnet: testMagnet, Category: "Film", Tags: []string{"site"}, UploadLimitKB: 24, Brush: brushDownloadRequest{TaskID: 7, SiteID: 33, Downloader: "1", Title: "Movie", RuleSource: "{}", Enclosure: "source", FeedURL: "feed", Size: 128}}
			media := downloadhistory.Media{}
			for _, k := range strings.Fields("TITLE YEAR TYPE TMDBID SE VOTE POSTER OVERVIEW TORRENT ENCLOSURE SITE DESC") {
				media[k] = nil
			}
			id := "71"
			media["TMDBID"] = &id
			media["TITLE"] = &q.Brush.Title
			media["ENCLOSURE"] = &q.Brush.Enclosure
			q.Media = media
			if kind != "aria2" {
				q.Magnet = ""
				q.Torrent = testTorrent
				if _, err := s.submitBrushWhole(t.Context(), f.db, item, q); err == nil {
					t.Fatal("cloud torrent accepted")
				}
				q.Magnet = testMagnet
				q.Torrent = nil
			}
			q.RuleSource = "stale settings"
			if got, err := s.submitBrushWhole(t.Context(), f.db, item, q); err != nil || got.Task != "" || f.adds != 0 {
				t.Fatal(got, err, f.adds)
			}
			q.RuleSource = ""
			exec("CREATE TRIGGER break_history BEFORE INSERT ON DOWNLOAD_HISTORY BEGIN SELECT RAISE(ABORT,'fixture'); END")
			result, err := s.submitBrushWhole(t.Context(), f.db, item, q)
			if err != nil || !result.Pending || result.Task == "" || f.adds != 1 {
				t.Fatal(result, err, f.adds)
			}
			count := func(query string, want int) {
				t.Helper()
				var n int
				if err := f.db.QueryRow(query).Scan(&n); err != nil || n != want {
					t.Fatal(query, n, want, err)
				}
			}
			count("SELECT COUNT(*) FROM SITE_BRUSH_TORRENTS", 0)
			count("SELECT DOWNLOAD_COUNT FROM SITE_BRUSH_TASK WHERE ID=7", 0)
			exec("DROP TRIGGER break_history")
			n, err := recoverBrushDownloads(t.Context(), f.db, 7)
			if err != nil || n != 1 {
				t.Fatal(n, err)
			}
			count("SELECT DOWNLOAD_COUNT FROM SITE_BRUSH_TASK WHERE ID=7", 1)
			count("SELECT COUNT(*) FROM DOWNLOAD_HISTORY WHERE ENCLOSURE='source' AND SAVE_PATH IS NULL", 1)
			count("SELECT COUNT(*) FROM GO_BRUSH_WHOLE", 0)
			if got, err := s.submitBrushWhole(t.Context(), f.db, item, q); err != nil || got.Task != "" || f.adds != 1 {
				t.Fatal(got, err, f.adds)
			}
			q.Brush.Enclosure = "second"
			f.lost = true
			if _, err := s.submitBrushWhole(t.Context(), f.db, item, q); err == nil || f.adds != 2 {
				t.Fatal(err, f.adds)
			}
			if err := recoverBrushWhole(t.Context(), f.db, f.path, 7); err != nil {
				t.Fatal(err)
			}
			if got, err := s.submitBrushWhole(t.Context(), f.db, item, q); err != nil || got.Task != "" || f.adds != 2 {
				t.Fatal(got, err, f.adds)
			}
			count("SELECT COUNT(*) FROM GO_BRUSH_SUBMISSION WHERE ready=0 AND remote_id IS NULL", 1)
			count("SELECT COUNT(*) FROM DOWNLOAD_HISTORY", 1)
		})
	}
}
