package httpserver

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func organizationMoveRequest(t *testing.T, f *subscriptionRunFixture, id, action string) (int, organization.Job, string) {
	t.Helper()
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+id+"/"+action, f.token, `{"confirm":true,"confirmSourceRemoval":true}`)
	var payload struct{ Data organization.Job }
	if err := json.Unmarshal(r.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return r.Code, payload.Data, r.Body.String()
}

func organizationReadMoveJob(t *testing.T, f *subscriptionRunFixture, id string) organization.Job {
	t.Helper()
	r := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/jobs/"+id, f.token, "")
	var payload struct{ Data organization.Job }
	if err := json.Unmarshal(r.Body.Bytes(), &payload); err != nil || r.Code != 200 {
		t.Fatal(r.Code, r.Body.String(), err)
	}
	return payload.Data
}

func assertOrganizationMoveBytes(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatal(path, string(data), err)
	}
}
func organizationMoveHold(source string, job organization.Job, index int) string {
	return filepath.Join(filepath.Dir(filepath.Join(source, job.Items[index].Source)), organization.MoveHoldName(job.ID, index))
}
func organizationMoveStage(target string, job organization.Job, index int) string {
	return filepath.Join(filepath.Dir(filepath.Join(target, job.Items[index].Target)), organization.TempName(job.ID, index))
}

func TestOrganizationMoveRequiresIndependentConsentBeforeClaimOrAnyWrite(t *testing.T) {
	f, source, target, input, plan := organizationLinkFixture(t, "move")
	job := organizationCreateJob(t, f, input, plan)
	canonicalSource, _ := filepath.EvalSymlinks(source)
	canonicalTarget, _ := filepath.EvalSymlinks(target)
	if job.SourceRoot != canonicalSource || job.TargetRoot != canonicalTarget || job.Mode != "move" {
		t.Fatal(job)
	}
	roots := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/roots", f.token, "")
	if roots.Code != 200 || !strings.Contains(roots.Body.String(), `"executionModes":["copy","link","softlink","move"]`) {
		t.Fatal(roots.Code, roots.Body.String())
	}
	for _, action := range []string{"execute", "resume-move"} {
		for _, body := range []string{`{"confirm":true}`, `{"confirm":true,"confirmSourceRemoval":false}`, `{"confirm":false,"confirmSourceRemoval":true}`} {
			r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/"+action, f.token, body)
			if r.Code != 400 {
				t.Fatal(action, r.Code, r.Body.String())
			}
		}
	}
	status, _, body := organizationJobRequest(t, f, job.ID, "reconcile")
	if status != 200 {
		t.Fatal(status, body)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "resume-move")
	if status != 200 {
		t.Fatal(status, body)
	} // Recovery never starts planned items.
	for _, item := range job.Items {
		assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal("unconfirmed move wrote target", entries, err)
	}
	var count int
	if err = f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_ITEMS WHERE STATE!='planned'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("unconfirmed move claimed items", count, err)
	}
}

func TestOrganizationMoveExecutesMediaAndAttachmentsWithSingleCompatibleHistory(t *testing.T) {
	f, source, target, input, plan := organizationLinkFixture(t, "move")
	job := organizationCreateJob(t, f, input, plan)
	status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
	if status != 200 || done.State != "completed" {
		t.Fatal(status, body)
	}
	for _, item := range done.Items {
		if item.State != "completed" || item.Reason != "" {
			t.Fatal(item)
		}
		if _, err := os.Lstat(filepath.Join(source, item.Source)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("source name retained", err)
		}
		assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
		for _, path := range []string{organizationMoveHold(source, job, item.Index), organizationMoveStage(target, job, item.Index)} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovery left behind", path, err)
			}
		}
	}
	for _, action := range []string{"execute", "resume-move", "resume-move"} {
		status, done, body = organizationMoveRequest(t, f, job.ID, action)
		if status != 200 || done.State != "completed" {
			t.Fatal(action, status, body)
		}
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE MODE='移动' AND SOURCE='手动整理' AND TITLE='Movie'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if entries, err := os.ReadDir(source); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	if f.adds != 0 || f.queries != 0 {
		t.Fatal("move contacted downloader/indexer")
	}
}

