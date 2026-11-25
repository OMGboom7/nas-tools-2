package transmission

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

var transferMetainfo = []byte("d4:infod6:lengthi8e4:name9:movie.mkv6:pieces20:xxxxxxxxxxxxxxxxxxxxee")

func TestTransferRejectsRepeatedPaddingPathsBeforeRPC(t *testing.T) {
	const payload = "d6:lengthi1e4:pathl5:a.mkvee"
	const padding = "d4:attr1:p6:lengthi1e4:pathl4:.pad1:1ee"
	content := []byte("d4:infod5:filesl" + payload + padding + padding + "e4:name4:show12:piece lengthi4e6:pieces20:xxxxxxxxxxxxxxxxxxxxee")
	meta, err := torrentmeta.PrepareTransfer(content, nil)
	if err != nil || len(meta.Files) != 3 {
		t.Fatal(meta.Files, err)
	}
	c, _ := New("tr.fixture", "", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected RPC for unrepresentable layout: %s", r.URL.Path)
		return nil, nil
	}))
	if _, err := c.TransferWireID(t.Context(), meta); !errors.Is(err, torrenttransfer.ErrProtocol) {
		t.Fatal(err)
	}
	if _, err := c.AddTransferPaused(t.Context(), content, "/library"); !errors.Is(err, torrenttransfer.ErrProtocol) {
		t.Fatal(err)
	}
}

func TestTransferAddDistinguishesDuplicateInBothRPCDialects(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, duplicate := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{true: "modern", false: "legacy"}[modern], map[bool]string{true: "duplicate", false: "added"}[duplicate]}, "/"), func(t *testing.T) {
				meta, _ := torrentmeta.PrepareTransfer(transferMetainfo, nil)
				adds := 0
				c, _ := New("tr.fixture", "", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("X-Transmission-Session-Id") == "" {
						out := reply(409, "")
						out.Header.Set("X-Transmission-Session-Id", "fixture-token")
						if modern {
							out.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
						}
						return out, nil
					}
					var in struct {
						Method    string         `json:"method"`
						Arguments map[string]any `json:"arguments"`
						Params    map[string]any `json:"params"`
					}
					if json.NewDecoder(r.Body).Decode(&in) != nil {
						t.Fatal("invalid request")
					}
					args := in.Arguments
					key := func(old, new string) string {
						if modern {
							return new
						}
						return old
					}
					if modern {
						args = in.Params
					}
					result := map[string]any{}
					switch in.Method {
					case key("session-get", "session_get"):
						result = map[string]any{"version": "4.0.6", key("rpc-version", "rpc_version"): 17}
					case key("torrent-add", "torrent_add"):
						adds++
						if args["paused"] != true || args[key("download-dir", "download_dir")] != "/library" || args["metainfo"] == nil || len(args["labels"].([]any)) != 2 {
							t.Fatal(args)
						}
						name := key("torrent-added", "torrent_added")
						if duplicate {
							name = key("torrent-duplicate", "torrent_duplicate")
						}
						result[name] = map[string]any{key("hashString", "hash_string"): meta.V1Hash, "id": 7, "name": "movie.mkv"}
					default:
						t.Fatal(in.Method)
					}
					out := map[string]any{"result": "success", "arguments": result, "tag": 1}
					if modern {
						out = map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}
					}
					body, _ := json.Marshal(out)
					return reply(200, string(body)), nil
				}))
				r, err := c.AddTransferPaused(t.Context(), transferMetainfo, "/library")
				if err != nil || r.WireID != meta.V1Hash || r.Duplicate != duplicate || !r.CreatedKnown || adds != 1 {
					t.Fatal(r, err, adds)
				}
			})
		}
	}
}

func TestTransferMutationLostAcknowledgmentIsNotRetried(t *testing.T) {
	adds := 0
	c, _ := New("tr.fixture", "", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
		var in struct {
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Method == "session-get" {
			return reply(200, `{"result":"success","tag":1,"arguments":{"version":"4.0.6","rpc-version":17}}`), nil
		}
		if in.Method != "torrent-add" {
			t.Fatal(in.Method)
		}
		adds++
		return nil, io.ErrUnexpectedEOF
	}))
	if _, err := c.AddTransferPaused(t.Context(), transferMetainfo, "/library"); err == nil || adds != 1 {
		t.Fatal(adds, err)
	}
}

