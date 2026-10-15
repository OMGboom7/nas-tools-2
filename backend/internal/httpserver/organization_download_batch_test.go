package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type downloadBatchWire struct {
	opts                   *completionTestOptions
	files                  []string
	removed                bool
	markCalls, removeCalls int
	lostAck                bool
	beforeMutation         func()
}

func downloadBatchFixture(t *testing.T, kind, mode string, modern bool) (*subscriptionRunFixture, string, string, *downloadBatchWire) {
	t.Helper()
	opts := &completionTestOptions{kind: kind, modern: modern, allowMetadata: true}
	if mode == "move" {
		if kind == "qbittorrent" {
			opts.state = "pausedUP"
		}
		if kind == "transmission" {
			opts.state = "stopped"
		}
	}
	f, source, target := completionHTTPFixture(t, opts)
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET RMT_MODE=? WHERE ID=1`, organization.ModeLabel(mode)); err != nil {
		t.Fatal(err)
	}
	wire := &downloadBatchWire{opts: opts, files: []string{"Movie.2026.1080p.mkv", "Movie.2026.1080p.en.srt"}}
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "completion.test" {
			return base.RoundTrip(r)
		}
		var payload struct {
			Method    string                     `json:"method"`
			ID        json.RawMessage            `json:"id"`
			Arguments map[string]json.RawMessage `json:"arguments"`
			Params    json.RawMessage            `json:"params"`
		}
		if kind != "qbittorrent" {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if json.Unmarshal(body, &payload) != nil {
				t.Fatal("request")
			}
		}
		trReply := func(arguments any) *http.Response {
			env := map[string]any{"result": "success", "arguments": arguments}
			if modern {
				env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": arguments}
			}
			raw, _ := json.Marshal(env)
			return jsonResponse(r, string(raw))
		}
		if kind == "transmission" && r.Header.Get("X-Transmission-Session-Id") == "" {
			resp := jsonResponse(r, "")
			resp.StatusCode = 409
			resp.Header.Set("X-Transmission-Session-Id", "session")
			if modern {
				resp.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
			}
			return resp, nil
		}
		mutation := false
		remove := false
		if kind == "qbittorrent" && (strings.HasSuffix(r.URL.Path, "/addTags") || strings.HasSuffix(r.URL.Path, "/delete")) {
			mutation = true
			_ = r.ParseForm()
			if r.Method != "POST" || r.Form.Get("hashes") != completionHash {
				t.Fatal("widened mutation", r.Form)
			}
			if strings.HasSuffix(r.URL.Path, "/delete") {
				remove = true
				if r.Form.Get("deleteFiles") != "false" {
					t.Fatal("downloader must never delete local data", r.Form)
				}
			} else {
				if r.Form.Get("tags") != "已整理" {
					t.Fatal(r.Form)
				}
				if !containsDownloadLabel(opts.labels, "已整理") {
					opts.labels = append(opts.labels, "已整理")
				}
			}
		}
		if kind == "transmission" && (payload.Method == "torrent-set" || payload.Method == "torrent_set" || payload.Method == "torrent-remove" || payload.Method == "torrent_remove") {
			mutation = true
			args := payload.Arguments
			if modern {
				if json.Unmarshal(payload.Params, &args) != nil {
					t.Fatal("params")
				}
			}
			var ids []string
			_ = json.Unmarshal(args["ids"], &ids)
			if len(ids) != 1 || ids[0] != completionHash {
				t.Fatal("scope", ids)
			}
			if strings.Contains(payload.Method, "remove") {
				remove = true
				key := "delete-local-data"
				if modern {
					key = "delete_local_data"
				}
				var flag *bool
				if json.Unmarshal(args[key], &flag) != nil || flag == nil || *flag {
					t.Fatal("delete local data", args)
				}
			} else {
				var labels []string
				if json.Unmarshal(args["labels"], &labels) != nil || !containsDownloadLabel(labels, "NASTOOL") || !containsDownloadLabel(labels, "已整理") {
					t.Fatal("labels", labels)
				}
				opts.labels = labels
			}
		}
		if kind == "aria2" && payload.Method == "aria2.removeDownloadResult" {
			mutation = true
			remove = true
			var params []string
			if json.Unmarshal(payload.Params, &params) != nil || len(params) != 2 || params[1] != completionGID {
				t.Fatal("gid scope", params)
			}
		}
		if mutation {
			var state, proof string
			if err := f.db.QueryRow(`SELECT STATE,RESULT_PROOF FROM GO_DOWNLOAD_ORGANIZATION_BATCHES`).Scan(&state, &proof); err != nil || state != "result_pending" || !strings.Contains(proof, `"actor"`) {
				t.Fatal("intent missing before outbound write", state, proof, err)
			}
			if wire.beforeMutation != nil {
				wire.beforeMutation()
			}
			if remove {
				wire.removeCalls++
				wire.removed = true
			} else {
				wire.markCalls++
			}
			if wire.lostAck {
				return nil, errors.New("lost mutation response after acceptance")
			}
			if kind == "qbittorrent" {
				return jsonResponse(r, ""), nil
			}
			if kind == "transmission" {
				return trReply(map[string]any{}), nil
			}
			return jsonResponse(r, `{"jsonrpc":"2.0","id":"nastool-organized-result","result":"OK"}`), nil
		}
		if kind == "qbittorrent" && strings.HasSuffix(r.URL.Path, "/info") && (wire.removed || r.URL.Query().Get("filter") == "all") {
			if wire.removed {
				return jsonResponse(r, `[]`), nil
			}
			return jsonResponse(r, `[{"hash":"`+completionHash+`"}]`), nil
		}
		if kind == "transmission" && wire.removed && (payload.Method == "torrent-get" || payload.Method == "torrent_get") {
			return trReply(map[string]any{"torrents": []any{}}), nil
		}
		if kind == "aria2" && (payload.Method == "aria2.tellActive" || payload.Method == "aria2.tellWaiting" || payload.Method == "aria2.tellStopped") {
			tasks := []any{}
			if payload.Method == "aria2.tellStopped" && !wire.removed {
				tasks = append(tasks, map[string]string{"gid": completionGID})
			}
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": payload.ID, "result": tasks})
			return jsonResponse(r, string(raw)), nil
		}
		resp, err := base.RoundTrip(r)
		if err != nil || resp.StatusCode != 200 {
			return resp, err
		}
		if kind == "qbittorrent" && !strings.HasSuffix(r.URL.Path, "/files") {
			return resp, nil
		}
		if kind == "aria2" && payload.Method != "aria2.tellStatus" {
			return resp, nil
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var env map[string]any
		if kind != "qbittorrent" && json.Unmarshal(data, &env) != nil {
			t.Fatal(string(data))
		}
		files := []any{}
		stats := []any{}
		for _, name := range wire.files {
			if kind == "qbittorrent" {
				files = append(files, map[string]any{"name": name, "size": 14, "progress": 1, "priority": 1})
			} else if kind == "transmission" {
				key := "bytesCompleted"
				if modern {
					key = "bytes_completed"
				}
				files = append(files, map[string]any{"name": name, "length": 14, key: 14})
				stats = append(stats, map[string]any{"wanted": true, key: 14})
			} else {
				files = append(files, map[string]any{"path": opts.directory + "/" + name, "length": "14", "completedLength": "14", "selected": "true"})
			}
		}
		if kind == "qbittorrent" {
			files = append(files, map[string]any{"name": "unselected.mkv", "size": 100, "progress": 0, "priority": 0})
			data, _ = json.Marshal(files)
		} else if kind == "transmission" {
			args := env["arguments"]
			key := "fileStats"
			if modern {
				args = env["result"]
				key = "file_stats"
			}
			row := args.(map[string]any)["torrents"].([]any)[0].(map[string]any)
			row["files"] = files
			row[key] = stats
			data, _ = json.Marshal(env)
		} else {
			row := env["result"].(map[string]any)
			row["files"] = files
			row["totalLength"] = strconv.Itoa(14 * len(wire.files))
			row["completedLength"] = row["totalLength"]
			data, _ = json.Marshal(env)
		}
		resp.Body = io.NopCloser(bytes.NewReader(data))
		return resp, nil
	})
	monitorRebuild(t, f)
	return f, source, target, wire
}
func containsDownloadLabel(labels []string, value string) bool {
	for _, v := range labels {
		if v == value {
			return true
		}
	}
	return false
}
func downloadTaskID(kind string) string {
	if kind == "aria2" {
		return completionGID
	}
	return completionHash
}
func downloadPlanHTTP(t *testing.T, f *subscriptionRunFixture, kind string) (int, downloadOrganizationReview, string) {
	t.Helper()
	raw, _ := json.Marshal(downloadCompletionInput{DownloaderID: "1", TaskID: downloadTaskID(kind)})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/plan", f.token, string(raw))
	var reply struct{ Data downloadOrganizationReview }
	_ = json.Unmarshal(w.Body.Bytes(), &reply)
	return w.Code, reply.Data, w.Body.String()
}
func downloadBatchSaveHTTP(t *testing.T, f *subscriptionRunFixture, kind string, review downloadOrganizationReview, omitted bool) (int, organization.DownloadBatch, string) {
	t.Helper()
	raw, _ := json.Marshal(downloadBatchRequest{downloadCompletionInput: downloadCompletionInput{DownloaderID: "1", TaskID: downloadTaskID(kind)}, Fingerprint: review.Fingerprint, Confirm: true, ConfirmOldExecutorsStopped: true, ConfirmOmitted: omitted})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/batches", f.token, string(raw))
	var reply struct{ Data organization.DownloadBatch }
	_ = json.Unmarshal(w.Body.Bytes(), &reply)
	return w.Code, reply.Data, w.Body.String()
}
func saveDownloadFixtureBatch(t *testing.T, f *subscriptionRunFixture, kind string) organization.DownloadBatch {
	t.Helper()
	status, review, body := downloadPlanHTTP(t, f, kind)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, b, body := downloadBatchSaveHTTP(t, f, kind, review, true)
	if status != 201 || b.State != "ready" || len(b.Jobs) != b.Expected {
		t.Fatal(status, body)
	}
	return b
}
func downloadBatchActionHTTP(t *testing.T, f *subscriptionRunFixture, b organization.DownloadBatch, action string, input downloadBatchRequest) (int, organization.DownloadBatch, string) {
	t.Helper()
	raw, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/batches/"+b.ID+"/"+action, f.token, string(raw))
	var reply struct{ Data organization.DownloadBatch }
	_ = json.Unmarshal(w.Body.Bytes(), &reply)
	return w.Code, reply.Data, w.Body.String()
}
func downloadResultHTTP(t *testing.T, f *subscriptionRunFixture, b organization.DownloadBatch) (int, downloadResultReview, string) {
	t.Helper()
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/batches/"+b.ID+"/result-review", f.token, `{}`)
	var reply struct{ Data downloadResultReview }
	_ = json.Unmarshal(w.Body.Bytes(), &reply)
	return w.Code, reply.Data, w.Body.String()
}
func TestDownloadOrganizationAllLocalModesAndProtocolsExecuteThenFinalizeWithoutDeletingLeftovers(t *testing.T) {
	for _, spec := range []struct {
		kind   string
		modern bool
	}{{"qbittorrent", false}, {"transmission", false}, {"transmission", true}, {"aria2", false}} {
		for _, mode := range []string{"copy", "link", "softlink", "move"} {
			t.Run(spec.kind+mode+map[bool]string{true: "modern"}[spec.modern], func(t *testing.T) {
				f, source, target, wire := downloadBatchFixture(t, spec.kind, mode, spec.modern)
				organizationFile(t, source, "notes.nfo")
				old := time.Now().Add(-time.Hour)
				_ = os.Chtimes(filepath.Join(source, "notes.nfo"), old, old)
				wire.files = append(wire.files, "notes.nfo")
				organizationFile(t, source, "foreign-task.mkv")
				status, preview, body := downloadPlanHTTP(t, f, spec.kind)
				if status != 200 || len(preview.Plans) != 1 || len(preview.Omitted) != 1 {
					t.Fatal(status, body)
				}
				status, _, body = downloadBatchSaveHTTP(t, f, spec.kind, preview, false)
				if status != 409 {
					t.Fatal("missing omitted consent", status, body)
				}
				status, b, body := downloadBatchSaveHTTP(t, f, spec.kind, preview, true)
				if status != 201 || b.State != "ready" {
					t.Fatal(status, body)
				}
				if entries, _ := os.ReadDir(target); len(entries) != 0 {
					t.Fatal("save executed", entries)
				}
				job, err := f.runner.organizer.jobs.Get(context.Background(), b.Jobs[0])
				if err != nil || len(job.Items) != 2 || job.Download == nil || job.Download.BatchID != b.ID {
					t.Fatal(job, err)
				}
				status, b2, body := downloadBatchSaveHTTP(t, f, spec.kind, preview, true)
				if status != 200 || b2.ID != b.ID {
					t.Fatal("reservation replay", status, body)
				}
				if mode == "move" {
					status, _, body = downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
					if status != 409 {
						t.Fatal("move consent", status, body)
					}
				}
				status, b, body = downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true, ConfirmSourceRemoval: true, ConfirmTaskStopped: true})
				if status != 200 || b.State != "files_done" || wire.markCalls+wire.removeCalls != 0 {
					t.Fatal(status, body)
				}
				job, err = f.runner.organizer.jobs.Get(context.Background(), b.Jobs[0])
				if err != nil || job.State != "completed" {
					t.Fatal(job, err)
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
					if mode == "link" {
						a, _ := os.Stat(filepath.Join(source, item.Source))
						c, _ := os.Stat(filepath.Join(target, item.Target))
						if !os.SameFile(a, c) {
							t.Fatal("not a hard link")
						}
					}
					if mode == "softlink" {
						info, _ := os.Lstat(filepath.Join(target, item.Target))
						if info.Mode()&os.ModeSymlink == 0 {
							t.Fatal("not a symlink")
						}
					}
				}
				assertOrganizationMoveBytes(t, filepath.Join(source, "notes.nfo"), "original-media")
				assertOrganizationMoveBytes(t, filepath.Join(source, "foreign-task.mkv"), "original-media")
				status, result, body := downloadResultHTTP(t, f, b)
				if status != 200 || result.Actor == "" {
					t.Fatal(status, body)
				}
				consent := downloadBatchRequest{Confirm: true, Fingerprint: result.Fingerprint, ConfirmTaskRemoval: true, ConfirmTaskStopped: true, ConfirmExclusiveLabels: true}
				if spec.kind == "transmission" && mode != "move" {
					bad := consent
					bad.ConfirmExclusiveLabels = false
					status, _, body = downloadBatchActionHTTP(t, f, b, "finalize", bad)
					if status != 409 {
						t.Fatal("exclusive labels", status, body)
					}
				}
				status, b, body = downloadBatchActionHTTP(t, f, b, "finalize", consent)
				if status != 200 || b.State != "completed" || wire.markCalls+wire.removeCalls != 1 || !strings.Contains(b.ResultProof, `"actor"`) {
					t.Fatal(status, body)
				}
				assertOrganizationMoveBytes(t, filepath.Join(source, "notes.nfo"), "original-media")
				var count int
				if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE SOURCE='下载完成整理'`).Scan(&count); err != nil || count != 1 {
					t.Fatal(count, err)
				}
				status, _, _ = downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true, ConfirmSourceRemoval: true, ConfirmTaskStopped: true})
				if status != 409 || wire.markCalls+wire.removeCalls != 1 {
					t.Fatal("completed batch replay", status)
				}
			})
		}
	}
}
func TestDownloadOrganizationLostResultResponseReconcilesWithoutRepeatingAnyMutation(t *testing.T) {
	for _, kind := range []string{"qbittorrent", "transmission", "aria2"} {
		t.Run(kind, func(t *testing.T) {
			f, _, _, wire := downloadBatchFixture(t, kind, "copy", false)
			b := saveDownloadFixtureBatch(t, f, kind)
			status, b, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
			if status != 200 {
				t.Fatal(status, body)
			}
			status, result, body := downloadResultHTTP(t, f, b)
			if status != 200 {
				t.Fatal(status, body)
			}
			wire.lostAck = true
			status, _, _ = downloadBatchActionHTTP(t, f, b, "finalize", downloadBatchRequest{Confirm: true, Fingerprint: result.Fingerprint, ConfirmTaskRemoval: true, ConfirmExclusiveLabels: true})
			if status == 200 {
				t.Fatal("lost response treated as success")
			}
			b, err := f.runner.organizer.jobs.GetDownloadBatch(context.Background(), b.ID)
			if err != nil || b.State != "result_pending" || wire.markCalls+wire.removeCalls != 1 {
				t.Fatal(b, err)
			}
			status, _, _ = downloadBatchActionHTTP(t, f, b, "finalize", downloadBatchRequest{Confirm: true, Fingerprint: result.Fingerprint, ConfirmTaskRemoval: true, ConfirmExclusiveLabels: true})
			if status != 409 || wire.markCalls+wire.removeCalls != 1 {
				t.Fatal("mutation replay")
			}
			status, b, body = downloadBatchActionHTTP(t, f, b, "reconcile-result", downloadBatchRequest{Confirm: true})
			if status != 200 || b.State != "completed" || wire.markCalls+wire.removeCalls != 1 {
				t.Fatal(status, body)
			}
		})
	}
}
func TestDownloadOrganizationChangingSelectionOrConfigInvalidatesSavedAuthority(t *testing.T) {
	f, _, _, wire := downloadBatchFixture(t, "qbittorrent", "copy", false)
	status, review, body := downloadPlanHTTP(t, f, "qbittorrent")
	if status != 200 {
		t.Fatal(status, body)
	}
	wire.files = wire.files[:1]
	status, _, body = downloadBatchSaveHTTP(t, f, "qbittorrent", review, true)
	if status != 409 {
		t.Fatal(status, body)
	}
	b := saveDownloadFixtureBatch(t, f, "qbittorrent")
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET MATCH_PATH=0 WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	status, _, body = downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
	if status != 409 {
		t.Fatal(status, body)
	}
	job, err := f.runner.organizer.jobs.Get(context.Background(), b.Jobs[0])
	if err != nil || job.Items[0].State != "planned" {
		t.Fatal(job, err)
	}
}
func TestDownloadOrganizationCheckpointFailureNeverMarksSuccessOrAutomaticallyRetries(t *testing.T) {
	f, _, target, wire := downloadBatchFixture(t, "qbittorrent", "copy", false)
	b := saveDownloadFixtureBatch(t, f, "qbittorrent")
	if _, err := f.db.Exec(`CREATE TRIGGER stop_download AFTER UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='running' BEGIN UPDATE DOWNLOADER SET TRANSFER=0 WHERE ID=1; END`); err != nil {
		t.Fatal(err)
	}
	status, _, _ := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
	if status == 200 {
		t.Fatal("checkpoint did not stop")
	}
	b, err := f.runner.organizer.jobs.GetDownloadBatch(context.Background(), b.ID)
	if err != nil || b.State != "needs_review" || wire.markCalls+wire.removeCalls != 0 {
		t.Fatal(b, err)
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 0 {
		t.Fatal("write after revoked config", entries)
	}
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET TRANSFER=1 WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	status, _, _ = downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
	if status != 409 {
		t.Fatal("uncertain batch replayed", status)
	}
	status, _, _ = downloadBatchActionHTTP(t, f, b, "reconcile", downloadBatchRequest{Confirm: true})
	if status != 409 {
		t.Fatal("uncertain ledger invented completion", status)
	}
}
func TestDownloadOrganizationTargetCorruptionAndChangedLabelsBlockResultWrite(t *testing.T) {
	for _, kind := range []string{"target", "labels"} {
		t.Run(kind, func(t *testing.T) {
			f, _, _, wire := downloadBatchFixture(t, "qbittorrent", "copy", false)
			b := saveDownloadFixtureBatch(t, f, "qbittorrent")
			status, b, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
			if status != 200 {
				t.Fatal(status, body)
			}
			if kind == "target" {
				job, err := f.runner.organizer.jobs.Get(context.Background(), b.Jobs[0])
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(job.TargetRoot, job.Items[0].Target), []byte("corruption"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				wire.opts.labels = []string{"NASTOOL"}
			}
			status, _, body = downloadResultHTTP(t, f, b)
			if status != 409 || wire.markCalls+wire.removeCalls != 0 {
				t.Fatal(status, body)
			}
		})
	}
}
func TestDownloadOrganizationMoveRequiresActuallyStoppedTaskAndIndependentConsents(t *testing.T) {
	f, source, _, wire := downloadBatchFixture(t, "qbittorrent", "move", false)
	wire.opts.state = "stalledUP"
	b := saveDownloadFixtureBatch(t, f, "qbittorrent")
	status, _, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true, ConfirmSourceRemoval: true, ConfirmTaskStopped: true})
	if status != 409 {
		t.Fatal(status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, wire.files[0]), "original-media")
	wire.opts.state = "stoppedUP"
	status, b, body = downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true, ConfirmSourceRemoval: true, ConfirmTaskStopped: true})
	if status != 200 {
		t.Fatal(status, body)
	}
	status, result, body := downloadResultHTTP(t, f, b)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, _, body = downloadBatchActionHTTP(t, f, b, "finalize", downloadBatchRequest{Confirm: true, Fingerprint: result.Fingerprint, ConfirmTaskStopped: true})
	if status != 409 || wire.removeCalls != 0 {
		t.Fatal("task record removal lacks consent", status, body)
	}
}
func TestDownloadOrganizationPersistentBatchAndJobGuardsPreventOverlappingRequests(t *testing.T) {
	f, _, _, _ := downloadBatchFixture(t, "qbittorrent", "copy", false)
	b := saveDownloadFixtureBatch(t, f, "qbittorrent")
	guard, err := f.runner.organizer.jobs.AcquireJob(context.Background(), downloadBatchGuard(b))
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
	if status != 409 {
		t.Fatal(status)
	}
	status, _, _ = organizationJobRequest(t, f, b.Jobs[0], "execute")
	if status != 409 {
		t.Fatal("individual action bypassed batch guard", status)
	}
	guard.Close()
	status, job, body := organizationJobRequest(t, f, b.Jobs[0], "execute")
	if status != 200 || job.State != "completed" {
		t.Fatal(status, body)
	}
	status, b, body = downloadBatchActionHTTP(t, f, b, "reconcile", downloadBatchRequest{Confirm: true})
	if status != 200 || b.State != "files_done" {
		t.Fatal(status, body)
	}
}

