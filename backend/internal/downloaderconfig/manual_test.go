package downloaderconfig

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/rssreceipt"
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
	if err := s.RejectManual(t.Context(), first, true); err != nil {
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

func TestCustomRSSReceiptSurvivesFailureAndFinishesOnce(t *testing.T) {
	s, _, d := manualFixture(t)
	ctx := t.Context()
	d.Actor = "classic:fixture"
	_, err := s.database.Exec(`CREATE TABLE CONFIG_USER_RSS(ID INTEGER PRIMARY KEY, USES TEXT, PROCESS_COUNT TEXT, UPDATE_TIME TEXT, MEDIAINFOS TEXT);
 CREATE TABLE RSS_TORRENTS(ID INTEGER PRIMARY KEY,TORRENT_NAME TEXT,ENCLOSURE TEXT,TITLE TEXT,YEAR TEXT,TYPE TEXT,SEASON TEXT,EPISODE TEXT);
 CREATE TABLE USERRSS_TASK_HISTORY(ID INTEGER PRIMARY KEY,TASK_ID TEXT,TITLE TEXT,DOWNLOADER TEXT,DATE TEXT);
 INSERT INTO CONFIG_USER_RSS VALUES(7,'D','3','yesterday',NULL),(8,'R','0',NULL,NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	title := "Fixture"
	item := rssreceipt.Article{TaskID: 7, Torrent: "Fixture.S00E02", Title: &title, Enclosure: "https://fixture.invalid/download/1", Scheduled: true, TV: &rssreceipt.TVIdentity{ID: "55", Name: title, Season: 0}}
	h := ClassicAcknowledgement{RSS: &item, Title: title}
	r, created, err := s.ReserveClassic(ctx, d, h)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if err := s.RejectManual(ctx, r, false); err != nil {
		t.Fatal(err)
	}
	r, created, err = s.ReserveClassic(ctx, d, h)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if _, ok, err := s.RestoreAcknowledgedRSS(ctx, r.Definition.RSSKey, item.TaskID, r.ID, d.Actor); err != nil || ok {
		t.Fatal("unconfirmed receipt restored", ok, err)
	}
	if err := s.AcknowledgeRSS(ctx, r, d.InfoHash); err != nil {
		t.Fatal(err)
	}
	if err := s.RejectManual(ctx, r, true); err == nil {
		t.Fatal("released a confirmed task")
	}
	second := d
	second.InfoHash = strings.Repeat("b", 40)
	if _, _, err := s.ReserveClassic(ctx, second, h); !errors.Is(err, rssreceipt.ErrClaim) {
		t.Fatal("same URL under a different hash", err)
	}
	if _, err := s.database.Exec(`CREATE TRIGGER block_credit BEFORE UPDATE OF PROCESS_COUNT ON CONFIG_USER_RSS BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteWithHistory(ctx, r, d.InfoHash, "acknowledged", d.Actor, &h); err == nil {
		t.Fatal("accepted partial credit")
	}
	var count int
	if err := s.database.QueryRow(`SELECT (SELECT COUNT(*) FROM RSS_TORRENTS)+(SELECT COUNT(*) FROM USERRSS_TASK_HISTORY)+(SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE STATE='submitted')`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if _, err := s.database.Exec("DROP TRIGGER block_credit"); err != nil {
		t.Fatal(err)
	}
	restored, ok, err := s.RestoreAcknowledgedRSS(ctx, r.Definition.RSSKey, item.TaskID, r.ID, "admin")
	if err != nil || !ok || restored.TaskID != d.InfoHash || restored.State != "submitted" {
		t.Fatal(restored, ok, err)
	}
	if _, _, err := s.ReserveClassic(ctx, second, h); !errors.Is(err, rssreceipt.ErrProcessed) {
		t.Fatal("completed resource not recognized", err)
	}
	if err := s.database.QueryRow(`SELECT COUNT(*) FROM CONFIG_USER_RSS WHERE ID=7 AND PROCESS_COUNT='4' AND json_extract(MEDIAINFOS,'$[0].season')=0`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := s.database.QueryRow(`SELECT count(*) FROM USERRSS_TASK_HISTORY WHERE TITLE='Fixture.S00E02' AND DOWNLOADER='Client'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	item.TaskID = 8
	item.Enclosure += "2"
	if _, _, err := s.ReserveClassic(ctx, second, h); !errors.Is(err, rssreceipt.ErrTaskMissing) {
		t.Fatal("task type changed", err)
	}
	item.Scheduled, item.TV = false, nil
	r, created, err = s.ReserveClassic(ctx, second, h)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if err := s.CompleteWithHistory(ctx, r, second.InfoHash, "acknowledged", second.Actor, &h); err != nil {
		t.Fatal(err)
	}
	if err := s.database.QueryRow(`SELECT COUNT(*) FROM CONFIG_USER_RSS WHERE ID=8 AND PROCESS_COUNT='0' AND UPDATE_TIME IS NULL AND MEDIAINFOS IS NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatal("manual count changed", count, err)
	}
}
