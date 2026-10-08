package httpserver

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func organizationPublicationFixture(t *testing.T, mode string) (*subscriptionRunFixture, string, string, organization.Job) {
	t.Helper()
	f, source, target, input, plan := organizationLinkFixture(t, mode)
	draft := organizationCreateJob(t, f, input, plan)
	s, err := organization.OpenStore(filepath.Join(filepath.Dir(f.path), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err := s.Get(t.Context(), draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := job.Items[0]
	if won, err := s.Claim(t.Context(), job.ID, 0); err != nil || !won {
		t.Fatal(won, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	transfer := organization.Transfer
	if mode == "move" {
		transfer = organization.PrepareMoveTarget
	}
	_, err = transfer(ctx, job.Definition, item.Entry, organization.TempName(job.ID, 0), func(p organization.Proof) error {
		if err := s.Prepare(ctx, job.ID, 0, p); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	return f, source, target, organizationReadMoveJob(t, f, job.ID)
}

func TestOrganizationPublicationExplicitlyResumesAllLocalModesWithoutRecopy(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, job := organizationPublicationFixture(t, mode)
			item := job.Items[0]
			stage := filepath.Join(organizationMoveStage(target, job, 0), "payload")
			before, err := os.Lstat(stage)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = os.Lstat(filepath.Join(target, item.Target)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("already published", err)
			}
			status, checked, body := organizationJobRequest(t, f, job.ID, "reconcile")
			if status != 200 || checked.Items[0].State != "prepared" {
				t.Fatal(status, body)
			}
			if _, err = os.Lstat(filepath.Join(target, item.Target)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("ordinary reconcile published", err)
			}
			status, _, body = organizationJobRequest(t, f, job.ID, "resume-publication")
			if mode == "move" {
				if status != 400 {
					t.Fatal("move source consent bypass", status, body)
				}
				status, _, body = organizationMoveRequest(t, f, job.ID, "resume-publication")
			}
			if status != 200 {
				t.Fatal(status, body)
			}
			done := organizationReadMoveJob(t, f, job.ID)
			if done.State != "ready" || done.Items[0].State != "completed" || done.Items[1].State != "planned" {
				t.Fatal(done)
			}
			after, err := os.Lstat(filepath.Join(target, item.Target))
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("recovery re-copied target", err)
			}
			assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
			if mode == "move" {
				if _, err = os.Lstat(filepath.Join(source, item.Source)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			} else {
				assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[1].Source), "original-media")
			var count int
			var label string
			if err = f.db.QueryRow(`SELECT COUNT(*),MODE FROM TRANSFER_HISTORY`).Scan(&count, &label); err != nil || count != 1 || label != organization.ModeLabel(mode) {
				t.Fatal(count, label, err)
			}
			status, _, body = organizationMoveRequest(t, f, job.ID, "resume-publication")
			if status != 200 {
				t.Fatal(status, body)
			}
			f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count)
			if count != 1 {
				t.Fatal("duplicate history", count)
			}
		})
	}
}

func TestOrganizationPublicationHistoryFailureRecoversAfterRestartWithoutReexecution(t *testing.T) {
	f, source, target, job := organizationPublicationFixture(t, "copy")
	if _, err := f.db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY, MODE TEXT, TYPE TEXT, CATEGORY TEXT, TMDBID INTEGER, TITLE TEXT, YEAR TEXT, SEASON_EPISODE TEXT, SOURCE TEXT, SOURCE_PATH TEXT, SOURCE_FILENAME TEXT, DEST TEXT, DEST_PATH TEXT, DEST_FILENAME TEXT, DATE TEXT); INSERT INTO TRANSFER_HISTORY(TITLE) VALUES ('legacy'); CREATE TRIGGER deny_history BEFORE INSERT ON TRANSFER_HISTORY BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body := organizationJobRequest(t, f, job.ID, "resume-publication")
	if status != 503 {
		t.Fatal(status, body)
	}
	before, err := os.Stat(filepath.Join(target, job.Items[0].Target))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`DROP TRIGGER deny_history`); err != nil {
		t.Fatal(err)
	}
	f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	status, done, body := organizationJobRequest(t, f, job.ID, "resume-publication")
	if status != 200 || done.Items[0].State != "completed" {
		t.Fatal(status, body)
	}
	after, err := os.Stat(filepath.Join(target, job.Items[0].Target))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal(err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[0].Source), "original-media")
	var count int
	if err = f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}

