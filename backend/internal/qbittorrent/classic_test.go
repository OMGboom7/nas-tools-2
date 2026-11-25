package qbittorrent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

// The downloaded content may be the same under several category/management
// settings. Test both the classic Web API reply and the original directory rules.
func TestClassicQbManagementAndReceipt(t *testing.T) {
	hash := strings.Repeat("a", 40)
	receipt := strings.Repeat("b", 32)
	marker := "NT" + receipt
	for _, tc := range []struct {
		name, mode, path, wantAuto string
		remoteAuto, update         bool
	}{
		{"manual", "manual", "", "false", true, false},
		{"server automatic", "default", "", "true", true, true},
		{"server manual", "default", "", "", false, false},
		{"automatic", "auto", "", "true", false, true},
		{"explicit path", "auto", "/chosen", "false", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			added, edited := 0, 0
			tags := ""
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch strings.TrimPrefix(r.URL.Path, "/api/v2/") {
				case "auth/login":
					w.Write([]byte("Ok."))
				case "app/version":
					w.Write([]byte("v4.3.9"))
				case "app/webapiVersion":
					w.Write([]byte("2.8.0"))
				case "app/preferences":
					json.NewEncoder(w).Encode(map[string]bool{"auto_tmm_enabled": tc.remoteAuto, "incomplete_files_ext": true})
				case "torrents/categories":
					w.Write([]byte(`{"series":{"savePath":"/old"}}`))
				case "torrents/editCategory":
					if r.FormValue("category") != "series" || r.FormValue("savePath") != "/series" {
						t.Error("wrong category update")
					}
					edited++
				case "torrents/info":
					if r.URL.Query().Get("hashes") != hash {
						t.Error("unscoped lookup")
					}
					if added == 0 {
						w.Write([]byte("[]"))
						return
					}
					json.NewEncoder(w).Encode([]map[string]string{{"hash": hash, "tags": tags}})
				case "torrents/add":
					added++
					if r.FormValue("paused") != "true" || r.FormValue("stopped") != "" || r.FormValue("contentLayout") != "Original" ||
						r.FormValue("autoTMM") != tc.wantAuto || r.FormValue("savepath") != tc.path || r.FormValue("category") != "series" {
						t.Error("wrong add options", r.Form)
					}
					tags = r.FormValue("tags")
					if tags != "site,"+marker {
						t.Error("wrong tags", tags)
					}
					w.Write([]byte("Ok."))
				default:
					t.Error("unexpected operation", r.URL.Path)
				}
			}))
			defer remote.Close()
			endpoint, _ := url.Parse(remote.URL)
			c, err := New(endpoint.Hostname(), endpoint.Port(), "", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			o := AddOptions{SavePath: tc.path, Category: "series", Tags: []string{"site"}, Paused: true}
			identity := torrentmeta.TransferMetadata{V1Hash: hash}
			submit := func() (string, error) {
				return c.AddClassic(t.Context(), "magnet:?xt=urn:btih:"+hash, nil, o, tc.mode, []ClassicDirectory{{Label: "series", SavePath: "/series"}}, receipt, identity)
			}
			if id, err := submit(); err != nil || id != hash || (edited == 1) != tc.update {
				t.Fatal(id, err, edited)
			}
			if _, err := submit(); err == nil || added != 1 {
				t.Fatal("existing task resubmitted")
			}
			if id, err := c.VerifyClassic(t.Context(), receipt, identity); err != nil || id != hash {
				t.Fatal(id, err)
			}
			if _, err := c.VerifyClassic(t.Context(), strings.Repeat("c", 32), identity); err == nil {
				t.Fatal("a foreign receipt claimed this task")
			}
		})
	}
}