func TestDownloadOrganizationMixedLibrariesAndForeignDirectoryDoNotBroadenScope(t *testing.T) {
	f, source, movie, wire := downloadBatchFixture(t, "qbittorrent", "copy", false)
	tv := t.TempDir()
	if err := f.runner.organizer.service.configStore.Update(map[string]any{"media.tv_path": tv}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Show.S01E01.mkv", "Show.S01E01.en.srt"} {
		organizationFile(t, source, name)
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(filepath.Join(source, name), old, old)
		wire.files = append(wire.files, name)
	}
	// A recursive root scan would exceed the organizer limit; the task manifest
	// must not inspect or consume these other downloads.
	for i := 0; i < 1002; i++ {
		organizationFile(t, source, "foreign/"+strconv.Itoa(i)+".mkv")
	}
	b := saveDownloadFixtureBatch(t, f, "qbittorrent")
	if b.Expected != 2 {
		t.Fatal(b)
	}
	status, b, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
	if status != 200 || b.State != "files_done" {
		t.Fatal(status, body)
	}
	physicalMovie, _ := filepath.EvalSymlinks(movie)
	physicalTV, _ := filepath.EvalSymlinks(tv)
	seen := map[string]bool{}
	for _, id := range b.Jobs {
		job, err := f.runner.organizer.jobs.Get(context.Background(), id)
		if err != nil || job.State != "completed" || len(job.Items) != 2 {
			t.Fatal(job, err)
		}
		seen[job.TargetRoot] = true
		for _, item := range job.Items {
			if strings.HasPrefix(item.Source, "foreign/") {
				t.Fatal(item)
			}
		}
	}
	if !seen[physicalMovie] || !seen[physicalTV] {
		t.Fatal(seen)
	}
}
func TestDownloadOrganizationInterruptedReservationRemainsVisibleAndNeverReservesMissingJobsOnRead(t *testing.T) {
	f, source, _, wire := downloadBatchFixture(t, "qbittorrent", "copy", false)
	if err := f.runner.organizer.service.configStore.Update(map[string]any{"media.tv_path": t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Show.S01E01.mkv", "Show.S01E01.en.srt"} {
		organizationFile(t, source, name)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
			t.Fatal(err)
		}
		wire.files = append(wire.files, name)
	}
	api := f.runner.organizer
	review, failure := api.planDownload(context.Background(), downloadCompletionInput{DownloaderID: "1", TaskID: completionHash}, "admin")
	if failure != nil {
		t.Fatal(failure)
	}
	definitions := []organization.Definition{}
	for _, p := range review.Plans {
		definitions = append(definitions, p.definition)
	}
	if len(definitions) != 2 {
		t.Fatal("fixture must reserve multiple plans", len(definitions))
	}
	b, defs, err := api.jobs.BeginDownloadBatch(context.Background(), review.Fingerprint, review.origin, definitions, review.Omitted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.jobs.Create(context.Background(), organization.Digest(defs[0]), defs[0]); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, f)
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/downloads/batches?downloaderId=1&taskId="+completionHash, f.token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reserving"`) || !strings.Contains(w.Body.String(), `"expected":2`) {
		t.Fatal(w.Code, w.Body.String())
	}
	status, _, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
	if status != 409 {
		t.Fatal(status, body)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
}
func TestDownloadOrganizationPreparedInterruptionCanOnlyResumeThroughExplicitJobsAndReceiptRecheck(t *testing.T) {
	f, _, _, wire := downloadBatchFixture(t, "qbittorrent", "copy", false)
	b := saveDownloadFixtureBatch(t, f, "qbittorrent")
	if _, err := f.db.Exec(`CREATE TRIGGER cfg_after_prepare AFTER UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='prepared' BEGIN UPDATE DOWNLOADER SET TRANSFER=0 WHERE ID=1; END`); err != nil {
		t.Fatal(err)
	}
	status, _, _ := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
	if status == 200 {
		t.Fatal("stale config history committed")
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("false history", count, err)
	}
	if wire.markCalls != 0 {
		t.Fatal("false organized mark")
	}
	if _, err := f.db.Exec(`DROP TRIGGER cfg_after_prepare;UPDATE DOWNLOADER SET TRANSFER=1 WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	status, _, body := organizationJobRequest(t, f, b.Jobs[0], "resume-publication")
	if status != 200 {
		t.Fatal(status, body)
	}
	status, job, body := organizationJobRequest(t, f, b.Jobs[0], "execute")
	if status != 200 || job.State != "completed" {
		t.Fatal(status, body)
	}
	status, b, body = downloadBatchActionHTTP(t, f, b, "reconcile", downloadBatchRequest{Confirm: true})
	if status != 200 || b.State != "files_done" {
		t.Fatal(status, body)
	}
}
func TestDownloadOrganizationResultReviewBindsNewUserLabelsAndInvalidatesIfTheyChange(t *testing.T) {
	f, _, _, wire := downloadBatchFixture(t, "transmission", "copy", true)
	b := saveDownloadFixtureBatch(t, f, "transmission")
	status, b, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
	if status != 200 {
		t.Fatal(status, body)
	}
	wire.opts.labels = append(wire.opts.labels, "new-user-label")
	status, result, body := downloadResultHTTP(t, f, b)
	if status != 200 || !containsDownloadLabel(result.AfterLabels, "new-user-label") {
		t.Fatal(status, body)
	}
	wire.opts.labels = append(wire.opts.labels, "even-newer")
	status, _, _ = downloadBatchActionHTTP(t, f, b, "finalize", downloadBatchRequest{Confirm: true, Fingerprint: result.Fingerprint, ConfirmExclusiveLabels: true})
	if status != 409 || wire.markCalls != 0 {
		t.Fatal("stale label replacement")
	}
	status, result, body = downloadResultHTTP(t, f, b)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, b, body = downloadBatchActionHTTP(t, f, b, "finalize", downloadBatchRequest{Confirm: true, Fingerprint: result.Fingerprint, ConfirmExclusiveLabels: true})
	if status != 200 || b.State != "completed" || !containsDownloadLabel(wire.opts.labels, "new-user-label") || !containsDownloadLabel(wire.opts.labels, "even-newer") {
		t.Fatal(status, body)
	}
}
func TestDownloadOrganizationPrivateOriginInjectionAndConsentCannotBypassReview(t *testing.T) {
	f, _, _, _ := downloadBatchFixture(t, "qbittorrent", "copy", false)
	for _, path := range []string{"/api/v1/organization/plan", "/api/v1/organization/downloads/plan", "/api/v1/organization/downloads/batches"} {
		w := nativeJSONRequest(f.handler, "POST", path, f.token, `{"download":{"batchId":"forged"}}`)
		if w.Code != 400 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	status, review, body := downloadPlanHTTP(t, f, "qbittorrent")
	if status != 200 {
		t.Fatal(status, body)
	}
	raw, _ := json.Marshal(downloadBatchRequest{downloadCompletionInput: downloadCompletionInput{DownloaderID: "1", TaskID: completionHash}, Fingerprint: review.Fingerprint, Confirm: true})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/batches", f.token, string(raw))
	if w.Code != 400 {
		t.Fatal("old executor consent", w.Code, w.Body.String())
	}
	b := saveDownloadFixtureBatch(t, f, "qbittorrent")
	for _, action := range []string{"execute", "reconcile", "finalize", "reconcile-result"} {
		status, _, body := downloadBatchActionHTTP(t, f, b, action, downloadBatchRequest{})
		if status != 400 {
			t.Fatal(action, status, body)
		}
	}
}

func TestDownloadOrganizationCompletedProofCannotAdoptUncertainOrDanglingWork(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, _, wire := downloadBatchFixture(t, "qbittorrent", mode, false)
			b := saveDownloadFixtureBatch(t, f, "qbittorrent")
			status, b, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true, ConfirmSourceRemoval: true, ConfirmTaskStopped: true})
			if status != 200 {
				t.Fatal(status, body)
			}
			job, err := f.runner.organizer.jobs.Get(context.Background(), b.Jobs[0])
			if err != nil {
				t.Fatal(err)
			}
			item := job.Items[0]
			item.State = "prepared"
			if err := organization.VerifyCompletedTarget(context.Background(), job, item); !errors.Is(err, organization.ErrState) {
				t.Fatal("adopted uncertain publication", err)
			}
			if mode == "softlink" {
				if err := os.Rename(filepath.Join(source, job.Items[0].Source), filepath.Join(source, "retained-original.mkv")); err != nil {
					t.Fatal(err)
				}
				status, _, _ = downloadResultHTTP(t, f, b)
				if status != 409 || wire.markCalls != 0 {
					t.Fatal("dangling target marked organized", status)
				}
			}
		})
	}
}
func TestDownloadOrganizationEveryRouteRequiresAdminAndPureGo(t *testing.T) {
	f, _, _, _ := downloadBatchFixture(t, "qbittorrent", "copy", false)
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	mixed, err := newHandler(config.Config{ApplicationConfigPath: f.path}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct{ method, path string }{{"POST", "/api/v1/organization/downloads/plan"}, {"POST", "/api/v1/organization/downloads/batches"}, {"GET", "/api/v1/organization/downloads/batches?downloaderId=1&taskId=" + completionHash}, {"GET", "/api/v1/organization/downloads/batches/missing"}, {"POST", "/api/v1/organization/downloads/batches/missing/execute"}, {"POST", "/api/v1/organization/downloads/batches/missing/finalize"}, {"POST", "/api/v1/organization/downloads/batches/missing/reconcile-result"}} {
		for _, token := range []string{"", viewer} {
			w := nativeJSONRequest(f.handler, spec.method, spec.path, token, `{}`)
			want := 403
			if token == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(spec.path, w.Code, w.Body.String())
			}
		}
		w := nativeJSONRequest(mixed, spec.method, spec.path, f.token, `{}`)
		if w.Code != 501 {
			t.Fatal(spec.path, w.Code, w.Body.String())
		}
	}
}