func TestOrganizationMoveHistoryFailureRequiresExplicitRecoveryAndPreservesNewArrival(t *testing.T) {
	f, source, target, input, plan := organizationLinkFixture(t, "move")
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY, MODE TEXT, TYPE TEXT, CATEGORY TEXT, TMDBID INTEGER, TITLE TEXT, YEAR TEXT, SEASON_EPISODE TEXT, SOURCE TEXT, SOURCE_PATH TEXT, SOURCE_FILENAME TEXT, DEST TEXT, DEST_PATH TEXT, DEST_FILENAME TEXT, DATE TEXT); INSERT INTO TRANSFER_HISTORY(TITLE) VALUES ('legacy'); CREATE TRIGGER deny_history BEFORE INSERT ON TRANSFER_HISTORY BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body := organizationMoveRequest(t, f, job.ID, "execute")
	if status != 503 {
		t.Fatal(status, body)
	}
	job = organizationReadMoveJob(t, f, job.ID)
	if job.State != "needs_review" || job.Items[0].State != "quarantined" || job.Items[1].State != "planned" {
		t.Fatal(job)
	}
	media := job.Items[0]
	hold := organizationMoveHold(source, job, 0)
	for _, name := range []string{"payload", "witness"} {
		assertOrganizationMoveBytes(t, filepath.Join(hold, name), "original-media")
	}
	if err := os.WriteFile(filepath.Join(source, media.Source), []byte("new arrival"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER deny_history`); err != nil {
		t.Fatal(err)
	}
	var err error
	f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	status, checked, body := organizationJobRequest(t, f, job.ID, "reconcile")
	if status != 200 || checked.Items[0].State != "quarantined" {
		t.Fatal("ordinary reconcile disposed source", status, body)
	}
	for _, name := range []string{"payload", "witness"} {
		assertOrganizationMoveBytes(t, filepath.Join(hold, name), "original-media")
	}
	status, _, body = organizationJobRequest(t, f, job.ID, "resume-move")
	if status != 400 {
		t.Fatal(status, body)
	}
	status, recovered, body := organizationMoveRequest(t, f, job.ID, "resume-move")
	if status != 200 || recovered.State != "ready" || recovered.Items[0].State != "completed" || recovered.Items[1].State != "planned" {
		t.Fatal(status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, media.Source), "new arrival")
	assertOrganizationMoveBytes(t, filepath.Join(target, media.Target), "original-media")
	assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[1].Source), "original-media")
	status, recovered, body = organizationMoveRequest(t, f, job.ID, "execute")
	if status != 200 || recovered.State != "completed" {
		t.Fatal(status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, media.Source), "new arrival")
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count)
	if count != 2 {
		t.Fatal("duplicate move or erased legacy history", count)
	}
}

func TestOrganizationMoveInterruptedBeforeQuarantineCommitRecoversWithoutRecopy(t *testing.T) {
	f, source, target, input, plan := organizationLinkFixture(t, "move")
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`CREATE TRIGGER deny_quarantine BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='quarantined' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body := organizationMoveRequest(t, f, job.ID, "execute")
	if status != 503 {
		t.Fatal(status, body)
	}
	job = organizationReadMoveJob(t, f, job.ID)
	if job.Items[0].State != "moving" {
		t.Fatal(job)
	}
	media := job.Items[0]
	before, err := os.Stat(filepath.Join(target, media.Target))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`DROP TRIGGER deny_quarantine`); err != nil {
		t.Fatal(err)
	}
	status, recovered, body := organizationMoveRequest(t, f, job.ID, "resume-move")
	if status != 200 || recovered.Items[0].State != "completed" || recovered.Items[1].State != "planned" {
		t.Fatal(status, body)
	}
	after, err := os.Stat(filepath.Join(target, media.Target))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("recovery re-copied target", err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[1].Source), "original-media")
}

