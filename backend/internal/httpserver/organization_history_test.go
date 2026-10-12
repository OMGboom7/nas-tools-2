package httpserver

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func historyFixture(t *testing.T, mode string) (*subscriptionRunFixture, string, string, organizationPlanInput, organization.HistoryRecord) {
	t.Helper()
	f, source, target, input, plan := organizationLinkFixture(t, mode)
	var filename string
	for _, item := range plan.Items {
		if item.Kind == "media" {
			filename = item.Source
		}
	}
	if _, err := f.db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY,MODE TEXT,TYPE TEXT,CATEGORY TEXT,TMDBID INTEGER,TITLE TEXT,YEAR TEXT,SEASON_EPISODE TEXT,SOURCE TEXT,SOURCE_PATH TEXT,SOURCE_FILENAME TEXT,DEST TEXT,DEST_PATH TEXT,DEST_FILENAME TEXT,DATE TEXT)`); err != nil {
		t.Fatal(err)
	}
	record := organization.HistoryRecord{ID: 7, Mode: organization.ModeLabel(mode), Type: "电影", TMDBID: "999", Title: "Wrong", Year: "2020", Source: "旧手动整理", SourcePath: source, SourceFilename: filename, Dest: target, DestPath: filepath.Join(target, "Old"), DestFilename: "Wrong.mkv", Date: "2026-01-01 00:00:00"}
	if _, err := f.db.Exec(`INSERT INTO TRANSFER_HISTORY VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.ID, record.Mode, record.Type, record.Category, record.TMDBID, record.Title, record.Year, record.SeasonEpisode, record.Source, record.SourcePath, record.SourceFilename, record.Dest, record.DestPath, record.DestFilename, record.Date); err != nil {
		t.Fatal(err)
	}
	organizationFile(t, target, "Old/Wrong.mkv")
	input.Path = filename
	input.HistoryRecord = &organizationHistorySelector{ID: record.ID, Fingerprint: organization.Digest(record)}
	return f, source, target, input, record
}
func historyList(t *testing.T, f *subscriptionRunFixture) []organizationHistoryItem {
	t.Helper()
	r := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/history", f.token, "")
	var payload struct {
		Data struct{ Items []organizationHistoryItem }
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &payload) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return payload.Data.Items
}

