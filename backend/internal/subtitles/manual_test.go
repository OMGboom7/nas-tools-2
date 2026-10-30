package subtitles

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

func originalSubtitle(t *testing.T, s *Store) Receipt {
	t.Helper()
	tx, e := s.db.BeginTx(t.Context(), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if e = Emit(t.Context(), tx, strings.Repeat("a", 32), 0, "Original movie", "/media/Movie.mkv", "电影", bus.Hash("original bytes")); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	rows, e := s.List(t.Context(), true)
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	return rows[0]
}
func TestManualSubtitleSnapshotPreservesRawNumbersAndOnlyCurrentConsumers(t *testing.T) {
	s, db := subtitleStore(t)
	update := func(raw string) {
		t.Helper()
		if _, e := db.Exec(`UPDATE SYSTEM_DICT SET VALUE=? WHERE KEY='plugin.ChineseSubFinder'`, raw); e != nil {
			t.Fatal(e)
		}
	}
	update(`{"host":"https://csf.local/private-prefix","api_key":"private-key","unknown":9007199254740993,"local_path":"/media","remote_path":"D:/Movies"}`)
	first, e := s.Snapshot(t.Context(), "/media/Movie.mkv", "电影", false)
	if e != nil || first.Origin != "https://csf.local" || first.HTTP || first.Payload.Path != "D:/Movies/Movie.mkv" {
		t.Fatal(first, e)
	}
	update(`{"host":"https://csf.local/private-prefix","api_key":"private-key","unknown":9007199254740992,"local_path":"/media","remote_path":"D:/Movies"}`)
	second, e := s.Snapshot(t.Context(), "/media/Movie.mkv", "电影", false)
	if e != nil || first.Basis == second.Basis {
		t.Fatal("distinct original integers were adopted", first, second, e)
	}
	if _, e = db.Exec(`UPDATE SYSTEM_DICT SET VALUE='["ChineseSubFinder","OpenSubtitles"]' WHERE KEY='UserInstalledPlugins';INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig','plugin.OpenSubtitles','{"enable":false,"old":9007199254740993}')`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Snapshot(t.Context(), "/media/Movie.mkv", "电影", false); e != nil {
		t.Fatal("disabled optional consumer blocked native request", e)
	}
	if _, e = db.Exec(`UPDATE SYSTEM_DICT SET VALUE='{"enable":true}' WHERE KEY='plugin.OpenSubtitles'`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Snapshot(t.Context(), "/media/Movie.mkv", "电影", false); !errors.Is(e, ErrConsumer) {
		t.Fatal("active unmigrated consumer silently omitted", e)
	}
}
func TestSubtitleOriginalReceiptAndWaitingNeverCreateAnotherSubmission(t *testing.T) {
	s, db := subtitleStore(t)
	p := originalSubtitle(t, s)
	got, e := s.Get(t.Context(), p.ID)
	if e != nil || receiptDigest(got) != receiptDigest(p) {
		t.Fatal(got, e)
	}
	waits := func(*sql.Tx, Receipt) error { return bus.ErrEventWaiting }
	if _, _, e = s.Claim(t.Context(), p, waits); !errors.Is(e, bus.ErrEventWaiting) {
		t.Fatal(e)
	}
	if e = s.Wait(t.Context(), p); e != nil {
		t.Fatal(e)
	}
	if pending, e := s.List(t.Context(), true); e != nil || len(pending) != 0 {
		t.Fatal("waiting item still blocks FIFO", pending, e)
	}
	if _, e = db.Exec(`UPDATE GO_SUBTITLE_WAITS SET NEXT_CHECK='2000-01-01T00:00:00.000000000Z'`); e != nil {
		t.Fatal(e)
	}
	pending, e := s.List(t.Context(), true)
	if e != nil || len(pending) != 1 {
		t.Fatal(pending, e)
	}
	if _, _, e = s.Claim(t.Context(), pending[0], func(*sql.Tx, Receipt) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if e = s.Finish(t.Context(), p.ID, "uncertain", ""); e != nil {
		t.Fatal(e)
	}
	if pending, e = s.List(t.Context(), true); e != nil || len(pending) != 0 {
		t.Fatal("unknown response replayed", pending, e)
	}
	got, e = s.Get(t.Context(), p.ID)
	if e != nil || got.State != "uncertain" || got.ExternalID != "" {
		t.Fatal(got, e)
	}
}
func TestSubtitleWritesReadBackTheirCompleteOriginalProof(t *testing.T) {
	for _, phase := range []string{"emit", "claim", "finish"} {
		t.Run(phase, func(t *testing.T) {
			s, db := subtitleStore(t)
			p := originalSubtitle(t, s)
			switch phase {
			case "emit":
				if _, e := db.Exec(`DELETE FROM GO_SUBTITLE_JOBS;CREATE TRIGGER substitute_new_subtitle AFTER INSERT ON GO_SUBTITLE_JOBS BEGIN UPDATE GO_SUBTITLE_JOBS SET LOCAL_PATH='/foreign/Movie.mkv' WHERE ID=NEW.ID;END`); e != nil {
					t.Fatal(e)
				}
				tx, e := db.BeginTx(t.Context(), nil)
				if e != nil {
					t.Fatal(e)
				}
				e = Emit(t.Context(), tx, p.JobID, 0, p.Name, p.LocalPath, p.MediaType, p.OriginDigest)
				_ = tx.Rollback()
				if !errors.Is(e, ErrState) {
					t.Fatal("insert substitution reported success", e)
				}
			case "claim":
				if _, e := db.Exec(`CREATE TRIGGER substitute_claimed_subtitle AFTER UPDATE OF STATE ON GO_SUBTITLE_JOBS WHEN NEW.STATE='sending' BEGIN UPDATE GO_SUBTITLE_JOBS SET PAYLOAD='{"video_type":0,"physical_video_file_full_path":"/foreign.mkv","task_priority_level":3}' WHERE ID=NEW.ID;END`); e != nil {
					t.Fatal(e)
				}
				if _, _, e := s.Claim(t.Context(), p); !errors.Is(e, ErrState) {
					t.Fatal("intent substitution adopted", e)
				}
				got, e := s.Get(t.Context(), p.ID)
				if e != nil || got.State != "pending" || got.Payload.Path != p.Payload.Path {
					t.Fatal(got, e)
				}
			case "finish":
				if _, _, e := s.Claim(t.Context(), p); e != nil {
					t.Fatal(e)
				}
				if _, e := db.Exec(`CREATE TRIGGER substitute_subtitle_result AFTER UPDATE OF STATE ON GO_SUBTITLE_JOBS WHEN NEW.STATE='submitted' BEGIN UPDATE GO_SUBTITLE_JOBS SET EXTERNAL_ID='different-task' WHERE ID=NEW.ID;END`); e != nil {
					t.Fatal(e)
				}
				if e := s.Finish(t.Context(), p.ID, "submitted", "original-task"); !errors.Is(e, ErrState) {
					t.Fatal("false final result committed", e)
				}
				got, e := s.Get(t.Context(), p.ID)
				if e != nil || got.State != "sending" || got.ExternalID != "" {
					t.Fatal(got, e)
				}
			}
		})
	}
}
