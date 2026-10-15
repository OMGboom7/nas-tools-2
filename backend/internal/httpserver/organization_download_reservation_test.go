package httpserver

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func interruptedDownloadReservation(t *testing.T, kind, mode string, modern bool, saved int) (*subscriptionRunFixture, string, string, *downloadBatchWire, organization.DownloadBatch, []organization.Definition) {
	t.Helper()
	f, source, target, wire := downloadBatchFixture(t, kind, mode, modern)
	if err := f.runner.organizer.service.configStore.Update(map[string]any{"media.tv_path": t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Show.S01E01.mkv", "Show.S01E01.en.srt"} {
		organizationFile(t, source, name)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
			t.Fatal(err)
		}
		wire.files = append(wire.files, name)
	}
	taskID := completionHash
	if kind == "aria2" {
		taskID = completionGID
	}
	api := f.runner.organizer
	review, failure := api.planDownload(context.Background(), downloadCompletionInput{DownloaderID: "1", TaskID: taskID}, "admin")
	if failure != nil {
		t.Fatal(failure)
	}
	definitions := []organization.Definition{}
	for _, p := range review.Plans {
		definitions = append(definitions, p.definition)
	}
	if len(definitions) != 2 {
		t.Fatal("two media roots required", len(definitions))
	}
	b, defs, err := api.jobs.BeginDownloadBatch(context.Background(), review.Fingerprint, review.origin, definitions, review.Omitted)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < saved; i++ {
		if _, err := api.jobs.Create(context.Background(), organization.Digest(defs[i]), defs[i]); err != nil {
			t.Fatal(err)
		}
	}
	b, err = api.jobs.GetDownloadBatch(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, f)
	return f, source, target, wire, b, defs
}

func downloadReservationReviewHTTP(t *testing.T, f *subscriptionRunFixture, b organization.DownloadBatch, reason string) (int, downloadReservationReview, string) {
	t.Helper()
	raw, _ := json.Marshal(downloadBatchRequest{Reason: reason})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/batches/"+b.ID+"/reservation-review", f.token, string(raw))
	var result struct {
		Data downloadReservationReview `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w.Code, result.Data, w.Body.String()
}
func reservationConsent(review downloadReservationReview) downloadBatchRequest {
	return downloadBatchRequest{Confirm: true, ConfirmOldExecutorsStopped: true, ConfirmOmitted: true, Reason: review.Reason, Fingerprint: review.Fingerprint}
}

func TestDownloadReservationRecoveryAllClientsModesPreservesOriginalJobsThenExecutesSeparately(t *testing.T) {
	for _, spec := range []struct {
		kind   string
		modern bool
	}{{"qbittorrent", false}, {"transmission", false}, {"transmission", true}, {"aria2", false}} {
		for _, mode := range []string{"copy", "link", "softlink", "move"} {
			t.Run(spec.kind+"/"+mode+map[bool]string{true: "/modern"}[spec.modern], func(t *testing.T) {
				f, source, _, wire, b, _ := interruptedDownloadReservation(t, spec.kind, mode, spec.modern, 1)
				status, review, body := downloadReservationReviewHTTP(t, f, b, "recover after restart")
				if status != 200 || len(review.Missing) != 1 || len(review.Jobs) != 1 || review.Jobs[0] != b.Jobs[0] || review.Actor != "admin" {
					t.Fatal(status, body)
				}
				status, recovered, body := downloadBatchActionHTTP(t, f, b, "recover-reservation", reservationConsent(review))
				if status != 200 || recovered.State != "ready" || len(recovered.Jobs) != 2 {
					t.Fatal(status, body)
				}
				var actor, reason string
				if err := f.db.QueryRow(`SELECT ACTOR,REASON FROM GO_DOWNLOAD_RESERVATION_RECOVERY WHERE BATCH_ID=?`, b.ID).Scan(&actor, &reason); err != nil || actor != "admin" || reason != "recover after restart" {
					t.Fatal(actor, reason, err)
				}
				var count int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&count); err != nil || count != 0 {
					t.Fatal("false history", count, err)
				}
				for _, name := range wire.files {
					assertOrganizationMoveBytes(t, filepath.Join(source, name), "original-media")
				}
				if wire.markCalls+wire.removeCalls != 0 {
					t.Fatal("recovery mutated downloader")
				}
				status, _, body = downloadBatchActionHTTP(t, f, b, "recover-reservation", reservationConsent(review))
				if status != 409 {
					t.Fatal("lost response replay", status, body)
				}
				status, recovered, body = downloadBatchActionHTTP(t, f, recovered, "execute", downloadBatchRequest{Confirm: true, ConfirmTaskStopped: true, ConfirmSourceRemoval: true})
				if status != 200 || recovered.State != "files_done" {
					t.Fatal(status, body)
				}
				status, result, body := downloadResultHTTP(t, f, recovered)
				if status != 200 {
					t.Fatal(status, body)
				}
				status, recovered, body = downloadBatchActionHTTP(t, f, recovered, "finalize", downloadBatchRequest{Confirm: true, Fingerprint: result.Fingerprint, ConfirmTaskStopped: true, ConfirmTaskRemoval: true, ConfirmExclusiveLabels: true})
				if status != 200 || recovered.State != "completed" || wire.markCalls+wire.removeCalls != 1 {
					t.Fatal(status, body)
				}
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE SOURCE='下载完成整理'`).Scan(&count); err != nil || count != 2 {
					t.Fatal(count, err)
				}
			})
		}
	}
}

