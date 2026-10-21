package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func directSyncFixture(t *testing.T, mode string) (*subscriptionRunFixture, string, string, directSyncInput) {
	t.Helper()
	f, source, target, _, _ := organizationCopyFixture(t)
	if _, err := f.db.Exec(`CREATE TABLE CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,UNKNOWN TEXT,MODE TEXT,COMPATIBILITY INTEGER,RENAME INTEGER,ENABLED INTEGER,NOTE TEXT,LOCATING INTEGER);INSERT INTO CONFIG_SYNC_PATHS VALUES(7,?,?,'',?,1,0,1,NULL,1)`, source, target, organization.ModeLabel(mode)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"misc/readme.txt", "misc/archive.bin"} {
		organizationFile(t, source, name)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.min_filesize": 100000}); err != nil {
		t.Fatal(err)
	}
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("direct sync made a metadata/legacy request: %s", r.URL)
		return nil, nil
	})
	var err error
	f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	return f, source, target, directSyncInput{ID: "7", Path: "."}
}
func directSyncPreview(t *testing.T, f *subscriptionRunFixture, input directSyncInput) (int, organizationPlan, string) {
	t.Helper()
	raw, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/plan", f.token, string(raw))
	var result struct{ Data organizationPlan }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return w.Code, result.Data, w.Body.String()
}
func directSyncSave(t *testing.T, f *subscriptionRunFixture, input directSyncInput, plan organizationPlan) (int, organization.Job, string) {
	t.Helper()
	input.Fingerprint = plan.Fingerprint
	raw, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/jobs", f.token, string(raw))
	var result struct{ Data organization.Job }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return w.Code, result.Data, w.Body.String()
}

