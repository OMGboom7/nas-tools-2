//go:build linux || darwin

package organization

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/opensubtitles"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

func openGrant(t *testing.T, s *Store, enabled bool) opensubtitles.Snapshot {
	t.Helper()
	if _, e := s.db.Exec(`CREATE TABLE IF NOT EXISTS SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT)`); e != nil {
		t.Fatal(e)
	}
	raw := `{"enable":true,"provider":"org","unknown":9007199254740993}`
	for key, value := range map[string]string{"UserInstalledPlugins": `["OpenSubtitles","unknown-native-id"]`, opensubtitles.ConfigKey: raw} {
		if _, e := s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, key, value); e != nil {
			t.Fatal(e)
		}
	}
	if enabled {
		c := opensubtitles.Control{Version: 1, Epoch: strings.Repeat("e", 32), Actor: "admin", Authority: Digest("admin authority"), Config: Digest(raw), Authorized: true, Created: time.Now().UTC().Format(time.RFC3339Nano)}
		encoded, _ := json.Marshal(c)
		if _, e := s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, opensubtitles.ControlKey, string(encoded)); e != nil {
			t.Fatal(e)
		}
	}
	_, snap, _, _, e := s.OpenSubtitleProvider(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	return snap
}
func openIntakeFixture(t *testing.T, metadata, grant bool) (*Store, Job) {
	t.Helper()
	d, e := copyFixture(t)
	d.ScraperEnabled = metadata
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	openGrant(t, s, grant)
	if metadata {
		if _, err = s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,'{}')`, scraper.ConfigKey); err != nil {
			t.Fatal(err)
		}
	}
	id, err := s.Create(t.Context(), Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(t.Context(), id, 0); err != nil || !won {
		t.Fatal(won, err)
	}
	if _, err = Copy(t.Context(), d, e, TempName(id, 0), func(p Proof) error { return s.Prepare(t.Context(), id, 0, p) }); err != nil {
		t.Fatal(err)
	}
	job, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(t.Context(), job, job.Items[0]); err != nil {
		t.Fatal(err)
	}
	job, err = s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s, job
}
func onlyOpen(t *testing.T, s *Store) OpenSubtitleReceipt {
	t.Helper()
	rows, e := s.OpenSubtitles(t.Context(), false, 1)
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	return rows[0]
}
func openClaim(t *testing.T, s *Store, job Job, p OpenSubtitleReceipt) (OpenSubtitleReceipt, LibraryDefinition, LibraryMedia) {
	t.Helper()
	lib := LibraryDefinition{Root: LibraryRoot{ID: job.Definition.TargetID, Path: job.Definition.TargetRoot, Kind: "movie", Identity: job.Definition.TargetIdentity}, Selection: job.Items[0].Target, Mode: "no_force", Actor: p.Definition.Provider.Actor, RequestID: p.ID[:32], ConfigDigest: job.Definition.ConfigDigest, Count: 1, Allowed: []LibraryRoot{}, OpenSubtitle: &OpenSubtitleLibrary{Request: p.ID, Fingerprint: p.Fingerprint, Query: opensubtitles.Query{Movie: true, Name: p.Definition.Media["title"].(string), Year: p.Definition.Media["year"].(string)}}}
	m, e := CaptureLibraryMedia(t.Context(), lib, lib.Selection)
	if e != nil {
		t.Fatal(e)
	}
	claimed, e := s.ClaimOpenSubtitle(t.Context(), p, lib, m, func() error { return VerifyCompletedTarget(t.Context(), job, job.Items[0]) })
	if e != nil {
		t.Fatal(e)
	}
	return claimed, lib, m
}
func TestOpenSubtitleProducerNeverBackfillsAFormerlyUncapturedIntake(t *testing.T) {
	s, job := openIntakeFixture(t, false, false)
	rows, e := s.OpenSubtitles(t.Context(), false, 1)
	if e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	var raw string
	if e = s.db.QueryRow(`SELECT VALUE FROM SYSTEM_DICT WHERE KEY=? ORDER BY ID DESC LIMIT 1`, opensubtitles.ConfigKey).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	c := opensubtitles.Control{Version: 1, Epoch: strings.Repeat("f", 32), Actor: "admin", Authority: Digest("admin authority"), Config: Digest(raw), Authorized: true, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	encoded, _ := json.Marshal(c)
	if _, e = s.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, opensubtitles.ControlKey, string(encoded)); e != nil {
		t.Fatal(e)
	}
	tx, e := s.db.BeginTx(t.Context(), nil)
	if e != nil {
		t.Fatal(e)
	}
	source, e := subtitleSavedSource(t.Context(), tx, job.ID, 0)
	if e != nil {
		t.Fatal(e)
	}
	item := job.Items[0]
	d := OpenSubtitleDefinition{Kind: "intake", Reference: job.ID, Ordinal: 0, Origin: SubtitleOriginDigest(job, item), File: filepath.Join(job.Definition.TargetRoot, item.Target), Extension: filepath.Ext(item.Source), Media: map[string]any{"title": item.Title, "year": item.Year, "type": item.MediaType, "tmdb_id": item.TMDBID, "season_episode": item.SeasonEpisode}, Source: source}
	if e = emitOpenSubtitle(t.Context(), tx, d, nil); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	rows, e = s.OpenSubtitles(t.Context(), false, 1)
	if e != nil || len(rows) != 0 {
		t.Fatal("later grant captured historical source", rows, e)
	}
	var captured int
	if e = s.db.QueryRow(`SELECT CAPTURED FROM GO_OPEN_SUBTITLE_OBSERVATIONS WHERE ID=?`, OpenSubtitleID("intake", job.ID, 0)).Scan(&captured); e != nil || captured != 0 {
		t.Fatal(captured, e)
	}
}
func TestOpenSubtitleWaitsForItsOwnOriginalMetadataAndWakesWithoutIntent(t *testing.T) {
	s, _ := openIntakeFixture(t, true, true)
	p := onlyOpen(t, s)
	if e := s.OpenSubtitleReady(t.Context(), p); !errors.Is(e, bus.ErrEventWaiting) {
		t.Fatal(e)
	}
	if e := s.WaitOpenSubtitle(t.Context(), p); e != nil {
		t.Fatal(e)
	}
	if rows, e := s.OpenSubtitles(t.Context(), true, 1); e != nil || len(rows) != 0 {
		t.Fatal("waiting request starves FIFO", rows, e)
	}
	metadata, e := s.Scrapes(t.Context(), true)
	if e != nil || len(metadata) != 1 {
		t.Fatal(metadata, e)
	}
	if e = s.ClaimScrape(t.Context(), metadata[0]); e != nil {
		t.Fatal(e)
	}
	if e = s.FinishScrape(t.Context(), metadata[0].ID, "completed", ""); e != nil {
		t.Fatal(e)
	}
	rows, e := s.OpenSubtitles(t.Context(), true, 1)
	if e != nil || len(rows) != 1 || rows[0].Requests != 0 || rows[0].LibraryID != "" {
		t.Fatal(rows, e)
	}
	if e = s.OpenSubtitleReady(t.Context(), rows[0]); e != nil {
		t.Fatal(e)
	}
}
func TestOpenSubtitleUsesExistingPublicationClaimsWithoutInventingMoreTransfers(t *testing.T) {
	s, job := openIntakeFixture(t, false, true)
	p, lib, media := openClaim(t, s, job, onlyOpen(t, s))
	if rows, e := s.LibraryJobs(t.Context(), true); e != nil || len(rows) != 0 {
		t.Fatal("NFO worker adopted subtitle job", rows, e)
	}
	data := []byte("1\n00:00:01,000 --> 00:00:02,000\n中文字幕\n")
	name := OpenSubtitleName(p.Definition, ".srt")
	target, e := LibraryTarget(lib, media, "media", name)
	if e != nil {
		t.Fatal(e)
	}
	owner := Digest([]string{"open-subtitle-output", p.ID, target})
	current := func() error { return VerifyCompletedTarget(t.Context(), job, job.Items[0]) }
	proof, state, e := PublishLibrarySidecar(t.Context(), lib, media, "media", name, data, func(proof LibraryOutputProof) error {
		return s.SaveOpenSubtitleOutput(t.Context(), p, lib.Root, target, owner, "writing", "original intent", proof, current)
	}, current)
	if e != nil || state != "created" {
		t.Fatal(state, e)
	}
	if e = s.SaveOpenSubtitleOutput(t.Context(), p, lib.Root, target, owner, state, "published original subtitle", proof, current); e != nil {
		t.Fatal(e)
	}
	if e = s.FinishOpenSubtitle(t.Context(), p.ID, p.Fingerprint, "completed", "original output complete"); e != nil {
		t.Fatal(e)
	}
	got, e := s.OpenSubtitle(t.Context(), p.ID)
	if e != nil || got.State != "completed" || len(got.Outputs) != 1 || got.Outputs[0].State != "created" {
		t.Fatal(got, e)
	}
	actual, e := os.ReadFile(filepath.Join(lib.Root.Path, target))
	if e != nil || string(actual) != string(data) {
		t.Fatal(e)
	}
	if e = VerifyCompletedTarget(t.Context(), job, job.Items[0]); e != nil {
		t.Fatal("video changed", e)
	}
	var histories int
	if e = s.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&histories); e != nil || histories != 1 {
		t.Fatal("subtitle fabricated transfer history", histories, e)
	}
	if e = s.OpenSubtitleRunning(t.Context(), p); e == nil {
		t.Fatal("completed request reopened")
	}
}
func TestOpenSubtitleChecksProviderAndSourceAgainInsideActualIntentTransaction(t *testing.T) {
	for _, change := range []string{"provider", "source", "binding"} {
		t.Run(change, func(t *testing.T) {
			s, job := openIntakeFixture(t, false, true)
			p, _, _ := openClaim(t, s, job, onlyOpen(t, s))
			statement := `UPDATE SYSTEM_DICT SET VALUE='{"enable":false}' WHERE KEY='plugin.OpenSubtitles';`
			if change == "source" {
				statement = `UPDATE GO_SUBTITLE_INTAKE_SOURCES SET FINGERPRINT='damaged' WHERE JOB_ID='` + job.ID + `';`
			}
			if change == "binding" {
				statement = `UPDATE GO_OPEN_SUBTITLE_REQUESTS SET BINDING='` + strings.Repeat("a", 64) + `' WHERE ID=NEW.ID;`
			}
			trigger := `CREATE TRIGGER change_open_intent AFTER UPDATE OF OPERATIONS ON GO_OPEN_SUBTITLE_REQUESTS WHEN NEW.OPERATIONS!=OLD.OPERATIONS BEGIN ` + statement + ` END`
			if _, e := s.db.Exec(trigger); e != nil {
				t.Fatal(e)
			}
			if _, e := s.OpenSubtitleStep(t.Context(), p.ID, p.Fingerprint, p.Binding, "search", Digest("original search"), func() error { return nil }); e == nil {
				t.Fatal("changed source or provider committed an intent")
			}
			got, e := s.OpenSubtitle(t.Context(), p.ID)
			if e != nil || got.Requests != 0 || got.State != "running" {
				t.Fatal("failed transaction left partial intent", got, e)
			}
		})
	}
}
func TestOpenSubtitleUnknownIntentAndRestartCannotRepeatOrAdoptAnotherOperation(t *testing.T) {
	s, job := openIntakeFixture(t, false, true)
	p, _, _ := openClaim(t, s, job, onlyOpen(t, s))
	index, e := s.OpenSubtitleStep(t.Context(), p.ID, p.Fingerprint, p.Binding, "archive", Digest("original archive"), func() error { return nil })
	if e != nil || index != 0 {
		t.Fatal(index, e)
	}
	if _, e = s.OpenSubtitleStep(t.Context(), p.ID, p.Fingerprint, p.Binding, "archive", Digest("second archive"), func() error { return nil }); e == nil {
		t.Fatal("unacknowledged intent replayed")
	}
	if e = s.FinishOpenSubtitleStep(t.Context(), p.ID, p.Fingerprint, p.Binding, index, "uncertain", ""); e != nil {
		t.Fatal(e)
	}
	if e = s.FinishOpenSubtitle(t.Context(), p.ID, p.Fingerprint, "uncertain", "original service acknowledgement lost"); e != nil {
		t.Fatal(e)
	}
	var seq int
	var databaseName, databasePath string
	if e = s.db.QueryRow("PRAGMA database_list").Scan(&seq, &databaseName, &databasePath); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := OpenStore(databasePath)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	s = reopened
	if rows, e := s.OpenSubtitles(t.Context(), true, 1); e != nil || len(rows) != 0 {
		t.Fatal("uncertain request requeued", rows, e)
	}
	got, e := s.OpenSubtitle(t.Context(), p.ID)
	if e != nil || got.State != "uncertain" || got.Requests != 1 || got.Operations[0].State != "uncertain" {
		t.Fatal(got, e)
	}
	if _, e = s.ClaimOpenSubtitle(t.Context(), p, LibraryDefinition{}, LibraryMedia{}, func() error { return nil }); e == nil {
		t.Fatal("old original request adopted a new publication scope")
	}
}
func TestOpenSubtitleDenialStopsSameGrantAndPendingStopPreservesAllMedia(t *testing.T) {
	s, job := openIntakeFixture(t, false, true)
	p, _, _ := openClaim(t, s, job, onlyOpen(t, s))
	index, e := s.OpenSubtitleStep(t.Context(), p.ID, p.Fingerprint, p.Binding, "search", Digest("denied search"), func() error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	if e = s.FinishOpenSubtitleStep(t.Context(), p.ID, p.Fingerprint, p.Binding, index, "denied", Digest("denial")); e != nil {
		t.Fatal(e)
	}
	denied, e := s.OpenSubtitleDenied(t.Context(), p.Definition.Provider.Epoch)
	if e != nil || !denied {
		t.Fatal("same grant could continue denied requests", denied, e)
	}
	s2, job2 := openIntakeFixture(t, false, true)
	pending := onlyOpen(t, s2)
	stopped, e := s2.StopOpenSubtitle(t.Context(), pending.ID, "new-root", "stop original pending request", func() error { return nil })
	if e != nil || stopped.State != "stopped" || stopped.Requests != 0 || stopped.LibraryID != "" {
		t.Fatal(stopped, e)
	}
	if e = VerifyCompletedTarget(t.Context(), job2, job2.Items[0]); e != nil {
		t.Fatal(e)
	}
	if _, e = s2.StopOpenSubtitle(t.Context(), pending.ID, "new-root", "repeat", func() error { return nil }); e == nil {
		t.Fatal("stop result was silently replayed")
	}
}
func TestOpenSubtitleReadbackRejectsSilentObservationAndOutputSubstitution(t *testing.T) {
	for _, change := range []string{"ignore-output", "replace-owner", "replace-target"} {
		t.Run(change, func(t *testing.T) {
			s, job := openIntakeFixture(t, false, true)
			p, lib, media := openClaim(t, s, job, onlyOpen(t, s))
			trigger := `CREATE TRIGGER corrupt_open_output BEFORE INSERT ON GO_LIBRARY_SCRAPE_OUTPUTS BEGIN SELECT RAISE(IGNORE);END`
			if change == "replace-owner" {
				trigger = `CREATE TRIGGER corrupt_open_output AFTER INSERT ON GO_LIBRARY_SCRAPE_OUTPUTS BEGIN UPDATE GO_LIBRARY_SCRAPE_OUTPUTS SET OWNER='foreign' WHERE ID=NEW.ID;END`
			}
			if change == "replace-target" {
				trigger = `CREATE TRIGGER corrupt_open_output AFTER INSERT ON GO_LIBRARY_SCRAPE_OUTPUTS BEGIN UPDATE GO_LIBRARY_SCRAPE_OUTPUTS SET TARGET='foreign.srt' WHERE ID=NEW.ID;END`
			}
			if _, e := s.db.Exec(trigger); e != nil {
				t.Fatal(e)
			}
			name := OpenSubtitleName(p.Definition, ".srt")
			target, e := LibraryTarget(lib, media, "media", name)
			if e != nil {
				t.Fatal(e)
			}
			owner := Digest([]string{"open-subtitle-output", p.ID, target})
			_, _, e = PublishLibrarySidecar(t.Context(), lib, media, "media", name, []byte("subtitle"), func(proof LibraryOutputProof) error {
				return s.SaveOpenSubtitleOutput(t.Context(), p, lib.Root, target, owner, "writing", "original intent", proof, func() error { return nil })
			}, func() error { return nil })
			if e == nil {
				t.Fatal("corrupt output intent accepted")
			}
			if _, e = os.Stat(filepath.Join(lib.Root.Path, target)); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("failed intent published subtitle", e)
			}
			var count int
			if e = s.db.QueryRow(`SELECT COUNT(*) FROM GO_LIBRARY_SCRAPE_OUTPUTS WHERE JOB_ID=?`, p.LibraryID).Scan(&count); e != nil || count != 0 {
				t.Fatal("partial output receipt survived rollback", count, e)
			}
		})
	}
}
func TestOpenSubtitleOutputIntentRejectsForeignNamesOwnersRootsAndBindings(t *testing.T) {
	s, job := openIntakeFixture(t, false, true)
	p, lib, media := openClaim(t, s, job, onlyOpen(t, s))
	target, e := LibraryTarget(lib, media, "media", OpenSubtitleName(p.Definition, ".srt"))
	if e != nil {
		t.Fatal(e)
	}
	owner := Digest([]string{"open-subtitle-output", p.ID, target})
	badRoot := lib.Root
	badRoot.Path = filepath.Dir(lib.Root.Path)
	badReceipt := p
	badReceipt.Binding = Digest("another binding")
	for _, v := range []struct {
		p                    OpenSubtitleReceipt
		root                 LibraryRoot
		target, owner, state string
	}{
		{p, lib.Root, target, Digest("foreign owner"), "writing"},
		{p, lib.Root, "foreign.srt", Digest([]string{"open-subtitle-output", p.ID, "foreign.srt"}), "writing"},
		{p, badRoot, target, owner, "writing"},
		{badReceipt, lib.Root, target, owner, "writing"},
		{p, lib.Root, target, owner, "replaced"},
	} {
		if e = s.SaveOpenSubtitleOutput(t.Context(), v.p, v.root, v.target, v.owner, v.state, "must reject", LibraryOutputProof{}, func() error { return nil }); e == nil {
			t.Fatal("foreign subtitle output tuple accepted", v)
		}
	}
	if e = s.OpenSubtitleRunning(t.Context(), badReceipt); e == nil {
		t.Fatal("substituted original binding accepted")
	}
	var count int
	if e = s.db.QueryRow("SELECT COUNT(*) FROM GO_LIBRARY_SCRAPE_OUTPUTS WHERE JOB_ID=?", p.LibraryID).Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
}
func TestManualOpenSubtitlesOnlyRequestUsesIndependentNamespaceAndAtomicChildren(t *testing.T) {
	s, d := manualSubtitleFixture(t, false)
	scope := openGrant(t, s, true)
	d.Version = 2
	d.Plugin = subtitles.Snapshot{}
	d.OpenSubtitles = &scope
	// A manual review is bound to the same current actor authority as its provider.
	d.Authority = scope.Authority
	d.RequestID = strings.Repeat("c", 32)
	p := manualSubtitleReview(t, s, d)
	queued, e := s.QueueSubtitleRequest(t.Context(), p.ID, d.Actor, p.Fingerprint, func() error { return nil })
	if e != nil || queued.Service != opensubtitles.Plugin || queued.OpenSubtitles == nil || queued.ServiceState != "pending" {
		t.Fatal(queued, e)
	}
	var count int
	if e = s.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); e != nil || count != 0 {
		t.Fatal("manual subtitle fabricated intake", count, e)
	}
	var hasCF bool
	if e = s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_SUBTITLE_JOBS')`).Scan(&hasCF); e != nil || hasCF {
		t.Fatal("OpenSubtitles-only request created a ChineseSubFinder child", hasCF, e)
	}
	original, e := s.SubtitleRequest(t.Context(), p.ID)
	if e != nil || original.OpenSubtitles.ID != queued.OpenSubtitles.ID {
		t.Fatal(original, e)
	}
	cancelled, e := s.CancelSubtitleRequest(t.Context(), p.ID, "current-root", "stop original request")
	if e != nil || cancelled.State != "cancelled" || cancelled.OpenSubtitles.State != "stopped" {
		t.Fatal(cancelled, e)
	}
	var blob string
	var note sql.NullString
	if e = s.db.QueryRow(`SELECT hex(EXTRA),NOTE FROM TRANSFER_HISTORY WHERE ID=9007199254740993`).Scan(&blob, &note); e != nil || blob != "00FF01" || note.Valid {
		t.Fatal("original typed history changed", blob, note, e)
	}
	if e = VerifySubtitleRequestSource(t.Context(), d, nil); e != nil {
		t.Fatal("original video changed", e)
	}
}