func TestOrganizationMoveCleanupReceiptsSurviveFailuresAtBothStages(t *testing.T) {
	for _, phase := range []string{"source", "target"} {
		t.Run(phase, func(t *testing.T) {
			f, source, target, input, plan := organizationLinkFixture(t, "move")
			job := organizationCreateJob(t, f, input, plan)
			trigger := `CREATE TRIGGER deny_receipt BEFORE UPDATE OF REASON ON GO_ORGANIZATION_ITEMS WHEN NEW.REASON='Move target recovery cleanup pending' BEGIN SELECT RAISE(ABORT,'fixture'); END`
			want := organization.MoveSourceCleanupPending
			if phase == "target" {
				trigger = `CREATE TRIGGER deny_receipt BEFORE UPDATE OF REASON ON GO_ORGANIZATION_ITEMS WHEN OLD.REASON='Move target recovery cleanup pending' AND NEW.REASON='' BEGIN SELECT RAISE(ABORT,'fixture'); END`
				want = organization.MoveTargetCleanupPending
			}
			if _, err := f.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			status, _, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 503 {
				t.Fatal(status, body)
			}
			job = organizationReadMoveJob(t, f, job.ID)
			if job.State != "needs_review" || job.Items[0].State != "completed" || job.Items[0].Reason != want {
				t.Fatal(job)
			}
			stage := organizationMoveStage(target, job, 0)
			if phase == "source" {
				assertOrganizationMoveBytes(t, filepath.Join(stage, "payload"), "original-media")
			} else {
				if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			media := job.Items[0]
			if err := os.WriteFile(filepath.Join(source, media.Source), []byte("new arrival"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`DROP TRIGGER deny_receipt`); err != nil {
				t.Fatal(err)
			}
			var err error
			f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
			if err != nil {
				t.Fatal(err)
			}
			status, done, body := organizationMoveRequest(t, f, job.ID, "resume-move")
			if status != 200 || done.Items[0].Reason != "" || done.Items[1].State != "planned" {
				t.Fatal(status, body)
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, media.Source), "new arrival")
			assertOrganizationMoveBytes(t, filepath.Join(target, media.Target), "original-media")
			var count int
			f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count)
			if count != 1 {
				t.Fatal(count)
			}
		})
	}
}

