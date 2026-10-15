package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

const completionHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const completionGID = "0123456789abcdef"

type completionTestOptions struct {
	allowMetadata bool
	kind          string
	modern        bool
	labels        []string
	state         string
	directory     string
	filePath      string
	size          int
	readHook      func(int)
}

func completionHTTPFixture(t *testing.T, opts *completionTestOptions) (*subscriptionRunFixture, string, string) {
	t.Helper()
	f, source, target, _, _ := organizationCopyFixture(t)
	kind := opts.kind
	if kind == "" {
		kind = "qbittorrent"
	}
	if opts.labels == nil {
		opts.labels = []string{"NASTOOL", "user"}
	}
	if opts.directory == "" {
		opts.directory = "/remote-downloads"
	}
	if opts.filePath == "" {
		opts.filePath = "Movie.2026.1080p.mkv"
	}
	if opts.size == 0 {
		opts.size = 14
	}
	raw, _ := json.Marshal(map[string]string{"host": "http://completion.test", "port": "8080", "username": "admin", "password": "private-password", "secret": "private-token"})
	only := 1
	if kind == "aria2" {
		only = 0
	}
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET TYPE=?,CONFIG=?,TRANSFER=1,ONLY_NASTOOL=?,MATCH_PATH=1,RMT_MODE='复制' WHERE ID=1`, kind, string(raw), only); err != nil {
		t.Fatal(err)
	}
	reads := 0
	metadataTransport := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "completion.test" {
			if opts.allowMetadata {
				return metadataTransport.RoundTrip(r)
			}
			t.Fatal("metadata/legacy request", r.URL)
		}
		if strings.HasSuffix(r.URL.Path, "/auth/login") {
			return jsonResponse(r, "Ok."), nil
		}
		if kind == "qbittorrent" {
			if r.Method != "GET" {
				t.Fatal("task mutation", r.Method, r.URL)
			}
			switch r.URL.Path {
			case "/api/v2/torrents/info":
				if r.URL.Query().Get("hashes") != completionHash || r.URL.Query().Has("filter") {
					t.Fatal("scope", r.URL)
				}
				reads++
				if opts.readHook != nil {
					opts.readHook(reads)
				}
				state := opts.state
				if state == "" {
					state = "stalledUP"
				}
				raw, _ := json.Marshal([]any{map[string]any{"hash": completionHash, "name": "display-name-is-not-a-source-path", "save_path": opts.directory, "state": state, "progress": 1, "amount_left": 0, "completion_on": 123, "tags": strings.Join(opts.labels, ",")}})
				return jsonResponse(r, string(raw)), nil
			case "/api/v2/torrents/files":
				if r.URL.Query().Get("hash") != completionHash {
					t.Fatal(r.URL)
				}
				raw, _ := json.Marshal([]any{map[string]any{"name": opts.filePath, "size": opts.size, "progress": 1, "priority": 1}, map[string]any{"name": "not-selected-and-missing.mkv", "size": 999, "progress": 0, "priority": 0}})
				return jsonResponse(r, string(raw)), nil
			default:
				t.Fatal("task mutation", r.URL)
			}
		}
		if kind == "transmission" {
			if r.Header.Get("X-Transmission-Session-Id") == "" {
				v := jsonResponse(r, "")
				v.StatusCode = 409
				v.Header.Set("X-Transmission-Session-Id", "session")
				if opts.modern {
					v.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
				}
				return v, nil
			}
			var payload struct {
				Method string `json:"method"`
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Method != "torrent-get" && payload.Method != "torrent_get" {
				t.Fatal("mutation", payload)
			}
			reads++
			if opts.readHook != nil {
				opts.readHook(reads)
			}
			key := func(a, b string) string {
				if opts.modern {
					return b
				}
				return a
			}
			state := 6
			if opts.state == "stopped" {
				state = 0
			}
			task := map[string]any{key("hashString", "hash_string"): completionHash, "name": "display-only", key("downloadDir", "download_dir"): opts.directory, key("percentDone", "percent_done"): 1, key("metadataPercentComplete", "metadata_percent_complete"): 1, key("leftUntilDone", "left_until_done"): 0, "status": state, "error": 0, "labels": opts.labels, "files": []any{map[string]any{"name": opts.filePath, "length": opts.size, key("bytesCompleted", "bytes_completed"): opts.size}}, key("fileStats", "file_stats"): []any{map[string]any{"wanted": true, key("bytesCompleted", "bytes_completed"): opts.size}}}
			env := map[string]any{"result": "success", "arguments": map[string]any{"torrents": []any{task}}}
			if opts.modern {
				env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"torrents": []any{task}}}
			}
			raw, _ := json.Marshal(env)
			return jsonResponse(r, string(raw)), nil
		}
		if kind == "aria2" {
			var payload struct {
				Method string `json:"method"`
				ID     string `json:"id"`
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Method != "aria2.tellStatus" {
				t.Fatal("task mutation", payload)
			}
			reads++
			if opts.readHook != nil {
				opts.readHook(reads)
			}
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": payload.ID, "result": map[string]any{"gid": completionGID, "status": "complete", "dir": opts.directory, "totalLength": "14", "completedLength": "14", "files": []any{map[string]any{"path": opts.directory + "/" + opts.filePath, "length": "14", "completedLength": "14", "selected": "true", "uris": []any{map[string]any{"uri": "https://private-token"}}}}}})
			return jsonResponse(r, string(raw)), nil
		}
		t.Fatal("unsupported network", kind, r.URL)
		return nil, nil
	})
	monitorRebuild(t, f)
	return f, source, target
}
func completionReviewHTTP(t *testing.T, f *subscriptionRunFixture, id, task string) (int, downloadCompletionReview, string) {
	t.Helper()
	raw, _ := json.Marshal(downloadCompletionInput{DownloaderID: id, TaskID: task})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/completion/review", f.token, string(raw))
	var result struct{ Data downloadCompletionReview }
	if json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Body.String())
	}
	return w.Code, result.Data, w.Body.String()
}
func TestDownloadCompletionReviewThreeProtocolsReadExactSelectedFilesWithoutMutation(t *testing.T) {
	for _, spec := range []completionTestOptions{{kind: "qbittorrent"}, {kind: "transmission"}, {kind: "transmission", modern: true}, {kind: "aria2"}} {
		t.Run(spec.kind+map[bool]string{true: "modern"}[spec.modern], func(t *testing.T) {
			opts := spec
			f, source, target := completionHTTPFixture(t, &opts)
			organizationFile(t, source, "foreign-task.mkv")
			task := completionHash
			if spec.kind == "aria2" {
				task = completionGID
			}
			status, review, body := completionReviewHTTP(t, f, "1", task)
			if status != 200 || !review.ReviewOnly || !review.Eligible || len(review.Files) != 1 || review.Files[0].Path != "Movie.2026.1080p.mkv" || len(review.Fingerprint) != 64 || !review.Location.Mapped {
				t.Fatal(status, body)
			}
			for _, secret := range []string{"private-password", "private-token", "uris", "foreign-task.mkv"} {
				if strings.Contains(body, secret) {
					t.Fatal("private/foreign evidence leaked", body)
				}
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, review.Files[0].Path), "original-media")
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatal("review touched target", entries, err)
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
				t.Fatal("review created jobs", count, err)
			}
			status, again, body := completionReviewHTTP(t, f, "1", task)
			if status != 200 || again.Fingerprint != review.Fingerprint {
				t.Fatal("unstable readonly snapshot", status, body)
			}
		})
	}
}
func TestDownloadCompletionExactLabelsAndNonreadyStatesDoNotBecomeEligible(t *testing.T) {
	for _, opts := range []completionTestOptions{{labels: []string{"NASTOOL-extra"}}, {labels: []string{"NASTOOL", "已整理"}}, {labels: []string{"NASTOOL"}, state: "checkingUP"}, {labels: []string{"NASTOOL"}, state: "downloading"}} {
		f, _, _ := completionHTTPFixture(t, &opts)
		status, review, body := completionReviewHTTP(t, f, "1", completionHash)
		if status != 200 || review.Eligible || len(review.Reasons) == 0 {
			t.Fatal(status, body)
		}
	}
}
func TestDownloadCompletionSizeQuietMissingSymlinkAndHiddenBoundaries(t *testing.T) {
	for _, kind := range []string{"size", "young", "missing", "symlink", "hidden", "temporary", "directory", "escape"} {
		t.Run(kind, func(t *testing.T) {
			opts := completionTestOptions{}
			f, source, _ := completionHTTPFixture(t, &opts)
			want := 422
			switch kind {
			case "size":
				opts.size = 99
				want = 200
			case "young":
				now := time.Now()
				if err := os.Chtimes(filepath.Join(source, opts.filePath), now, now); err != nil {
					t.Fatal(err)
				}
				want = 200
			case "missing":
				opts.filePath = "missing.mkv"
			case "symlink":
				opts.filePath = "alias.mkv"
				if err := os.Symlink(filepath.Join(source, "Movie.2026.1080p.mkv"), filepath.Join(source, opts.filePath)); err != nil {
					t.Fatal(err)
				}
			case "hidden":
				opts.filePath = ".hidden/Film.mkv"
				organizationFile(t, source, opts.filePath)
			case "temporary":
				opts.filePath = "downloading.!qb/Film.mkv"
				organizationFile(t, source, opts.filePath)
			case "directory":
				opts.filePath = "subdir"
				organizationFile(t, source, "subdir/Film.mkv")
			case "escape":
				opts.filePath = "../Film.mkv"
				want = 502
			}
			status, review, body := completionReviewHTTP(t, f, "1", completionHash)
			if status != want || review.Eligible {
				t.Fatal(kind, status, body)
			}
		})
	}
}
func TestDownloadCompletionTaskConfigAndFileChangesInvalidateReview(t *testing.T) {
	for _, kind := range []string{"task", "config", "file"} {
		t.Run(kind, func(t *testing.T) {
			opts := completionTestOptions{}
			f, source, _ := completionHTTPFixture(t, &opts)
			opts.readHook = func(read int) {
				if read != 2 {
					return
				}
				switch kind {
				case "task":
					opts.directory = "/remote-downloads/new"
				case "config":
					if _, err := f.db.Exec(`UPDATE DOWNLOADER SET ONLY_NASTOOL=0 WHERE ID=1`); err != nil {
						t.Fatal(err)
					}
				case "file":
					if err := os.WriteFile(filepath.Join(source, opts.filePath), []byte("modified"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			status, _, body := completionReviewHTTP(t, f, "1", completionHash)
			if status != 409 {
				t.Fatal(kind, status, body)
			}
		})
	}
}
func TestDownloadCompletionAriaOwnershipRequiresExactPersistedHistory(t *testing.T) {
	opts := completionTestOptions{kind: "aria2"}
	f, _, _ := completionHTTPFixture(t, &opts)
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET ONLY_NASTOOL=1 WHERE ID=1;CREATE TABLE IF NOT EXISTS DOWNLOAD_HISTORY(ID INTEGER PRIMARY KEY,DOWNLOADER TEXT,DOWNLOAD_ID TEXT)`); err != nil {
		t.Fatal(err)
	}
	status, review, body := completionReviewHTTP(t, f, "1", completionGID)
	if status != 200 || review.Eligible {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`INSERT INTO DOWNLOAD_HISTORY(DOWNLOADER,DOWNLOAD_ID) VALUES('2',?)`, completionGID); err != nil {
		t.Fatal(err)
	}
	status, review, body = completionReviewHTTP(t, f, "1", completionGID)
	if status != 200 || review.Eligible {
		t.Fatal("other downloader ownership", status, body)
	}
	if _, err := f.db.Exec(`INSERT INTO DOWNLOAD_HISTORY(DOWNLOADER,DOWNLOAD_ID) VALUES('1',?)`, completionGID); err != nil {
		t.Fatal(err)
	}
	status, review, body = completionReviewHTTP(t, f, "1", completionGID)
	if status != 200 || !review.Eligible {
		t.Fatal(status, body)
	}
}
func TestDownloadCompletionMappingIsolationAndLargeIDs(t *testing.T) {
	opts := completionTestOptions{}
	f, source, _ := completionHTTPFixture(t, &opts)
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET ID=9223372036854775807 WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	status, review, body := completionReviewHTTP(t, f, "9223372036854775807", completionHash)
	if status != 200 || !review.Eligible || review.DownloaderID != "9223372036854775807" {
		t.Fatal(status, body)
	}
	opts.directory = source
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET MATCH_PATH=0 WHERE ID=9223372036854775807`); err != nil {
		t.Fatal(err)
	}
	status, review, body = completionReviewHTTP(t, f, "9223372036854775807", completionHash)
	if status != 200 || !review.Eligible || review.Location.Mapped {
		t.Fatal(status, body)
	}
	opts.directory = "/remote-downloads-other"
	status, _, body = completionReviewHTTP(t, f, "9223372036854775807", completionHash)
	if status != 422 {
		t.Fatal(status, body)
	}
}
func TestDownloadCompletionPermissionsUnsupportedModesAndPrivateFields(t *testing.T) {
	opts := completionTestOptions{}
	f, _, _ := completionHTTPFixture(t, &opts)
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	for _, token := range []string{"", viewer} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/completion/review", token, `{}`)
		want := 403
		if token == "" {
			want = 401
		}
		if w.Code != want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{"downloaderId":"1","taskId":"` + completionHash + `","sourceRoot":"/etc"}`, `{"downloaderId":"01","taskId":"` + completionHash + `"}`, `{"downloaderId":"1","taskId":"all"}`} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/completion/review", f.token, body)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, sql := range []string{`UPDATE DOWNLOADER SET RMT_MODE='unsupported' WHERE ID=1`, `UPDATE DOWNLOADER SET RMT_MODE='复制',TYPE='pikpak' WHERE ID=1`} {
		if _, err := f.db.Exec(sql); err != nil {
			t.Fatal(err)
		}
		status, _, body := completionReviewHTTP(t, f, "1", completionHash)
		if status != 501 {
			t.Fatal(status, body)
		}
	}
	mixed, err := newHandler(config.Config{ApplicationConfigPath: f.path}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(mixed, "POST", "/api/v1/organization/downloads/completion/review", f.token, `{}`)
	if w.Code != 501 {
		t.Fatal(w.Code, w.Body.String())
	}
}
