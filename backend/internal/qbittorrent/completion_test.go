package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCompletionReadsExactTaskAndFileEvidenceWithoutTaskMutation(t *testing.T) {
	hash := strings.Repeat("a", 40)
	for _, state := range []string{"uploading", "stalledUP", "queuedUP", "forcedUP", "pausedUP", "stoppedUP", "checkingUP", "error", "moving", "downloading"} {
		t.Run(state, func(t *testing.T) {
			calls := 0
			client, err := New("qb.example", "8080", "user", "private", transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				switch r.URL.Path {
				case "/api/v2/auth/login":
					return reply(200, "Ok."), nil
				case "/api/v2/torrents/info":
					if r.Method != "GET" || r.URL.Query().Get("hashes") != hash || r.URL.Query().Has("filter") {
						t.Fatal(r.URL)
					}
					raw, _ := json.Marshal([]any{map[string]any{"hash": hash, "name": "not-guessed.mkv", "save_path": "/downloads/", "state": state, "progress": 1, "amount_left": 0, "completion_on": 123, "tags": "NASTOOL, user"}})
					return reply(200, string(raw)), nil
				case "/api/v2/torrents/files":
					if r.Method != "GET" || r.URL.Query().Get("hash") != hash {
						t.Fatal(r.URL)
					}
					return reply(200, `[{"name":"Bundle/file.mkv","size":14,"progress":1,"priority":1},{"name":"unwanted.mkv","size":99,"progress":0.5,"priority":0}]`), nil
				default:
					t.Fatal("mutation", r.URL)
					return nil, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			task, err := client.Completion(context.Background(), hash)
			want := state != "checkingUP" && state != "error" && state != "moving" && state != "downloading"
			if err != nil || task.Complete != want || len(task.Files) != 2 || task.Files[0].Path != "Bundle/file.mkv" || !task.HasLabel("NASTOOL") || calls != 3 {
				t.Fatal(task, err, calls)
			}
		})
	}
}
func TestCompletionRejectsIncompleteMalformedAndMismatchedEvidence(t *testing.T) {
	hash := strings.Repeat("a", 40)
	good := map[string]any{"hash": hash, "name": "Film", "save_path": "/downloads", "state": "stalledUP", "progress": 1, "amount_left": 0, "completion_on": 123, "tags": ""}
	for _, kind := range []string{"missing-progress", "null-left", "wrong-hash", "empty-files", "file-parent", "missing-priority", "invalid-progress", "invalid-priority", "file-incomplete"} {
		t.Run(kind, func(t *testing.T) {
			info := map[string]any{}
			for k, v := range good {
				info[k] = v
			}
			files := `[{"name":"Film.mkv","size":14,"progress":1,"priority":1}]`
			switch kind {
			case "missing-progress":
				delete(info, "progress")
			case "null-left":
				info["amount_left"] = nil
			case "wrong-hash":
				info["hash"] = strings.Repeat("b", 40)
			case "empty-files":
				files = `[]`
			case "file-parent":
				files = strings.ReplaceAll(files, "Film.mkv", "../Film.mkv")
			case "missing-priority":
				files = `[{"name":"Film.mkv","size":14,"progress":1}]`
			case "invalid-progress":
				files = `[{"name":"Film.mkv","size":14,"progress":2,"priority":1}]`
			case "invalid-priority":
				files = `[{"name":"Film.mkv","size":14,"progress":1,"priority":2}]`
			case "file-incomplete":
				files = strings.ReplaceAll(files, `"progress":1`, `"progress":0.9`)
			}
			raw, _ := json.Marshal([]any{info})
			c, _ := New("qb.example", "8080", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "auth/login") {
					return reply(200, "Ok."), nil
				}
				if strings.HasSuffix(r.URL.Path, "torrents/info") {
					return reply(200, string(raw)), nil
				}
				return reply(200, files), nil
			}))
			task, err := c.Completion(context.Background(), hash)
			if kind == "file-incomplete" {
				if err != nil || task.Complete {
					t.Fatal(task, err)
				}
			} else if !errors.Is(err, ErrResponse) {
				t.Fatal(task, err)
			}
		})
	}
}