func TestOrganizationPublicationRejectsUncertainOrChangedStateAndPreservesEvidence(t *testing.T) {
	for _, kind := range []string{"running", "proof-missing", "stage-missing", "source-changed", "target-conflict", "config-changed"} {
		t.Run(kind, func(t *testing.T) {
			f, source, target, job := organizationPublicationFixture(t, "copy")
			item := job.Items[0]
			switch kind {
			case "running":
				if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running' WHERE JOB_ID=? AND ORDINAL=0`, job.ID); err != nil {
					t.Fatal(err)
				}
			case "proof-missing":
				if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET PROOF='{}' WHERE JOB_ID=? AND ORDINAL=0`, job.ID); err != nil {
					t.Fatal(err)
				}
			case "stage-missing":
				if err := os.Rename(organizationMoveStage(target, job, 0), organizationMoveStage(target, job, 0)+".saved"); err != nil {
					t.Fatal(err)
				}
			case "source-changed":
				if err := os.WriteFile(filepath.Join(source, item.Source), []byte("changed-source"), 0600); err != nil {
					t.Fatal(err)
				}
			case "target-conflict":
				if err := os.WriteFile(filepath.Join(target, item.Target), []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "config-changed":
				if err := config.NewStore(f.path).Update(map[string]any{"media.min_filesize": 1}); err != nil {
					t.Fatal(err)
				}
			}
			status, _, body := organizationJobRequest(t, f, job.ID, "resume-publication")
			if status != 409 {
				t.Fatal(kind, status, body)
			}
			current := organizationReadMoveJob(t, f, job.ID)
			if current.State != "needs_review" || current.Items[0].State == "completed" || current.Items[1].State != "planned" {
				t.Fatal(current)
			}
			if kind == "target-conflict" {
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "foreign")
			} else {
				if _, err := os.Lstat(filepath.Join(target, item.Target)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, job.Items[1].Source), "original-media")
			status, _, body = organizationJobRequest(t, f, job.ID, "cancel")
			if status != 409 {
				t.Fatal("released uncertain target claim", status, body)
			}
		})
	}
}

func TestOrganizationPublicationRequiresConfirmationAdminAndExclusiveGo(t *testing.T) {
	f, _, target, job := organizationPublicationFixture(t, "copy")
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/resume-publication", f.token, `{"confirm":false}`)
	if r.Code != 400 {
		t.Fatal(r.Code, r.Body.String())
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"publication-viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "publication-viewer", "strong-password")
	for _, token := range []string{"", viewer} {
		r = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/resume-publication", token, `{"confirm":true}`)
		want := 403
		if token == "" {
			want = 401
		}
		if r.Code != want {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: f.path}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	r = nativeJSONRequest(handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/resume-publication", f.token, `{"confirm":true}`)
	if r.Code != 501 {
		t.Fatal(r.Code, r.Body.String())
	}
	if _, err = os.Lstat(filepath.Join(target, job.Items[0].Target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denied action published", err)
	}
}

func TestOrganizationPublicationConcurrentRequestsCommitOneHistory(t *testing.T) {
	f, _, target, job := organizationPublicationFixture(t, "copy")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/resume-publication", f.token, `{"confirm":true}`)
			if r.Code != 200 && r.Code != 409 {
				t.Errorf("%d: %s", r.Code, r.Body.String())
			}
		}()
	}
	wg.Wait()
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(target, job.Items[0].Target), "original-media")
}