func TestDirectSyncPreservesAllFilePathsAndWritesOnlySyncHistoryInLiteralModes(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input := directSyncFixture(t, mode)
			status, plan, body := directSyncPreview(t, f, input)
			if status != 200 || len(plan.Items) != 4 {
				t.Fatal(status, body)
			}
			for _, item := range plan.Items {
				if item.Kind != "file" || item.Source != item.Target || item.Status != "available" || item.TMDBID != "" {
					t.Fatal(item)
				}
			}
			if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
				t.Fatal(entries, err)
			}
			status, job, body := directSyncSave(t, f, input, plan)
			if status != 201 || job.State != "ready" || job.Sync == nil || job.Sync.Record.Note != "" || len(job.Sync.Record.NullFields) != 1 || job.Sync.Record.NullFields[0] != "note" || job.Sync.Actor == "" {
				t.Fatal(status, job, body)
			}
			if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
				t.Fatal("save executed", entries, err)
			}
			if mode == "move" {
				status, _, body = organizationJobRequest(t, f, job.ID, "execute")
				if status != 400 {
					t.Fatal(status, body)
				}
			}
			status, job, body = organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || job.State != "completed" || len(job.Items) != 4 || job.Mode != mode {
				t.Fatal(status, job, body)
			}
			for _, item := range job.Items {
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
				if mode == "move" {
					if _, err := os.Lstat(filepath.Join(source, item.Source)); !os.IsNotExist(err) {
						t.Fatal(err)
					}
				} else {
					assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
				}
			}
			var count, mediaHistory int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM SYNC_HISTORY`).Scan(&count); err != nil || count != 4 {
				t.Fatal(count, err)
			}
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='TRANSFER_HISTORY'`).Scan(&mediaHistory); err != nil || mediaHistory != 0 {
				t.Fatal("invented media history", mediaHistory, err)
			}
			status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 {
				t.Fatal(status, body)
			}
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM SYNC_HISTORY`).Scan(&count); err != nil || count != 4 {
				t.Fatal("duplicate history", count, err)
			}
		})
	}
}

func TestDirectSyncHistorySuppressesReexecutionWithoutAssumingTargetExists(t *testing.T) {
	f, source, target, input := directSyncFixture(t, "copy")
	if _, err := f.db.Exec(`CREATE TABLE SYNC_HISTORY(ID INTEGER PRIMARY KEY,PATH TEXT,SRC TEXT,DEST TEXT);INSERT INTO SYNC_HISTORY VALUES(1,?,?,?)`, filepath.Join(source, "misc/readme.txt"), source, target); err != nil {
		t.Fatal(err)
	}
	status, plan, body := directSyncPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Source == "misc/readme.txt" && item.Status != "recorded" {
			t.Fatal(item)
		}
	}
	status, job, body := directSyncSave(t, f, input, plan)
	if status != 201 || len(job.Items) != 3 {
		t.Fatal(status, body)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if status != 200 {
		t.Fatal(status, body)
	}
	if _, err := os.Lstat(filepath.Join(target, "misc/readme.txt")); !os.IsNotExist(err) {
		t.Fatal("old history was treated as repair permission", err)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "misc/readme.txt"), "original-media")
}

func TestDirectSyncIgnoresDestinationRegexTemporaryAndSystemPaths(t *testing.T) {
	f, source, target, input := directSyncFixture(t, "copy")
	for _, name := range []string{"skip/sub.txt", "notes.tmp", "partial.!qB", "@Recycle/old.txt", "#recycle/old.txt", "@eaDir/preview.jpg", ".hidden/private.txt"} {
		organizationFile(t, source, name)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.ignored_paths": "(?<=/)skip;", "media.ignored_files": "\\.tmp$;"}); err != nil {
		t.Fatal(err)
	}
	status, plan, body := directSyncPreview(t, f, input)
	if status != 200 || plan.Skipped != 4 {
		t.Fatal(status, plan.Skipped, body)
	}
	filtered := 0
	for _, item := range plan.Items {
		if item.Status == "filtered" {
			filtered++
		}
	}
	if filtered != 3 {
		t.Fatal(plan)
	}
	status, job, body := directSyncSave(t, f, input, plan)
	if status != 201 || len(job.Items) != 4 {
		t.Fatal(status, body)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, name := range []string{"skip/sub.txt", "notes.tmp", "partial.!qB"} {
		if _, err := os.Lstat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	for _, rule := range []any{"[", 42} {
		if err := config.NewStore(f.path).Update(map[string]any{"media.ignored_paths": rule}); err != nil {
			t.Fatal(err)
		}
		status, _, body := directSyncPreview(t, f, input)
		if status != 422 {
			t.Fatal(status, body)
		}
	}
}

func TestDirectSyncConfigurationAndGraphChangesInvalidateSaveAndExecution(t *testing.T) {
	for _, change := range []string{"note null", "mode", "disabled", "new nested source"} {
		t.Run(change, func(t *testing.T) {
			f, source, _, input := directSyncFixture(t, "copy")
			status, plan, body := directSyncPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			status, job, body := directSyncSave(t, f, input, plan)
			if status != 201 {
				t.Fatal(status, body)
			}
			var err error
			switch change {
			case "note null":
				_, err = f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET NOTE='' WHERE ID=7`)
			case "mode":
				_, err = f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET MODE='移动' WHERE ID=7`)
			case "disabled":
				_, err = f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET ENABLED=0 WHERE ID=7`)
			case "new nested source":
				_, err = f.db.Exec(`INSERT INTO CONFIG_SYNC_PATHS VALUES(8,?,'','', '复制',0,0,1,'nested',0)`, filepath.Join(source, "nested"))
			}
			if err != nil {
				t.Fatal(err)
			}
			status, _, body = directSyncSave(t, f, input, plan)
			if status < 400 {
				t.Fatal(status, body)
			}
			status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
			if status != 409 {
				t.Fatal(status, body)
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, "misc/readme.txt"), "original-media")
		})
	}
}

