package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func profileRow(hash string) map[string]any {
	return map[string]any{"hash": hash, "state": "stoppedDL", "save_path": "/downloads", "category": "tv", "tags": "user,NASTOOL-QB-PROFILE-id", "auto_tmm": false, "force_start": false, "up_limit": 10240, "dl_limit": -1, "ratio_limit": -1, "seeding_time_limit": -1, "inactive_seeding_time_limit": -1, "max_ratio": -1, "max_seeding_time": -1, "max_inactive_seeding_time": -1, "share_limit_action": "Default"}
}
func TestPTProfileVersionedAddLimitsAndStartupNeverUseFallbackMutation(t *testing.T) {
	for _, version := range []string{"v4.6.7", "v5.0.2", "v5.1.0", "v5.2.3"} {
		for _, lost := range []bool{false, true} {
			t.Run(version+map[bool]string{false: "/ok", true: "/lost"}[lost], func(t *testing.T) {
				mutations := []string{}
				hash := strings.Repeat("a", 40)
				c, _ := New("qb.test", "8080", "user", "secret", transportFunc(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "auth/login") {
						return reply(200, "Ok."), nil
					}
					if strings.HasSuffix(r.URL.Path, "app/version") {
						return reply(200, version), nil
					}
					if r.Method == "GET" {
						if r.URL.Query().Get("hashes") != hash || r.URL.Query().Get("filter") != "all" {
							t.Fatal(r.URL)
						}
						row := profileRow(hash)
						if strings.HasPrefix(version, "v4") {
							row["state"] = "pausedDL"
						}
						if !needsShareAction(strings.TrimPrefix(version, "v")) {
							delete(row, "share_limit_action")
						}
						body, _ := json.Marshal([]any{row})
						return reply(200, string(body)), nil
					}
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					mutations = append(mutations, r.URL.Path)
					switch {
					case strings.HasSuffix(r.URL.Path, "torrents/add"):
						key, other := "stopped", "paused"
						if strings.HasPrefix(version, "v4") {
							key, other = other, key
						}
						if r.Form.Get(key) != "true" || r.Form.Get(other) != "" || r.Form.Get("autoTMM") != "false" || r.Form.Get("stopCondition") != "None" || r.Form.Get("ratioLimit") != "-1" || r.Form.Get("seedingTimeLimit") != "-1" || r.Form.Get("inactiveSeedingTimeLimit") != "-1" {
							t.Fatal(r.Form)
						}
						return reply(200, "Ok."), nil
					case strings.HasSuffix(r.URL.Path, "setShareLimits"):
						if r.Form.Get("hashes") != hash || r.Form.Get("ratioLimit") != "-1" || r.Form.Get("seedingTimeLimit") != "-1" || r.Form.Get("inactiveSeedingTimeLimit") != "-1" {
							t.Fatal(r.Form)
						}
						return reply(200, ""), nil
					default:
						want := "/api/v2/torrents/start"
						if strings.HasPrefix(version, "v4") {
							want = "/api/v2/torrents/resume"
						}
						if r.URL.Path != want || r.Form.Get("hashes") != hash {
							t.Fatal(r.URL, r.Form)
						}
						if lost {
							return nil, context.DeadlineExceeded
						}
						return reply(200, ""), nil
					}
				}))
				if err := c.AddMagnetWithOptions(t.Context(), "magnet:?xt=urn:btih:"+hash, AddOptions{ManagedPT: true}); err != nil {
					t.Fatal(err)
				}
				state, err := c.Profile(t.Context(), hash)
				if err != nil || !state.Unlimited() || !state.Paused() || !state.SpeedsMatch(AddOptions{UploadLimitKB: 10}) {
					t.Fatal(state, err)
				}
				if err := c.SetUnlimited(t.Context(), hash, state.ShareAction); err != nil {
					t.Fatal(err)
				}
				err = c.StartProfile(t.Context(), hash)
				if (err != nil) != lost || len(mutations) != 3 {
					t.Fatal(err, mutations)
				}
			})
		}
	}
}
func TestPTProfileMissingIdentityAndLimitsAreNeverInferred(t *testing.T) {
	hash := strings.Repeat("b", 40)
	for _, field := range []string{"hash", "state", "save_path", "category", "tags", "auto_tmm", "force_start", "up_limit", "dl_limit", "ratio_limit", "seeding_time_limit", "inactive_seeding_time_limit", "max_ratio", "max_seeding_time", "max_inactive_seeding_time", "share_limit_action"} {
		t.Run(field, func(t *testing.T) {
			row := profileRow(hash)
			delete(row, field)
			body, _ := json.Marshal([]any{row})
			c, _ := New("qb.test", "8080", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "auth/login") {
					return reply(200, "Ok."), nil
				}
				if strings.HasSuffix(r.URL.Path, "app/version") {
					return reply(200, "v5.2.3"), nil
				}
				return reply(200, string(body)), nil
			}))
			if _, err := c.Profile(t.Context(), hash); err == nil {
				t.Fatal("missing field accepted", field)
			}
		})
	}
	for _, version := range []string{"v4.5.5", "v6.0.0", "v5.0.0beta", "v5", "<html>secret</html>"} {
		writes := 0
		c, _ := New("qb.test", "8080", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "auth/login") {
				return reply(200, "Ok."), nil
			}
			if r.Method != "GET" {
				writes++
			}
			return reply(200, version), nil
		}))
		if err := c.StartProfile(t.Context(), hash); err == nil || writes != 0 {
			t.Fatal(version, err, writes)
		}
	}
}
