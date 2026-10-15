package transmission

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func completionFixtureMap(modern bool) map[string]any {
	key := func(a, b string) string {
		if modern {
			return b
		}
		return a
	}
	return map[string]any{key("hashString", "hash_string"): strings.Repeat("a", 40), "name": "display-only", "labels": []string{"NASTOOL", "用户"}, key("downloadDir", "download_dir"): "/downloads", key("percentDone", "percent_done"): 1, key("metadataPercentComplete", "metadata_percent_complete"): 1, key("leftUntilDone", "left_until_done"): 0, "status": 6, "error": 0, "files": []any{map[string]any{"name": "Bundle/Film.mkv", "length": 14, key("bytesCompleted", "bytes_completed"): 14}}, key("fileStats", "file_stats"): []any{map[string]any{"wanted": true, key("bytesCompleted", "bytes_completed"): 14}}}
}
func completionTestClient(t *testing.T, modern bool, payload map[string]any) *Client {
	t.Helper()
	c, err := New("tr.example", "9091", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Transmission-Session-Id") == "" {
			v := reply(409, "")
			v.Header.Set("X-Transmission-Session-Id", "token")
			if modern {
				v.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
			}
			return v, nil
		}
		var req map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Fatal("payload")
		}
		var method string
		_ = json.Unmarshal(req["method"], &method)
		want := "torrent-get"
		args := req["arguments"]
		if modern {
			want = "torrent_get"
			args = req["params"]
		}
		var values struct {
			IDs    []string `json:"ids"`
			Fields []string `json:"fields"`
		}
		if method != want || json.Unmarshal(args, &values) != nil || len(values.IDs) != 1 || values.IDs[0] != strings.Repeat("a", 40) || len(values.Fields) < 10 {
			t.Fatal("mutation or widened scope", method, string(args))
		}
		result := map[string]any{"result": "success", "arguments": map[string]any{"torrents": []any{payload}}}
		if modern {
			result = map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"torrents": []any{payload}}}
		}
		raw, _ := json.Marshal(result)
		return reply(200, string(raw)), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestCompletionReadsBothTransmissionProtocolsAndDoesNotGuessFromName(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, status := range []int{0, 1, 2, 3, 4, 5, 6} {
			m := completionFixtureMap(modern)
			m["status"] = status
			c := completionTestClient(t, modern, m)
			task, err := c.Completion(context.Background(), strings.Repeat("a", 40))
			if err != nil || task.Complete != (status == 0 || status == 5 || status == 6) || len(task.Files) != 1 || task.Files[0].Path != "Bundle/Film.mkv" || !task.HasLabel("NASTOOL") {
				t.Fatal(modern, status, task, err)
			}
		}
	}
}
func TestCompletionRejectsMissingOrInconsistentTransmissionCounters(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, field := range []string{"status", "error", "files", "left", "stats", "metadata", "labels"} {
			m := completionFixtureMap(modern)
			key := field
			switch field {
			case "left":
				key = "leftUntilDone"
				if modern {
					key = "left_until_done"
				}
			case "stats":
				key = "fileStats"
				if modern {
					key = "file_stats"
				}
			case "metadata":
				key = "metadataPercentComplete"
				if modern {
					key = "metadata_percent_complete"
				}
			}
			delete(m, key)
			c := completionTestClient(t, modern, m)
			task, err := c.Completion(context.Background(), strings.Repeat("a", 40))
			if field == "labels" {
				if err != nil || task.LabelsKnown {
					t.Fatal(task, err)
				}
			} else if !errors.Is(err, ErrResponse) {
				t.Fatal(field, task, err)
			}
		}
	}
}
