package transmission

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func globalReplyFixture(modern bool, up float64) string {
	values := map[string]any{"speed-limit-up": up, "speed-limit-down": 32, "speed-limit-up-enabled": true, "speed-limit-down-enabled": true, "alt-speed-enabled": false, "alt-speed-time-enabled": true, "alt-speed-up": 50, "alt-speed-down": 100}
	if modern {
		for key, v := range values {
			delete(values, key)
			values[strings.ReplaceAll(key, "-", "_")] = v
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": values})
		return string(b)
	}
	b, _ := json.Marshal(map[string]any{"result": "success", "arguments": values})
	return string(b)
}
func TestGlobalRatesNegotiateReadOnlyThenSetOnlyNormalPair(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(strconv.FormatBool(modern), func(t *testing.T) {
			handshakes, writes := 0, 0
			conflict := false
			client, _ := New("tr.test", "", "user", "private", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("X-Transmission-Session-Id") == "" {
					handshakes++
					p := reply(409, "")
					p.Header.Set("X-Transmission-Session-Id", "session")
					if modern {
						p.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
					}
					return p, nil
				}
				var v struct {
					Method    string
					Arguments map[string]any
					Params    map[string]any
				}
				if json.NewDecoder(r.Body).Decode(&v) != nil {
					t.Fatal("bad JSON")
				}
				method, args := "session-get", v.Arguments
				if modern {
					method, args = "session_get", v.Params
				}
				if v.Method == method {
					return reply(200, globalReplyFixture(modern, 0.5)), nil
				}
				if v.Method != strings.ReplaceAll(method, "get", "set") || len(args) != 4 {
					t.Fatal("wrong session mutation", v)
				}
				writes++
				key := "speed-limit-up"
				if modern {
					key = "speed_limit_up"
				}
				enabled := key + "-enabled"
				if modern {
					enabled = key + "_enabled"
				}
				if args[key] != float64(64) || args[enabled] != true {
					t.Fatal("wrong normal upload settings", args)
				}
				for name := range args {
					if strings.Contains(name, "alt") || strings.Contains(name, "torrent") {
						t.Fatal("unrelated mutation", args)
					}
				}
				if conflict {
					p := reply(409, "")
					p.Header.Set("X-Transmission-Session-Id", "new")
					return p, nil
				}
				if modern {
					return reply(200, `{"jsonrpc":"2.0","id":1,"result":{}}`), nil
				}
				return reply(200, `{"result":"success","arguments":{}}`), nil
			}))
			s, before, err := client.OpenGlobalRates(t.Context())
			if err != nil || before.Upload != 0.5 || !before.Scheduled || before.OtherUpload != 50 {
				t.Fatal(before, err)
			}
			after, err := before.Desired("transmission", 128, 64)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Write(t.Context(), "pair", after); err != nil {
				t.Fatal(err)
			}
			conflict = true
			if err := s.Write(t.Context(), "pair", after); err == nil {
				t.Fatal("write handshake replayed or false success")
			}
			if writes != 2 || handshakes != 1 {
				t.Fatal(writes, handshakes)
			}
		})
	}
}
func TestGlobalRateReplyRejectsIncompleteAndWrongIdentity(t *testing.T) {
	for _, tc := range []struct {
		modern bool
		body   string
	}{{false, `{"result":"success","arguments":{}}`}, {false, `{"result":"no","arguments":{}}`}, {true, `{"jsonrpc":"2.0","id":2,"result":{}}`}, {true, `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1}}`}, {false, strings.Replace(globalReplyFixture(false, 1), `"alt-speed-enabled":false`, `"alt-speed-enabled":null`, 1)}} {
		s := &GlobalRateSession{modern: tc.modern}
		if _, err := s.parse([]byte(tc.body)); err == nil {
			t.Fatal(tc.body)
		}
	}
}
