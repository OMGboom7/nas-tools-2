package httpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func TestOrganizationAllMutatingActionsRespectIndependentExecutorGuard(t *testing.T) {
	for _, mode := range []string{"copy", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input, plan := organizationLinkFixture(t, mode)
			job := organizationCreateJob(t, f, input, plan)
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
			for _, action := range []string{"execute", "resume-move", "resume-publication", "reconcile", "cancel"} {
				r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/"+action, f.token, `{"confirm":true,"confirmSourceRemoval":true}`)
				if r.Code != 409 {
					t.Fatal(action, r.Code, r.Body.String())
				}
				if (mode == "move" || action != "resume-move") && !strings.Contains(r.Body.String(), "already active") {
					t.Fatal("busy response did not identify active peer", action, r.Body.String())
				}
			}
			read := organizationReadMoveJob(t, f, job.ID)
			if read.State != "ready" || read.Items[0].State != "planned" {
				t.Fatal("busy action changed ledger", read)
			}
			for _, item := range job.Items {
				assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
			}
			if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
				t.Fatal("busy action wrote target", entries, err)
			}
			guard.Close()
			status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" {
				t.Fatal("guard not released", status, body)
			}
		})
	}
}

func TestOrganizationMissingMoveConsentDoesNotEvenCreateExecutionGuard(t *testing.T) {
	f, _, _, input, plan := organizationLinkFixture(t, "move")
	job := organizationCreateJob(t, f, input, plan)
	for _, action := range []string{"execute", "resume-move", "resume-publication"} {
		r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/"+action, f.token, `{"confirm":true}`)
		if r.Code != 400 {
			t.Fatal(action, r.Code, r.Body.String())
		}
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(f.path), ".nastool-organization-locks")); !os.IsNotExist(err) {
		t.Fatal("missing consent created guard metadata", err)
	}
}

func TestOrganizationGuardReleasesAfterFailedActionWithoutResettingUncertainState(t *testing.T) {
	f, source, _, input, plan := organizationCopyFixture(t)
	job := organizationCreateJob(t, f, input, plan)
	if err := os.WriteFile(filepath.Join(source, job.Items[0].Source), []byte("changed-source"), 0600); err != nil {
		t.Fatal(err)
	}
	status, _, body := organizationJobRequest(t, f, job.ID, "execute")
	if status != 409 {
		t.Fatal(status, body)
	}
	s, err := organization.OpenStore(filepath.Join(filepath.Dir(f.path), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	guard, err := s.AcquireJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal("failed operation leaked guard", err)
	}
	guard.Close()
	current := organizationReadMoveJob(t, f, job.ID)
	if current.State != "needs_review" || current.Items[0].State != "running" {
		t.Fatal("failure released persistent reservation", current)
	}
}
