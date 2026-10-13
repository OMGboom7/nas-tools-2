package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func legacyRedoRequest(t *testing.T, f *subscriptionRunFixture, flag string, ids []int64, move bool) (int, bool, []legacyRedoResult, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"flag": flag, "ids": ids, "confirmSourceRemoval": move})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/redo", f.token, string(raw))
	var result struct {
		Success bool
		Data    struct{ Results []legacyRedoResult }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w.Code, result.Success, result.Data.Results, w.Body.String()
}
func TestLegacyRedoExecutesUnknownLiteralModesAndKeepsProcessedState(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		for _, processed := range []bool{false, true} {
			t.Run(mode+"/"+map[bool]string{true: "processed", false: "pending"}[processed], func(t *testing.T) {
				f, source, target, _ := unknownFixture(t, mode)
				if processed {
					if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET STATE='Y' WHERE ID=7`); err != nil {
						t.Fatal(err)
					}
				}
				status, success, results, body := legacyRedoRequest(t, f, "unidentification", []int64{7}, mode == "move")
				if status != 200 || !success || len(results) != 1 || results[0].JobID == "" || results[0].State != "completed" {
					t.Fatal(status, body)
				}
				job := organizationReadMoveJob(t, f, results[0].JobID)
				if job.Mode != mode || job.UnknownState != "resolved" || job.LegacyOrigin == nil || job.LegacyOrigin.Action != "redo" || job.LegacyOrigin.RecordID != "7" || job.LegacyOrigin.Actor == "" || unknownState(t, f, 7) != "Y" {
					t.Fatal(job)
				}
				for _, item := range job.Items {
					assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
				}
				if mode == "move" {
					if _, err := os.Lstat(filepath.Join(source, "Movie.2026.1080p.mkv")); !os.IsNotExist(err) {
						t.Fatal(err)
					}
				} else {
					assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
				}
				var count int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
					t.Fatal(count, err)
				}
			})
		}
	}
}
func TestLegacyRedoRequiresSourceConsentBeforeCreatingMoveJob(t *testing.T) {
	f, source, _, _ := unknownFixture(t, "move")
	status, success, _, body := legacyRedoRequest(t, f, "unidentification", []int64{7}, false)
	if status != 400 || success || unknownState(t, f, 7) != "N" {
		t.Fatal(status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
}
func TestLegacyRedoUsesConfiguredDefaultAndReturnsPerRecordFailure(t *testing.T) {
	f, source, _, _ := unknownFixture(t, "copy")
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET MODE='',DEST='' WHERE ID=7;INSERT INTO TRANSFER_UNKNOWN VALUES(8,'/not-configured','','Rclone移动','N')`); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"pt.rmt_mode": "link"}); err != nil {
		t.Fatal(err)
	}
	status, success, results, body := legacyRedoRequest(t, f, "unidentification", []int64{7, 8}, false)
	if status != 200 || success || len(results) != 2 || results[0].State != "completed" || results[1].Code == 0 || results[1].JobID != "" || unknownState(t, f, 8) != "N" {
		t.Fatal(status, body)
	}
	job := organizationReadMoveJob(t, f, results[0].JobID)
	if job.Mode != "link" {
		t.Fatal(job)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
}
func TestLegacyRedoFormValidationAndNoAutomaticRetry(t *testing.T) {
	f, source, _, _ := unknownFixture(t, "copy")
	for _, body := range []string{`{"flag":"unidentification","ids":[]}`, `{"flag":"invalid","ids":[7]}`, `{"flag":"unidentification","ids":[7,7]}`, `{"flag":"unidentification","ids":[7],"actor":"forged"}`} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/redo", f.token, body)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := performFormRequest(f.handler, "/api/v1/organization/unknown/redo", f.token, url.Values{"flag": {"unknow"}, "ids": {"7"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	status, success, _, body := legacyRedoRequest(t, f, "unidentification", []int64{7}, false)
	if status != 200 || success {
		t.Fatal("repeated request claimed a new transfer", status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
}

func TestLegacyRedoHistoricalOriginalsExecuteWithoutChangingOldHistoryOrPublishedMedia(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, _, original := historyFixture(t, mode)
			status, success, results, body := legacyRedoRequest(t, f, "history", []int64{7}, mode == "move")
			if status != 200 || !success || len(results) != 1 || results[0].State != "completed" {
				t.Fatal(status, body)
			}
			job := organizationReadMoveJob(t, f, results[0].JobID)
			if job.HistoryRecordID != 7 || job.LegacyOrigin == nil || job.LegacyOrigin.Kind != "history" {
				t.Fatal(job)
			}
			assertOrganizationMoveBytes(t, filepath.Join(target, "Old/Wrong.mkv"), "original-media")
			var title string
			var count int
			if err := f.db.QueryRow(`SELECT TITLE FROM TRANSFER_HISTORY WHERE ID=7`).Scan(&title); err != nil || title != original.Title {
				t.Fatal(title, err)
			}
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 2 {
				t.Fatal(count, err)
			}
			if mode != "move" {
				assertOrganizationMoveBytes(t, filepath.Join(source, original.SourceFilename), "original-media")
			}
		})
	}
}

func TestLegacyRedoNeverAdoptsUnknownRunningStateOrMissingHistoricalSource(t *testing.T) {
	f, _, _, input := unknownFixture(t, "copy")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running' WHERE JOB_ID=? AND ORDINAL=0`, job.ID); err != nil {
		t.Fatal(err)
	}
	status, success, results, body := legacyRedoRequest(t, f, "unidentification", []int64{7}, false)
	if status != 200 || success || len(results) != 1 || results[0].Code == 0 || unknownState(t, f, 7) != "N" {
		t.Fatal(status, body)
	}
	h, source, target, _, original := historyFixture(t, "move")
	if err := os.Remove(filepath.Join(source, original.SourceFilename)); err != nil {
		t.Fatal(err)
	}
	status, success, results, body = legacyRedoRequest(t, h, "history", []int64{7}, true)
	if status != 200 || success || results[0].JobID != "" {
		t.Fatal(status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(target, "Old/Wrong.mkv"), "original-media")
}

func TestLegacyRedoRevalidatesOriginalRecordAndNativeProcessedFlagIsNotClientControlled(t *testing.T) {
	f, _, _, input := unknownFixture(t, "copy")
	base := f.transport
	changed := false
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "tmdb.test" && r.URL.Path == "/3/movie/100" && !changed {
			changed = true
			if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET DEST='/changed' WHERE ID=7`); err != nil {
				t.Fatal(err)
			}
		}
		return base.RoundTrip(r)
	})
	var err error
	f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	status, success, results, body := legacyRedoRequest(t, f, "unidentification", []int64{7}, false)
	if status != 200 || success || len(results) != 1 || results[0].JobID != "" || !changed {
		t.Fatal(status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	raw, _ := json.Marshal(input)
	raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"AllowProcessedUnknown":true}`)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/plan", f.token, string(raw))
	if w.Code != 400 {
		t.Fatal("native client set private retry authority", w.Code, w.Body.String())
	}
	for _, token := range []string{"", "forged"} {
		r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/redo", token, `{"flag":"unidentification","ids":[7]}`)
		if r.Code != 401 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
}
