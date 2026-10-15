package aria2

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestOrganizationAbsenceRequiresAllQueuesAndNeverTreatsFailureOrBoundsAsAbsence(t *testing.T) {
	for _, kind := range []string{"absent", "present", "failure", "bound"} {
		calls := 0
		c, _ := New("aria.test", "6800", "token", transportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			var req struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if !strings.HasPrefix(req.Method, "aria2.tell") {
				t.Fatal("mutation", req)
			}
			if kind == "failure" {
				return nil, errors.New("network")
			}
			rows := []map[string]string{}
			if req.Method == "aria2.tellStopped" {
				if kind == "present" {
					rows = append(rows, map[string]string{"gid": "0123456789abcdef"})
				}
				if kind == "bound" {
					for i := 0; i < 1000; i++ {
						rows = append(rows, map[string]string{"gid": "fedcba9876543210"})
					}
				}
			}
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": rows})
			return reply(200, string(raw)), nil
		}))
		present, err := c.HasOrganizationTask(context.Background(), "0123456789abcdef")
		switch kind {
		case "absent":
			if err != nil || present || calls != 3 {
				t.Fatal(kind, present, err, calls)
			}
		case "present":
			if err != nil || !present {
				t.Fatal(kind, present, err)
			}
		default:
			if err == nil {
				t.Fatal("uncertainty became absence", kind)
			}
		}
	}
}
