package httpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func libraryTriggerBody(t *testing.T, event bool, in libraryTriggerInput) string {
	t.Helper()
	var payload any
	if event {
		payload = map[string]any{"event": "media.scrap.start", "data": in}
	} else {
		payload = map[string]any{"cmd": "media_path_scrap", "data": in}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestLibraryLegacyAjaxAndEventUseReviewedDurableJobsWithoutPython(t *testing.T) {
	for _, event := range []bool{false, true} {
		for _, force := range []bool{false, true} {
			t.Run(organization.Digest([]bool{event, force})[:6], func(t *testing.T) {
				x := newLibraryHTTPFixture(t, false)
				organizationFile(t, x.root, "Movie/Movie.2026.mkv")
				if event {
					libraryPluginInstall(t, x)
				}
				path := "/do"
				if event {
					path = libraryEndpoint + "/event"
				}
				in := libraryTriggerInput{Path: filepath.Join(x.root, "Movie"), Force: force}
				r := nativeJSONRequest(x.f.handler, "POST", path, x.f.token, libraryTriggerBody(t, event, in))
				if r.Code != 400 {
					t.Fatal("implicit file trigger accepted", r.Code, r.Body.String())
				}
				in.Review = true
				r = nativeJSONRequest(x.f.handler, "POST", path, x.f.token, libraryTriggerBody(t, event, in))
				var p struct{ Data libraryPreview }
				if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &p) != nil || p.Data.Total != 1 || x.lookups.Load() != 0 {
					t.Fatal(r.Code, r.Body.String())
				}
				wantMode := "no_force"
				if force || !event {
					wantMode = "force_all"
				}
				if p.Data.Input.Mode != wantMode {
					t.Fatal(p.Data.Input.Mode, wantMode)
				}
				jobs, _ := x.f.runner.organizer.jobs.LibraryJobs(t.Context(), false)
				if len(jobs) != 0 {
					t.Fatal("review created job", jobs)
				}
				in.Review = false
				in.RequestID = p.Data.Input.RequestID
				in.Fingerprint = p.Data.Fingerprint
				in.Confirm, in.ConfirmOldExecutorsStopped, in.ConfirmOverwrite = true, true, true
				r = nativeJSONRequest(x.f.handler, "POST", path, x.f.token, libraryTriggerBody(t, event, in))
				var j struct{ Data organization.LibraryJob }
				if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &j) != nil || j.Data.State != "ready" {
					t.Fatal(r.Code, r.Body.String())
				}
				if err := x.f.runner.library.tick(t.Context()); err != nil {
					t.Fatal(err)
				}
				if job := x.job(t, j.Data.ID); job.State != "completed" {
					t.Fatal(job)
				}
				if _, err := os.Stat(filepath.Join(x.root, "Movie/Movie.2026.nfo")); err != nil {
					t.Fatal(err)
				}
				before := x.lookups.Load()
				monitorRebuild(t, x.f)
				r = nativeJSONRequest(x.f.handler, "POST", path, x.f.token, libraryTriggerBody(t, event, in))
				if r.Code != 200 || !strings.Contains(r.Body.String(), j.Data.ID) {
					t.Fatal(r.Code, r.Body.String())
				}
				if err := x.f.runner.library.tick(t.Context()); err != nil || x.lookups.Load() != before {
					t.Fatal("lost trigger response resubmitted", err)
				}
				if event {
					runs, err := x.f.runner.organizer.jobs.LibraryPluginRuns(t.Context())
					if err != nil || len(runs) != 1 || runs[0].State != "completed" || runs[0].Source != "event" {
						t.Fatal(runs, err)
					}
				}
			})
		}
	}
}
func TestLibraryScrapeEventRequiresInstalledPluginAndUninstallRevokesIt(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	organizationFile(t, x.root, "Movie.2026.mkv")
	in := libraryTriggerInput{Path: filepath.Join(x.root, "Movie.2026.mkv"), Review: true}
	r := nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/event", x.f.token, libraryTriggerBody(t, true, in))
	if r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
	libraryPluginInstall(t, x)
	r = nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/event", x.f.token, libraryTriggerBody(t, true, in))
	var p struct{ Data libraryPreview }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &p) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	in.Review = false
	in.RequestID = p.Data.Input.RequestID
	in.Fingerprint = p.Data.Fingerprint
	in.Confirm, in.ConfirmOldExecutorsStopped = true, true
	r = nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/event", x.f.token, libraryTriggerBody(t, true, in))
	var j struct{ Data organization.LibraryJob }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &j) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	r = nativeJSONRequest(x.f.handler, "DELETE", libraryPluginEndpoint, x.f.token, "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	libraryPluginInstall(t, x)
	if job := x.job(t, j.Data.ID); job.State != "cancelled" {
		t.Fatal(job)
	}
	if err := x.f.runner.library.tick(t.Context()); err != nil || x.lookups.Load() != 0 {
		t.Fatal("reinstall reauthorized old event", err)
	}
}
func TestLibraryLegacyTriggerCannotEscapeRootsOrRunOtherCommands(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	organizationFile(t, x.root, "Movie.2026.mkv")
	for _, path := range []string{filepath.Join(t.TempDir(), "Movie.mkv"), "relative.mkv"} {
		r := nativeJSONRequest(x.f.handler, "POST", "/do", x.f.token, libraryTriggerBody(t, false, libraryTriggerInput{Path: path, Review: true}))
		if r.Code != 400 && r.Code != 409 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	r := nativeJSONRequest(x.f.handler, "POST", "/do", x.f.token, `{"cmd":"run_command","data":{}}`)
	if r.Code != 501 {
		t.Fatal(r.Code, r.Body.String())
	}
	r = nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/event", x.f.token, `{"event":"system.command","data":{}}`)
	if r.Code != 400 {
		t.Fatal(r.Code, r.Body.String())
	}
	r = nativeJSONRequest(x.f.handler, "POST", "/do", "", `{"cmd":"media_path_scrap","data":{}}`)
	if r.Code != 401 {
		t.Fatal(r.Code, r.Body.String())
	}
}
