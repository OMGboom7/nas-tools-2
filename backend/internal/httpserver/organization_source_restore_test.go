package httpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func sourceRestoreDenyHistory(t *testing.T, f *subscriptionRunFixture) {
	t.Helper()
	if _, err := f.db.Exec(`CREATE TABLE IF NOT EXISTS TRANSFER_HISTORY (ID INTEGER PRIMARY KEY, MODE TEXT, TYPE TEXT, CATEGORY TEXT, TMDBID INTEGER, TITLE TEXT, YEAR TEXT, SEASON_EPISODE TEXT, SOURCE TEXT, SOURCE_PATH TEXT, SOURCE_FILENAME TEXT, DEST TEXT, DEST_PATH TEXT, DEST_FILENAME TEXT, DATE TEXT); CREATE TRIGGER deny_restore_history BEFORE INSERT ON TRANSFER_HISTORY BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
}
func sourceRestoreHTTPReview(t *testing.T, f *subscriptionRunFixture, id string) organization.SourceRestoreReview {
	t.Helper()
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/jobs/"+id+"/source-restore", f.token, "")
	var p struct {
		Data organization.SourceRestoreReview
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return p.Data
}
func sourceRestoreHTTPBody(t *testing.T, r organization.SourceRestoreReview) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"confirm": true, "confirmRestoreSource": true, "confirmOldExecutorsStopped": true, "reviewFingerprint": r.Fingerprint, "reason": "restore original source"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func sourceRestoreInterruptedFixture(t *testing.T, disc bool) (*subscriptionRunFixture, string, string, organization.Job) {
	t.Helper()
	f, source, target, input, plan := organizationLinkFixture(t, "move")
	if disc {
		// Use an independent configured source containing only the full disc.
		f, source, target, input = organizationFixture(t)
		input.Mode = "move"
		input.Path = "Movie.2026.BLURAY"
		organizationDiscTree(t, source, input.Path)
		status, p, body := organizationPreview(t, f, input)
		if status != 200 {
			t.Fatal(status, body)
		}
		plan = p
	}
	j := organizationCreateJob(t, f, input, plan)
	sourceRestoreDenyHistory(t, f)
	status, _, body := organizationMoveRequest(t, f, j.ID, "execute")
	if status != 503 {
		t.Fatal(status, body)
	}
	j = organizationReadMoveJob(t, f, j.ID)
	if j.State != "needs_review" || j.Items[0].State != "quarantined" {
		t.Fatal(j)
	}
	return f, source, target, j
}
func TestOrganizationSourceRestoreWholeTaskIsIndependentFromMoveAndRetainsTargets(t *testing.T) {
	for _, disc := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary-with-subtitle", true: "BluRay"}[disc], func(t *testing.T) {
			f, source, target, j := sourceRestoreInterruptedFixture(t, disc)
			r := sourceRestoreHTTPReview(t, f, j.ID)
			if !r.CanRestore || len(r.Items) != len(j.Items) {
				t.Fatal(r)
			}
			w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/restore-source", f.token, sourceRestoreHTTPBody(t, r))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			j = organizationReadMoveJob(t, f, j.ID)
			if j.State != organization.SourceRestored || j.SourceRestore == nil || j.SourceRestore.Actor != "admin" {
				t.Fatal(j)
			}
			for _, item := range j.Items {
				if item.State == organization.SourceRestored {
					if disc {
						for _, name := range []string{"BDMV/index.bdmv", "BDMV/STREAM/00000.m2ts", "CERTIFICATE/id.bdmv"} {
							assertOrganizationMoveBytes(t, filepath.Join(source, item.Source, name), "original-media")
							assertOrganizationMoveBytes(t, filepath.Join(target, item.Target, name), "original-media")
						}
					} else {
						assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
						assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
					}
				} else if item.State != "source_restore_stopped" {
					t.Fatal(item)
				}
			}
			var n int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&n); err != nil || n != 0 {
				t.Fatal("restoration wrote successful history", n, err)
			}
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, j.ID).Scan(&n); err != nil || n != len(j.Items) {
				t.Fatal("retained targets lost claims", n, err)
			}
			if strings.Contains(w.Body.String(), "sourceHoldIdentity") || strings.Contains(w.Body.String(), "InitialReview") || strings.Contains(w.Body.String(), ".hold") {
				t.Fatal("private recovery proof exposed", w.Body.String())
			}
			for _, action := range []string{"execute", "resume-move", "resume-publication", "reconcile", "cancel", "abandon-unpublished"} {
				body := `{"confirm":true,"confirmSourceRemoval":true,"confirmDiscardStaging":true,"confirmOldExecutorsStopped":true}`
				w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/"+action, f.token, body)
				if w.Code != 409 {
					t.Fatal("restored task reopened", action, w.Code, w.Body.String())
				}
			}
			monitorRebuild(t, f)
			w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/restore-source", f.token, sourceRestoreHTTPBody(t, r))
			if w.Code != 200 {
				t.Fatal("lost final response not observable", w.Code, w.Body.String())
			}
		})
	}
}
func TestOrganizationSourceRestoreMissingConsentStaleScopeRevocationAndBusyGuard(t *testing.T) {
	f, source, _, j := sourceRestoreInterruptedFixture(t, false)
	r := sourceRestoreHTTPReview(t, f, j.ID)
	endpoint := "/api/v1/organization/jobs/" + j.ID + "/restore-source"
	for _, body := range []string{`{"confirm":true,"confirmSourceRemoval":true}`, `{"confirm":true,"confirmRestoreSource":true}`, `{"confirm":true,"confirmRestoreSource":true,"confirmOldExecutorsStopped":true}`, `{"confirm":false,"confirmRestoreSource":true,"confirmOldExecutorsStopped":true}`} {
		w := nativeJSONRequest(f.handler, "POST", endpoint, f.token, body)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	guard, err := f.runner.organizer.jobs.AcquireJob(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(f.handler, "POST", endpoint, f.token, sourceRestoreHTTPBody(t, r))
	guard.Close()
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if fresh := sourceRestoreHTTPReview(t, f, j.ID); fresh.Fingerprint != r.Fingerprint {
		t.Fatal("readonly review or rejected requests changed authority", fresh)
	}
	organizationFile(t, source, j.Items[0].Source)
	w = nativeJSONRequest(f.handler, "POST", endpoint, f.token, sourceRestoreHTTPBody(t, r))
	if w.Code != 409 {
		t.Fatal("new arrival overwritten", w.Code, w.Body.String())
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_SOURCE_RESTORE`).Scan(&n); err != nil || n != 0 {
		t.Fatal("unsafe intent saved", n, err)
	}
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET ENABLED=0`); err != nil {
		t.Fatal(err)
	}
	w = nativeJSONRequest(f.handler, "GET", "/api/v1/organization/jobs/"+j.ID+"/source-restore", f.token, "")
	if w.Code != 409 {
		t.Fatal("removed source still authorized", w.Code, w.Body.String())
	}
}
func TestOrganizationSourceRestoreAllowsChangedNamingAndUnavailableTargetWithoutChangingThem(t *testing.T) {
	f, source, target, j := sourceRestoreInterruptedFixture(t, false)
	if err := os.Rename(target, target+".offline"); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_name_format": "new/{title}", "media.tv_path": target + ".new"}); err != nil {
		t.Fatal(err)
	}
	r := sourceRestoreHTTPReview(t, f, j.ID)
	if !r.CanRestore {
		t.Fatal(r)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/restore-source", f.token, sourceRestoreHTTPBody(t, r))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, j.Items[0].Source), "original-media")
	assertOrganizationMoveBytes(t, filepath.Join(target+".offline", j.Items[0].Target), "original-media")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("recreated offline target", err)
	}
}
func TestOrganizationSourceRestoreRetainsEarlierCompletedHistoryAndStopsRestOfGroup(t *testing.T) {
	f, source, target, input, plan := organizationLinkFixture(t, "move")
	j := organizationCreateJob(t, f, input, plan)
	if len(j.Items) != 2 {
		t.Fatal(j)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_second_completion BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.ORDINAL=1 AND NEW.STATE='completed' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body := organizationMoveRequest(t, f, j.ID, "execute")
	if status != 503 {
		t.Fatal(status, body)
	}
	j = organizationReadMoveJob(t, f, j.ID)
	if j.Items[0].State != "completed" || j.Items[0].Reason != "" || j.Items[1].State != "quarantined" {
		t.Fatal(j)
	}
	r := sourceRestoreHTTPReview(t, f, j.ID)
	if !r.CanRestore {
		t.Fatal(r)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/restore-source", f.token, sourceRestoreHTTPBody(t, r))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	j = organizationReadMoveJob(t, f, j.ID)
	if j.Items[0].State != "completed" || j.Items[1].State != organization.SourceRestored {
		t.Fatal(j)
	}
	if _, err := os.Stat(filepath.Join(source, j.Items[0].Source)); !os.IsNotExist(err) {
		t.Fatal("reversed an already successful move", err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, j.Items[1].Source), "original-media")
	for _, item := range j.Items {
		assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
func TestOrganizationSourceRestoreLostReceiptRequiresFreshExplicitReviewAndNoRestartReplay(t *testing.T) {
	f, source, target, j := sourceRestoreInterruptedFixture(t, false)
	r := sourceRestoreHTTPReview(t, f, j.ID)
	if _, err := f.db.Exec(`CREATE TRIGGER lose_source_restore BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='source_restored' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/v1/organization/jobs/" + j.ID + "/restore-source"
	w := nativeJSONRequest(f.handler, "POST", endpoint, f.token, sourceRestoreHTTPBody(t, r))
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	j = organizationReadMoveJob(t, f, j.ID)
	if j.State != organization.SourceRestoring || j.SourceRestore == nil {
		t.Fatal(j)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, j.Items[0].Source), "original-media")
	if _, err := f.db.Exec(`DROP TRIGGER lose_source_restore; DROP TRIGGER deny_restore_history`); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, f)
	if err := f.runner.organizer.syncMonitorTick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	j = organizationReadMoveJob(t, f, j.ID)
	if j.State != organization.SourceRestoring {
		t.Fatal("restarted intent replayed", j)
	}
	w = nativeJSONRequest(f.handler, "POST", endpoint, f.token, sourceRestoreHTTPBody(t, r))
	if w.Code != 409 {
		t.Fatal("stale intent replayed", w.Code, w.Body.String())
	}
	fresh := sourceRestoreHTTPReview(t, f, j.ID)
	if !fresh.CanRestore || fresh.Fingerprint == r.Fingerprint {
		t.Fatal(fresh)
	}
	w = nativeJSONRequest(f.handler, "POST", endpoint, f.token, sourceRestoreHTTPBody(t, fresh))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	assertOrganizationMoveBytes(t, filepath.Join(target, j.Items[0].Target), "original-media")
}
func TestOrganizationSourceRestoreConcurrentRequestsHaveOneImmutableReceipt(t *testing.T) {
	f, source, _, j := sourceRestoreInterruptedFixture(t, false)
	r := sourceRestoreHTTPReview(t, f, j.ID)
	body := sourceRestoreHTTPBody(t, r)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/restore-source", f.token, body)
			if w.Code != 200 && w.Code != 409 {
				t.Error(w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	j = organizationReadMoveJob(t, f, j.ID)
	if j.State != organization.SourceRestored {
		t.Fatal(j)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, j.Items[0].Source), "original-media")
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_SOURCE_RESTORE`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestOrganizationSourceRestoreUnknownAssociationKeepsLegacyRowUnprocessed(t *testing.T) {
	f, source, _, input := unknownFixture(t, "move")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	j := organizationCreateJob(t, f, input, plan)
	sourceRestoreDenyHistory(t, f)
	status, _, body = organizationMoveRequest(t, f, j.ID, "execute")
	if status != 503 {
		t.Fatal(status, body)
	}
	r := sourceRestoreHTTPReview(t, f, j.ID)
	if !r.CanRestore {
		t.Fatal(r)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/restore-source", f.token, sourceRestoreHTTPBody(t, r))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	j = organizationReadMoveJob(t, f, j.ID)
	if j.UnknownState != "cancelled" || unknownState(t, f, 7) != "N" {
		t.Fatal("restoration declared unknown record processed", j)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, j.Items[0].Source), "original-media")
}
func TestOrganizationSourceRestoreDownloadBatchNeverWritesRemoteResult(t *testing.T) {
	for _, kind := range []string{"qbittorrent", "transmission", "aria2"} {
		t.Run(kind, func(t *testing.T) {
			f, source, _, wire := downloadBatchFixture(t, kind, "move", false)
			b := saveDownloadFixtureBatch(t, f, kind)
			sourceRestoreDenyHistory(t, f)
			status, _, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true, ConfirmSourceRemoval: true, ConfirmTaskStopped: true})
			if status != 503 {
				t.Fatal(status, body)
			}
			if len(b.Jobs) != 1 {
				t.Fatal(b)
			}
			j := organizationReadMoveJob(t, f, b.Jobs[0])
			r := sourceRestoreHTTPReview(t, f, j.ID)
			if !r.CanRestore {
				t.Fatal(j, r)
			}
			w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/restore-source", f.token, sourceRestoreHTTPBody(t, r))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, j.Items[0].Source), "original-media")
			status, _, body = downloadResultHTTP(t, f, b)
			if status == 200 {
				t.Fatal("restoration authorized successful downloader writeback", body)
			}
			if wire.markCalls != 0 || wire.removeCalls != 0 {
				t.Fatal("restoration changed downloader labels/task", wire)
			}
		})
	}
}
func TestOrganizationSourceRestoreDirectSyncRetainsOriginalAndDoesNotWriteSyncHistory(t *testing.T) {
	f, source, _, input := directSyncFixture(t, "move")
	monitorRebuild(t, f)
	status, plan, body := directSyncPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, j, body := directSyncSave(t, f, input, plan)
	if status != 201 {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`CREATE TABLE SYNC_HISTORY(ID INTEGER PRIMARY KEY,PATH TEXT,SRC TEXT,DEST TEXT);CREATE TRIGGER deny_sync_restore BEFORE INSERT ON SYNC_HISTORY BEGIN SELECT RAISE(ABORT,'fixture');END`); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationMoveRequest(t, f, j.ID, "execute")
	if status != 503 {
		t.Fatal(status, body)
	}
	r := sourceRestoreHTTPReview(t, f, j.ID)
	if !r.CanRestore {
		t.Fatal(r)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/restore-source", f.token, sourceRestoreHTTPBody(t, r))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	j = organizationReadMoveJob(t, f, j.ID)
	assertOrganizationMoveBytes(t, filepath.Join(source, j.Items[0].Source), "original-media")
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM SYNC_HISTORY`).Scan(&n); err != nil || n != 0 {
		t.Fatal("restoration invented sync success", n, err)
	}
}
