package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func legacyRenameRequest(t *testing.T, f *subscriptionRunFixture, input map[string]any, preview bool) (*httptest.ResponseRecorder, string) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/organization/unknown/rename"
	if preview {
		path += "/plan"
	}
	w := nativeJSONRequest(f.handler, "POST", path, f.token, string(raw))
	var result struct{ Data struct{ JobID string } }
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w, result.Data.JobID
}

func renameReviewFingerprint(t *testing.T, f *subscriptionRunFixture, input map[string]any) string {
	t.Helper()
	w, id := legacyRenameRequest(t, f, input, true)
	var result struct {
		Data struct{ Plan organizationPlan }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || id != "" || len(result.Data.Plan.Fingerprint) != 64 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	return result.Data.Plan.Fingerprint
}

func TestLegacyRenameUnknownLiteralModesOverrideWithoutChangingProvenance(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		for _, state := range []string{"N", "Y"} {
			t.Run(mode+state, func(t *testing.T) {
				f, source, target, _ := unknownFixture(t, "copy")
				if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET MODE='Rclone移动',STATE=? WHERE ID=7`, state); err != nil {
					t.Fatal(err)
				}
				input := map[string]any{"unknown_id": "7", "syncmod": mode, "type": "MOV", "tmdb": "100"}
				fingerprint := renameReviewFingerprint(t, f, input)
				var count int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
					t.Fatal(count, err)
				}
				if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
					t.Fatal(entries, err)
				}
				input["reviewFingerprint"], input["confirmSourceRemoval"] = fingerprint, mode == "move"
				w, id := legacyRenameRequest(t, f, input, false)
				if w.Code != 200 || id == "" {
					t.Fatal(w.Code, w.Body.String())
				}
				job := organizationReadMoveJob(t, f, id)
				if job.State != "completed" || job.Mode != mode || job.LegacyOrigin == nil || job.LegacyOrigin.Action != "rename" || job.LegacyOrigin.RecordID != "7" || job.LegacyRename == nil || job.UnknownState != "resolved" || unknownState(t, f, 7) != "Y" {
					t.Fatal(job)
				}
				var stored string
				if err := f.db.QueryRow(`SELECT MODE FROM TRANSFER_UNKNOWN WHERE ID=7`).Scan(&stored); err != nil || stored != "Rclone移动" {
					t.Fatal(stored, err)
				}
				for _, item := range job.Items {
					assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
				}
				if mode == "move" {
					if _, err := os.Lstat(filepath.Join(source, "Movie.2026.1080p.mkv")); !os.IsNotExist(err) {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func renameTVFiles(t *testing.T, source string) {
	t.Helper()
	for _, names := range [][2]string{{"Movie.2026.1080p.mkv", "Show.11.mkv"}, {"Movie.2026.1080p.en.srt", "Show.11.en.srt"}} {
		if err := os.Rename(filepath.Join(source, names[0]), filepath.Join(source, names[1])); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"Show.12.mkv", "Show.12.en.srt"} {
		organizationFile(t, source, name)
	}
	for _, name := range []string{"Show.11.mkv", "Show.11.en.srt", "Show.12.mkv", "Show.12.en.srt"} {
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyRenameParentExpansionAndDirectoryFilteringPreserveUnknownRow(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		for _, expand := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "parent", false: "directory"}[expand], func(t *testing.T) {
				f, source, target, _ := unknownFixture(t, "copy")
				renameTVFiles(t, source)
				if expand {
					if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET PATH=? WHERE ID=7`, filepath.Join(source, "Show.11.mkv")); err != nil {
						t.Fatal(err)
					}
				}
				input := map[string]any{"unknown_id": 7, "syncmod": mode, "type": "TV", "tmdb": "200", "season": "1", "episode_format": "Show.{ep}.mkv", "episode_details": "11,11", "episode_offset": "-10", "confirmSourceRemoval": mode == "move"}
				if expand {
					w, id := legacyRenameRequest(t, f, input, false)
					if w.Code != 400 || id != "" {
						t.Fatal(w.Code, w.Body.String())
					}
					input["confirmParentScope"] = true
				}
				input["reviewFingerprint"] = renameReviewFingerprint(t, f, input)
				w, id := legacyRenameRequest(t, f, input, false)
				if w.Code != 200 || id == "" || !strings.Contains(w.Body.String(), `"unknownState":"preserved"`) {
					t.Fatal(w.Code, w.Body.String())
				}
				job := organizationReadMoveJob(t, f, id)
				if job.State != "completed" || job.UnknownState != "preserved" || job.LegacyRename == nil || job.LegacyRename.ExpandedScope != expand || !job.LegacyRename.PreserveUnknown || unknownState(t, f, 7) != "N" || len(job.Items) != 2 {
					t.Fatal(job)
				}
				for _, item := range job.Items {
					if !strings.Contains(item.Target, "S01E01") {
						t.Fatal(item)
					}
					assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
				}
				for _, name := range []string{"Show.12.mkv", "Show.12.en.srt"} {
					assertOrganizationMoveBytes(t, filepath.Join(source, name), "original-media")
				}
				if mode == "move" {
					if _, err := os.Lstat(filepath.Join(source, "Show.11.mkv")); !os.IsNotExist(err) {
						t.Fatal(err)
					}
				}
				var pending int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE STATE='pending'`).Scan(&pending); err != nil || pending != 0 {
					t.Fatal(pending, err)
				}
			})
		}
	}
}

func TestLegacyRenameHistoryReidentifiesOriginalAndPreservesOldPublishedMedia(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, _, original := historyFixture(t, "copy")
			input := map[string]any{"logid": "7", "syncmod": mode, "type": "TV", "tmdb": "200", "episode_details": "2-3", "confirmSourceRemoval": mode == "move"}
			input["reviewFingerprint"] = renameReviewFingerprint(t, f, input)
			w, id := legacyRenameRequest(t, f, input, false)
			if w.Code != 200 || id == "" {
				t.Fatal(w.Code, w.Body.String())
			}
			job := organizationReadMoveJob(t, f, id)
			if job.State != "completed" || job.HistoryRecordID != 7 || job.Mode != mode || len(job.Items) != 1 || !strings.Contains(job.Items[0].Target, "S01E02-E03") {
				t.Fatal(job)
			}
			assertOrganizationMoveBytes(t, filepath.Join(target, job.Items[0].Target), "original-media")
			assertOrganizationMoveBytes(t, filepath.Join(target, "Old/Wrong.mkv"), "original-media")
			var oldTitle, oldID, oldMode string
			if err := f.db.QueryRow(`SELECT TITLE,TMDBID,MODE FROM TRANSFER_HISTORY WHERE ID=7`).Scan(&oldTitle, &oldID, &oldMode); err != nil || oldTitle != original.Title || oldID != original.TMDBID || oldMode != original.Mode {
				t.Fatal(oldTitle, oldID, oldMode, err)
			}
			if mode != "move" {
				assertOrganizationMoveBytes(t, filepath.Join(source, original.SourceFilename), "original-media")
			}
		})
	}
}

func TestLegacyRenameHistoryParentExpansionBindsOriginalAndRequiresConsent(t *testing.T) {
	f, source, target, _, _ := historyFixture(t, "copy")
	renameTVFiles(t, source)
	if _, err := f.db.Exec(`UPDATE TRANSFER_HISTORY SET SOURCE_FILENAME='Show.11.mkv' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"logid": "7", "syncmod": "copy", "type": "TV", "tmdb": "200", "episode_format": "Show.{ep}.mkv", "episode_offset": "-10"}
	w, id := legacyRenameRequest(t, f, input, false)
	if w.Code != 400 || id != "" {
		t.Fatal(w.Code, w.Body.String())
	}
	input["confirmParentScope"] = true
	input["reviewFingerprint"] = renameReviewFingerprint(t, f, input)
	w, id = legacyRenameRequest(t, f, input, false)
	if w.Code != 200 || id == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	job := organizationReadMoveJob(t, f, id)
	if job.State != "completed" || !job.LegacyRename.ExpandedScope || job.LegacyRename.PreserveUnknown || len(job.Items) != 4 {
		t.Fatal(job)
	}
	assertOrganizationMoveBytes(t, filepath.Join(target, "Old/Wrong.mkv"), "original-media")
}

func TestLegacyRenameRejectsInvalidScopeConsentAndPrivateNativeAuthority(t *testing.T) {
	f, _, _, nativeInput := unknownFixture(t, "copy")
	for _, input := range []map[string]any{
		{"syncmod": "copy"}, {"unknown_id": "7", "logid": "7", "syncmod": "copy"}, {"unknown_id": "07", "syncmod": "copy"},
		{"unknown_id": "9223372036854775808", "syncmod": "copy"}, {"unknown_id": "7", "syncmod": "move"},
		{"unknown_id": "7", "syncmod": "copy", "inpath": "/arbitrary"}, {"unknown_id": "7", "syncmod": "copy", "confirmParentScope": true},
		{"unknown_id": "7", "syncmod": "copy", "confirmParentScope": "yes"}, {"unknown_id": "7", "syncmod": "copy", "reviewFingerprint": "invalid"},
	} {
		w, id := legacyRenameRequest(t, f, input, false)
		if w.Code < 400 || id != "" {
			t.Fatal(input, w.Code, w.Body.String())
		}
	}
	for _, key := range []string{"legacyRename", "LegacyRename", "LegacySelection"} {
		raw, _ := json.Marshal(nativeInput)
		body := strings.TrimSuffix(string(raw), "}") + `,"` + key + `":{}}`
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/plan", f.token, body)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	nativeInput.Mode = "link"
	status, _, body := organizationPreview(t, f, nativeInput)
	if status != 422 {
		t.Fatal(status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestLegacyRenameReviewStalenessFormAndNoOverwrite(t *testing.T) {
	f, source, _, _ := unknownFixture(t, "copy")
	input := map[string]any{"unknown_id": "7", "syncmod": "copy", "type": "MOV", "tmdb": "100"}
	input["reviewFingerprint"] = renameReviewFingerprint(t, f, input)
	organizationFile(t, source, "Added.2026.1080p.mkv")
	w, id := legacyRenameRequest(t, f, input, false)
	if w.Code != 409 && w.Code != 422 || id != "" {
		t.Fatal(w.Code, w.Body.String())
	}
	// Remove only the test-owned arrival; a fresh form is a new explicit request.
	if err := os.Remove(filepath.Join(source, "Added.2026.1080p.mkv")); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"unknown_id": {"7"}, "syncmod": {"硬链接"}, "type": {"MOV"}, "tmdb": {"100"}}
	w = performFormRequest(f.handler, "/api/v1/organization/unknown/rename", f.token, form)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = performFormRequest(f.handler, "/api/v1/organization/unknown/rename", f.token, form)
	if w.Code < 400 {
		t.Fatal("repeated form overwrote target", w.Body.String())
	}
}

func TestLegacyRenameRecordChangedDuringReservationRollsBack(t *testing.T) {
	f, _, _, _ := unknownFixture(t, "copy")
	base := f.transport
	calls := 0
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "tmdb.test" && r.URL.Path == "/3/movie/100" {
			calls++
			if calls == 2 {
				if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET MODE='changed' WHERE ID=7`); err != nil {
					t.Fatal(err)
				}
			}
		}
		return base.RoundTrip(r)
	})
	var err error
	f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	w, id := legacyRenameRequest(t, f, map[string]any{"unknown_id": "7", "syncmod": "copy", "tmdb": "100", "type": "MOV"}, false)
	if w.Code != 409 || id != "" || calls != 2 {
		t.Fatal(calls, w.Code, w.Body.String())
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestLegacyRenamePreserveReceiptDoesNotAdoptChangedRow(t *testing.T) {
	f, source, _, _ := unknownFixture(t, "copy")
	renameTVFiles(t, source)
	if _, err := f.db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY,MODE TEXT,TYPE TEXT,CATEGORY TEXT,TMDBID INTEGER,TITLE TEXT,YEAR TEXT,SEASON_EPISODE TEXT,SOURCE TEXT,SOURCE_PATH TEXT,SOURCE_FILENAME TEXT,DEST TEXT,DEST_PATH TEXT,DEST_FILENAME TEXT,DATE TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER rename_change AFTER INSERT ON TRANSFER_HISTORY BEGIN UPDATE TRANSFER_UNKNOWN SET MODE='changed' WHERE ID=7; END`); err != nil {
		t.Fatal(err)
	}
	w, id := legacyRenameRequest(t, f, map[string]any{"unknown_id": "7", "syncmod": "copy", "type": "TV", "tmdb": "200", "season": "1", "episode_format": "Show.{ep}.mkv", "episode_details": "11,11", "episode_offset": "-10"}, false)
	if w.Code < 400 || id == "" || strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	job := organizationReadMoveJob(t, f, id)
	if job.State == "completed" || job.UnknownState != "pending" || unknownState(t, f, 7) != "N" {
		t.Fatal(job)
	}
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/delete", f.token, `{"id":"7"}`)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestLegacyRenameExactLargeIDsAndConfiguredPhysicalAliases(t *testing.T) {
	f, source, target, _ := unknownFixture(t, "copy")
	const id = "9223372036854775807"
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET ID=? WHERE ID=7`, id); err != nil {
		t.Fatal(err)
	}
	sourceAlias, targetAlias := filepath.Join(t.TempDir(), "source"), filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(source, sourceAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, targetAlias); err != nil {
		t.Fatal(err)
	}
	physicalSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	physicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET PATH=?,DEST=? WHERE ID=?`, physicalSource, physicalTarget, id); err != nil {
		t.Fatal(err)
	}
	dirs, _ := json.Marshal([]map[string]string{{"save_path": sourceAlias}})
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET DOWNLOAD_DIR=?`, string(dirs)); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": targetAlias, "media.tv_path": targetAlias}); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"unknown_id": id, "syncmod": "copy", "type": "MOV", "tmdb": "100"}
	input["reviewFingerprint"] = renameReviewFingerprint(t, f, input)
	w, jobID := legacyRenameRequest(t, f, input, false)
	if w.Code != 200 || jobID == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	job := organizationReadMoveJob(t, f, jobID)
	if job.LegacyOrigin.RecordID != id || job.UnknownState != "resolved" {
		t.Fatal(job)
	}
}

func TestLegacyRenameMissingOriginalAndUnauthorizedNeverExecute(t *testing.T) {
	f, source, target, _, record := historyFixture(t, "copy")
	if err := os.Remove(filepath.Join(source, record.SourceFilename)); err != nil {
		t.Fatal(err)
	}
	w, id := legacyRenameRequest(t, f, map[string]any{"logid": "7", "syncmod": "copy", "type": "MOV", "tmdb": "100"}, false)
	if w.Code != 422 || id != "" {
		t.Fatal(w.Code, w.Body.String())
	}
	assertOrganizationMoveBytes(t, filepath.Join(target, "Old/Wrong.mkv"), "original-media")
	for _, path := range []string{"/api/v1/organization/unknown/rename", "/api/v1/organization/unknown/rename/plan"} {
		w := nativeJSONRequest(f.handler, "POST", path, "", `{"logid":"7","syncmod":"copy"}`)
		if w.Code != 401 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	viewer := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if viewer.Code != 200 {
		t.Fatal(viewer.Code, viewer.Body.String())
	}
	token := loginForTest(t, f.handler, "viewer", "strong-password")
	for _, path := range []string{"/api/v1/organization/unknown/rename", "/api/v1/organization/unknown/rename/plan"} {
		w := nativeJSONRequest(f.handler, "POST", path, token, `{"logid":"7","syncmod":"copy"}`)
		if w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
