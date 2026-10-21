package httpserver

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func mixedLegacyFixture(t *testing.T, mode string, anime bool) (*subscriptionRunFixture, *organizationAPI, string, []string) {
	t.Helper()
	var f *subscriptionRunFixture
	var api *organizationAPI
	var source string
	var roots []string
	if organization.RemoteMode(mode) {
		var physical string
		f, api, source, physical, _ = legacyRemoteFixture(t, "unidentification", mode, false)
		for _, kind := range []string{"movies", "tv", "anime"} {
			root := filepath.Join(physical, kind)
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			roots = append(roots, root)
		}
		if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": "/rclone-auto/library/movies", "media.tv_path": "/rclone-auto/library/tv", "media.anime_path": "/rclone-auto/library/anime"}); err != nil {
			t.Fatal(err)
		}
	} else {
		f, source, _, _ = unknownFixture(t, "copy")
		roots = []string{t.TempDir(), t.TempDir(), t.TempDir()}
		for i := range roots {
			canonical, err := filepath.EvalSymlinks(roots[i])
			if err != nil {
				t.Fatal(err)
			}
			roots[i] = canonical
		}
		if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": roots[0], "media.tv_path": roots[1], "media.anime_path": roots[2]}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET DEST='',MODE=? WHERE ID=7`, organization.ModeLabel(mode)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Show.S01E01.1080p.mkv", "Show.S01E01.1080p.en.srt"} {
		organizationFile(t, source, name)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if anime {
		for _, name := range []string{"Anime.S01E01.1080p.mkv", "Anime.S01E01.1080p.en.srt"} {
			organizationFile(t, source, name)
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "tmdb.test" {
			t.Fatalf("legacy/external request %s", r.URL)
		}
		if r.URL.Path == "/3/search/tv" && strings.Contains(strings.ToLower(r.URL.Query().Get("query")), "anime") {
			return jsonResponse(r, `{"results":[{"id":300,"name":"Anime","first_air_date":"2026-01-01"}]}`), nil
		}
		if r.URL.Path == "/3/tv/300" {
			return jsonResponse(r, `{"id":300,"name":"Anime","original_name":"Anime","first_air_date":"2026-01-01","genres":[{"id":16}],"origin_country":["JP"],"seasons":[{"season_number":1,"episode_count":3}]}`), nil
		}
		if r.URL.Path == "/3/tv/300/season/1" {
			return jsonResponse(r, `{"episodes":[{"episode_number":1,"name":"Episode 1"},{"episode_number":2,"name":"Episode 2"},{"episode_number":3,"name":"Episode 3"}]}`), nil
		}
		return base.RoundTrip(r)
	})
	if api == nil {
		api = monitorRebuild(t, f)
	} else {
		api.service.client.Transport = f.transport
		api.recognition.service.client.Transport = f.transport
	}
	previous := f.handler
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/organization/legacy/batches/{id}", api.serveLegacyBatch)
	mux.HandleFunc("POST /api/v1/organization/legacy/batches/{id}/{action}", api.serveLegacyBatchAction)
	mux.Handle("/", previous)
	f.handler = mux
	return f, api, source, roots
}
func mixedLegacyPreview(t *testing.T, f *subscriptionRunFixture) legacyRedoReview {
	t.Helper()
	w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo/plan", map[string]any{"flag": "unidentification", "ids": []string{"7"}})
	var result struct{ Data legacyRedoReview }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Data.Items) != 1 || result.Data.Items[0].Plan == nil || len(result.Data.Items[0].Plan.Partitions) < 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	return result.Data
}
func mixedLegacyReserve(t *testing.T, api *organizationAPI) organization.LegacyBatch {
	t.Helper()
	p, f := api.legacyRedoPlan(t.Context(), "unidentification", 7, "admin")
	if f != nil {
		t.Fatal(f)
	}
	b, err := api.jobs.CreateLegacyBatch(t.Context(), p.Fingerprint, "admin", planDefinitions(p))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func legacyBatchAction(t *testing.T, f *subscriptionRunFixture, b organization.LegacyBatch, action string, extra map[string]any) (int, organization.LegacyBatch, string) {
	t.Helper()
	input := map[string]any{"confirm": true, "reviewFingerprint": b.ReviewFingerprint, "confirmOldExecutorsStopped": true, "confirmSourceRemoval": true, "confirmRemoteExclusive": true, "confirmRemoteRecoveryCopy": true, "reason": "reviewed test batch"}
	for k, v := range extra {
		input[k] = v
	}
	w := legacyCloudJSON(t, f, "/api/v1/organization/legacy/batches/"+b.ID+"/"+action, input)
	var result struct{ Data organization.LegacyBatch }
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w.Code, result.Data, w.Body.String()
}
func assertMixedBatch(t *testing.T, f *subscriptionRunFixture, api *organizationAPI, source string, roots []string, b organization.LegacyBatch, mode string, expected int) {
	t.Helper()
	if b.State != "completed" || len(b.Jobs) != expected || b.UnknownState != "resolved" || unknownState(t, f, 7) != "Y" {
		t.Fatal(b)
	}
	jobs, err := api.jobs.LegacyBatchJobs(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		if j.State != "completed" || j.LegacyBatch == nil || j.Mode != mode {
			t.Fatal(j)
		}
		if j.TargetRoot == roots[0] || organization.RemoteMode(mode) && strings.HasSuffix(j.TargetRoot, "/movies") {
			seen["movie"] = true
			for _, item := range j.Items {
				if !strings.HasPrefix(item.Source, "Movie.") {
					t.Fatal(j)
				}
			}
		} else if j.TargetRoot == roots[2] || organization.RemoteMode(mode) && strings.HasSuffix(j.TargetRoot, "/anime") {
			seen["anime"] = true
			for _, item := range j.Items {
				if !strings.HasPrefix(item.Source, "Anime.") {
					t.Fatal(j)
				}
			}
		} else {
			seen["tv"] = true
			for _, item := range j.Items {
				if !strings.HasPrefix(item.Source, "Show.") {
					t.Fatal(j)
				}
			}
		}
		physical := j.TargetRoot
		if organization.RemoteMode(mode) {
			if strings.HasSuffix(j.TargetRoot, "/movies") {
				physical = roots[0]
			} else if strings.HasSuffix(j.TargetRoot, "/anime") {
				physical = roots[2]
			} else {
				physical = roots[1]
			}
		}
		for _, item := range j.Items {
			assertOrganizationMoveBytes(t, filepath.Join(physical, item.Target), "original-media")
			if organization.MoveMode(mode) {
				if _, err := os.Lstat(filepath.Join(source, item.Source)); !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else {
				assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
			}
		}
	}
	if !seen["movie"] || !seen["tv"] || expected == 3 && !seen["anime"] {
		t.Fatal(seen)
	}
	var histories, links int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&histories); err != nil || histories != expected {
		t.Fatal(histories, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE RECORD_ID=7`).Scan(&links); err != nil || links != 1 {
		t.Fatal("owner link duplicated", links, err)
	}
}
func TestLegacyMixedTargetsExecuteAllNativeModesAsOneCompleteRecord(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, api, source, roots := mixedLegacyFixture(t, mode, false)
			review := mixedLegacyPreview(t, f)
			w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", map[string]any{"flag": "unidentification", "ids": []string{"7"}, "confirmSourceRemoval": true})
			if w.Code != 400 {
				t.Fatal(w.Code, w.Body.String())
			}
			input := map[string]any{"flag": "unidentification", "ids": []string{"7"}, "confirmSourceRemoval": organization.MoveMode(mode), "confirmOldExecutorsStopped": true, "reviewFingerprint": review.Fingerprint}
			w = legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", input)
			var result struct {
				Success bool
				Data    struct{ Results []legacyRedoResult }
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Success || result.Data.Results[0].BatchID == "" {
				t.Fatal(w.Code, w.Body.String())
			}
			b, err := api.jobs.GetLegacyBatch(t.Context(), result.Data.Results[0].BatchID)
			if err != nil {
				t.Fatal(err)
			}
			assertMixedBatch(t, f, api, source, roots, b, mode, 2)
			if status, _, body := legacyBatchAction(t, f, b, "reconcile", nil); status != 200 {
				t.Fatal(status, body)
			}
		})
	}
}
func TestRealRcloneLegacyMixedTargetsKeepAllRootsAndOneOriginalReceipt(t *testing.T) {
	for _, mode := range []string{"rclonecopy", "rclone"} {
		t.Run(mode, func(t *testing.T) {
			f, api, source, roots := mixedLegacyFixture(t, mode, true)
			review := mixedLegacyPreview(t, f)
			input := legacyCloudConsent(map[string]any{"flag": "unidentification", "ids": []string{"7"}, "confirmSourceRemoval": true, "reviewFingerprint": review.Fingerprint})
			w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", input)
			var result struct {
				Success bool
				Data    struct{ Results []legacyRedoResult }
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Success {
				t.Fatal(w.Code, w.Body.String())
			}
			b, err := api.jobs.GetLegacyBatch(t.Context(), result.Data.Results[0].BatchID)
			if err != nil {
				t.Fatal(err)
			}
			assertMixedBatch(t, f, api, source, roots, b, mode, 3)
		})
	}
}
func TestLegacyMixedPartialSuccessCannotResolveOrReplayAndReceiptsAreAtomic(t *testing.T) {
	f, api, source, _ := mixedLegacyFixture(t, "move", false)
	b := mixedLegacyReserve(t, api)
	if err := api.jobs.SetLegacyBatch(t.Context(), b.ID, "ready", "executing", "", b.ReviewFingerprint); err != nil {
		t.Fatal(err)
	}
	first, loadErr := api.jobs.Get(t.Context(), b.Jobs[0])
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if err := api.runJob(t.Context(), first, false); err != nil {
		t.Fatal(err)
	}
	first, loadErr = api.jobs.Get(t.Context(), first.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !organization.LegacyFilesDone(first) || first.State == "completed" || unknownState(t, f, 7) != "N" {
		t.Fatal("one target resolved a whole record", first)
	}
	if err := api.jobs.ResolveUnknown(t.Context(), first); err != nil || unknownState(t, f, 7) != "N" {
		t.Fatal(err)
	}
	if err := api.jobs.SetLegacyBatch(t.Context(), b.ID, "executing", "needs_review", "simulated interrupted request", ""); err != nil {
		t.Fatal(err)
	}
	b, _ = api.jobs.GetLegacyBatch(t.Context(), b.ID)
	if status, _, body := legacyBatchAction(t, f, b, "execute", nil); status != 409 {
		t.Fatal("replayed an interrupted batch", status, body)
	}
	if status, _, body := legacyBatchAction(t, f, b, "reconcile", nil); status != 409 {
		t.Fatal("partial batch reported success", status, body)
	}
	if status, _, body := organizationMoveRequest(t, f, b.Jobs[1], "execute"); status != 200 {
		t.Fatal(status, body)
	}
	if unknownState(t, f, 7) != "N" {
		t.Fatal("individual child resolved the parent")
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_parent BEFORE UPDATE OF STATE ON TRANSFER_UNKNOWN WHEN NEW.STATE='Y' BEGIN SELECT RAISE(ABORT,'test receipt'); END`); err != nil {
		t.Fatal(err)
	}
	b, _ = api.jobs.GetLegacyBatch(t.Context(), b.ID)
	if status, _, _ := legacyBatchAction(t, f, b, "reconcile", nil); status < 400 {
		t.Fatal("lost receipt committed")
	}
	if unknownState(t, f, 7) != "N" {
		t.Fatal("non-atomic legacy row update")
	}
	var pending int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE STATE='pending'`).Scan(&pending); err != nil || pending != 1 {
		t.Fatal(pending, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_parent`); err != nil {
		t.Fatal(err)
	}
	organizationFile(t, source, "Late.2026.mkv")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(source, "Late.2026.mkv"), old, old); err != nil {
		t.Fatal(err)
	}
	b, _ = api.jobs.GetLegacyBatch(t.Context(), b.ID)
	if status, _, _ := legacyBatchAction(t, f, b, "reconcile", nil); status != 409 {
		t.Fatal("unreviewed source joined compatibility completion", status)
	}
	if err := os.Remove(filepath.Join(source, "Late.2026.mkv")); err != nil {
		t.Fatal(err)
	}
	b, _ = api.jobs.GetLegacyBatch(t.Context(), b.ID)
	if status, done, body := legacyBatchAction(t, f, b, "reconcile", nil); status != 200 || done.State != "completed" {
		t.Fatal(status, body)
	}
	var histories int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&histories); err != nil || histories != 2 {
		t.Fatal("receipt retried file transfer", histories, err)
	}
}
func TestLegacyMixedStopKeepsTheOwnerClaimUntilEveryChildIsDisposed(t *testing.T) {
	f, api, _, _ := mixedLegacyFixture(t, "copy", false)
	b := mixedLegacyReserve(t, api)
	if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running' WHERE JOB_ID=? AND ORDINAL=0`, b.Jobs[1]); err != nil {
		t.Fatal(err)
	}
	if status, _, body := legacyBatchAction(t, f, b, "stop", nil); status != 200 {
		t.Fatal(status, body)
	}
	b, _ = api.jobs.GetLegacyBatch(t.Context(), b.ID)
	if status, _, body := organizationJobRequest(t, f, b.Jobs[0], "execute"); status != 409 {
		t.Fatal(status, body)
	}
	if status, _, _ := legacyBatchAction(t, f, b, "close", nil); status != 409 {
		t.Fatal("closed an uncertain child", status)
	}
	owner := organizationReadMoveJob(t, f, b.Jobs[0])
	if owner.State != "cancelled" || owner.UnknownState != "pending" || unknownState(t, f, 7) != "N" {
		t.Fatal(owner)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE RECORD_ID=7 AND STATE='pending'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("owner cancellation released sibling claim", count, err)
	}
	if _, err := api.jobs.Create(t.Context(), strings.Repeat("a", 64), owner.Definition); err == nil {
		t.Fatal("recreated an old batch member")
	}
}
func TestLegacyMixedReservationRollsBackEveryChildAndRecordClaim(t *testing.T) {
	f, api, _, _ := mixedLegacyFixture(t, "copy", false)
	p, failure := api.legacyRedoPlan(t.Context(), "unidentification", 7, "admin")
	if failure != nil || len(p.partitions) != 2 {
		t.Fatal(failure)
	}
	d := p.partitions[1].definition
	d.Unknown = nil
	d.UnknownScope = ""
	d.UnknownScopeIdentity = ""
	d.UnknownDirectory = false
	d.LegacyOrigin = nil
	d.LegacyScope = ""
	if _, err := api.jobs.Create(t.Context(), organization.Digest(d), d); err != nil {
		t.Fatal(err)
	}
	if _, err := api.jobs.CreateLegacyBatch(t.Context(), p.Fingerprint, "admin", planDefinitions(p)); err == nil {
		t.Fatal("claimed an occupied target")
	}
	var batches, claims, jobs int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM GO_LEGACY_ORGANIZATION_BATCHES`).Scan(&batches)
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_LINKS`).Scan(&claims)
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&jobs)
	if batches != 0 || claims != 0 || jobs != 1 {
		t.Fatal("partial reservation escaped transaction", batches, claims, jobs)
	}
}
func TestLegacyMixedRenameCustomAndHistoricalOriginsKeepTheirOwnScope(t *testing.T) {
	for _, entry := range []string{"rename", "renameudf", "history"} {
		t.Run(entry, func(t *testing.T) {
			f, api, source, _ := mixedLegacyFixture(t, "copy", false)
			if entry == "history" {
				if _, err := f.db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY,MODE TEXT,TYPE TEXT,CATEGORY TEXT,TMDBID INTEGER,TITLE TEXT,YEAR TEXT,SEASON_EPISODE TEXT,SOURCE TEXT,SOURCE_PATH TEXT,SOURCE_FILENAME TEXT,DEST TEXT,DEST_PATH TEXT,DEST_FILENAME TEXT,DATE TEXT);INSERT INTO TRANSFER_HISTORY VALUES(17,'复制','电影','',999,'Original','2020','','manual',?,?,'','/original','old.mkv','2020-01-01')`, filepath.Dir(source), filepath.Base(source)); err != nil {
					t.Fatal(err)
				}
				w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo/plan", map[string]any{"flag": "history", "ids": []string{"17"}})
				var review struct{ Data legacyRedoReview }
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil || review.Data.Items[0].Plan == nil || len(review.Data.Items[0].Plan.Partitions) != 2 {
					t.Fatal(w.Code, w.Body.String())
				}
				w = legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", map[string]any{"flag": "history", "ids": []string{"17"}, "reviewFingerprint": review.Data.Fingerprint, "confirmOldExecutorsStopped": true})
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) {
					t.Fatal(w.Code, w.Body.String())
				}
				row, err := api.jobs.GetHistory(t.Context(), 17)
				if err != nil || row.Title != "Original" || row.Dest != "" {
					t.Fatal(row, err)
				}
				var count int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 3 {
					t.Fatal(count, err)
				}
			} else {
				input := map[string]any{"syncmod": "copy"}
				if entry == "rename" {
					input["unknown_id"] = "7"
				} else {
					input["inpath"], input["outpath"] = source, ""
				}
				path := "/api/v1/organization/unknown/" + entry
				w := legacyCloudJSON(t, f, path+"/plan", input)
				var review struct {
					Data struct{ Plan organizationPlan }
				}
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil || len(review.Data.Plan.Partitions) != 2 {
					t.Fatal(w.Code, w.Body.String())
				}
				input["reviewFingerprint"], input["confirmOldExecutorsStopped"] = review.Data.Plan.Fingerprint, true
				w = legacyCloudJSON(t, f, path, input)
				var done struct {
					Data struct {
						BatchID string
						Jobs    []string
						State   string
					}
				}
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &done) != nil || done.Data.BatchID == "" || len(done.Data.Jobs) != 2 || done.Data.State != "completed" {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			if entry != "rename" && unknownState(t, f, 7) != "N" {
				t.Fatal("unrelated unknown row changed")
			}
		})
	}
}
func TestLegacyMixedDiscAndSeriesRemainWholeSourceObjects(t *testing.T) {
	for _, mode := range []string{"copy", "rclone"} {
		t.Run(mode, func(t *testing.T) {
			f, api, source, _ := mixedLegacyFixture(t, mode, false)
			for _, name := range []string{"Movie.2026.1080p.mkv", "Movie.2026.1080p.en.srt"} {
				if err := os.Remove(filepath.Join(source, name)); err != nil {
					t.Fatal(err)
				}
			}
			organizationDiscTree(t, source, "Movie.2026.1080p")
			review := mixedLegacyPreview(t, f)
			input := legacyCloudConsent(map[string]any{"flag": "unidentification", "ids": []string{"7"}, "reviewFingerprint": review.Fingerprint, "confirmSourceRemoval": true})
			w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", input)
			var result struct {
				Success bool
				Data    struct{ Results []legacyRedoResult }
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Success {
				for _, r := range result.Data.Results {
					if r.BatchID != "" {
						b, e := api.jobs.GetLegacyBatch(t.Context(), r.BatchID)
						t.Log("batch diagnostic", b.State, b.Reason, e)
						t.Log("reconciliation diagnostic", api.reconcileLegacyBatch(t.Context(), b.ID))
						files, scanErr := organization.Scan(t.Context(), source, ".")
						t.Log("source scan diagnostic", files, scanErr)
						nodes, readErr := os.ReadDir(source)
						for _, node := range nodes {
							t.Log("source node diagnostic", node.Name())
						}
						t.Log("source read diagnostic", readErr)
						for _, id := range b.Jobs {
							j, e := api.jobs.Get(t.Context(), id)
							t.Log("child diagnostic", id, j.State, e, api.validateJob(t.Context(), j))
							for _, item := range j.Items {
								t.Log("phase", item.Source, item.State, item.Reason, item.Proof.SourceHold)
							}
						}
					}
				}
				t.Fatal(w.Code, w.Body.String())
			}
			b, err := api.jobs.GetLegacyBatch(t.Context(), result.Data.Results[0].BatchID)
			if err != nil || b.State != "completed" || unknownState(t, f, 7) != "Y" {
				t.Fatal(b, err)
			}
			jobs, err := api.jobs.LegacyBatchJobs(t.Context(), b)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, j := range jobs {
				for _, item := range j.Items {
					if item.Disc != nil {
						found = true
						if organization.MoveMode(mode) {
							if _, err := os.Lstat(filepath.Join(source, filepath.Dir(item.Source), item.Proof.SourceHold)); !os.IsNotExist(err) {
								t.Fatal("completed local source hold was retained", err)
							}
						}
						if item.Disc.Files < 2 {
							t.Fatal(item)
						}
					}
				}
			}
			if !found {
				t.Fatal("disc fragments replaced a whole object")
			}
		})
	}
}
func TestLegacyMixedStopAndReauthorizationNeverAutoRunOrReuseStaleReview(t *testing.T) {
	f, api, source, _ := mixedLegacyFixture(t, "copy", false)
	b := mixedLegacyReserve(t, api)
	status, stopped, body := legacyBatchAction(t, f, b, "stop", nil)
	if status != 200 || stopped.State != "closing" {
		t.Fatal(status, body)
	}
	if ok, err := api.jobs.Claim(t.Context(), b.Jobs[0], 0); err == nil || ok {
		t.Fatal("revoked batch could claim new work", ok, err)
	}
	if status, _, _ := legacyBatchAction(t, f, b, "resume", nil); status != 400 {
		t.Fatal("stale scope review reopened authority", status)
	}
	status, resumed, body := legacyBatchAction(t, f, stopped, "resume", nil)
	if status != 200 || resumed.State != "needs_review" {
		t.Fatal(status, body)
	}
	for _, id := range resumed.Jobs {
		j := organizationReadMoveJob(t, f, id)
		for _, item := range j.Items {
			if item.State != "planned" {
				t.Fatal("reauthorization executed a file", item)
			}
		}
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
	status, stopped, body = legacyBatchAction(t, f, resumed, "stop", nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, closed, body := legacyBatchAction(t, f, stopped, "close", nil)
	if status != 200 || closed.State != "closed" || closed.UnknownState != "cancelled" || unknownState(t, f, 7) != "N" {
		t.Fatal(status, body)
	}
}