func TestOrganizationMoveRecoveryNeverGuessesMissingProofOrChangedConfiguration(t *testing.T) {
	for _, kind := range []string{"running", "target-missing", "target-replaced", "config-changed"} {
		t.Run(kind, func(t *testing.T) {
			f, source, target, input, plan := organizationLinkFixture(t, "move")
			job := organizationCreateJob(t, f, input, plan)
			if kind == "running" {
				if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running' WHERE JOB_ID=? AND ORDINAL=0`, job.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.db.Exec(`CREATE TRIGGER deny_quarantine BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='quarantined' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
				status, _, body := organizationMoveRequest(t, f, job.ID, "execute")
				if status != 503 {
					t.Fatal(status, body)
				}
				if _, err := f.db.Exec(`DROP TRIGGER deny_quarantine`); err != nil {
					t.Fatal(err)
				}
				job = organizationReadMoveJob(t, f, job.ID)
				media := job.Items[0]
				if kind != "config-changed" {
					if err := os.Rename(filepath.Join(target, media.Target), filepath.Join(target, media.Target)+".saved"); err != nil {
						t.Fatal(err)
					}
					if kind == "target-replaced" {
						if err := os.WriteFile(filepath.Join(target, media.Target), []byte("original-media"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					if err := config.NewStore(f.path).Update(map[string]any{"media.min_filesize": 1}); err != nil {
						t.Fatal(err)
					}
				}
			}
			status, _, body := organizationMoveRequest(t, f, job.ID, "resume-move")
			if status != 409 {
				t.Fatal(kind, status, body)
			}
			current := organizationReadMoveJob(t, f, job.ID)
			if current.Items[0].State == "completed" || current.Items[1].State != "planned" {
				t.Fatal(current)
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[1].Source), "original-media")
			if kind != "running" {
				assertOrganizationMoveBytes(t, filepath.Join(organizationMoveHold(source, job, 0), "payload"), "original-media")
			}
		})
	}
}

func TestOrganizationMoveActionsRequireAdministratorAndExclusiveGoMode(t *testing.T) {
	f, _, _, input, plan := organizationLinkFixture(t, "move")
	job := organizationCreateJob(t, f, input, plan)
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"move-viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "move-viewer", "strong-password")
	for _, action := range []string{"execute", "resume-move", "reconcile"} {
		for _, token := range []string{"", viewer} {
			r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/"+action, token, `{"confirm":true,"confirmSourceRemoval":true}`)
			want := 403
			if token == "" {
				want = 401
			}
			if r.Code != want {
				t.Fatal(action, r.Code, r.Body.String())
			}
		}
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: f.path}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	r := nativeJSONRequest(handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/resume-move", f.token, `{"confirm":true,"confirmSourceRemoval":true}`)
	if r.Code != 501 {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestOrganizationMoveConcurrentExecutionHasOneHistoryAndNoFallback(t *testing.T) {
	f, _, target, input, plan := organizationLinkFixture(t, "move")
	job := organizationCreateJob(t, f, input, plan)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/execute", f.token, `{"confirm":true,"confirmSourceRemoval":true}`)
			if r.Code != 200 && r.Code != 409 {
				t.Errorf("%d: %s", r.Code, r.Body.String())
			}
		}()
	}
	wg.Wait()
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE MODE='移动'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	for _, item := range job.Items {
		assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
	}
}

// Construct exact crash boundaries using the same filesystem and ledger APIs,
// without exposing a production endpoint or a test-only execution hook.
func organizationPreparedMove(t *testing.T, f *subscriptionRunFixture, job organization.Job, quarantine bool) (*organization.Store, organization.Job) {
	t.Helper()
	s, err := organization.OpenStore(filepath.Join(filepath.Dir(f.path), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	job, err = s.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := job.Items[0]
	if won, err := s.Claim(t.Context(), job.ID, 0); err != nil || !won {
		t.Fatal(won, err)
	}
	p, err := organization.PrepareMoveTarget(t.Context(), job.Definition, item.Entry, organization.TempName(job.ID, 0), func(p organization.Proof) error { return s.Prepare(t.Context(), job.ID, 0, p) })
	if err != nil {
		t.Fatal(err)
	}
	if quarantine {
		p, err = organization.PrepareMove(t.Context(), job.Definition, item.Entry, p, organization.MoveHoldName(job.ID, 0))
		if err != nil {
			t.Fatal(err)
		}
		if err = s.BeginMove(t.Context(), job.ID, 0, p); err != nil {
			t.Fatal(err)
		}
		if err = organization.ContinueMove(t.Context(), job.Definition, item.Entry, p, false, func() error { return s.MarkQuarantined(t.Context(), job.ID, 0, p) }); err != nil {
			t.Fatal(err)
		}
	}
	job, err = s.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, job
}

func TestOrganizationMovePreparedPublicationNeedsSeparateSourceRecovery(t *testing.T) {
	f, source, target, input, plan := organizationLinkFixture(t, "move")
	draft := organizationCreateJob(t, f, input, plan)
	_, job := organizationPreparedMove(t, f, draft, false)
	before, _ := os.Stat(filepath.Join(target, job.Items[0].Target))
	status, checked, body := organizationJobRequest(t, f, job.ID, "reconcile")
	if status != 200 || checked.Items[0].State != "prepared" {
		t.Fatal(status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[0].Source), "original-media")
	status, _, body = organizationJobRequest(t, f, job.ID, "resume-move")
	if status != 400 {
		t.Fatal(status, body)
	}
	status, done, body := organizationMoveRequest(t, f, job.ID, "resume-move")
	if status != 200 || done.Items[0].State != "completed" || done.Items[1].State != "planned" {
		t.Fatal(status, body)
	}
	after, err := os.Stat(filepath.Join(target, job.Items[0].Target))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("prepared recovery re-copied target", err)
	}
}

func TestOrganizationMovePostHistoryCleanupRetainsUnknownFilesOrInvalidProof(t *testing.T) {
	for _, kind := range []string{"source-unknown", "target-invalid", "source-restored", "target-unknown"} {
		t.Run(kind, func(t *testing.T) {
			f, source, target, input, plan := organizationLinkFixture(t, "move")
			draft := organizationCreateJob(t, f, input, plan)
			s, job := organizationPreparedMove(t, f, draft, true)
			item := job.Items[0]
			if err := s.Complete(t.Context(), job, item); err != nil {
				t.Fatal(err)
			}
			hold, stage := organizationMoveHold(source, job, 0), organizationMoveStage(target, job, 0)
			switch kind {
			case "source-unknown":
				if err := os.WriteFile(filepath.Join(hold, "unknown"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "target-invalid":
				if err := os.Rename(filepath.Join(target, item.Target), filepath.Join(target, item.Target)+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, item.Target), []byte("other"), 0600); err != nil {
					t.Fatal(err)
				}
			case "source-restored":
				if err := os.Link(filepath.Join(hold, "payload"), filepath.Join(source, item.Source)); err != nil {
					t.Fatal(err)
				}
			case "target-unknown":
				if err := organization.CleanupMovedSource(t.Context(), job.Definition, item.Entry, item.Proof); err != nil {
					t.Fatal(err)
				}
				if err := s.AdvanceMoveCleanup(t.Context(), job.ID, 0, item.Proof, true); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stage, "unknown"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			status, _, body := organizationMoveRequest(t, f, job.ID, "resume-move")
			want := 409
			if strings.HasSuffix(kind, "unknown") {
				want = 503
			}
			if status != want {
				t.Fatal(kind, status, body)
			}
			current := organizationReadMoveJob(t, f, job.ID)
			if current.State != "needs_review" || current.Items[0].State != "completed" || current.Items[0].Reason == "" {
				t.Fatal(current)
			}
			if kind == "source-unknown" || kind == "target-unknown" {
				path := filepath.Join(hold, "unknown")
				if kind == "target-unknown" {
					path = filepath.Join(stage, "unknown")
				}
				assertOrganizationMoveBytes(t, path, "keep")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				} // Explicit fixture disposition, not a product action.
				status, done, body := organizationMoveRequest(t, f, job.ID, "resume-move")
				if status != 200 || done.Items[0].Reason != "" {
					t.Fatal(status, body)
				}
			} else {
				for _, name := range []string{"payload", "witness"} {
					assertOrganizationMoveBytes(t, filepath.Join(hold, name), "original-media")
				}
				assertOrganizationMoveBytes(t, filepath.Join(stage, "payload"), "original-media")
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestOrganizationMoveCannotAdoptUnjournaledSourceHoldAfterIntentSaveFailure(t *testing.T) {
	f, source, target, input, plan := organizationLinkFixture(t, "move")
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`CREATE TRIGGER deny_intent BEFORE UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='moving' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body := organizationMoveRequest(t, f, job.ID, "execute")
	if status != 503 {
		t.Fatal(status, body)
	}
	job = organizationReadMoveJob(t, f, job.ID)
	if job.Items[0].State != "prepared" {
		t.Fatal(job)
	}
	if _, err := f.db.Exec(`DROP TRIGGER deny_intent`); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "resume-move")
	if status != 409 {
		t.Fatal("adopted unknown hold", status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[0].Source), "original-media")
	assertOrganizationMoveBytes(t, filepath.Join(organizationMoveHold(source, job, 0), "witness"), "original-media")
	assertOrganizationMoveBytes(t, filepath.Join(target, job.Items[0].Target), "original-media")
}
