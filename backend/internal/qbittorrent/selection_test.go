package qbittorrent

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSelectionUsesExplicitIndexesAndBothStoppedAddAPIs(t *testing.T) {
	for _, version := range []string{"v4.6.7", "v5.2.1"} {
		t.Run(version, func(t *testing.T) {
			writes := 0
			hash := strings.Repeat("a", 40)
			c, _ := New("qb.test", "8080", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "auth/login"):
					return reply(200, "Ok."), nil
				case strings.HasSuffix(r.URL.Path, "app/version"):
					return reply(200, version), nil
				case strings.HasSuffix(r.URL.Path, "torrents/info"):
					return reply(200, `[{"hash":"`+hash+`","state":"stoppedDL","save_path":"/downloads","tags":"NASTOOL,NASTOOL-FILES-id"}]`), nil
				case strings.HasSuffix(r.URL.Path, "torrents/files"):
					return reply(200, `[{"index":7,"name":"Show/S01E02.mkv","size":12,"priority":0},{"index":2,"name":"Show/S01E01.mkv","size":11,"priority":1}]`), nil
				case strings.HasSuffix(r.URL.Path, "torrents/add"):
					if r.ParseMultipartForm(1<<20) != nil {
						t.Fatal("multipart")
					}
					if r.MultipartForm != nil {
						defer r.MultipartForm.RemoveAll()
					}
					key := "stopped"
					if strings.HasPrefix(version, "v4") {
						key = "paused"
					}
					if r.FormValue(key) != "true" || r.FormValue("contentLayout") != "Original" {
						t.Fatal(r.Form)
					}
					writes++
					return reply(200, "Ok."), nil
				case strings.HasSuffix(r.URL.Path, "filePrio"):
					r.ParseForm()
					if r.Form.Get("hash") != hash || r.Form.Get("id") != "7" || r.Form.Get("priority") != "1" {
						t.Fatal(r.Form)
					}
					writes++
					return reply(200, ""), nil
				default:
					t.Fatal("unexpected request", r.URL)
					return nil, ErrResponse
				}
			}))
			s, err := c.Selection(t.Context(), hash)
			if err != nil || len(s.Files) != 2 || s.Files[0].Index != 2 || s.Files[1].Index != 7 || !s.Paused {
				t.Fatal(s, err)
			}
			if err := c.AddTorrentWithOptions(t.Context(), []byte("fixture"), AddOptions{Paused: true, SelectedFiles: true}); err != nil {
				t.Fatal(err)
			}
			if err := c.SetFilePriority(t.Context(), hash, []int{7}, true); err != nil {
				t.Fatal(err)
			}
			for _, ids := range [][]int{nil, {-1}, {7, 7}} {
				if c.SetFilePriority(t.Context(), hash, ids, true) == nil {
					t.Fatal("invalid ids accepted", ids)
				}
			}
			if writes != 2 {
				t.Fatal(writes)
			}
		})
	}
}
func TestSelectionRejectsMissingPriorityIndexAndExtraPaths(t *testing.T) {
	for _, bad := range []string{"index", "priority", "size", "name"} {
		t.Run(bad, func(t *testing.T) {
			f := map[string]any{"index": 0, "name": "Show/S01E01.mkv", "size": 12, "priority": 1}
			delete(f, bad)
			raw, _ := json.Marshal([]any{f})
			c, _ := New("qb.test", "8080", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "auth/login") {
					return reply(200, "Ok."), nil
				}
				if strings.HasSuffix(r.URL.Path, "files") {
					return reply(200, string(raw)), nil
				}
				return reply(200, `[{"hash":"`+strings.Repeat("a", 40)+`","state":"stoppedDL","save_path":"/downloads","tags":""}]`), nil
			}))
			if _, err := c.Selection(t.Context(), strings.Repeat("a", 40)); err == nil {
				t.Fatal("missing selection field accepted", bad)
			}
		})
	}
}
