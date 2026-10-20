//go:build linux || darwin

package organization

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func libraryPluginStore(t *testing.T) (*Store, map[string]string) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT)`); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{"UserInstalledPlugins": "", "plugin.LibraryScraper": "", "GoLibraryScraper": "", "UserScraperConf": ""}
	if err := s.LibraryPluginMutation(t.Context(), expected, map[string]string{"UserInstalledPlugins": `["LibraryScraper"]`, "plugin.LibraryScraper": `{"onlyonce":false}`, "GoLibraryScraper": "initial"}, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	expected["UserInstalledPlugins"], expected["plugin.LibraryScraper"], expected["GoLibraryScraper"] = `["LibraryScraper"]`, `{"onlyonce":false}`, "initial"
	return s, expected
}
func libraryPreparedRun(t *testing.T, d LibraryDefinition, m LibraryMedia, event bool) (LibraryPrepared, LibraryPluginRun) {
	t.Helper()
	epoch, batch := strings.Repeat("e", 32), strings.Repeat("b", 32)
	d.Schedule = &LibraryScheduleOrigin{Epoch: epoch, Batch: batch, Event: event}
	id := Digest([]string{d.Actor, d.RequestID})
	run := LibraryPluginRun{ID: batch, Epoch: epoch, State: "queued", Jobs: []string{id}}
	if event {
		run.Source = "event"
	}
	return LibraryPrepared{Fingerprint: Digest([]any{d, m}), Definition: d, Files: []LibraryMedia{m}}, run
}
func TestLibraryPluginJobBatchSettingsAndRunAreOneTransaction(t *testing.T) {
	d, m := libraryFixture(t)
	s, raw := libraryPluginStore(t)
	p, run := libraryPreparedRun(t, d, m, false)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_library_batch BEFORE INSERT ON GO_LIBRARY_SCRAPE_ITEMS BEGIN SELECT RAISE(ABORT,'cannot save media'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.LibraryPluginMutation(t.Context(), raw, map[string]string{"GoLibraryScraper": "queued"}, "", &run, []LibraryPrepared{p}); err == nil {
		t.Fatal("partial batch committed")
	}
	jobs, err := s.LibraryJobs(t.Context(), false)
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	runs, err := s.LibraryPluginRuns(t.Context())
	if err != nil || len(runs) != 0 {
		t.Fatal(runs, err)
	}
	var actual string
	s.db.QueryRow(`SELECT VALUE FROM SYSTEM_DICT WHERE KEY='GoLibraryScraper'`).Scan(&actual)
	if actual != "initial" {
		t.Fatal("settings committed without jobs", actual)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_library_batch`); err != nil {
		t.Fatal(err)
	}
	if err := s.LibraryPluginMutation(t.Context(), raw, map[string]string{"GoLibraryScraper": "queued"}, "", &run, []LibraryPrepared{p}); err != nil {
		t.Fatal(err)
	}
	if err := s.LibraryPluginMutation(t.Context(), raw, map[string]string{"GoLibraryScraper": "replayed"}, "", &run, []LibraryPrepared{p}); !errors.Is(err, ErrState) {
		t.Fatal("old control replayed", err)
	}
	jobs, err = s.LibraryJobs(t.Context(), false)
	if err != nil || len(jobs) != 1 || jobs[0].State != "ready" {
		t.Fatal(jobs, err)
	}
}
func TestLibraryPluginRevocationPreservesManualJobsAndUncertainClaims(t *testing.T) {
	d, m := libraryFixture(t)
	s, raw := libraryPluginStore(t)
	p, run := libraryPreparedRun(t, d, m, true)
	if err := s.LibraryPluginMutation(t.Context(), raw, map[string]string{"GoLibraryScraper": "queued"}, "", &run, []LibraryPrepared{p}); err != nil {
		t.Fatal(err)
	}
	id := run.Jobs[0]
	if err := s.ClaimLibraryJob(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	proof := LibraryOutputProof{Target: "Movie/poster.png", ParentIdentity: "parent", StageIdentity: "stage", NewIdentity: "new", NewDigest: strings.Repeat("a", 64), NewSize: 1}
	if err := s.SaveLibraryOutput(t.Context(), id, d.Root, proof.Target, Digest("owner"), "writing", "", proof); err != nil {
		t.Fatal(err)
	}
	manual := d
	manual.RequestID = strings.Repeat("c", 32)
	manualID, err := s.CreateLibraryJob(t.Context(), Digest(manual), manual, []LibraryMedia{m})
	if err != nil {
		t.Fatal(err)
	}
	raw["GoLibraryScraper"] = "queued"
	if err := s.LibraryPluginMutation(t.Context(), raw, map[string]string{"GoLibraryScraper": "stopped", "UserInstalledPlugins": "[]"}, "*", nil, nil); err != nil {
		t.Fatal(err)
	}
	job, err := s.LibraryJob(t.Context(), id)
	if err != nil || job.State != "cancelled" {
		t.Fatal(job, err)
	}
	job, err = s.LibraryJob(t.Context(), manualID)
	if err != nil || job.State != "ready" {
		t.Fatal("manual job revoked", job, err)
	}
	if uncertain, err := s.LibraryPluginUncertain(t.Context(), []string{id}); err != nil || !uncertain {
		t.Fatal("revocation released uncertain output", uncertain, err)
	}
	runs, err := s.LibraryPluginRuns(t.Context())
	if err != nil || len(runs) != 1 || runs[0].State != "revoked" {
		t.Fatal(runs, err)
	}
}
func TestLibraryEventCompletionReceiptRollsBackWithJobFinish(t *testing.T) {
	d, m := libraryFixture(t)
	s, raw := libraryPluginStore(t)
	p, run := libraryPreparedRun(t, d, m, true)
	if err := s.LibraryPluginMutation(t.Context(), raw, map[string]string{"GoLibraryScraper": "queued"}, "", &run, []LibraryPrepared{p}); err != nil {
		t.Fatal(err)
	}
	id := run.Jobs[0]
	if err := s.ClaimLibraryJob(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := s.LibraryItemState(t.Context(), id, 0, "pending", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.LibraryItemState(t.Context(), id, 0, "running", "completed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_library_event_completion BEFORE UPDATE ON GO_LIBRARY_SCRAPE_RUNS BEGIN SELECT RAISE(ABORT,'receipt unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishLibraryJob(t.Context(), id, "completed", ""); err == nil {
		t.Fatal("failed receipt committed job finish")
	}
	job, err := s.LibraryJob(t.Context(), id)
	if err != nil || job.State != "running" {
		t.Fatal(job, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_library_event_completion`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishLibraryJob(t.Context(), id, "completed", ""); err != nil {
		t.Fatal(err)
	}
	runs, err := s.LibraryPluginRuns(t.Context())
	if err != nil || len(runs) != 1 || runs[0].State != "completed" {
		t.Fatal(runs, err)
	}
	data, _ := json.Marshal(runs)
	if strings.Contains(string(data), run.Epoch) || strings.Contains(string(data), d.Root.Path) {
		t.Fatal("private schedule scope leaked", string(data))
	}
}
func TestLibraryPluginControlWriteReadbackFailureRollsBackAllRows(t *testing.T) {
	d, m := libraryFixture(t)
	s, raw := libraryPluginStore(t)
	p, run := libraryPreparedRun(t, d, m, false)
	if _, err := s.db.Exec(`CREATE TRIGGER corrupt_library_control AFTER UPDATE ON SYSTEM_DICT WHEN NEW.KEY='GoLibraryScraper' BEGIN UPDATE SYSTEM_DICT SET VALUE='corrupt' WHERE ID=NEW.ID; END`); err != nil {
		t.Fatal(err)
	}
	if err := s.LibraryPluginMutation(t.Context(), raw, map[string]string{"GoLibraryScraper": "queued"}, "", &run, []LibraryPrepared{p}); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	jobs, err := s.LibraryJobs(t.Context(), false)
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	runs, err := s.LibraryPluginRuns(t.Context())
	if err != nil || len(runs) != 0 {
		t.Fatal(runs, err)
	}
}
