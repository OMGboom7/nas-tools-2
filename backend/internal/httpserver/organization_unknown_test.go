package httpserver

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func unknownFixture(t *testing.T, mode string) (*subscriptionRunFixture, string, string, organizationPlanInput) {
	t.Helper()
	f, source, target, input, _ := organizationLinkFixture(t, mode)
	if _, err := f.db.Exec(`CREATE TABLE TRANSFER_UNKNOWN (ID INTEGER PRIMARY KEY,PATH TEXT,DEST TEXT,MODE TEXT,STATE TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO TRANSFER_UNKNOWN VALUES (7,?,?,?,'N')`, source, target, organization.ModeLabel(mode)); err != nil {
		t.Fatal(err)
	}
	record := organization.UnknownRecord{ID: 7, Path: source, Dest: target, Mode: organization.ModeLabel(mode), State: "N"}
	input.UnknownRecord = &organizationUnknownSelector{ID: 7, Fingerprint: organization.Digest(record)}
	return f, source, target, input
}
func unknownState(t *testing.T, f *subscriptionRunFixture, id int) string {
	t.Helper()
	var state string
	if err := f.db.QueryRow(`SELECT STATE FROM TRANSFER_UNKNOWN WHERE ID=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
func unknownList(t *testing.T, f *subscriptionRunFixture, query string) []organizationUnknownItem {
	t.Helper()
	r := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/unknown"+query, f.token, "")
	var payload struct {
		Data struct{ Items []organizationUnknownItem }
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &payload) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return payload.Data.Items
}

func TestUnknownRecordsRemainUntilEntireLiteralJobAndCleanupComplete(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input := unknownFixture(t, mode)
			if _, err := f.db.Exec(`INSERT INTO TRANSFER_UNKNOWN VALUES (8,?,?,?,'N')`, source, target, organization.ModeLabel(mode)); err != nil {
				t.Fatal(err)
			}
			items := unknownList(t, f, "")
			if len(items) != 2 || items[0].ID != 8 || items[0].Relative != "." || items[0].SourceID != input.SourceID || items[0].NativeMode != mode {
				t.Fatal(items)
			}
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			job := organizationCreateJob(t, f, input, plan)
			if job.UnknownRecordID != 7 || job.UnknownState != "pending" || unknownState(t, f, 7) != "N" {
				t.Fatal(job)
			}
			for _, item := range unknownList(t, f, "") {
				if item.ID == 7 && item.JobID != job.ID {
					t.Fatal(item)
				}
			}
			status, _, body = organizationJobRequest(t, f, job.ID, "reconcile-unknown")
			if status != 200 || unknownState(t, f, 7) != "N" {
				t.Fatal("marked before transfer", status, body)
			}
			status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" || done.UnknownState != "resolved" || unknownState(t, f, 7) != "Y" || unknownState(t, f, 8) != "N" {
				t.Fatal(status, body)
			}
			for _, item := range done.Items {
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
			status, _, body = organizationJobRequest(t, f, job.ID, "reconcile-unknown")
			if status != 200 {
				t.Fatal(status, body)
			}
			if len(unknownList(t, f, "")) != 1 {
				t.Fatal("did not preserve duplicate row")
			}
		})
	}
}

func TestUnknownCompatibilityFailureRetriesOnlySavedReceiptsNotFiles(t *testing.T) {
	f, source, target, input := unknownFixture(t, "move")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_unknown BEFORE UPDATE OF STATE ON TRANSFER_UNKNOWN WHEN NEW.STATE='Y' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if status != 503 || unknownState(t, f, 7) != "N" {
		t.Fatal(status, body)
	}
	job = organizationReadMoveJob(t, f, job.ID)
	if job.State != "needs_review" || job.UnknownState != "pending" {
		t.Fatal(job)
	}
	for _, item := range job.Items {
		if item.State != "completed" || item.Reason != "" {
			t.Fatal(item)
		}
		assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
	}
	if err := os.WriteFile(filepath.Join(source, job.Items[0].Source), []byte("new-arrival"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_unknown`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET DEST='/changed' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationJobRequest(t, f, job.ID, "reconcile-unknown")
	if status != 409 || unknownState(t, f, 7) != "N" {
		t.Fatal("marked changed record", status, body)
	}
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET DEST=? WHERE ID=7`, target); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationJobRequest(t, f, job.ID, "reconcile-unknown")
	if status != 409 || unknownState(t, f, 7) != "N" {
		t.Fatal("ignored new source arrival", status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[0].Source), "new-arrival")
	if err := os.Remove(filepath.Join(source, job.Items[0].Source)); err != nil {
		t.Fatal(err)
	}
	status, done, body := organizationJobRequest(t, f, job.ID, "reconcile-unknown")
	if status != 200 || done.UnknownState != "resolved" {
		t.Fatal(status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestUnknownSelectionRejectsChangedOrPartialScopeAndPreservesRecordOnCancellation(t *testing.T) {
	f, source, _, input := unknownFixture(t, "copy")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	partial := input
	partial.Path = plan.Items[0].Source
	status, _, body = organizationPreview(t, f, partial)
	if status != 422 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET MODE='移动' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationJobRequest(t, f, job.ID, "execute")
	if status != 409 {
		t.Fatal(status, body)
	}
	status, cancelled, body := organizationJobRequest(t, f, job.ID, "cancel")
	if status != 200 || cancelled.UnknownState != "cancelled" || unknownState(t, f, 7) != "N" {
		t.Fatal(status, body)
	}
	for _, item := range job.Items {
		assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
	}
	input.UnknownRecord = nil
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	organizationFile(t, source, "Unrelated.srt")
	items := unknownList(t, f, "")
	input.UnknownRecord = &organizationUnknownSelector{ID: items[0].ID, Fingerprint: items[0].Fingerprint}
	input.Mode = "move"
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	raw, _ := json.Marshal(struct {
		organizationPlanInput
		Fingerprint string `json:"fingerprint"`
	}{input, plan.Fingerprint})
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw))
	if r.Code != 422 || unknownState(t, f, 7) != "N" {
		t.Fatal("created partial linked folder", r.Code, r.Body.String())
	}
}

func TestUnknownListPreservesUnsupportedAndUnmappedRowsAndIsReadOnly(t *testing.T) {
	f, _, _, _ := unknownFixture(t, "copy")
	if _, err := f.db.Exec(`INSERT INTO TRANSFER_UNKNOWN VALUES (9,'/outside/movie.mkv','','Rclone复制','N'), (10,'/outside/other.mkv','','复制','N'), (11,NULL,NULL,NULL,'N'), (12,'/processed','','复制','Y')`); err != nil {
		t.Fatal(err)
	}
	items := unknownList(t, f, "")
	if len(items) != 4 {
		t.Fatal(items)
	}
	for _, item := range items {
		if item.ID != 7 && item.Reason == "" {
			t.Fatal("adopted unsupported path/mode", item)
		}
	}
	filtered := unknownList(t, f, "?keyword="+url.QueryEscape("other"))
	if len(filtered) != 1 || filtered[0].ID != 10 {
		t.Fatal(filtered)
	}
	if unknownState(t, f, 9) != "N" || unknownState(t, f, 12) != "Y" {
		t.Fatal("list changed records")
	}
	r := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/unknown", "", "")
	if r.Code != 401 {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestUnknownRecordReservationIsIndependentOfDifferentTargets(t *testing.T) {
	f, source, target, input := unknownFixture(t, "copy")
	other := t.TempDir()
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": []any{target, other}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET DEST='' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	input.UnknownRecord.Fingerprint = organization.Digest(organization.UnknownRecord{ID: 7, Path: source, Mode: "复制", State: "N"})
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	first := organizationCreateJob(t, f, input, plan)
	input.TargetID = subscriptionResourceKey("target:" + other + ":MOV")
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	raw, _ := json.Marshal(struct {
		organizationPlanInput
		Fingerprint string `json:"fingerprint"`
	}{input, plan.Fingerprint})
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw))
	if r.Code != 409 {
		t.Fatal("record simultaneously claimed for a different target", r.Code, r.Body.String())
	}
	status, _, body = organizationJobRequest(t, f, first.ID, "cancel")
	if status != 200 {
		t.Fatal(status, body)
	}
	second := organizationCreateJob(t, f, input, plan)
	if second.ID == first.ID || unknownState(t, f, 7) != "N" {
		t.Fatal(second)
	}
}

func TestUnknownMoveCleanupReceiptMustFinishBeforeLegacyState(t *testing.T) {
	f, _, _, input := unknownFixture(t, "move")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_move_receipt BEFORE UPDATE OF REASON ON GO_ORGANIZATION_ITEMS WHEN NEW.REASON='Move target recovery cleanup pending' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if status != 503 || unknownState(t, f, 7) != "N" {
		t.Fatal(status, body)
	}
	status, _, body = organizationJobRequest(t, f, job.ID, "reconcile-unknown")
	if status != 200 || unknownState(t, f, 7) != "N" {
		t.Fatal("ignored pending move cleanup", status, body)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_move_receipt`); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "resume-move")
	if status != 200 || unknownState(t, f, 7) != "N" {
		t.Fatal("marked before planned attachment", status, body)
	}
	status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
	if status != 200 || done.UnknownState != "resolved" || unknownState(t, f, 7) != "Y" {
		t.Fatal(status, body)
	}
}