func TestTransferSnapshotBothDialectsRequireFileAndIncarnationProof(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "modern"}[modern], func(t *testing.T) {
			meta, _ := torrentmeta.PrepareTransfer(transferMetainfo, nil)
			status, done, added := 0, int64(8), int64(123)
			key := func(a, b string) string {
				if modern {
					return b
				}
				return a
			}
			c, _ := New("tr.fixture", "", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("X-Transmission-Session-Id") == "" {
					out := reply(409, "")
					out.Header.Set("X-Transmission-Session-Id", "fixture-token")
					if modern {
						out.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
					}
					return out, nil
				}
				var request struct {
					Method string `json:"method"`
				}
				json.NewDecoder(r.Body).Decode(&request)
				if request.Method != key("torrent-get", "torrent_get") {
					t.Fatal("unexpected mutation", request.Method)
				}
				row := map[string]any{key("hashString", "hash_string"): meta.V1Hash, "name": "movie.mkv", key("downloadDir", "download_dir"): "/library", key("addedDate", "added_date"): added, "status": status, key("percentDone", "percent_done"): 1, key("metadataPercentComplete", "metadata_percent_complete"): 1, key("leftUntilDone", "left_until_done"): 0, "error": 0, "labels": []string{}, "files": []any{map[string]any{"name": "movie.mkv", "length": 8, key("bytesCompleted", "bytes_completed"): done}}, key("fileStats", "file_stats"): []any{map[string]any{"wanted": true, key("bytesCompleted", "bytes_completed"): done}}, key("torrentFile", "torrent_file"): "/daemon/torrents/movie.fixture.torrent", "trackers": []any{map[string]any{"announce": "https://fixture.test/tracker"}}}
				body := map[string]any{"torrents": []any{row}}
				out := map[string]any{"result": "success", "arguments": body, "tag": 1}
				if modern {
					out = map[string]any{"jsonrpc": "2.0", "id": 1, "result": body}
				}
				raw, _ := json.Marshal(out)
				return reply(200, string(raw)), nil
			}))
			s, err := c.TransferTask(t.Context(), meta.V1Hash)
			if err != nil || !s.FullComplete(meta) || !s.Paused || s.MetainfoFile != "movie.fixture.torrent" {
				t.Fatal(s, err)
			}
			done = 7
			s, err = c.TransferTask(t.Context(), meta.V1Hash)
			if err != nil || s.FullComplete(meta) {
				t.Fatal("aggregate progress replaced file evidence", err)
			}
			done, status = 8, 2
			s, err = c.TransferTask(t.Context(), meta.V1Hash)
			if err != nil || !s.Checking || s.FullComplete(meta) {
				t.Fatal(s, err)
			}
			added = 0
			if _, err := c.TransferTask(t.Context(), meta.V1Hash); err == nil {
				t.Fatal("missing incarnation accepted")
			}
		})
	}
}

func TestReseedVerifiedRequiresExactInstanceAndVerifiedBytes(t *testing.T) {
	meta, err := torrentmeta.PrepareTransfer(transferMetainfo, nil)
	if err != nil {
		t.Fatal(err)
	}
	label := "iyuu-" + strings.Repeat("f", 64)
	task := torrenttransfer.Task{Kind: "transmission", WireID: meta.V1Hash, V1Hash: meta.V1Hash, AddedAt: 33, Directory: "/data", Name: "movie.mkv", State: "stopped", Complete: true, Paused: true, Labels: []string{label}, Files: meta.Files, Done: []int{0}}
	task.Files = slices.Clone(meta.Files)
	task.Files[0].Selected = true
	if !task.FullComplete(meta) {
		t.Fatal("fixture files")
	}
	for _, modern := range []bool{false, true} {
		row := map[string]any{}
		key := func(old, new string) string {
			if modern {
				return new
			}
			return old
		}
		row[key("hashString", "hash_string")] = meta.V1Hash
		row[key("addedDate", "added_date")] = 33
		row["status"] = 0
		row[key("totalSize", "total_size")] = 8
		row["labels"] = []string{label}
		row[key("downloadDir", "download_dir")] = "/data"
		c, _ := New("tr.fixture", "", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("X-Transmission-Session-Id") == "" {
				o := reply(409, "")
				o.Header.Set("X-Transmission-Session-Id", "fixture")
				if modern {
					o.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
				}
				return o, nil
			}
			var q struct{ Method string }
			if json.NewDecoder(r.Body).Decode(&q) != nil || q.Method != key("torrent-get", "torrent_get") {
				t.Fatal(q.Method)
			}
			data := map[string]any{"torrents": []any{row}}
			env := map[string]any{"result": "success", "tag": 1, "arguments": data}
			if modern {
				env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": data}
			}
			b, _ := json.Marshal(env)
			return reply(200, string(b)), nil
		}))
		for _, v := range []struct {
			valid, unchecked, added int
			want                    bool
		}{{8, 0, 33, true}, {7, 1, 33, false}, {8, 0, 34, false}} {
			row[key("haveValid", "have_valid")] = v.valid
			row[key("haveUnchecked", "have_unchecked")] = v.unchecked
			row[key("addedDate", "added_date")] = v.added
			ok, err := c.ReseedVerified(t.Context(), task, meta)
			if err != nil || ok != v.want {
				t.Fatal(modern, v, ok, err)
			}
		}
	}
}
