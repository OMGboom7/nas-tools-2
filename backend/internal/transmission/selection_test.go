package transmission

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestFileSelectionBothRPCDialectsNeverSendEmptyUnwantedArray(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, all := range []bool{false, true} {
			t.Run(map[bool]string{false: "legacy", true: "modern"}[modern]+map[bool]string{false: "/subset", true: "/all"}[all], func(t *testing.T) {
				writes := 0
				hash := strings.Repeat("b", 40)
				c, _ := New("tr.test", "9091", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("X-Transmission-Session-Id") == "" {
						v := reply(409, "")
						v.Header.Set("X-Transmission-Session-Id", "token")
						if modern {
							v.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
						}
						return v, nil
					}
					var in map[string]json.RawMessage
					if json.NewDecoder(r.Body).Decode(&in) != nil {
						t.Fatal("invalid RPC")
					}
					var method string
					json.Unmarshal(in["method"], &method)
					args := in["arguments"]
					key := func(a, b string) string {
						if modern {
							return b
						}
						return a
					}
					if modern {
						args = in["params"]
					}
					if method == key("torrent-set", "torrent_set") {
						var v map[string]any
						json.Unmarshal(args, &v)
						if len(v[key("files-wanted", "files_wanted")].([]any)) != 1 {
							t.Fatal(v)
						}
						unwanted, present := v[key("files-unwanted", "files_unwanted")]
						if all && present || !all && (!present || len(unwanted.([]any)) != 1) {
							t.Fatal("empty array widened selection", v)
						}
						writes++
						if modern {
							return reply(200, `{"jsonrpc":"2.0","result":{},"id":1}`), nil
						}
						return reply(200, `{"result":"success","arguments":{}}`), nil
					}
					if method != key("torrent-get", "torrent_get") {
						t.Fatal(method)
					}
					row := map[string]any{key("hashString", "hash_string"): hash, key("downloadDir", "download_dir"): "/downloads", "labels": []string{"NASTOOL"}, "status": 0, key("metadataPercentComplete", "metadata_percent_complete"): 1, "files": []any{map[string]any{"name": "Show/S01E02.mkv", "length": 12}}, key("fileStats", "file_stats"): []any{map[string]any{"wanted": true}}}
					out := map[string]any{"result": "success", "arguments": map[string]any{"torrents": []any{row}}}
					if modern {
						out = map[string]any{"jsonrpc": "2.0", "result": map[string]any{"torrents": []any{row}}, "id": 1}
					}
					raw, _ := json.Marshal(out)
					return reply(200, string(raw)), nil
				}))
				s, err := c.Selection(t.Context(), hash)
				if err != nil || len(s.Files) != 1 || s.Files[0].Index != 0 || !s.Paused {
					t.Fatal(s, err)
				}
				unwanted := []int{0}
				if all {
					unwanted = nil
				}
				if c.SelectFiles(t.Context(), hash, []int{1}, unwanted) != nil {
					t.Fatal("selection failed")
				}
				if c.SelectFiles(t.Context(), hash, nil, nil) == nil || c.SelectFiles(t.Context(), hash, []int{1}, []int{1}) == nil || writes != 1 {
					t.Fatal("unsafe request emitted", writes)
				}
			})
		}
	}
}
