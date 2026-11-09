package qbittorrent

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

var transferMetainfo = []byte("d4:infod6:lengthi8e4:name9:movie.mkv6:pieces20:xxxxxxxxxxxxxxxxxxxxee")

func TestTransferAddUsesVersionedStoppedFormAndNeverRetries(t *testing.T) {
	for _, version := range []string{"v4.6.7", "v5.0.0"} {
		t.Run(version, func(t *testing.T) {
			adds := 0
			c, _ := New("qb.fixture", "", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "auth/login"):
					return reply(200, "Ok."), nil
				case strings.HasSuffix(r.URL.Path, "app/version"):
					return reply(200, version), nil
				case strings.HasSuffix(r.URL.Path, "app/webapiVersion"):
					return reply(200, "2.11.0"), nil
				case strings.HasSuffix(r.URL.Path, "app/buildInfo"):
					return reply(200, `{"libtorrent":"2.0.11.0"}`), nil
				case strings.HasSuffix(r.URL.Path, "torrents/info"):
					return reply(200, `[]`), nil
				case strings.HasSuffix(r.URL.Path, "torrents/add"):
					adds++
					if r.ParseMultipartForm(1<<20) != nil {
						t.Fatal("multipart")
					}
					defer r.MultipartForm.RemoveAll()
					key, absent := "paused", "stopped"
					if strings.HasPrefix(version, "v5.") {
						key, absent = absent, key
					}
					if r.FormValue(key) != "true" || r.FormValue(absent) != "" || r.FormValue("autoTMM") != "false" || r.FormValue("contentLayout") != "Original" || r.FormValue("skip_checking") != "false" || r.FormValue("savepath") != "/library" || r.FormValue("tags") != "已整理,转移做种" {
						t.Fatal("wrong stopped transfer form", r.Form)
					}
					f, _, err := r.FormFile("torrents")
					if err != nil {
						t.Fatal(err)
					}
					defer f.Close()
					got, _ := io.ReadAll(f)
					if string(got) != string(transferMetainfo) {
						t.Fatal("encoded info changed")
					}
					return nil, io.ErrUnexpectedEOF
				default:
					t.Fatal(r.URL)
					return nil, ErrResponse
				}
			}))
			if _, err := c.AddTransferPaused(t.Context(), transferMetainfo, "/library"); err == nil || adds != 1 {
				t.Fatal("lost add acknowledgment replayed", adds, err)
			}
		})
	}
}

func TestTransferSnapshotPreservesRawLabelsAndCheckingEvidence(t *testing.T) {
	m, _ := torrentmeta.PrepareTransfer(transferMetainfo, nil)
	row := map[string]any{"hash": m.V1Hash, "name": "movie.mkv", "state": "checkingUP", "added_on": 123, "save_path": "/library", "tags": "keep, exclude-substring", "auto_tmm": false, "progress": 1, "amount_left": 0}
	c, _ := New("qb.fixture", "", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "auth/login"):
			return reply(200, "Ok."), nil
		case strings.HasSuffix(r.URL.Path, "app/version"):
			return reply(200, "v5.0.0"), nil
		case strings.HasSuffix(r.URL.Path, "app/webapiVersion"):
			return reply(200, "2.11.0"), nil
		case strings.HasSuffix(r.URL.Path, "app/buildInfo"):
			return reply(200, `{"libtorrent":"2.0.11.0"}`), nil
		case strings.HasSuffix(r.URL.Path, "torrents/info"):
			body, _ := json.Marshal([]any{row})
			return reply(200, string(body)), nil
		case strings.HasSuffix(r.URL.Path, "torrents/properties"):
			body, _ := json.Marshal(map[string]any{"infohash_v1": m.V1Hash, "infohash_v2": ""})
			return reply(200, string(body)), nil
		case strings.HasSuffix(r.URL.Path, "torrents/files"):
			return reply(200, `[{"index":7,"name":"movie.mkv","size":8,"priority":1,"progress":1}]`), nil
		default:
			t.Fatal(r.URL)
			return nil, ErrResponse
		}
	}))
	s, err := c.TransferTask(t.Context(), m.V1Hash)
	if err != nil || s.RawQbTags != "keep, exclude-substring" || !s.Checking || s.FullComplete(m) || !s.Matches(m) {
		t.Fatal(s, err)
	}
	row["state"] = "stoppedUP"
	s, err = c.TransferTask(t.Context(), m.V1Hash)
	if err != nil || !s.FullComplete(m) || !s.Paused {
		t.Fatal(s, err)
	}
	row["added_on"] = 124
	if c.StartTransfer(t.Context(), s, m) == nil {
		t.Fatal("replaced task authorized start")
	}
}

func TestTransferWireIDDependsOnActualEngineAndKeepsFullHashes(t *testing.T) {
	m := torrentmeta.TransferMetadata{V1Hash: strings.Repeat("a", 40), V2Hash: strings.Repeat("b", 64)}
	for _, v2 := range []bool{false, true} {
		p := TransferProtocol{V2: v2}
		id, err := p.WireID(m)
		want := m.V1Hash
		if v2 {
			want = m.V2Hash[:40]
		}
		if err != nil || id != want {
			t.Fatal(id, err)
		}
	}
	m.V1Hash = ""
	if _, err := (TransferProtocol{}).WireID(m); err == nil {
		t.Fatal("pure v2 accepted by v1 engine")
	}
}