func TestHistoryReplanPreservesOriginalAndCreatesNewLiteralTransfer(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input, original := historyFixture(t, mode)
			items := historyList(t, f)
			if len(items) != 1 || items[0].Fingerprint != input.HistoryRecord.Fingerprint || items[0].Relative != input.Path || items[0].SourceID != input.SourceID || items[0].NativeMode != mode {
				t.Fatal(items)
			}
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 || len(plan.Items) != 1 || plan.Items[0].Status != "available" || plan.Items[0].TMDBID != "100" {
				t.Fatal(status, body)
			}
			selection := assignmentFor(plan.Items[0], "movie", "100")
			input.Assignments = []organizationMediaAssignment{selection}
			status, plan, body = organizationPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			job := organizationCreateJob(t, f, input, plan)
			if job.HistoryRecordID != 7 || !job.Items[0].Assigned {
				t.Fatal(job)
			}
			status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" {
				t.Fatal(status, body)
			}
			assertOrganizationMoveBytes(t, filepath.Join(target, "Old/Wrong.mkv"), "original-media")
			assertOrganizationMoveBytes(t, filepath.Join(target, done.Items[0].Target), "original-media")
			if mode != "move" {
				assertOrganizationMoveBytes(t, filepath.Join(source, input.Path), "original-media")
			}
			stored, err := organization.ScanHistory(f.db.QueryRow(`SELECT ` + organization.HistoryColumns + ` FROM TRANSFER_HISTORY WHERE ID=7`))
			if err != nil || organization.Digest(stored) != organization.Digest(original) {
				t.Fatal("rewrote original history", stored, err)
			}
			var count int
			if err = f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 2 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestHistoryReplanRejectsChangedRowMissingOriginalOrDifferentScope(t *testing.T) {
	for _, kind := range []string{"changed", "missing-source", "scope", "mode", "both", "existing-target"} {
		t.Run(kind, func(t *testing.T) {
			f, source, target, input, _ := historyFixture(t, "copy")
			switch kind {
			case "changed":
				if _, err := f.db.Exec(`UPDATE TRANSFER_HISTORY SET TITLE='changed' WHERE ID=7`); err != nil {
					t.Fatal(err)
				}
			case "missing-source":
				if err := os.Remove(filepath.Join(source, input.Path)); err != nil {
					t.Fatal(err)
				}
			case "scope":
				input.Path = "."
			case "mode":
				input.Mode = "move"
			case "both":
				input.UnknownRecord = &organizationUnknownSelector{ID: 1, Fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
			case "existing-target":
				organizationFile(t, target, "Movie (2026)/Movie (2026)-1080p.mkv")
			}
			status, plan, body := organizationPreview(t, f, input)
			if kind == "existing-target" {
				if status != 200 || plan.Items[0].Status != "conflict" {
					t.Fatal(status, body)
				}
			} else if status != 409 && status != 422 {
				t.Fatal(kind, status, body)
			}
			assertOrganizationMoveBytes(t, filepath.Join(target, "Old/Wrong.mkv"), "original-media")
		})
	}
}

func TestHistoryReplanChecksOriginalRecordAgainBeforeExecution(t *testing.T) {
	f, source, target, input, _ := historyFixture(t, "copy")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`UPDATE TRANSFER_HISTORY SET DEST_FILENAME='changed.mkv' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationJobRequest(t, f, job.ID, "execute")
	if status != 409 {
		t.Fatal(status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, input.Path), "original-media")
	assertOrganizationMoveBytes(t, filepath.Join(target, "Old/Wrong.mkv"), "original-media")
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestHistoryProjectionRetainsConfiguredRootAliasesAndRejectsUnsafeFilename(t *testing.T) {
	source, _ := filepath.EvalSymlinks(t.TempDir())
	target, _ := filepath.EvalSymlinks(t.TempDir())
	aliases := t.TempDir()
	sourceAlias, targetAlias := filepath.Join(aliases, "source"), filepath.Join(aliases, "target")
	if err := os.Symlink(source, sourceAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, targetAlias); err != nil {
		t.Fatal(err)
	}
	roots := organizationRoots{Sources: []organizationRoot{{ID: "s", Path: sourceAlias}}, Targets: []organizationRoot{{ID: "t", Path: targetAlias}}}
	record := organization.HistoryRecord{ID: 7, Mode: "复制", SourcePath: source, SourceFilename: "Movie.mkv", Dest: target}
	item := projectHistory(record, roots)
	if item.Reason != "" || item.SourceID != "s" || item.TargetID != "t" || item.Relative != "Movie.mkv" || !historyPathMatches(sourceAlias, item.Relative, filepath.Join(source, "Movie.mkv")) {
		t.Fatal(item)
	}
	record.SourceFilename = "../outside.mkv"
	if item := projectHistory(record, roots); item.Reason == "" {
		t.Fatal(item)
	}
}

func TestHistoryOriginalIsRecheckedInsideSuccessTransaction(t *testing.T) {
	f, _, target, input, _ := historyFixture(t, "copy")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	draft := organizationCreateJob(t, f, input, plan)
	s, err := organization.OpenStore(filepath.Join(filepath.Dir(f.path), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err := s.Get(t.Context(), draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := job.Items[0]
	if ok, err := s.Claim(t.Context(), job.ID, item.Index); err != nil || !ok {
		t.Fatal(ok, err)
	}
	proof, err := organization.TransferJournaled(t.Context(), job.Definition, item.Entry, organization.TempName(job.ID, item.Index), func(p organization.Proof) error { return s.Stage(t.Context(), job.ID, item.Index, p) }, func(p organization.Proof) error {
		if err := s.Prepare(t.Context(), job.ID, item.Index, p); err != nil {
			return err
		}
		_, err := f.db.Exec(`UPDATE TRANSFER_HISTORY SET TITLE='changed after prepare' WHERE ID=7`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	item.Proof = proof
	if err = s.Complete(t.Context(), job, item); !errors.Is(err, organization.ErrState) {
		t.Fatal("appended after original changed", err)
	}
	var count int
	if err = f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
	current, err := s.Get(t.Context(), job.ID)
	if err != nil || current.Items[0].State != "prepared" {
		t.Fatal(current, err)
	}
}
