package transmission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestHasTorrentScopesBothProtocolsAndKeepsCompletedTasks(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, modern := range []bool{false, true} {
		for _, state := range []string{"found", "absent", "missing", "wrong", "duplicate"} {
			t.Run(fmt.Sprint(modern)+state, func(t *testing.T) {
				client, err := New("tr.example", "9091", "admin", "private", transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("X-Transmission-Session-Id") == "" {
						response := reply(409, "")
						response.Header.Set("X-Transmission-Session-Id", "session")
						if modern {
							response.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
						}
						return response, nil
					}
					var request struct {
						Method    string                         `json:"method"`
						Arguments struct{ IDs, Fields []string } `json:"arguments"`
						Params    struct{ IDs, Fields []string } `json:"params"`
					}
					if json.NewDecoder(r.Body).Decode(&request) != nil {
						t.Fatal("bad verification JSON")
					}
					method, field := "torrent-get", "hashString"
					arguments := request.Arguments
					if modern {
						method, field = "torrent_get", "hash_string"
						arguments = request.Params
					}
					if request.Method != method || len(arguments.IDs) != 1 || arguments.IDs[0] != hash || len(arguments.Fields) != 1 || arguments.Fields[0] != field {
						t.Fatal("unscoped verification", request)
					}
					tasks := `[]`
					switch state {
					case "found":
						tasks = `[{"` + field + `":"` + hash + `","status":6}]`
					case "missing":
						tasks = `null`
					case "wrong":
						tasks = `[{"` + field + `":"1123456789abcdef0123456789abcdef01234567"}]`
					case "duplicate":
						tasks = `[{"` + field + `":"` + hash + `"},{"` + field + `":"` + hash + `"}]`
					}
					if modern {
						return reply(200, `{"jsonrpc":"2.0","result":{"torrents":`+tasks+`},"id":1}`), nil
					}
					return reply(200, `{"result":"success","arguments":{"torrents":`+tasks+`}}`), nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				found, err := client.HasTorrent(context.Background(), hash)
				if found != (state == "found") || (err != nil) != (state != "found" && state != "absent") {
					t.Fatal(found, err)
				}
			})
		}
	}
}
