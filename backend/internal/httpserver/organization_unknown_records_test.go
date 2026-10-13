package httpserver

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func unknownDeleteReview(t *testing.T, f *subscriptionRunFixture, id int64) organization.UnknownDeleteReview {
	t.Helper()
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/unknown/"+strconv.FormatInt(id, 10)+"/delete-review", f.token, "")
	var result struct {
		Data organization.UnknownDeleteReview
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return result.Data
}
func deleteUnknownRequest(t *testing.T, f *subscriptionRunFixture, id int64, review organization.UnknownDeleteReview) (int, organization.UnknownDeleteReceipt, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"confirm": true, "confirmRecordDeletion": true, "reviewFingerprint": review.Fingerprint})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/"+strconv.FormatInt(id, 10)+"/delete", f.token, string(raw))
	var result struct {
		Data organization.UnknownDeleteReceipt
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w.Code, result.Data, w.Body.String()
}
func restoreUnknownRequest(t *testing.T, f *subscriptionRunFixture, id string) (int, organization.UnknownDeleteReceipt, string) {
	t.Helper()
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/deletions/"+id+"/restore", f.token, `{"confirm":true,"confirmRecordRestore":true}`)
	var result struct {
		Data organization.UnknownDeleteReceipt
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w.Code, result.Data, w.Body.String()
}
func TestCompatUnknownListPreservesEnvelopeAllRowsAndOldDisplayWithoutLegacy(t *testing.T) {
	f, source, target, _ := unknownFixture(t, "copy")
	if _, err := f.db.Exec(`INSERT INTO TRANSFER_UNKNOWN VALUES(8,'C:\电影','D:\媒体','Rclone复制','N'),(9,'','',NULL,'N'),(10,'processed','','复制','Y'),(11,'odd','','unknown','N')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<50) INSERT INTO TRANSFER_UNKNOWN SELECT 100+i,'entry-'||i,'','','N' FROM n`); err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/list", f.token, "")
	var result struct {
		Code    int
		Success bool
		Data    struct {
			Items []struct {
				ID                       int64
				Path, To, Name, SyncMode string
				Mode                     any `json:"rmt_mode"`
			}
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Success || result.Code != 0 || len(result.Data.Items) != 53 || result.Data.Items[52].ID != 150 {
		t.Fatal(w.Code, w.Body.String())
	}
	if result.Data.Items[1].Path != "C:/电影" || result.Data.Items[1].To != "D:/媒体" || result.Data.Items[1].Name != "C:/电影" || result.Data.Items[1].Mode != "rclonecopy" || result.Data.Items[2].Mode != nil {
		t.Fatal(w.Body.String())
	}
	for _, token := range []string{"", "forged"} {
		if r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/list", token, ""); r.Code != 401 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
	_ = target
	missing, _, _, _ := organizationFixture(t)
	r := nativeJSONRequest(missing.handler, "POST", "/api/v1/organization/unknown/list", missing.token, "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	var count int
	if err := missing.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_UNKNOWN'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
func TestUnknownRecordDeleteRestorePreservesNullsFilesHistoryAndHugeIDs(t *testing.T) {
	f, source, target, _ := unknownFixture(t, "copy")
	if _, err := f.db.Exec(`INSERT INTO TRANSFER_UNKNOWN VALUES (9223372036854775807,NULL,NULL,NULL,NULL)`); err != nil {
		t.Fatal(err)
	}
	review := unknownDeleteReview(t, f, 9223372036854775807)
	if !review.Eligible || review.Record.Path != nil || review.Record.State != nil {
		t.Fatal(review)
	}
	status, receipt, body := deleteUnknownRequest(t, f, review.Record.ID, review)
	if status != 200 || receipt.Actor == "" || receipt.Review.Record.ID != review.Record.ID || receipt.Restore != nil {
		t.Fatal(status, body)
	}
	status, restored, body := restoreUnknownRequest(t, f, receipt.ID)
	if status != 200 || restored.Restore == nil {
		t.Fatal(status, body)
	}
	var p, d, m, s string
	if err := f.db.QueryRow(`SELECT typeof(PATH),typeof(DEST),typeof(MODE),typeof(STATE) FROM TRANSFER_UNKNOWN WHERE ID=9223372036854775807`).Scan(&p, &d, &m, &s); err != nil || p != "null" || d != "null" || m != "null" || s != "null" {
		t.Fatal(p, d, m, s, err)
	}
	status, again, body := restoreUnknownRequest(t, f, receipt.ID)
	if status != 200 || again.Restore.Created != restored.Restore.Created {
		t.Fatal(status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
	assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.en.srt"), "original-media")
	_ = target
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
func TestUnknownDeleteSnapshotPendingAssociationAndAtomicBulk(t *testing.T) {
	f, source, target, input := unknownFixture(t, "copy")
	review := unknownDeleteReview(t, f, 7)
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET DEST='changed' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	if status, _, body := deleteUnknownRequest(t, f, 7, review); status != 409 {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET DEST=? WHERE ID=7; INSERT INTO TRANSFER_UNKNOWN VALUES(8,'invalid','','','N')`, target); err != nil {
		t.Fatal(err)
	}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	review = unknownDeleteReview(t, f, 7)
	if review.Eligible || review.PendingJobID != job.ID {
		t.Fatal(review)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/delete", f.token, `{"id":[8,7]}`)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	var rows, audits int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_UNKNOWN`).Scan(&rows); err != nil || rows != 2 {
		t.Fatal(rows, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_DELETE`).Scan(&audits); err != nil || audits != 0 {
		t.Fatal(audits, err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
}
func TestUnknownDeleteAndRestoreTransactionsRollBackAndNeverOverwrite(t *testing.T) {
	f, _, _, _ := unknownFixture(t, "copy")
	review := unknownDeleteReview(t, f, 7)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_delete BEFORE DELETE ON TRANSFER_UNKNOWN BEGIN SELECT RAISE(ABORT,'receipt test'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body := deleteUnknownRequest(t, f, 7, review)
	if status != 503 || unknownState(t, f, 7) != "N" {
		t.Fatal(status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_DELETE`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_delete`); err != nil {
		t.Fatal(err)
	}
	status, receipt, body := deleteUnknownRequest(t, f, 7, review)
	if status != 200 {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`INSERT INTO TRANSFER_UNKNOWN VALUES(7,'new','new','new','N')`); err != nil {
		t.Fatal(err)
	}
	status, _, body = restoreUnknownRequest(t, f, receipt.ID)
	if status != 409 {
		t.Fatal(status, body)
	}
	var path string
	if err := f.db.QueryRow(`SELECT PATH FROM TRANSFER_UNKNOWN WHERE ID=7`).Scan(&path); err != nil || path != "new" {
		t.Fatal(path, err)
	}
	if _, err := f.db.Exec(`DELETE FROM TRANSFER_UNKNOWN WHERE ID=7; CREATE TRIGGER fail_restore BEFORE INSERT ON TRANSFER_UNKNOWN BEGIN SELECT RAISE(ABORT,'restore test');END`); err != nil {
		t.Fatal(err)
	}
	status, _, body = restoreUnknownRequest(t, f, receipt.ID)
	if status != 503 {
		t.Fatal(status, body)
	}
	var raw string
	if err := f.db.QueryRow(`SELECT RESTORE FROM GO_ORGANIZATION_UNKNOWN_DELETE WHERE ID=?`, receipt.ID).Scan(&raw); err != nil || raw != "" {
		t.Fatal(raw, err)
	}
}
func TestUnknownLegacyDeletionFormAndStrictPermissions(t *testing.T) {
	f, _, _, _ := unknownFixture(t, "copy")
	for _, body := range []string{`{"id":[7,7]}`, `{"id":7,"deleteFiles":true}`, `{"id":"07"}`} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/delete", f.token, body)
		if w.Code != 400 && w.Code != 409 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	for _, endpoint := range []string{"/api/v1/organization/unknown/list", "/api/v1/organization/unknown/delete"} {
		w := nativeJSONRequest(f.handler, "POST", endpoint, viewer, `{"id":7}`)
		if w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := performFormRequest(f.handler, "/api/v1/organization/unknown/delete", f.token, url.Values{"id": {"7"}})
	var result struct {
		Code    int
		Success bool
		Data    struct {
			Receipts []organization.UnknownDeleteReceipt
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Success || len(result.Data.Receipts) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestUnknownDeleteConcurrentCallsCommitOneAuditAndPreserveOtherTables(t *testing.T) {
	f, _, _, _ := unknownFixture(t, "copy")
	if _, err := f.db.Exec(`CREATE TABLE TRANSFER_HISTORY(ID INTEGER PRIMARY KEY,NOTE TEXT);INSERT INTO TRANSFER_HISTORY VALUES(100,'unchanged');INSERT INTO GO_ORGANIZATION_TARGETS VALUES('reserved','historical-job')`); err != nil {
		t.Fatal(err)
	}
	review := unknownDeleteReview(t, f, 7)
	var group sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() { defer group.Done(); status, _, _ := deleteUnknownRequest(t, f, 7, review); statuses <- status }()
	}
	group.Wait()
	close(statuses)
	success := 0
	for status := range statuses {
		if status == 200 {
			success++
		} else if status != 404 {
			t.Fatal(status)
		}
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_DELETE`).Scan(&count); err != nil || count != 1 || success != 1 {
		t.Fatal(count, success, err)
	}
	var note, owner string
	if err := f.db.QueryRow(`SELECT NOTE FROM TRANSFER_HISTORY WHERE ID=100`).Scan(&note); err != nil || note != "unchanged" {
		t.Fatal(note, err)
	}
	if err := f.db.QueryRow(`SELECT JOB_ID FROM GO_ORGANIZATION_TARGETS WHERE TARGET_KEY='reserved'`).Scan(&owner); err != nil || owner != "historical-job" {
		t.Fatal(owner, err)
	}
}

func TestUnknownDeleteNullSnapshotAndAuditCorruptionAreNotAdopted(t *testing.T) {
	f, _, _, _ := unknownFixture(t, "copy")
	if _, err := f.db.Exec(`INSERT INTO TRANSFER_UNKNOWN VALUES(9,NULL,NULL,NULL,'N')`); err != nil {
		t.Fatal(err)
	}
	review := unknownDeleteReview(t, f, 9)
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET PATH='' WHERE ID=9`); err != nil {
		t.Fatal(err)
	}
	if status, _, body := deleteUnknownRequest(t, f, 9, review); status != 409 {
		t.Fatal(status, body)
	}
	review = unknownDeleteReview(t, f, 9)
	status, receipt, body := deleteUnknownRequest(t, f, 9, review)
	if status != 200 {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_UNKNOWN_DELETE SET RECEIPT=json_set(RECEIPT,'$.review.record.path','tampered') WHERE ID=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	status, _, body = restoreUnknownRequest(t, f, receipt.ID)
	if status != 409 {
		t.Fatal("adopted corrupt audit", status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_UNKNOWN WHERE ID=9`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestUnknownDeleteExplicitConfirmationAndHybridWritesAreRejected(t *testing.T) {
	f, _, _, _ := unknownFixture(t, "copy")
	review := unknownDeleteReview(t, f, 7)
	for _, body := range []string{`{}`, `{"confirm":true}`, `{"confirm":true,"confirmRecordDeletion":true,"reviewFingerprint":"` + review.Fingerprint + `","actor":"forged"}`, `{"confirm":true,"confirmRecordDeletion":true,"reviewFingerprint":"` + review.Fingerprint + `"} {}`} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/unknown/7/delete", f.token, body)
		if w.Code != 400 || unknownState(t, f, 7) != "N" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	hybrid, err := newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: false}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	w := performFormRequest(hybrid, "/api/v1/organization/unknown/delete", f.token, url.Values{"id": {"7"}})
	if w.Code != 501 || unknownState(t, f, 7) != "N" {
		t.Fatal(w.Code, w.Body.String())
	}
}
