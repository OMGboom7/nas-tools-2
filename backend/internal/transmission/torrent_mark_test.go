package transmission

import (
	"context"
	"encoding/json"
	"github.com/0xforee/nas-tools/backend/internal/torrentmark"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestTorrentMarkBothProtocolsPreserveLabels(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "modern"}[modern], func(t *testing.T) {
			hash := strings.Repeat("a", 40)
			writes := 0
			status := 6
			c, _ := New("tr.test", "9091", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("X-Transmission-Session-Id") == "" {
					resp := reply(409, "")
					resp.Header.Set("X-Transmission-Session-Id", "fixture")
					if modern {
						resp.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
					}
					return resp, nil
				}
				var call struct {
					Method    string
					Arguments map[string]json.RawMessage
					Params    map[string]json.RawMessage
				}
				if json.NewDecoder(r.Body).Decode(&call) != nil {
					t.Fatal("bad call")
				}
				key := func(a, b string) string {
					if modern {
						return b
					}
					return a
				}
				args := call.Arguments
				if modern {
					args = call.Params
				}
				data := map[string]any{}
				if call.Method == key("torrent-set", "torrent_set") {
					var labels []string
					_ = json.Unmarshal(args["labels"], &labels)
					if !slices.Equal(labels, []string{"PT", "keep", "已整理"}) {
						t.Fatal(labels)
					}
					writes++
				} else if call.Method == key("torrent-get", "torrent_get") {
					data["torrents"] = []any{map[string]any{key("hashString", "hash_string"): hash, key("addedDate", "added_date"): 1, key("percentDone", "percent_done"): 1, key("metadataPercentComplete", "metadata_percent_complete"): 1, key("leftUntilDone", "left_until_done"): 0, "status": status, "error": 0, "labels": []string{"keep", "BT", "已整理"}, "trackers": []any{map[string]string{"announce": "https://tracker.test/a?passkey=fake"}}, key("isPrivate", "is_private"): true}}
				} else {
					t.Fatal(call.Method)
				}
				env := map[string]any{"result": "success", "arguments": data}
				if modern {
					env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": data}
				}
				b, _ := json.Marshal(env)
				return reply(200, string(b)), nil
			}))
			status = 0
			if _, err := c.MarkEvidence(context.Background(), hash); err == nil {
				t.Fatal("stopped torrent entered original seeding-only scope")
			}
			status = 6
			e, err := c.MarkEvidence(context.Background(), hash)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.SetMark(context.Background(), e, torrentmark.PT); err != nil || writes != 1 {
				t.Fatal(err, writes)
			}
		})
	}
}
