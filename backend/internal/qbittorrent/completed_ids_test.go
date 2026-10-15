package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCompletedIDsRejectNullDuplicateInvalidAndOverLimitLists(t *testing.T) {
	hash := strings.Repeat("a", 40)
	for _, kind := range []string{"valid", "empty", "null", "duplicate", "bad", "limit"} {
		rows := []map[string]string{{"hash": hash}}
		switch kind {
		case "empty":
			rows = []map[string]string{}
		case "null":
			rows = nil
		case "duplicate":
			rows = append(rows, map[string]string{"hash": strings.ToUpper(hash)})
		case "bad":
			rows[0]["hash"] = "all"
		case "limit":
			rows = make([]map[string]string, 1001)
		}
		raw, _ := json.Marshal(rows)
		c, _ := New("qb.test", "8080", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "auth/login") {
				return reply(200, "Ok."), nil
			}
			if r.Method != "GET" || r.URL.Query().Get("filter") != "completed" || r.URL.Query().Get("limit") != "1001" {
				t.Fatal(r.URL)
			}
			return reply(200, string(raw)), nil
		}))
		ids, err := c.CompletedIDs(context.Background())
		if kind == "valid" || kind == "empty" {
			if err != nil {
				t.Fatal(kind, ids, err)
			}
		} else if !errors.Is(err, ErrResponse) {
			t.Fatal(kind, ids, err)
		}
	}
}
func TestStopForOrganizationSelectsOneVersionedMutationAndNeverFallsBackAfterFailure(t *testing.T) {
	for _, version := range []string{"v4.6.7", "v5.0.2", "v6.0.0", "unknown"} {
		for _, lost := range []bool{false, true} {
			calls := 0
			c, _ := New("qb.test", "8080", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "auth/login") {
					return reply(200, "Ok."), nil
				}
				if strings.HasSuffix(r.URL.Path, "app/version") {
					return reply(200, version), nil
				}
				calls++
				path := "/api/v2/torrents/pause"
				if strings.HasPrefix(version, "v5") {
					path = "/api/v2/torrents/stop"
				}
				_ = r.ParseForm()
				if r.URL.Path != path || r.Method != "POST" || r.Form.Get("hashes") != strings.Repeat("a", 40) {
					t.Fatal(r.URL, r.Form)
				}
				if lost {
					return nil, errors.New("lost response")
				}
				return reply(200, ""), nil
			}))
			err := c.StopForOrganization(context.Background(), strings.Repeat("a", 40))
			valid := strings.HasPrefix(version, "v4") || strings.HasPrefix(version, "v5")
			if !valid {
				if err == nil || calls != 0 {
					t.Fatal(version, err, calls)
				}
			} else if calls != 1 || lost && err == nil || !lost && err != nil {
				t.Fatal(version, lost, err, calls)
			}
		}
	}
}
