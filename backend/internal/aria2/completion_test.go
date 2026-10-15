package aria2

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCompletionReadsAriaSelectedFileEvidenceAndDoesNotExposeURIs(t *testing.T) {
	for _, status := range []string{"complete", "active", "paused", "waiting", "error", "removed"} {
		c, err := New("aria.example", "6800", "private-token", transportFunc(func(r *http.Request) (*http.Response, error) {
			var payload struct {
				ID     string            `json:"id"`
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Method != "aria2.tellStatus" || len(payload.Params) != 3 {
				t.Fatal("mutation", payload)
			}
			var token, gid string
			_ = json.Unmarshal(payload.Params[0], &token)
			_ = json.Unmarshal(payload.Params[1], &gid)
			if token != "token:private-token" || gid != "0123456789abcdef" {
				t.Fatal("scope")
			}
			return reply(200, `{"jsonrpc":"2.0","id":"nastool-completion","result":{"gid":"0123456789abcdef","status":"`+status+`","dir":"/downloads/","totalLength":"14","completedLength":"14","files":[{"path":"/downloads/Film.mkv","length":"14","completedLength":"14","selected":"true","uris":[{"uri":"https://private-token"}]}]}}`), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		task, err := c.Completion(context.Background(), "0123456789abcdef")
		raw, _ := json.Marshal(task)
		if err != nil || task.Complete != (status == "complete") || task.LabelsKnown || len(task.Files) != 1 || task.Files[0].Path != "Film.mkv" || strings.Contains(string(raw), "private-token") {
			t.Fatal(task, err)
		}
	}
}
func TestCompletionRejectsAriaMissingCountersUnsafePathsAndPartialSelectedFiles(t *testing.T) {
	good := `{"gid":"0123456789abcdef","status":"complete","dir":"/downloads","totalLength":"14","completedLength":"14","files":[{"path":"/downloads/Film.mkv","length":"14","completedLength":"14","selected":"true"}]}`
	for _, kind := range []string{"path", "missing-selected", "missing-total", "invalid-number", "wrong-id", "partial"} {
		raw := good
		switch kind {
		case "path":
			raw = strings.ReplaceAll(raw, "/downloads/Film.mkv", "/downloads/../Film.mkv")
		case "missing-selected":
			raw = strings.ReplaceAll(raw, `,"selected":"true"`, "")
		case "missing-total":
			raw = strings.ReplaceAll(raw, `"totalLength":"14",`, "")
		case "invalid-number":
			raw = strings.ReplaceAll(raw, `"length":"14"`, `"length":"-1"`)
		case "wrong-id":
			raw = strings.ReplaceAll(raw, "0123456789abcdef", "fedcba9876543210")
		case "partial":
			raw = strings.ReplaceAll(raw, `"completedLength":"14","selected"`, `"completedLength":"13","selected"`)
		}
		c, _ := New("aria.example", "6800", "private", transportFunc(func(r *http.Request) (*http.Response, error) {
			return reply(200, `{"jsonrpc":"2.0","id":"nastool-completion","result":`+raw+`}`), nil
		}))
		task, err := c.Completion(context.Background(), "0123456789abcdef")
		if kind == "partial" {
			if err != nil || task.Complete {
				t.Fatal(task, err)
			}
		} else if !errors.Is(err, ErrResponse) {
			t.Fatal(kind, task, err)
		}
	}
}
