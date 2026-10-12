package httpserver

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func unknownReview(t *testing.T, f *subscriptionRunFixture, id string) organization.UnknownReview {
	t.Helper()
	r := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/jobs/"+id+"/unknown-status", f.token, "")
	var payload struct{ Data organization.UnknownReview }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &payload) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return payload.Data
}
func detachUnknownRequest(t *testing.T, f *subscriptionRunFixture, id, fingerprint, reason string, confirm bool) (int, organization.Job, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"confirm": true, "confirmDetachUnknown": confirm, "reviewFingerprint": fingerprint, "reason": reason})
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+id+"/detach-unknown", f.token, string(raw))
	var payload struct{ Data organization.Job }
	if json.Unmarshal(r.Body.Bytes(), &payload) != nil {
		t.Fatal(r.Body.String())
	}
	return r.Code, payload.Data, r.Body.String()
}
func completedPendingUnknown(t *testing.T) (*subscriptionRunFixture, string, string, organization.Job) {
	t.Helper()
	f, source, target, input := unknownFixture(t, "copy")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_unknown BEFORE UPDATE OF STATE ON TRANSFER_UNKNOWN WHEN NEW.STATE='Y' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationJobRequest(t, f, job.ID, "execute")
	if status != 503 {
		t.Fatal(status, body)
	}
	return f, source, target, organizationReadMoveJob(t, f, job.ID)
}

func TestUnknownDetachPersistsAuditWithoutChangingLegacyDataHistoryOrFiles(t *testing.T) {
	f, source, target, job := completedPendingUnknown(t)
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET DEST='/changed' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	organizationFile(t, source, "new-arrival.mkv")
	review := unknownReview(t, f, job.ID)
	if !review.Eligible || review.Current == nil || review.Current.Dest != "/changed" || review.Original.Dest != target {
		t.Fatal(review)
	}
	status, _, body := detachUnknownRequest(t, f, job.ID, review.Fingerprint, "管理员核对：原任务成功，保留新记录继续处理", false)
	if status != 400 {
		t.Fatal("missing separate confirmation accepted", status, body)
	}
	status, done, body := detachUnknownRequest(t, f, job.ID, review.Fingerprint, "管理员核对：原任务成功，保留新记录继续处理", true)
	if status != 200 || done.State != "completed" || done.UnknownState != "detached" || done.UnknownDetach == nil || done.UnknownDetach.Actor == "" || done.UnknownDetach.Review.Current.Dest != "/changed" {
		t.Fatal(status, body)
	}
	if unknownState(t, f, 7) != "N" {
		t.Fatal("marked detached record successful")
	}
	for _, item := range done.Items {
		assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
		assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "new-arrival.mkv"), "original-media")
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, job.ID).Scan(&count); err != nil || count != 2 {
		t.Fatal("released successful media targets", count, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE RECORD_ID=7 AND STATE='pending'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	status, repeated, body := detachUnknownRequest(t, f, job.ID, review.Fingerprint, "管理员核对：原任务成功，保留新记录继续处理", true)
	if status != 200 || repeated.UnknownDetach.Created != done.UnknownDetach.Created {
		t.Fatal("replaced audit receipt", status, body)
	}
	if item := unknownList(t, f, "")[0]; item.JobID != "" || item.State != "N" {
		t.Fatal(item)
	}
}

func TestUnknownDetachRejectsStaleReviewAndRollsBackBothReservationAndAudit(t *testing.T) {
	f, _, _, job := completedPendingUnknown(t)
	review := unknownReview(t, f, job.ID)
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET MODE='移动' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	status, _, body := detachUnknownRequest(t, f, job.ID, review.Fingerprint, "旧记录已变更", true)
	if status != 409 {
		t.Fatal(status, body)
	}
	review = unknownReview(t, f, job.ID)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_detach BEFORE UPDATE OF STATE ON GO_ORGANIZATION_UNKNOWN_LINKS WHEN NEW.STATE='detached' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body = detachUnknownRequest(t, f, job.ID, review.Fingerprint, "旧记录已变更", true)
	if status != 503 {
		t.Fatal(status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_DETACH WHERE JOB_ID=?`, job.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("audit committed before reservation", count, err)
	}
	if current := organizationReadMoveJob(t, f, job.ID); current.UnknownState != "pending" {
		t.Fatal(current)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_detach`); err != nil {
		t.Fatal(err)
	}
	status, done, body := detachUnknownRequest(t, f, job.ID, review.Fingerprint, "旧记录已变更", true)
	if status != 200 || done.UnknownState != "detached" || unknownState(t, f, 7) != "N" {
		t.Fatal(status, body)
	}
}

func TestUnknownDetachCannotReleasePlannedWorkAndHandlesReviewedMissingRecord(t *testing.T) {
	f, _, _, input := unknownFixture(t, "move")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	review := unknownReview(t, f, job.ID)
	if review.Eligible {
		t.Fatal("unfinished work eligible")
	}
	status, _, body = detachUnknownRequest(t, f, job.ID, review.Fingerprint, "未完成不应解除", true)
	if status != 409 {
		t.Fatal(status, body)
	}
	g, _, _, done := completedPendingUnknown(t)
	if _, err := g.db.Exec(`DELETE FROM TRANSFER_UNKNOWN WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	review = unknownReview(t, g, done.ID)
	if review.Current != nil || !review.Eligible {
		t.Fatal(review)
	}
	status, detached, body := detachUnknownRequest(t, g, done.ID, review.Fingerprint, "旧记录由管理员移除，保留本次成功历史", true)
	if status != 200 || detached.UnknownState != "detached" || detached.UnknownDetach.Review.Current != nil {
		t.Fatal(status, body)
	}
	var count int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_UNKNOWN`).Scan(&count); err != nil || count != 0 {
		t.Fatal("recreated missing row", count, err)
	}
}

func TestUnknownDetachRespectsExecutorGuardAndRejectsPendingMoveCleanup(t *testing.T) {
	f, _, _, job := completedPendingUnknown(t)
	review := unknownReview(t, f, job.ID)
	s, err := organization.OpenStore(filepath.Join(filepath.Dir(f.path), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	guard, err := s.AcquireJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	status, _, body := detachUnknownRequest(t, f, job.ID, review.Fingerprint, "完成后解除", true)
	if status != 409 {
		t.Fatal("ignored active peer", status, body)
	}
	guard.Close()
	if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET REASON=? WHERE JOB_ID=? AND ORDINAL=0`, organization.MoveSourceCleanupPending, job.ID); err != nil {
		t.Fatal(err)
	}
	review = unknownReview(t, f, job.ID)
	if review.Eligible {
		t.Fatal("pending cleanup eligible")
	}
	status, _, body = detachUnknownRequest(t, f, job.ID, review.Fingerprint, "不能跳过清理", true)
	if status != 409 {
		t.Fatal(status, body)
	}
}
