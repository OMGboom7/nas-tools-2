package httpserver

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func strSync(value string) *string { return &value }
func boolSync(value bool) *bool    { return &value }
func syncConfigPreview(t *testing.T, f *subscriptionRunFixture, input syncConfigInput) (int, organization.SyncConfigReview, string) {
	t.Helper()
	raw, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/config/review", f.token, string(raw))
	var result struct{ Data organization.SyncConfigReview }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return w.Code, result.Data, w.Body.String()
}
func syncConfigSave(t *testing.T, f *subscriptionRunFixture, input syncConfigInput, review organization.SyncConfigReview, jobs bool) (int, organization.SyncConfigReceipt, string) {
	t.Helper()
	input.Fingerprint, input.Confirm, input.ConfirmInvalidateJobs = review.Fingerprint, true, jobs
	raw, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/config/apply", f.token, string(raw))
	var result struct {
		Data organization.SyncConfigReceipt
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return w.Code, result.Data, w.Body.String()
}

func TestSyncConfigUpdateKeepsIDNullsAndHistoryAndDoesNotTouchFiles(t *testing.T) {
	f, source, target, _ := directSyncFixture(t, "copy")
	input := syncConfigInput{Action: "update", ID: "7", Patch: syncConfigPatch{Compatibility: boolSync(false)}}
	status, review, body := syncConfigPreview(t, f, input)
	if status != 200 || len(review.After) != 1 || review.After[0].ID != 7 {
		t.Fatal(status, body)
	}
	var value int
	if err := f.db.QueryRow(`SELECT COMPATIBILITY FROM CONFIG_SYNC_PATHS WHERE ID=7`).Scan(&value); err != nil || value != 1 {
		t.Fatal("preview mutated row", value, err)
	}
	status, receipt, body := syncConfigSave(t, f, input, review, false)
	if status != 200 || receipt.ID == "" || receipt.After[0].ID != 7 || receipt.After[0].Mode != "复制" || len(receipt.After[0].NullFields) != 1 || receipt.After[0].NullFields[0] != "note" {
		t.Fatal(status, body)
	}
	var note any
	if err := f.db.QueryRow(`SELECT NOTE FROM CONFIG_SYNC_PATHS WHERE ID=7`).Scan(&note); err != nil || note != nil {
		t.Fatal("lost NULL", note, err)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "misc/readme.txt"), "original-media")
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/sync/config/receipts", f.token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), receipt.ID) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSyncConfigEnableReviewsAndAtomicallyDisablesOverlappingSources(t *testing.T) {
	f, source, _, _ := directSyncFixture(t, "copy")
	child := filepath.Join(source, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "not-created")
	input := syncConfigInput{Action: "create", Patch: syncConfigPatch{Source: &child, Dest: &missing, Enabled: boolSync(true)}}
	status, review, body := syncConfigPreview(t, f, input)
	if status != 200 || len(review.After) != 2 || review.After[0].Enabled != 0 || review.After[1].Enabled != 1 {
		t.Fatal(status, body)
	}
	status, receipt, body := syncConfigSave(t, f, input, review, false)
	if status != 200 || len(receipt.After) != 2 || receipt.After[1].ID < 1 {
		t.Fatal(status, body)
	}
	var oldEnabled, newEnabled int
	if err := f.db.QueryRow(`SELECT ENABLED FROM CONFIG_SYNC_PATHS WHERE ID=7`).Scan(&oldEnabled); err != nil || oldEnabled != 0 {
		t.Fatal(oldEnabled, err)
	}
	if err := f.db.QueryRow(`SELECT ENABLED FROM CONFIG_SYNC_PATHS WHERE ID=?`, receipt.After[1].ID).Scan(&newEnabled); err != nil || newEnabled != 1 {
		t.Fatal(newEnabled, err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("configuration created a directory", err)
	}
	status, _, body = syncConfigSave(t, f, input, review, false)
	if status != 409 {
		t.Fatal("repeated create not rejected", status, body)
	}
}

func TestSyncConfigDeleteAndUndoRestoreExactIDsWithoutFilesOrHistoryDeletion(t *testing.T) {
	f, source, target, _ := directSyncFixture(t, "copy")
	if _, err := f.db.Exec(`CREATE TABLE SYNC_HISTORY(ID INTEGER PRIMARY KEY,PATH TEXT,SRC TEXT,DEST TEXT);INSERT INTO SYNC_HISTORY VALUES(1,?,?,?)`, filepath.Join(source, "old.txt"), source, target); err != nil {
		t.Fatal(err)
	}
	input := syncConfigInput{Action: "delete", ID: "7"}
	status, review, body := syncConfigPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, receipt, body := syncConfigSave(t, f, input, review, false)
	if status != 200 || len(receipt.After) != 0 {
		t.Fatal(status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM SYNC_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	undo := syncConfigInput{Action: "restore", RestoreID: receipt.ID}
	status, restore, body := syncConfigPreview(t, f, undo)
	if status != 200 || restore.After[0].ID != 7 {
		t.Fatal(status, body)
	}
	status, restored, body := syncConfigSave(t, f, undo, restore, false)
	if status != 200 || restored.After[0].ID != 7 || len(restored.After[0].NullFields) != 1 {
		t.Fatal(status, body)
	}
	status, _, body = syncConfigPreview(t, f, undo)
	if status != 409 {
		t.Fatal("undo replay accepted", status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "misc/archive.bin"), "original-media")
}

func TestSyncConfigUndoRejectsLaterEditsAndCanReverseAutoDisable(t *testing.T) {
	f, source, _, _ := directSyncFixture(t, "copy")
	child := filepath.Join(source, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	input := syncConfigInput{Action: "create", Patch: syncConfigPatch{Source: &child, Dest: strSync(t.TempDir()), Enabled: boolSync(true)}}
	status, review, body := syncConfigPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, receipt, body := syncConfigSave(t, f, input, review, false)
	if status != 200 {
		t.Fatal(status, body)
	}
	undo := syncConfigInput{Action: "restore", RestoreID: receipt.ID}
	if _, err := f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET NOTE='later' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	status, _, body = syncConfigPreview(t, f, undo)
	if status != 409 {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET NOTE=NULL WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	status, review, body = syncConfigPreview(t, f, undo)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, receipt, body = syncConfigSave(t, f, undo, review, false)
	if status != 200 || len(receipt.After) != 1 || receipt.After[0].ID != 7 || receipt.After[0].Enabled != 1 {
		t.Fatal(status, body)
	}
}

func TestSyncConfigPendingJobsRequireIndependentInvalidationAndUndoDoesNotRun(t *testing.T) {
	f, source, target, input := directSyncFixture(t, "copy")
	status, plan, body := directSyncPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, job, body := directSyncSave(t, f, input, plan)
	if status != 201 {
		t.Fatal(status, body)
	}
	change := syncConfigInput{Action: "status", ID: "7", Patch: syncConfigPatch{Enabled: boolSync(false)}}
	status, review, body := syncConfigPreview(t, f, change)
	if status != 200 || len(review.Jobs) != 1 || review.Jobs[0].ID != job.ID {
		t.Fatal(status, body)
	}
	status, _, body = syncConfigSave(t, f, change, review, false)
	if status != 409 {
		t.Fatal(status, body)
	}
	status, receipt, body := syncConfigSave(t, f, change, review, true)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if status != 409 {
		t.Fatal(status, body)
	}
	undo := syncConfigInput{Action: "restore", RestoreID: receipt.ID}
	status, review, body = syncConfigPreview(t, f, undo)
	if status != 200 || len(review.Jobs) != 1 {
		t.Fatal(status, body)
	}
	status, _, body = syncConfigSave(t, f, undo, review, true)
	if status != 200 {
		t.Fatal(status, body)
	}
	job = organizationReadMoveJob(t, f, job.ID)
	if job.State != "ready" {
		t.Fatal(job)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatal("config undo executed files", entries, err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "misc/readme.txt"), "original-media")
}

func TestSyncConfigRevalidatesJobsAndRowsAndTransactionRollsBack(t *testing.T) {
	f, source, _, input := directSyncFixture(t, "copy")
	change := syncConfigInput{Action: "update", ID: "7", Patch: syncConfigPatch{Note: strSync("new")}}
	status, review, body := syncConfigPreview(t, f, change)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, plan, body := directSyncPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, _, body = directSyncSave(t, f, input, plan)
	if status != 201 {
		t.Fatal(status, body)
	}
	status, _, body = syncConfigSave(t, f, change, review, true)
	if status != 409 {
		t.Fatal("pending set changed without review", status, body)
	}
	// Test rollback directly after review, including an overlapping disable.
	f2, source2, _, _ := directSyncFixture(t, "copy")
	child := filepath.Join(source2, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	create := syncConfigInput{Action: "create", Patch: syncConfigPatch{Source: &child, Dest: strSync(t.TempDir()), Enabled: boolSync(true)}}
	status, review, body = syncConfigPreview(t, f2, create)
	if status != 200 {
		t.Fatal(status, body)
	}
	if _, err := f2.db.Exec(`CREATE TRIGGER stop_sync_config BEFORE UPDATE ON CONFIG_SYNC_PATHS BEGIN SELECT RAISE(ABORT,'test interruption');END`); err != nil {
		t.Fatal(err)
	}
	status, _, body = syncConfigSave(t, f2, create, review, false)
	if status < 400 {
		t.Fatal(status, body)
	}
	var count, enabled int
	if err := f2.db.QueryRow(`SELECT COUNT(*),MAX(ENABLED) FROM CONFIG_SYNC_PATHS`).Scan(&count, &enabled); err != nil || count != 1 || enabled != 1 {
		t.Fatal(count, enabled, err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "misc/readme.txt"), "original-media")
}

func TestSyncConfigLegacyFormsStatusAndExactLargeIDs(t *testing.T) {
	f, source, _, _ := directSyncFixture(t, "copy")
	if _, err := f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET ID=9223372036854775807 WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	const id = "9223372036854775807"
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/sync/directory/list", f.token, `{}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"`+id+`"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = performFormRequest(f.handler, "/api/v1/sync/directory/info", f.token, url.Values{"sid": {id}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"`+id+`"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = performFormRequest(f.handler, "/api/v1/sync/directory/update", f.token, url.Values{"sid": {id}, "from": {source}, "note": {"edited"}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, flag := range []string{"enable", "rename", "compatibility", "locating"} {
		w = performFormRequest(f.handler, "/api/v1/sync/directory/status", f.token, url.Values{"sid": {id}, "flag": {flag}, "checked": {"0"}})
		if w.Code != 200 {
			t.Fatal(flag, w.Code, w.Body.String())
		}
	}
	var stored string
	if err := f.db.QueryRow(`SELECT NOTE FROM CONFIG_SYNC_PATHS WHERE ID=?`, id).Scan(&stored); err != nil || stored != "edited" {
		t.Fatal(stored, err)
	}
	w = performFormRequest(f.handler, "/api/v1/sync/directory/delete", f.token, url.Values{"sid": {id}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSyncConfigRejectsUnsafeChangesButCanDisableMissingMounts(t *testing.T) {
	f, source, target, _ := directSyncFixture(t, "link")
	for _, input := range []syncConfigInput{
		{Action: "create", Patch: syncConfigPatch{Source: strSync("/not-existing-sync-source")}},
		{Action: "update", ID: "7", Patch: syncConfigPatch{Dest: strSync(filepath.Join(source, "dest"))}},
		{Action: "update", ID: "7", Patch: syncConfigPatch{Source: strSync(target)}},
		{Action: "status", ID: "7", Patch: syncConfigPatch{Enabled: boolSync(true), Rename: boolSync(true)}},
		{Action: "update", ID: "07", Patch: syncConfigPatch{Note: strSync("bad")}},
	} {
		status, _, body := syncConfigPreview(t, f, input)
		if status < 400 {
			t.Fatal(status, body)
		}
	}
	if _, err := f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET SOURCE='/not-mounted-source',DEST='invalid-old-local-destination' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	input := syncConfigInput{Action: "status", ID: "7", Patch: syncConfigPatch{Enabled: boolSync(false)}}
	status, review, body := syncConfigPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, _, body = syncConfigSave(t, f, input, review, false)
	if status != 200 {
		t.Fatal(status, body)
	}
}

func TestSyncConfigUnknownColumnsAndTamperedReceiptsAreNotDestroyedOrAdopted(t *testing.T) {
	f, _, _, _ := directSyncFixture(t, "copy")
	input := syncConfigInput{Action: "update", ID: "7", Patch: syncConfigPatch{Note: strSync("new")}}
	status, review, body := syncConfigPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, receipt, body := syncConfigSave(t, f, input, review, false)
	if status != 200 {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`UPDATE GO_SYNC_CONFIG_CHANGES SET RECEIPT=json_set(RECEIPT,'$.after[0].source','/tampered') WHERE ID=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	status, _, body = syncConfigPreview(t, f, syncConfigInput{Action: "restore", RestoreID: receipt.ID})
	if status != 409 {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`ALTER TABLE CONFIG_SYNC_PATHS ADD COLUMN USER_EXTRA TEXT;UPDATE CONFIG_SYNC_PATHS SET USER_EXTRA='preserve' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	input = syncConfigInput{Action: "delete", ID: "7"}
	status, review, body = syncConfigPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, _, body = syncConfigSave(t, f, input, review, false)
	if status != 409 {
		t.Fatal(status, body)
	}
	var extra string
	if err := f.db.QueryRow(`SELECT USER_EXTRA FROM CONFIG_SYNC_PATHS WHERE ID=7`).Scan(&extra); err != nil || extra != "preserve" {
		t.Fatal(extra, err)
	}
}

func TestSyncConfigWritesRequireAdminExplicitReviewAndPureGo(t *testing.T) {
	f, _, _, _ := directSyncFixture(t, "copy")
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/config/apply", f.token, `{"action":"delete","id":"7"}`)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	for _, path := range []string{"/api/v1/organization/sync/config/review", "/api/v1/organization/sync/config/apply", "/api/v1/sync/directory/delete"} {
		for _, token := range []string{"", viewer} {
			w := nativeJSONRequest(f.handler, "POST", path, token, `{"action":"delete","id":"7"}`)
			want := 403
			if token == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
		}
	}
	for _, raw := range []string{`{"sid":"7","flag":"enable","checked":"falsey"}`, `{"sid":"07","flag":"enable","checked":0}`, `{"sid":"7","flag":"unknown","checked":0}`} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/sync/directory/status", f.token, raw)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: f.path}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	w = nativeJSONRequest(handler, "POST", "/api/v1/sync/directory/delete", f.token, `{"sid":"7"}`)
	if w.Code != 501 {
		t.Fatal(w.Code, w.Body.String())
	}
}
