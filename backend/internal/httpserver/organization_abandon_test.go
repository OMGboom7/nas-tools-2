package httpserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

const abandonConsent = `{"confirm":true,"confirmDiscardStaging":true,"confirmOldExecutorsStopped":true}`

func TestOrganizationAbandonClosesWholeJobWithoutSourceRemovalOrHistory(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, job := organizationPublicationFixture(t, mode)
			response := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/abandon-unpublished", f.token, abandonConsent)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			saved := organizationReadMoveJob(t, f, job.ID)
			if saved.State != "abandoned" || len(saved.Items) != 2 {
				t.Fatal(saved)
			}
			for _, item := range saved.Items {
				if item.State != "abandoned" {
					t.Fatal(item)
				}
				assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
				if _, err := os.Lstat(filepath.Join(target, item.Target)); !os.IsNotExist(err) {
					t.Fatal("target written", err)
				}
			}
			if _, err := os.Lstat(organizationMoveStage(target, job, 0)); !os.IsNotExist(err) {
				t.Fatal("stage retained", err)
			}
			var n int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, job.ID).Scan(&n); err != nil || n != 0 {
				t.Fatal(n, err)
			}
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&n); err != nil || n != 0 {
				t.Fatal("invented history", n, err)
			}
			for _, action := range []string{"execute", "resume-move", "resume-publication", "reconcile", "cancel"} {
				response = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/"+action, f.token, `{"confirm":true,"confirmSourceRemoval":true}`)
				if response.Code != 409 {
					t.Fatal("reopened abandoned job", action, response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestOrganizationAbandonRequiresIndependentConsentsAndGuard(t *testing.T) {
	f, _, target, job := organizationPublicationFixture(t, "move")
	for _, body := range []string{`{"confirm":true}`, `{"confirm":true,"confirmSourceRemoval":true}`, `{"confirm":true,"confirmDiscardStaging":true}`, `{"confirm":true,"confirmOldExecutorsStopped":true}`} {
		response := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/abandon-unpublished", f.token, body)
		if response.Code != 400 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(f.path), ".nastool-organization-locks")); !os.IsNotExist(err) {
		t.Fatal("missing consent created guard", err)
	}
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
	response := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/abandon-unpublished", f.token, abandonConsent)
	if response.Code != 409 {
		t.Fatal(response.Code, response.Body.String())
	}
	assertOrganizationMoveBytes(t, filepath.Join(organizationMoveStage(target, job, 0), "payload"), "original-media")
	guard.Close()
	response = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/abandon-unpublished", f.token, abandonConsent)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestOrganizationAbandonRecoveryRetainsClaimsUntilDurableFinalReceipt(t *testing.T) {
	f, source, target, job := organizationPublicationFixture(t, "copy")
	if _, err := f.db.Exec(`CREATE TRIGGER fail_abandon BEFORE DELETE ON GO_ORGANIZATION_TARGETS BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	response := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/abandon-unpublished", f.token, abandonConsent)
	if response.Code != 503 {
		t.Fatal(response.Code, response.Body.String())
	}
	saved := organizationReadMoveJob(t, f, job.ID)
	if saved.State != "abandoning" || saved.Items[0].State != "abandoned" {
		t.Fatal(saved)
	}
	if _, err := os.Lstat(organizationMoveStage(target, job, 0)); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_abandon`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, job.Items[0].Target), []byte("foreign-media"), 0600); err != nil {
		t.Fatal(err)
	}
	response = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/abandon-unpublished", f.token, abandonConsent)
	if response.Code != 409 {
		t.Fatal(response.Code, response.Body.String())
	}
	assertOrganizationMoveBytes(t, filepath.Join(target, job.Items[0].Target), "foreign-media")
	assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[0].Source), "original-media")
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, job.ID).Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if err := os.Remove(filepath.Join(target, job.Items[0].Target)); err != nil {
		t.Fatal(err)
	}
	response = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/abandon-unpublished", f.token, abandonConsent)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
}
