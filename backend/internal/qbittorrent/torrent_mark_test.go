package qbittorrent

import (
	"context"
	"encoding/json"
	"github.com/0xforee/nas-tools/backend/internal/torrentmark"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestTorrentMarkUsesAtomicTagsAndRechecksBeforeRemovingOpposite(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "replacement"}[changed], func(t *testing.T) {
			hash := strings.Repeat("a", 40)
			labels := []string{"BT", "keep"}
			added := int64(1)
			adds, removes := 0, 0
			c, _ := New("qb.test", "8080", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "auth/login"):
					return reply(200, "Ok."), nil
				case strings.HasSuffix(r.URL.Path, "torrents/info"):
					b, _ := json.Marshal([]any{map[string]any{"hash": hash, "added_on": added, "state": "uploading", "tags": strings.Join(labels, ", "), "progress": 1, "amount_left": 0, "isPrivate": true}})
					return reply(200, string(b)), nil
				case strings.HasSuffix(r.URL.Path, "torrents/trackers"):
					return reply(200, `[{"url":"** [DHT] **"},{"url":"https://tracker.test/a?passkey=fake"}]`), nil
				case strings.HasSuffix(r.URL.Path, "torrents/addTags"):
					_ = r.ParseForm()
					if r.Form.Get("tags") != "PT" || r.Form.Get("hashes") != hash {
						t.Fatal(r.Form)
					}
					adds++
					labels = append(labels, "PT", "concurrent")
					if changed {
						added++
					}
					return reply(200, ""), nil
				case strings.HasSuffix(r.URL.Path, "torrents/removeTags"):
					_ = r.ParseForm()
					if r.Form.Get("tags") != "BT" {
						t.Fatal(r.Form)
					}
					removes++
					labels = slices.DeleteFunc(labels, func(s string) bool { return s == "BT" })
					return reply(200, ""), nil
				}
				t.Fatal(r.URL)
				return nil, ErrResponse
			}))
			e, err := c.MarkEvidence(context.Background(), hash)
			if err != nil {
				t.Fatal(err)
			}
			err = c.SetMark(context.Background(), e, torrentmark.PT)
			if adds != 1 || changed && (err == nil || removes != 0) || !changed && (err != nil || removes != 1) || !slices.Contains(labels, "keep") || !slices.Contains(labels, "concurrent") {
				t.Fatal(err, adds, removes, labels)
			}
		})
	}
}
