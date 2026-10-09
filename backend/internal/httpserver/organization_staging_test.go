package httpserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func TestOrganizationExecutionJournalsBeforeContentAndHoldsFailedPreparation(t *testing.T) {
	for _, mode := range []string{"copy", "move", "link", "softlink"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input, plan := organizationLinkFixture(t, mode)
			job := organizationCreateJob(t, f, input, plan)
			if _, err := f.db.Exec(`CREATE TRIGGER fail_prepared BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='prepared' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
				t.Fatal(err)
			}
			status, _, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 503 {
				t.Fatal(status, body)
			}
			saved := organizationReadMoveJob(t, f, job.ID)
			if saved.State != "needs_review" || saved.Items[0].State != "staging" || saved.Items[1].State != "planned" || saved.Items[0].Reason == "" {
				t.Fatal(saved)
			}
			s, err := organization.OpenStore(filepath.Join(filepath.Dir(f.path), "user.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			private, err := s.Get(t.Context(), job.ID)
			if err != nil || !private.Items[0].Proof.Incomplete || !private.Items[0].Proof.Anchor {
				t.Fatal(private, err)
			}
			stage := organizationMoveStage(target, job, 0)
			for _, name := range []string{"payload", "anchor"} {
				assertOrganizationMoveBytes(t, filepath.Join(stage, name), "original-media")
			}
			if _, err := os.Lstat(filepath.Join(target, job.Items[0].Target)); !os.IsNotExist(err) {
				t.Fatal("published without complete journal", err)
			}
			for _, action := range []string{"execute", "resume-publication", "resume-move", "cancel"} {
				status, _, body = organizationMoveRequest(t, f, job.ID, action)
				if status != 409 {
					t.Fatal("restarted incomplete transfer", action, status, body)
				}
			}
			status, checked, body := organizationJobRequest(t, f, job.ID, "reconcile")
			if status != 200 || checked.Items[0].State != "staging" {
				t.Fatal(status, body)
			}
			response := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/abandon-unpublished", f.token, abandonConsent)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			for _, item := range job.Items {
				assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
			}
			if _, err := os.Lstat(stage); !os.IsNotExist(err) {
				t.Fatal("did not clean verified stage", err)
			}
			saved = organizationReadMoveJob(t, f, job.ID)
			if saved.State != "abandoned" || saved.Items[1].State != "abandoned" {
				t.Fatal(saved)
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("invented success", count, err)
			}
		})
	}
}

func TestOrganizationInitialJournalFailureLeavesUnownedObjectsUntouched(t *testing.T) {
	f, source, target, input, plan := organizationCopyFixture(t)
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_stage BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='staging' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, saved, body := organizationJobRequest(t, f, job.ID, "execute")
	if status != 503 {
		t.Fatal(status, body)
	}
	saved = organizationReadMoveJob(t, f, job.ID)
	if saved.Items[0].State != "running" {
		t.Fatal(saved)
	}
	stage := organizationMoveStage(target, job, 0)
	for _, name := range []string{"payload", "anchor"} {
		info, err := os.Stat(filepath.Join(stage, name))
		if err != nil || info.Size() != 0 {
			t.Fatal("wrote bytes before transaction", info, err)
		}
	}
	response := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/abandon-unpublished", f.token, abandonConsent)
	if response.Code != 409 {
		t.Fatal("adopted unjournaled metadata", response.Code, response.Body.String())
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[0].Source), "original-media")
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, job.ID).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}
