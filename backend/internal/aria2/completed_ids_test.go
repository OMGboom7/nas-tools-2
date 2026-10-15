package aria2

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestCompletedIDsEnumeratesStoppedResultsNotActiveDownloads(t *testing.T) {
	for _, kind := range []string{"valid", "null", "limit", "unknown", "duplicate"} {
		c, _ := New("aria.test", "6800", "secret", transportFunc(func(r *http.Request) (*http.Response, error) {
			var req struct {
				Method string            `json:"method"`
				ID     string            `json:"id"`
				Params []json.RawMessage `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			var count int
			_ = json.Unmarshal(req.Params[2], &count)
			if req.Method != "aria2.tellStopped" || count != 1001 {
				t.Fatal(req)
			}
			rows := []map[string]string{{"gid": "0123456789abcdef", "status": "complete"}, {"gid": "fedcba9876543210", "status": "error"}}
			switch kind {
			case "null":
				rows = nil
			case "limit":
				rows = make([]map[string]string, 1001)
			case "unknown":
				rows[0]["status"] = "active"
			case "duplicate":
				rows[1]["gid"] = rows[0]["gid"]
			}
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": rows})
			return reply(200, string(raw)), nil
		}))
		ids, err := c.CompletedIDs(context.Background())
		if kind == "valid" {
			if err != nil || len(ids) != 1 || ids[0] != "0123456789abcdef" {
				t.Fatal(ids, err)
			}
		} else if err == nil {
			t.Fatal(kind, ids)
		}
	}
}
