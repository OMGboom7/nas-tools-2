package transmission

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCompletedIDsReadsBothProtocolsAndExcludesCheckingIncompleteAndFailedTasks(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			c, _ := New("tr.test", "9091", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("X-Transmission-Session-Id") == "" {
					resp := reply(409, "")
					resp.Header.Set("X-Transmission-Session-Id", "session")
					if modern {
						resp.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
					}
					return resp, nil
				}
				key := func(a, b string) string {
					if modern {
						return b
					}
					return a
				}
				rows := []any{}
				for i, status := range []int{6, 2, 4, 0} {
					row := map[string]any{key("hashString", "hash_string"): strings.Repeat(string(rune('a'+i)), 40), key("percentDone", "percent_done"): 1, key("metadataPercentComplete", "metadata_percent_complete"): 1, key("leftUntilDone", "left_until_done"): 0, "status": status, "error": 0}
					if i == 3 {
						row["error"] = 1
					}
					if missing && i == 0 {
						delete(row, "status")
					}
					rows = append(rows, row)
				}
				env := map[string]any{"result": "success", "arguments": map[string]any{"torrents": rows}}
				if modern {
					env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"torrents": rows}}
				}
				raw, _ := json.Marshal(env)
				return reply(200, string(raw)), nil
			}))
			ids, err := c.CompletedIDs(context.Background())
			if missing {
				if err == nil {
					t.Fatal("missing status assumed complete")
				}
			} else if err != nil || len(ids) != 1 || ids[0] != strings.Repeat("a", 40) {
				t.Fatal(modern, ids, err)
			}
		}
	}
}
