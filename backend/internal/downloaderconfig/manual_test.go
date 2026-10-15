package downloaderconfig

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

func manualFixture(t *testing.T) (*Store, Downloader, ManualDefinition) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c, err := s.Upsert(t.Context(), Downloader{Name: "Client", Type: "qbittorrent", Enabled: 1, Config: `{"host":"private.test","password":"private-token"}`})
	if err != nil {
		t.Fatal(err)
	}
	d := ManualDefinition{DownloaderID: c.ID, DownloaderName: c.Name, DownloaderType: c.Type, ConfigDigest: bus.Hash(c), OptionsDigest: bus.Hash("options"), InfoHash: strings.Repeat("a", 40), Actor: "admin", Title: "Movie", Source: "manual"}
	// Upsert returns the supplied zero-value nullable fields; Get is the
	// actual normalized snapshot used by the HTTP service.
	c, err = s.Get(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	d.ConfigDigest = bus.Hash(c)
	return s, c, d
}
func TestManualSubmissionReceiptAndHistoryAndEventCommitOnce(t *testing.T) {
	s, _, d := manualFixture(t)
	r, created, err := s.ReserveManual(t.Context(), d)
	if err != nil || !created || r.State != "pending" {
		t.Fatal(r, created, err)
	}
	if _, _, err := s.ReserveManual(t.Context(), d); err != ErrManualPending {
		t.Fatal(err)
	}
	if err := s.CompleteManual(t.Context(), r, d.InfoHash, "acknowledged", "admin"); err != nil {
		t.Fatal(err)
	}
	repeated, created, err := s.ReserveManual(t.Context(), d)
	if err != nil || created || repeated.ID != r.ID || repeated.TaskID != d.InfoHash {
		t.Fatal(repeated, created, err)
	}
	if err := s.CompleteManual(t.Context(), r, d.InfoHash, "acknowledged", "admin"); err != ErrManualPending {
		t.Fatal("duplicate completion", err)
	}
	var count int
	if err := s.database.QueryRow(`SELECT COUNT(*) FROM DOWNLOAD_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	events, err := bus.New(s.database).List(t.Context())
	if err != nil || len(events) != 1 || events[0].Kind != "download_start" {
		t.Fatal(events, err)
	}
	d.OptionsDigest = bus.Hash("new options")
	if _, _, err := s.ReserveManual(t.Context(), d); err != ErrManualPending {
		t.Fatal("different scope reused", err)
	}
}
func TestManualSubmissionKnownRejectionRetainsAuditButAllowsNewAttempt(t *testing.T) {
	s, _, d := manualFixture(t)
	first, _, err := s.ReserveManual(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RejectManual(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second, created, err := s.ReserveManual(t.Context(), d)
	if err != nil || !created || second.ID == first.ID {
		t.Fatal(second, created, err)
	}
	rows, err := s.ManualSubmissions(t.Context())
	if err != nil || len(rows) != 2 || rows[0].State != "pending" || rows[1].State != "rejected" {
		t.Fatal(rows, err)
	}
}
func TestManualSubmissionPendingCannotBeBypassedByChangingOwnerOrDownloader(t *testing.T) {
	s, _, d := manualFixture(t)
	if _, _, err := s.ReserveManual(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	d.Actor = "other"
	if _, _, err := s.ReserveManual(t.Context(), d); err != ErrManualPending {
		t.Fatal(err)
	}
	c, err := s.Upsert(t.Context(), Downloader{Name: "Other", Type: "qbittorrent", Enabled: 1, Config: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.Get(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	d.DownloaderID = c.ID
	d.DownloaderName = c.Name
	d.ConfigDigest = bus.Hash(c)
	if _, _, err := s.ReserveManual(t.Context(), d); err != ErrManualPending {
		t.Fatal("pending scope escaped", err)
	}
}
func TestManualSubmissionConcurrentReservationOneWinner(t *testing.T) {
	s, _, d := manualFixture(t)
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := s.ReserveManual(t.Context(), d)
			results <- created && err == nil
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for result := range results {
		if result {
			winners++
		}
	}
	if winners != 1 {
		t.Fatal(winners)
	}
}
func TestManualSubmissionTransactionChecksConfigAfterHistoryAndRollsBack(t *testing.T) {
	s, _, d := manualFixture(t)
	r, _, err := s.ReserveManual(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	// Establish the real legacy schema without recording a submission.
	if _, err := s.database.Exec(`CREATE TABLE DOWNLOAD_HISTORY(ID INTEGER PRIMARY KEY,TITLE TEXT,YEAR TEXT,TYPE TEXT,TMDBID TEXT,SE TEXT,VOTE TEXT,POSTER TEXT,OVERVIEW TEXT,TORRENT TEXT,ENCLOSURE TEXT,SITE TEXT,DESC TEXT,DOWNLOADER TEXT,DOWNLOAD_ID TEXT,SAVE_PATH TEXT,DATE TEXT);CREATE TRIGGER revoke_manual AFTER INSERT ON DOWNLOAD_HISTORY BEGIN UPDATE DOWNLOADER SET ENABLED=0; END`); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteManual(t.Context(), r, d.InfoHash, "acknowledged", "admin"); err != ErrManualPending {
		t.Fatal(err)
	}
	var count int
	if err := s.database.QueryRow(`SELECT COUNT(*) FROM DOWNLOAD_HISTORY`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	rows, err := s.ManualSubmissions(t.Context())
	if err != nil || rows[0].State != "pending" {
		t.Fatal(rows, err)
	}
	events, err := bus.New(s.database).List(t.Context())
	if err != nil || len(events) != 0 {
		t.Fatal(events, err)
	}
}