func TestDownloadReservationRecoveryEmptyAndFullUncommittedAndNeedsReview(t *testing.T) {
	for _, saved := range []int{0, 2} {
		t.Run(string(rune('0'+saved)), func(t *testing.T) {
			f, _, _, _, b, _ := interruptedDownloadReservation(t, "qbittorrent", "copy", false, saved)
			if err := f.runner.organizer.jobs.SetDownloadBatch(t.Context(), b.ID, "reserving", "needs_review", "interrupted", ""); err != nil {
				t.Fatal(err)
			}
			status, r, body := downloadReservationReviewHTTP(t, f, b, "complete original reservation")
			if status != 200 || len(r.Missing) != 2-saved {
				t.Fatal(status, body)
			}
			status, next, body := downloadBatchActionHTTP(t, f, b, "recover-reservation", reservationConsent(r))
			if status != 200 || next.State != "ready" || len(next.Jobs) != 2 {
				t.Fatal(status, body)
			}
		})
	}
}

func TestDownloadReservationRecoveryRequiresFreshIndependentConsent(t *testing.T) {
	f, _, _, _, b, _ := interruptedDownloadReservation(t, "qbittorrent", "copy", false, 1)
	status, r, body := downloadReservationReviewHTTP(t, f, b, "reason")
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, change := range []func(*downloadBatchRequest){func(v *downloadBatchRequest) { v.Confirm = false }, func(v *downloadBatchRequest) { v.ConfirmOldExecutorsStopped = false }, func(v *downloadBatchRequest) { v.Fingerprint = strings.Repeat("a", 64) }, func(v *downloadBatchRequest) { v.Reason = "changed reason" }} {
		input := reservationConsent(r)
		change(&input)
		status, _, body = downloadBatchActionHTTP(t, f, b, "recover-reservation", input)
		if status != 400 && status != 409 {
			t.Fatal(status, body)
		}
	}
	current, err := f.runner.organizer.jobs.GetDownloadBatch(t.Context(), b.ID)
	if err != nil || len(current.Jobs) != 1 || current.State != "reserving" {
		t.Fatal(current, err)
	}
}

