package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func legacyRemoteFixture(t *testing.T, kind, mode string, disc bool) (*subscriptionRunFixture, *organizationAPI, string, string, organizationPlanInput) {
	t.Helper()
	var f *subscriptionRunFixture
	var source string
	var input organizationPlanInput
	if kind == "history" {
		f, source, _, input, _ = historyFixture(t, "copy")
	} else {
		f, source, _, input = unknownFixture(t, "copy")
	}
	monitorRebuild(t, f)
	api, logical, target := remoteAutomationHTTP(t, f)
	previous := f.handler
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/organization/unknown", api.serveUnknown)
	mux.HandleFunc("GET /api/v1/organization/history", api.serveHistoryRecords)
	mux.HandleFunc("POST /api/v1/organization/unknown/redo", api.serveLegacyRedo)
	mux.HandleFunc("POST /api/v1/organization/unknown/redo/plan", api.serveLegacyRedoPreview)
	mux.HandleFunc("POST /api/v1/organization/unknown/rename", api.serveLegacyRename)
	mux.HandleFunc("POST /api/v1/organization/unknown/rename/plan", api.serveLegacyRenamePreview)
	mux.HandleFunc("POST /api/v1/organization/unknown/renameudf", api.serveLegacyCustomRename)
	mux.HandleFunc("POST /api/v1/organization/unknown/renameudf/plan", api.serveLegacyCustomPreview)
	mux.Handle("/", previous)
	f.handler = mux
	input.Mode = mode
	input.TargetID = subscriptionResourceKey("target:" + logical + ":MOV")
	if disc {
		input.Path = "Movie.2026.1080p"
		organizationDiscTree(t, source, input.Path)
	}
	if kind == "history" {
		if _, err := f.db.Exec(`UPDATE TRANSFER_HISTORY SET MODE=?,DEST=?,DEST_PATH=? WHERE ID=7`, organization.ModeLabel(mode), logical, filepath.Join(logical, "Old")); err != nil {
			t.Fatal(err)
		}
		organizationFile(t, target, "Old/Wrong.mkv")
		record, err := api.jobs.GetHistory(t.Context(), 7)
		if err != nil {
			t.Fatal(err)
		}
		input.HistoryRecord = &organizationHistorySelector{ID: 7, Fingerprint: organization.Digest(record)}
	} else {
		if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET PATH=?,DEST=?,MODE=? WHERE ID=7`, filepath.Join(source, input.Path), logical, organization.ModeLabel(mode)); err != nil {
			t.Fatal(err)
		}
		record, err := api.jobs.GetUnknown(t.Context(), 7)
		if err != nil {
			t.Fatal(err)
		}
		input.UnknownRecord = &organizationUnknownSelector{ID: 7, Fingerprint: organization.Digest(record)}
	}
	return f, api, source, target, input
}
func legacyCloudConsent(input map[string]any) map[string]any {
	result := map[string]any{}
	for k, v := range input {
		result[k] = v
	}
	result["confirmRemoteExclusive"], result["confirmRemoteRecoveryCopy"], result["confirmOldExecutorsStopped"] = true, true, true
	return result
}
func legacyCloudJSON(t *testing.T, f *subscriptionRunFixture, path string, input any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return nativeJSONRequest(f.handler, "POST", path, f.token, string(raw))
}
func remoteRedoReview(t *testing.T, f *subscriptionRunFixture, kind string) legacyRedoReview {
	t.Helper()
	w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo/plan", map[string]any{"flag": kind, "ids": []string{"7"}})
	var result struct{ Data legacyRedoReview }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Data.Items) != 1 || result.Data.Items[0].Plan == nil || result.Data.Items[0].Plan.Remote == nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ConfigFile") || strings.Contains(w.Body.String(), "ConfigDigest") {
		t.Fatal("private binding exposed", w.Body.String())
	}
	return result.Data
}
func TestRealRcloneLegacyNativeProvenanceAndRedoKeepOriginalRowsAndLiteralModes(t *testing.T) {
	for _, entry := range []string{"native", "redo"} {
		for _, kind := range []string{"unidentification", "history"} {
			for _, mode := range []string{"rclonecopy", "rclone"} {
				t.Run(entry+"/"+kind+"/"+mode, func(t *testing.T) {
					f, api, source, target, input := legacyRemoteFixture(t, kind, mode, false)
					var original string
					if kind == "history" {
						row, err := api.jobs.GetHistory(t.Context(), 7)
						if err != nil {
							t.Fatal(err)
						}
						original = organization.Digest(row)
					}
					var id string
					if entry == "native" {
						status, plan, body := organizationPreview(t, f, input)
						if status != 200 || plan.Remote == nil {
							t.Fatal(status, body)
						}
						job := organizationCreateJob(t, f, input, plan)
						id = job.ID
						status, done, body := remoteHTTPAction(t, f, id, "execute")
						if status != 200 || done.State != "completed" {
							t.Fatal(status, body)
						}
					} else {
						review := remoteRedoReview(t, f, kind)
						base := map[string]any{"flag": kind, "ids": []string{"7"}, "reviewFingerprint": review.Fingerprint, "confirmSourceRemoval": mode == "rclone"}
						for _, key := range []string{"reviewFingerprint", "confirmRemoteExclusive", "confirmRemoteRecoveryCopy", "confirmOldExecutorsStopped"} {
							bad := legacyCloudConsent(base)
							delete(bad, key)
							w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", bad)
							if w.Code != 400 {
								t.Fatal(key, w.Code, w.Body.String())
							}
						}
						if mode == "rclone" {
							bad := legacyCloudConsent(base)
							bad["confirmSourceRemoval"] = false
							w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", bad)
							if w.Code != 400 {
								t.Fatal(w.Code, w.Body.String())
							}
						}
						var jobs int
						if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&jobs); err != nil || jobs != 0 {
							t.Fatal("consent rejection reserved jobs", jobs, err)
						}
						w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", legacyCloudConsent(base))
						var result struct {
							Success bool
							Data    struct{ Results []legacyRedoResult }
						}
						if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Success || len(result.Data.Results) != 1 || result.Data.Results[0].State != "completed" {
							t.Fatal(w.Code, w.Body.String())
						}
						id = result.Data.Results[0].JobID
					}
					job := organizationReadMoveJob(t, f, id)
					if job.Mode != mode || job.Remote == nil {
						t.Fatal(job)
					}
					for _, item := range job.Items {
						assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
						if mode == "rclone" {
							if _, err := os.Lstat(filepath.Join(source, item.Source)); !os.IsNotExist(err) {
								t.Fatal(err)
							}
						} else {
							assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
						}
					}
					if kind == "unidentification" {
						if unknownState(t, f, 7) != "Y" || job.UnknownState != "resolved" {
							t.Fatal(job)
						}
						row, err := api.jobs.GetUnknown(t.Context(), 7)
						if err != nil || row.Mode != organization.ModeLabel(mode) {
							t.Fatal(row, err)
						}
					} else {
						row, err := api.jobs.GetHistory(t.Context(), 7)
						if err != nil || organization.Digest(row) != original {
							t.Fatal("original history changed", row, err)
						}
						assertOrganizationMoveBytes(t, filepath.Join(target, "Old/Wrong.mkv"), "original-media")
					}
					if entry == "redo" {
						review := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", legacyCloudConsent(map[string]any{"flag": kind, "ids": []string{"7"}, "confirmSourceRemoval": true}))
						if strings.Contains(review.Body.String(), `"success":true`) {
							t.Fatal("unreviewed/repeated transfer succeeded", review.Body.String())
						}
					}
				})
			}
		}
	}
}
func TestRealRcloneLegacyRenameCustomAndPartialScopeRemainIndependent(t *testing.T) {
	for _, entry := range []string{"rename-unknown", "rename-history", "custom", "partial"} {
		for _, mode := range []string{"rclonecopy", "rclone"} {
			t.Run(entry+"/"+mode, func(t *testing.T) {
				kind := "unidentification"
				if entry == "rename-history" {
					kind = "history"
				}
				f, api, source, target, _ := legacyRemoteFixture(t, kind, "rclonecopy", false)
				input := map[string]any{"syncmod": mode, "type": "MOV", "tmdb": "100", "confirmSourceRemoval": mode == "rclone"}
				action := "rename"
				if entry == "custom" {
					action = "renameudf"
					input["inpath"], input["outpath"] = source, ""
				} else if kind == "history" {
					input["logid"] = "7"
				} else {
					input["unknown_id"] = "7"
				}
				if entry == "partial" {
					renameTVFiles(t, source)
					input["type"], input["tmdb"], input["season"] = "TV", "200", "1"
					input["episode_format"], input["episode_details"], input["episode_offset"] = "Show.{ep}.mkv", "11,11", "-10"
					if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET PATH=? WHERE ID=7`, filepath.Join(source, "Show.11.mkv")); err != nil {
						t.Fatal(err)
					}
					w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/rename/plan", input)
					if w.Code != 400 {
						t.Fatal(w.Code, w.Body.String())
					}
					input["confirmParentScope"] = true
				}
				path := "/api/v1/organization/unknown/" + action
				w := legacyCloudJSON(t, f, path+"/plan", input)
				var review struct {
					Data struct{ Plan organizationPlan }
				}
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil || review.Data.Plan.Remote == nil {
					t.Fatal(w.Code, w.Body.String())
				}
				for _, key := range []string{"reviewFingerprint", "confirmRemoteExclusive", "confirmRemoteRecoveryCopy", "confirmOldExecutorsStopped"} {
					bad := legacyCloudConsent(input)
					bad["reviewFingerprint"] = review.Data.Plan.Fingerprint
					delete(bad, key)
					w := legacyCloudJSON(t, f, path, bad)
					if w.Code != 400 {
						t.Fatal(key, w.Code, w.Body.String())
					}
				}
				input = legacyCloudConsent(input)
				input["reviewFingerprint"] = review.Data.Plan.Fingerprint
				// Exercise the old form decoder as well as JSON without authorizing a shell.
				form := url.Values{}
				for k, v := range input {
					raw, _ := json.Marshal(v)
					if value, ok := v.(string); ok {
						form.Set(k, value)
					} else {
						form.Set(k, string(raw))
					}
				}
				w = performFormRequest(f.handler, path, f.token, form)
				var done struct{ Data struct{ JobID string } }
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &done) != nil || done.Data.JobID == "" {
					t.Fatal(w.Code, w.Body.String())
				}
				job := organizationReadMoveJob(t, f, done.Data.JobID)
				if job.State != "completed" || job.Mode != mode || job.LegacyOrigin == nil || job.LegacySelection == nil {
					t.Fatal(job)
				}
				for _, item := range job.Items {
					assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
				}
				if entry == "partial" {
					if job.UnknownState != "preserved" || unknownState(t, f, 7) != "N" || !job.LegacyRename.ExpandedScope {
						t.Fatal(job)
					}
					assertOrganizationMoveBytes(t, filepath.Join(source, "Show.12.mkv"), "original-media")
				} else if entry == "rename-unknown" {
					if unknownState(t, f, 7) != "Y" {
						t.Fatal(job)
					}
				} else if entry == "custom" {
					if job.UnknownRecordID != 0 || unknownState(t, f, 7) != "N" {
						t.Fatal("custom attached a legacy row", job)
					}
				}
				if kind == "history" {
					row, err := api.jobs.GetHistory(t.Context(), 7)
					if err != nil || row.Title != "Wrong" || row.Mode != "Rclone复制" {
						t.Fatal(row, err)
					}
					assertOrganizationMoveBytes(t, filepath.Join(target, "Old/Wrong.mkv"), "original-media")
				}
			})
		}
	}
}
func TestRealRcloneLegacyWholeDiscUnknownClosureIncludesSourceRemoval(t *testing.T) {
	for _, mode := range []string{"rclonecopy", "rclone"} {
		t.Run(mode, func(t *testing.T) {
			f, _, source, target, input := legacyRemoteFixture(t, "unidentification", mode, true)
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 || len(plan.Items) != 1 || plan.Items[0].Disc == nil {
				t.Fatal(status, body)
			}
			job := organizationCreateJob(t, f, input, plan)
			status, done, body := remoteHTTPAction(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" || done.UnknownState != "resolved" || unknownState(t, f, 7) != "Y" {
				t.Fatal(status, body)
			}
			if _, err := os.Stat(filepath.Join(target, done.Items[0].Target, "BDMV/index.bdmv")); err != nil {
				t.Fatal(err)
			}
			if mode == "rclone" {
				if _, err := os.Lstat(filepath.Join(source, input.Path)); !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestRealRcloneLegacyScopeAndAccountChangesNeverReserveOrAdoptUncertainWork(t *testing.T) {
	for _, change := range []string{"account", "old-row", "running"} {
		t.Run(change, func(t *testing.T) {
			f, api, source, target, input := legacyRemoteFixture(t, "unidentification", "rclonecopy", false)
			if change == "running" {
				status, plan, body := organizationPreview(t, f, input)
				if status != 200 {
					t.Fatal(status, body)
				}
				job := organizationCreateJob(t, f, input, plan)
				if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET STATE='running' WHERE JOB_ID=?`, job.ID); err != nil {
					t.Fatal(err)
				}
				status, _, body = remoteHTTPAction(t, f, job.ID, "execute")
				if status != 409 {
					t.Fatal(status, body)
				}
			} else {
				review := remoteRedoReview(t, f, "unidentification")
				if change == "account" {
					data, err := os.ReadFile(api.rclone.ConfigFile)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(api.rclone.ConfigFile, append(data, []byte("\n# replaced profile\n")...), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET MODE='Rclone移动' WHERE ID=7`); err != nil {
						t.Fatal(err)
					}
				}
				w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", legacyCloudConsent(map[string]any{"flag": "unidentification", "ids": []string{"7"}, "reviewFingerprint": review.Fingerprint, "confirmSourceRemoval": true}))
				if w.Code != 409 {
					t.Fatal(w.Code, w.Body.String())
				}
				var count int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
					t.Fatal(count, err)
				}
			}
			if files, err := os.ReadDir(target); err != nil || len(files) != 0 {
				t.Fatal("rejected operation wrote cloud data", files, err)
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
			if unknownState(t, f, 7) != "N" {
				t.Fatal("changed original row state")
			}
		})
	}
}
func TestRealRcloneLegacyUnknownReceiptRechecksFullRemoteContentWithoutRetransfer(t *testing.T) {
	f, api, _, target, input := legacyRemoteFixture(t, "unidentification", "rclonecopy", false)
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	status, done, body := remoteHTTPAction(t, f, job.ID, "execute")
	if status != 200 || done.State != "completed" {
		t.Fatal(status, body)
	}
	// Simulate a missing compatibility receipt, never a missing file receipt.
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET STATE='N' WHERE ID=7;UPDATE GO_ORGANIZATION_UNKNOWN_LINKS SET STATE='pending' WHERE JOB_ID=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(target, done.Items[0].Target)
	if err := os.WriteFile(file, []byte("changed-target"), 0600); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationJobRequest(t, f, job.ID, "reconcile-unknown")
	if status != 409 || unknownState(t, f, 7) != "N" {
		t.Fatal(status, body)
	}
	if err := os.WriteFile(file, []byte("original-media"), 0600); err != nil {
		t.Fatal(err)
	}
	status, receipt, body := organizationJobRequest(t, f, job.ID, "reconcile-unknown")
	if status != 200 || receipt.UnknownState != "resolved" {
		t.Fatal(status, body)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal("replayed transfer history", count, err)
	}
	row, err := api.jobs.GetUnknown(t.Context(), 7)
	if err != nil || row.Mode != "Rclone复制" || row.State != "Y" {
		t.Fatal(row, err)
	}
}
func TestLegacyRemoteTargetsDoNotInheritLocalSymlinkAliasAuthority(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	physical, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	roots := organizationRoots{Sources: []organizationRoot{{ID: "source", Path: source}}, Targets: []organizationRoot{{ID: "target", Path: alias}}}
	// Native history stores the canonical physical destination. On macOS the
	// temporary /var path itself aliases /private/var; do not invent a second
	// noncanonical historical alias that native history never promised to adopt.
	record := organization.UnknownRecord{ID: 7, Path: source, Dest: physical, Mode: "Rclone复制", State: "N"}
	if item := projectUnknown(record, roots); item.Reason == "" || item.TargetID != "" {
		t.Fatal("cloud inherited local alias", item)
	}
	record.Mode = "复制"
	if item := projectUnknown(record, roots); item.Reason != "" || item.TargetID != "target" {
		t.Fatal("native alias regressed", item)
	}
}
func TestRealRcloneLegacyProcessedDefaultAndExactIDsRemainOriginal(t *testing.T) {
	f, api, source, target, _ := legacyRemoteFixture(t, "unidentification", "rclonecopy", false)
	const id = "9223372036854775807"
	if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET ID=9223372036854775807,STATE='Y',MODE='',DEST='' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"pt.rmt_mode": "rclonecopy"}); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"flag": "unidentification", "ids": []string{id}}
	w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo/plan", request)
	var review struct{ Data legacyRedoReview }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil || len(review.Data.Items) != 1 || review.Data.Items[0].ID != id || review.Data.Items[0].Plan == nil {
		t.Fatal(w.Code, w.Body.String())
	}
	request = legacyCloudConsent(request)
	request["reviewFingerprint"] = review.Data.Fingerprint
	w = legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", request)
	var result struct {
		Success bool
		Data    struct{ Results []legacyRedoResult }
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Success || result.Data.Results[0].ID != id {
		t.Fatal(w.Code, w.Body.String())
	}
	row, err := api.jobs.GetUnknown(t.Context(), 9223372036854775807)
	if err != nil || row.Mode != "" || row.Dest != "" || row.State != "Y" {
		t.Fatal(row, err)
	}
	job := organizationReadMoveJob(t, f, result.Data.Results[0].JobID)
	if job.LegacyOrigin.RecordID != id || job.Mode != "rclonecopy" {
		t.Fatal(job)
	}
	for _, item := range job.Items {
		assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
		assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
	}
}
func TestLegacyDefaultIdentificationModeAndEmptyDestinationRemainConfigured(t *testing.T) {
	f, source, _, _, _ := organizationLinkFixture(t, "copy")
	if err := config.NewStore(f.path).Update(map[string]any{"pt.rmt_mode": "copy"}); err != nil {
		t.Fatal(err)
	}
	w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/renameudf/plan", map[string]any{"inpath": source, "tmdb": "100", "type": "MOV"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
