package transmission

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSeedingEvidenceBothVersionsUsesOnlyDaemonCounters(t *testing.T) {
	hash := strings.Repeat("a", 40)
	for _, modern := range []bool{false, true} {
		for _, test := range []struct {
			name              string
			status            int
			missing, sentinel bool
		}{
			{"seeding", 6, false, false}, {"stopped", 0, false, false}, {"checking", 2, false, false}, {"missing", 6, true, false}, {"sentinel", 6, false, true},
		} {
			t.Run(test.name+map[bool]string{false: "legacy", true: "modern"}[modern], func(t *testing.T) {
				c, _ := New("tr.test", "9091", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("X-Transmission-Session-Id") == "" {
						resp := reply(409, "")
						resp.Header.Set("X-Transmission-Session-Id", "session")
						if modern {
							resp.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
						}
						return resp, nil
					}
					var p struct {
						Method    string
						Arguments map[string]json.RawMessage
						Params    map[string]json.RawMessage
					}
					if json.NewDecoder(r.Body).Decode(&p) != nil {
						t.Fatal("bad request")
					}
					args := p.Arguments
					method := "torrent-get"
					hashKey, progressKey, leftKey, metaKey, secondsKey, ratioKey := "hashString", "percentDone", "leftUntilDone", "metadataPercentComplete", "secondsSeeding", "uploadRatio"
					if modern {
						args = p.Params
						method = "torrent_get"
						hashKey, progressKey, leftKey, metaKey, secondsKey, ratioKey = "hash_string", "percent_done", "left_until_done", "metadata_percent_complete", "seconds_seeding", "upload_ratio"
					}
					var ids, fields []string
					_ = json.Unmarshal(args["ids"], &ids)
					_ = json.Unmarshal(args["fields"], &fields)
					if p.Method != method || len(ids) != 1 || ids[0] != hash || len(fields) != 8 {
						t.Fatal(p)
					}
					row := map[string]any{hashKey: hash, "status": test.status, progressKey: 1, leftKey: 0, metaKey: 1, "error": 0, "doneDate": 1}
					if !test.missing {
						row[secondsKey] = 900
						row[ratioKey] = 1.2
					}
					if test.sentinel {
						row[secondsKey] = -1
						row[ratioKey] = -2
					}
					payload := map[string]any{"torrents": []any{row}}
					env := map[string]any{"result": "success", "arguments": payload}
					if modern {
						env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": payload}
					}
					body, _ := json.Marshal(env)
					return reply(200, string(body)), nil
				}))
				e, err := c.SeedingEvidence(context.Background(), hash)
				if err != nil {
					t.Fatal(err)
				}
				if e.Complete == nil || *e.Complete != (test.status == 0 || test.status == 6) {
					t.Fatal(e)
				}
				if test.missing || test.sentinel {
					if e.Seconds != nil || e.Ratio != nil {
						t.Fatal("invented counters", e)
					}
				} else if e.Seconds == nil || *e.Seconds != 900 || e.Ratio == nil || *e.Ratio != 1.2 {
					t.Fatal(e)
				}
			})
		}
	}
}

func TestSeedingEvidenceMalformedOrUnknownIsNotZero(t *testing.T) {
	hash := strings.Repeat("c", 40)
	for _, tc := range []struct {
		row  string
		fail bool
	}{
		{`"status":6`, false}, {`"status":6,"percentDone":null,"leftUntilDone":0,"metadataPercentComplete":1,"error":0`, false},
		{`"status":7`, true}, {`"status":null`, true}, {`"status":6,"secondsSeeding":"1"`, true}, {`"status":6,"percentDone":2`, true}, {`"status":6,"leftUntilDone":-1`, true},
	} {
		c, _ := New("tr.test", "9091", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
			return reply(200, `{"result":"success","arguments":{"torrents":[{"hashString":"`+hash+`",`+tc.row+`}]}}`), nil
		}))
		e, err := c.SeedingEvidence(context.Background(), hash)
		if (err != nil) != tc.fail {
			t.Fatal(tc, e, err)
		}
		if !tc.fail && (e.Complete != nil || e.Seconds != nil || e.Ratio != nil) {
			t.Fatal("unknown became known", e)
		}
	}
}