func TestDownloadReservationRecoveryRejectsStartedCancelledOrUncertainRows(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running'`,
		`UPDATE GO_ORGANIZATION_ITEMS SET STATE='prepared'`,
		`UPDATE GO_ORGANIZATION_ITEMS SET STATE='completed'`,
		`UPDATE GO_ORGANIZATION_ITEMS SET PROOF='{"temp":"unknown"}'`,
		`UPDATE GO_ORGANIZATION_ITEMS SET REASON='unknown'`,
		`DELETE FROM GO_ORGANIZATION_ITEMS WHERE ORDINAL=0`,
		`UPDATE GO_ORGANIZATION_JOBS SET STATE='cancelled'`,
		`DELETE FROM GO_ORGANIZATION_TARGETS`,
		`UPDATE GO_DOWNLOAD_ORGANIZATION_BATCHES SET STATE='result_pending'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			f, _, _, wire, b, _ := interruptedDownloadReservation(t, "qbittorrent", "copy", false, 1)
			if _, err := f.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			status, _, body := downloadReservationReviewHTTP(t, f, b, "review")
			if status != 409 {
				t.Fatal(status, body)
			}
			if wire.markCalls+wire.removeCalls != 0 {
				t.Fatal("unexpected mutation")
			}
		})
	}
}

func TestDownloadReservationRecoveryRejectsChangedEvidenceAndTarget(t *testing.T) {
	for _, change := range []string{"source", "config", "target", "task", "labels", "claim", "yaml", "root"} {
		t.Run(change, func(t *testing.T) {
			f, source, _, wire, b, defs := interruptedDownloadReservation(t, "qbittorrent", "copy", false, 1)
			status, r, body := downloadReservationReviewHTTP(t, f, b, "reason")
			if status != 200 {
				t.Fatal(status, body)
			}
			switch change {
			case "source":
				if err := os.WriteFile(filepath.Join(source, wire.files[0]), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "config":
				if _, err := f.db.Exec(`UPDATE DOWNLOADER SET TRANSFER=0`); err != nil {
					t.Fatal(err)
				}
			case "target":
				organizationFile(t, defs[1].TargetRoot, defs[1].Entries[0].Target)
			case "task":
				wire.files = wire.files[:2]
			case "labels":
				wire.opts.labels = []string{"changed"}
			case "claim":
				if _, err := f.db.Exec(`DELETE FROM GO_ORGANIZATION_TARGETS`); err != nil {
					t.Fatal(err)
				}
			case "yaml":
				if err := f.runner.organizer.service.configStore.Update(map[string]any{"media.min_filesize": 100}); err != nil {
					t.Fatal(err)
				}
			case "root":
				if err := os.Rename(defs[1].TargetRoot, defs[1].TargetRoot+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(defs[1].TargetRoot, 0700); err != nil {
					t.Fatal(err)
				}
			}
			status, _, body = downloadBatchActionHTTP(t, f, b, "recover-reservation", reservationConsent(r))
			if status != 409 {
				t.Fatal(change, status, body)
			}
			current, err := f.runner.organizer.jobs.GetDownloadBatch(t.Context(), b.ID)
			if err != nil || len(current.Jobs) != 1 {
				t.Fatal(current, err)
			}
		})
	}
}

func TestDownloadReservationRecoveryTransactionRollsBackMissingJobsAndReceipt(t *testing.T) {
	for _, trigger := range []string{
		`CREATE TRIGGER reject_recovery BEFORE INSERT ON GO_ORGANIZATION_ITEMS BEGIN SELECT RAISE(ABORT,'test fault'); END`,
		`CREATE TRIGGER revoke_recovery AFTER INSERT ON GO_ORGANIZATION_ITEMS BEGIN UPDATE DOWNLOADER SET TRANSFER=0; END`,
	} {
		t.Run(trigger, func(t *testing.T) {
			f, _, _, _, b, _ := interruptedDownloadReservation(t, "qbittorrent", "copy", false, 1)
			status, r, body := downloadReservationReviewHTTP(t, f, b, "reason")
			if status != 200 {
				t.Fatal(status, body)
			}
			if _, err := f.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			status, _, body = downloadBatchActionHTTP(t, f, b, "recover-reservation", reservationConsent(r))
			if status == 200 {
				t.Fatal(status, body)
			}
			current, err := f.runner.organizer.jobs.GetDownloadBatch(t.Context(), b.ID)
			if err != nil || len(current.Jobs) != 1 || current.State != "reserving" {
				t.Fatal(current, err)
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_DOWNLOAD_RESERVATION_RECOVERY'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial receipt", count, err)
			}
		})
	}
}

func TestDownloadReservationRecoveryBindsSnapshotInsideTransactionAndSharesBatchLock(t *testing.T) {
	f, _, _, _, b, defs := interruptedDownloadReservation(t, "qbittorrent", "copy", false, 1)
	status, r, body := downloadReservationReviewHTTP(t, f, b, "reason")
	if status != 200 {
		t.Fatal(status, body)
	}
	saved, err := f.runner.organizer.jobs.DownloadReservation(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := f.runner.organizer.jobs.AcquireJob(t.Context(), downloadBatchGuard(b))
	if err != nil {
		t.Fatal(err)
	}
	status, _, body = downloadBatchActionHTTP(t, f, b, "recover-reservation", reservationConsent(r))
	guard.Close()
	if status != 409 {
		t.Fatal(status, body)
	}
	if _, err := f.runner.organizer.jobs.Create(t.Context(), organization.Digest(defs[1]), defs[1]); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.organizer.jobs.CompleteDownloadReservation(t.Context(), b.ID, saved.Digest, "admin", "reason", r.Fingerprint); err == nil {
		t.Fatal("stale snapshot committed")
	}
	status, _, body = downloadBatchActionHTTP(t, f, b, "recover-reservation", reservationConsent(r))
	if status != 409 {
		t.Fatal(status, body)
	}
}

func TestDownloadReservationRecoveryOmittedFilesRequireIndependentAcknowledgement(t *testing.T) {
	f, source, _, wire := downloadBatchFixture(t, "qbittorrent", "copy", false)
	organizationFile(t, source, "notes.nfo")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(source, "notes.nfo"), old, old); err != nil {
		t.Fatal(err)
	}
	wire.files = append(wire.files, "notes.nfo")
	api := f.runner.organizer
	review, failure := api.planDownload(t.Context(), downloadCompletionInput{DownloaderID: "1", TaskID: completionHash}, "admin")
	if failure != nil {
		t.Fatal(failure)
	}
	defs := []organization.Definition{}
	for _, p := range review.Plans {
		defs = append(defs, p.definition)
	}
	b, _, err := api.jobs.BeginDownloadBatch(t.Context(), review.Fingerprint, review.origin, defs, review.Omitted)
	if err != nil {
		t.Fatal(err)
	}
	status, r, body := downloadReservationReviewHTTP(t, f, b, "reason")
	if status != 200 || len(r.Omitted) != 1 {
		t.Fatal(status, body)
	}
	input := reservationConsent(r)
	input.ConfirmOmitted = false
	status, _, body = downloadBatchActionHTTP(t, f, b, "recover-reservation", input)
	if status != 409 {
		t.Fatal(status, body)
	}
	input.ConfirmOmitted = true
	status, _, body = downloadBatchActionHTTP(t, f, b, "recover-reservation", input)
	if status != 200 {
		t.Fatal(status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "notes.nfo"), "original-media")
}

func TestDownloadReservationRecoveryPermissionRuntimeAndUnknownFields(t *testing.T) {
	f, _, _, _, b, _ := interruptedDownloadReservation(t, "qbittorrent", "copy", false, 1)
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	mixed, err := newHandler(config.Config{ApplicationConfigPath: f.path}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"reservation-review", "recover-reservation"} {
		path := "/api/v1/organization/downloads/batches/" + b.ID + "/" + action
		for _, token := range []string{"", viewer} {
			w := nativeJSONRequest(f.handler, "POST", path, token, `{}`)
			want := 403
			if token == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
		}
		w := nativeJSONRequest(mixed, "POST", path, f.token, `{}`)
		if w.Code != 501 {
			t.Fatal(w.Code, w.Body.String())
		}
		w = nativeJSONRequest(f.handler, "POST", path, f.token, `{"reason":"reason","definition":{"download":{"batchId":"injected"}}}`)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
