package httpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrganizationMoveFoundationDoesNotExposeSourceRemovalAPI(t *testing.T) {
	f, source, target, input, plan := organizationCopyFixture(t)
	input.Mode = "move"
	status, movePlan, body := organizationPreview(t, f, input)
	if status != 200 || len(movePlan.Fingerprint) != 64 {
		t.Fatal(status, body)
	}
	raw, _ := json.Marshal(struct {
		organizationPlanInput
		Fingerprint string `json:"fingerprint"`
	}{input, movePlan.Fingerprint})
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw))
	if r.Code != 400 {
		t.Fatal("move draft exposed without separate source-removal consent", r.Code, r.Body.String())
	}
	roots := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/roots", f.token, "")
	if roots.Code != 200 || !strings.Contains(roots.Body.String(), `"executionModes":["copy","link","softlink"]`) {
		t.Fatal(roots.Code, roots.Body.String())
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	input.Mode = "copy"
	job := organizationCreateJob(t, f, input, plan)
	// Even a future/internal move ledger cannot be executed or reconciled by
	// the old confirmation flow. Publication alone is not a complete move.
	if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_JOBS SET DEFINITION=json_set(DEFINITION,'$.mode','move') WHERE ID=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"execute", "reconcile"} {
		status, _, body = organizationJobRequest(t, f, job.ID, action)
		if status != 409 {
			t.Fatal(action, status, body)
		}
	}
	status, _, body = organizationJobRequest(t, f, job.ID, "resume-move")
	if status != 404 {
		t.Fatal("unfinished recovery action exposed", status, body)
	}
	for _, item := range job.Items {
		data, err := os.ReadFile(filepath.Join(source, item.Source))
		if err != nil || string(data) != "original-media" {
			t.Fatal("source changed", string(data), err)
		}
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal("move gate wrote target", entries, err)
	}
	if err = f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_ITEMS WHERE STATE!='planned'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("move gate claimed items", count, err)
	}
}
