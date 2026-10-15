package transmission

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type profileTransport func(*http.Request) (*http.Response, error)

func (f profileTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestProfileLimitsBothProtocolsUseOnlyExactTaskAndStrictEvidence(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "modern"}[modern], func(t *testing.T) {
			hash := strings.Repeat("a", 40)
			limits := Limits{UploadKB: 10, DownloadKB: 20, Ratio: 1.5, IdleMinutes: 60}
			mutation := 0
			client, err := New("tr.test", "", "", "", profileTransport(func(r *http.Request) (*http.Response, error) {
				reply := func(code int, body string) *http.Response {
					return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				}
				if r.Header.Get("X-Transmission-Session-Id") == "" {
					response := reply(409, "")
					response.Header.Set("X-Transmission-Session-Id", "session")
					if modern {
						response.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
					}
					return response, nil
				}
				var request struct {
					Method    string                     `json:"method"`
					Arguments map[string]json.RawMessage `json:"arguments"`
					Params    map[string]json.RawMessage `json:"params"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					t.Fatal("payload")
				}
				args := request.Arguments
				if modern {
					args = request.Params
				}
				var ids []string
				_ = json.Unmarshal(args["ids"], &ids)
				if len(ids) != 1 || ids[0] != hash {
					t.Fatal(ids)
				}
				data := map[string]any{}
				if strings.Contains(request.Method, "set") {
					mutation++
					if len(args) != 9 {
						t.Fatal("widened mutation", args)
					}
					key := "seedRatioLimit"
					if modern {
						key = "seed_ratio_limit"
					}
					if string(args[key]) != "1.5" {
						t.Fatal(args)
					}
				} else {
					keys := []string{"hashString", "downloadDir", "labels", "status", "uploadLimit", "uploadLimited", "downloadLimit", "downloadLimited", "seedRatioLimit", "seedRatioMode", "seedIdleLimit", "seedIdleMode"}
					if modern {
						keys = []string{"hash_string", "download_dir", "labels", "status", "upload_limit", "upload_limited", "download_limit", "download_limited", "seed_ratio_limit", "seed_ratio_mode", "seed_idle_limit", "seed_idle_mode"}
					}
					values := []any{hash, "/downloads", []string{"NASTOOL"}, 0, 10, true, 20, true, 1.5, 1, 60, 1}
					row := map[string]any{}
					for i, k := range keys {
						row[k] = values[i]
					}
					data["torrents"] = []any{row}
				}
				env := map[string]any{"result": "success", "arguments": data}
				if modern {
					env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": data}
				}
				body, _ := json.Marshal(env)
				return reply(200, string(body)), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if err := client.SetLimits(t.Context(), hash, limits); err != nil {
				t.Fatal(err)
			}
			s, err := client.Profile(t.Context(), hash)
			if err != nil || !s.Matches(limits) || mutation != 1 {
				t.Fatal(s, err, mutation)
			}
			if s.Matches(Limits{}) {
				t.Fatal("global and unlimited modes confused")
			}
			if err := client.SetLimits(context.Background(), "all", limits); err != ErrConfiguration {
				t.Fatal(err)
			}
		})
	}
}