func TestDirectSyncNewHistoryAndNewFilesRequireFreshReview(t *testing.T) {
	for _, change := range []string{"history", "source"} {
		t.Run(change, func(t *testing.T) {
			f, source, target, input := directSyncFixture(t, "copy")
			status, plan, body := directSyncPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			if change == "history" {
				if _, err := f.db.Exec(`CREATE TABLE SYNC_HISTORY(ID INTEGER PRIMARY KEY,PATH TEXT,SRC TEXT,DEST TEXT);INSERT INTO SYNC_HISTORY VALUES(1,?,?,?)`, filepath.Join(source, "misc/readme.txt"), source, target); err != nil {
					t.Fatal(err)
				}
			} else {
				organizationFile(t, source, "new.txt")
			}
			status, _, body = directSyncSave(t, f, input, plan)
			if status != 409 {
				t.Fatal(status, body)
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestDirectSyncGraphRejectsOverlapsLibrariesAndAliasLoops(t *testing.T) {
	for _, change := range []string{"nested target", "source in library", "other source at target", "unknown in source", "source alias target"} {
		t.Run(change, func(t *testing.T) {
			f, source, target, input := directSyncFixture(t, "copy")
			var err error
			switch change {
			case "nested target":
				_, err = f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET DEST=? WHERE ID=7`, filepath.Join(source, "new-target"))
			case "source in library":
				err = config.NewStore(f.path).Update(map[string]any{"media.movie_path": source})
			case "other source at target":
				_, err = f.db.Exec(`INSERT INTO CONFIG_SYNC_PATHS VALUES(8,?,?,'','复制',0,0,1,'loop',0)`, target, source)
			case "unknown in source":
				_, err = f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET UNKNOWN=? WHERE ID=7`, filepath.Join(source, "unknown"))
			case "source alias target":
				alias := filepath.Join(t.TempDir(), "alias")
				if err = os.Symlink(source, alias); err == nil {
					_, err = f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET DEST=? WHERE ID=7`, alias)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			status, _, body := directSyncPreview(t, f, input)
			if status != 422 {
				t.Fatal(status, body)
			}
		})
	}
}

func TestDirectSyncNeverOverwritesTargetsAndDoesNotRetryUncertainItems(t *testing.T) {
	f, source, target, input := directSyncFixture(t, "copy")
	organizationFile(t, target, "misc/readme.txt")
	status, plan, body := directSyncPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Source == "misc/readme.txt" && item.Status != "conflict" {
			t.Fatal(item)
		}
	}
	status, job, body := directSyncSave(t, f, input, plan)
	if status != 201 || len(job.Items) != 3 {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running' WHERE JOB_ID=? AND ORDINAL=0`, job.ID); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if status != 409 {
		t.Fatal(status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(target, "misc/readme.txt"), "original-media")
	assertOrganizationMoveBytes(t, filepath.Join(source, "misc/archive.bin"), "original-media")
}

func TestDirectSyncRecordListExactIDsAndUnsupportedOrUnconfiguredModes(t *testing.T) {
	f, source, target, input := directSyncFixture(t, "copy")
	if _, err := f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET ID=9223372036854775807 WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	input.ID = "9223372036854775807"
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/sync", f.token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"9223372036854775807"`) || !strings.Contains(w.Body.String(), `"nullFields":["note"]`) {
		t.Fatal(w.Code, w.Body.String())
	}
	status, _, body := directSyncPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, test := range []struct {
		sql    string
		status int
	}{
		{`UPDATE CONFIG_SYNC_PATHS SET RENAME=1`, 501},
		// Rclone now has a native implementation, but this fixture has no
		// reviewed account. It must reject without falling back to local I/O.
		{`UPDATE CONFIG_SYNC_PATHS SET RENAME=0,MODE='Rclone移动'`, 422},
		{`UPDATE CONFIG_SYNC_PATHS SET MODE='Minio移动'`, 501},
	} {
		if _, err := f.db.Exec(test.sql); err != nil {
			t.Fatal(err)
		}
		status, _, body = directSyncPreview(t, f, input)
		if status != test.status {
			t.Fatal(status, body)
		}
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatal("rejected preview wrote to a local target", entries, err)
	}
	for _, name := range []string{"misc/readme.txt", "misc/archive.bin"} {
		assertOrganizationMoveBytes(t, filepath.Join(source, name), "original-media")
	}
	for _, raw := range []string{`{"id":"07"}`, `{"id":"7","source":"/arbitrary"}`, `{"id":"7","Sync":{"actor":"forged"}}`} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/plan", f.token, raw)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestDirectSyncRejectsLinksAndRequiresAdminAndExclusiveRuntime(t *testing.T) {
	f, source, _, input := directSyncFixture(t, "copy")
	if err := os.Symlink(t.TempDir(), filepath.Join(source, "escape")); err != nil {
		t.Fatal(err)
	}
	input.Path = "escape"
	status, _, body := directSyncPreview(t, f, input)
	if status != 422 {
		t.Fatal(status, body)
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	for _, path := range []string{"/api/v1/organization/sync/plan", "/api/v1/organization/sync/jobs"} {
		for _, token := range []string{"", viewer} {
			w := nativeJSONRequest(f.handler, "POST", path, token, `{"id":"7","path":"."}`)
			want := 403
			if token == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
		}
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: f.path}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(handler, "POST", "/api/v1/organization/sync/plan", f.token, `{"id":"7","path":"."}`)
	if w.Code != 501 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestDirectSyncIgnoreBudgetAndCancellationAreNotOrdinaryFiltering(t *testing.T) {
	rules, err := compileDirectSyncRules(map[string]any{"ignored_files": "(a+)+$"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := rules.ignored(ctx, "/target/a.txt"); err == nil {
		t.Fatal("cancel treated as filtering")
	}
	if _, err := rules.ignored(t.Context(), "/target/"+strings.Repeat("a", 16000)+"!"); err == nil {
		t.Fatal("matching budget treated as filtering")
	}
}