func TestDownloadOrganizationBoundClientNeverFollowsAChangedHostAndReceiptCommitRejectsStaleConfig(t *testing.T) {
	f, _, _, wire := downloadBatchFixture(t, "qbittorrent", "copy", false)
	api := f.runner.organizer
	b := saveDownloadFixtureBatch(t, f, "qbittorrent")
	status, b, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true})
	if status != 200 {
		t.Fatal(status, body)
	}
	status, intent, body := downloadResultHTTP(t, f, b)
	if status != 200 {
		t.Fatal(status, body)
	}
	c, err := api.completionOrganizationClient(context.Background(), b.Origin)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(intent)
	if err := api.jobs.SetDownloadBatch(context.Background(), b.ID, "files_done", "result_pending", "fixture accepted intent", string(raw)); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]string{"host": "http://unreviewed.test", "port": "8080", "username": "admin", "password": "private-password"})
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET CONFIG=? WHERE ID=1`, string(cfg)); err != nil {
		t.Fatal(err)
	}
	// The constructed client still targets the originally authorized host; the
	// transport fails this test on any request to the new unreviewed address.
	if err := c.mark(context.Background(), b.Origin.Task.ID, intent.AfterLabels); err != nil {
		t.Fatal(err)
	}
	if wire.markCalls != 1 {
		t.Fatal(wire.markCalls)
	}
	if err := api.jobs.SetDownloadBatch(context.Background(), b.ID, "result_pending", "completed", "", string(raw)); err == nil {
		t.Fatal("stale configuration committed a success receipt")
	}
	current, err := api.jobs.GetDownloadBatch(context.Background(), b.ID)
	if err != nil || current.State != "result_pending" {
		t.Fatal(current, err)
	}
}
