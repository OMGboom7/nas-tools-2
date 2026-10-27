package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

func filesBencode(v any) string {
	switch v := v.(type) {
	case string:
		return strconv.Itoa(len(v)) + ":" + v
	case int:
		return fmt.Sprintf("i%de", v)
	case []any:
		s := "l"
		for _, x := range v {
			s += filesBencode(x)
		}
		return s + "e"
	case map[string]any:
		keys := []string{}
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s := "d"
		for _, k := range keys {
			s += filesBencode(k) + filesBencode(v[k])
		}
		return s + "e"
	}
	panic("fixture")
}

type subscriptionFilesFixture struct {
	*subscriptionRunFixture
	kind, version, hash, loss                    string
	raw                                          []byte
	files                                        []torrentfiles.File
	present, paused, ignoreWrite, changeManifest bool
	labels                                       []string
	operations                                   []string
	starts, priorities, settings                 int
	ratio                                        float64
	ratioMode, idleMode                          int
	afterSelection                               func()
	expectedPaths                                []string
}

func newSubscriptionFilesFixture(t *testing.T, kind string) *subscriptionFilesFixture {
	t.Helper()
	base := newSubscriptionRunFixture(t)
	w := &subscriptionFilesFixture{subscriptionRunFixture: base, kind: kind, version: "v5.2.1", paused: true, ratioMode: 2, idleMode: 2}
	if kind == "qb4" {
		w.version = "v4.6.7"
	}
	entries := []any{}
	for _, f := range []struct {
		name string
		size int
	}{{"Show.S01E01.mkv", 11}, {"Show.S01E02.mkv", 12}, {"Show.S01E03.mkv", 13}, {"Show.S01E02.zh.srt", 2}, {"readme.exe", 1}} {
		entries = append(entries, map[string]any{"length": f.size, "path": []any{f.name}})
	}
	w.raw = []byte(filesBencode(map[string]any{"info": map[string]any{"files": entries, "name": "Show.S01", "piece length": 16384, "pieces": strings.Repeat("x", 20)}}))
	meta, files, err := torrentmeta.Manifest(w.raw)
	if err != nil {
		t.Fatal(err)
	}
	w.hash = meta.InfoHash
	w.files = files
	if strings.HasPrefix(kind, "qb") {
		for i, id := range []int{2, 4, 7, 8, 9} {
			w.files[i].Index = id
		}
	}
	for i := range w.files {
		w.files[i].Selected = true
	}
	previous := base.transport
	base.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "torrent.fixture" {
			if r.Method != "GET" {
				t.Fatal("torrent fetch mutation")
			}
			return jsonResponse(r, string(w.raw)), nil
		}
		if r.URL.Hostname() == "indexer.local" && r.URL.Path == "/api/v1/search" {
			base.queries++
			raw, _ := json.Marshal([]map[string]any{{"indexerId": 42, "title": "Show.S01.1080p.WEB-DL", "size": 39, "downloadUrl": "http://torrent.fixture/bundle.torrent"}})
			return jsonResponse(r, string(raw)), nil
		}
		if r.URL.Hostname() == "qb.local" && strings.HasPrefix(kind, "qb") {
			switch {
			case strings.HasSuffix(r.URL.Path, "auth/login"):
				if base.loginFailure {
					return jsonResponse(r, "Fails."), nil
				}
				return jsonResponse(r, "Ok."), nil
			case strings.HasSuffix(r.URL.Path, "app/version"):
				return jsonResponse(r, w.version), nil
			case strings.HasSuffix(r.URL.Path, "torrents/info"):
				if r.URL.Query().Get("hashes") != w.hash || r.URL.Query().Get("filter") != "all" {
					t.Fatal("wrong hash scope", r.URL)
				}
				if !w.present {
					return jsonResponse(r, "[]"), nil
				}
				state := "downloading"
				if w.paused {
					state = "stoppedDL"
					if kind == "qb4" {
						state = "pausedDL"
					}
				}
				row := map[string]any{"hash": w.hash, "state": state, "save_path": "/downloads", "category": "", "tags": strings.Join(w.labels, ","), "auto_tmm": false, "force_start": false, "up_limit": 0, "dl_limit": 0, "ratio_limit": -1, "seeding_time_limit": -1, "inactive_seeding_time_limit": -1, "max_ratio": -1, "max_seeding_time": -1, "max_inactive_seeding_time": -1, "share_limit_action": "Default"}
				raw, _ := json.Marshal([]any{row})
				return jsonResponse(r, string(raw)), nil
			case strings.HasSuffix(r.URL.Path, "torrents/files"):
				out := []map[string]any{}
				for i, f := range w.files {
					priority := 0
					if f.Selected {
						priority = 1
					}
					size := f.Size
					if w.changeManifest && i == 0 {
						size++
					}
					out = append(out, map[string]any{"index": f.Index, "name": f.Path, "size": size, "priority": priority})
				}
				raw, _ := json.Marshal(out)
				return jsonResponse(r, string(raw)), nil
			case strings.HasSuffix(r.URL.Path, "torrents/add"):
				if r.ParseMultipartForm(8<<20) != nil {
					t.Fatal("invalid multipart")
				}
				if r.MultipartForm != nil {
					defer r.MultipartForm.RemoveAll()
				}
				file, _, err := r.FormFile("torrents")
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(file)
				file.Close()
				if string(raw) != string(w.raw) {
					t.Fatal("torrent bytes changed")
				}
				paused := "stopped"
				if kind == "qb4" {
					paused = "paused"
				}
				if r.FormValue(paused) != "true" || r.FormValue("contentLayout") != "Original" {
					t.Fatal("not safely stopped", r.Form)
				}
				w.labels = strings.Split(r.FormValue("tags"), ",")
				w.acceptAdd(t)
				if w.loss == "add" {
					return nil, context.DeadlineExceeded
				}
				return jsonResponse(r, "Ok."), nil
			case strings.HasSuffix(r.URL.Path, "torrents/filePrio"):
				r.ParseForm()
				if !w.paused || r.Form.Get("hash") != w.hash {
					t.Fatal("selected while running or wrong hash")
				}
				value := r.Form.Get("priority") == "1"
				w.priorities++
				operation := "unwant"
				if value {
					operation = "want"
				}
				w.operations = append(w.operations, operation)
				ids := strings.Split(r.Form.Get("id"), "|")
				if len(ids) == 0 || ids[0] == "" {
					t.Fatal("empty file ids")
				}
				for _, id := range ids {
					n, _ := strconv.Atoi(id)
					found := false
					for i := range w.files {
						if w.files[i].Index == n {
							found = true
							if !w.ignoreWrite {
								w.files[i].Selected = value
							}
						}
					}
					if !found {
						t.Fatal("used array position not exact index", id)
					}
				}
				if value && w.afterSelection != nil {
					w.afterSelection()
				}
				if w.loss == operation {
					return nil, context.DeadlineExceeded
				}
				return jsonResponse(r, ""), nil
			case strings.HasSuffix(r.URL.Path, "torrents/setShareLimits"):
				w.settings++
				w.operations = append(w.operations, "settings")
				if w.loss == "settings" {
					return nil, context.DeadlineExceeded
				}
				return jsonResponse(r, ""), nil
			case strings.HasSuffix(r.URL.Path, "torrents/start") || strings.HasSuffix(r.URL.Path, "torrents/resume"):
				r.ParseForm()
				if r.Form.Get("hashes") != w.hash {
					t.Fatal("wrong start")
				}
				w.acceptStart(t)
				if w.loss == "start" {
					return nil, context.DeadlineExceeded
				}
				return jsonResponse(r, ""), nil
			default:
				t.Fatal("unexpected qB mutation", r.URL)
				return nil, context.Canceled
			}
		}
		if r.URL.Hostname() == "tr.local" {
			modern := kind == "tr-new"
			key := func(a, b string) string {
				if modern {
					return b
				}
				return a
			}
			if r.Header.Get("X-Transmission-Session-Id") == "" {
				v := jsonResponse(r, "")
				v.StatusCode = 409
				v.Header.Set("X-Transmission-Session-Id", "token")
				if modern {
					v.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
				}
				return v, nil
			}
			var in map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&in) != nil {
				t.Fatal("bad RPC")
			}
			var method string
			json.Unmarshal(in["method"], &method)
			var args map[string]json.RawMessage
			argKey := "arguments"
			if modern {
				argKey = "params"
			}
			json.Unmarshal(in[argKey], &args)
			reply := func(data map[string]any) (*http.Response, error) {
				env := map[string]any{"result": "success", "arguments": data}
				if modern {
					env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": data}
				}
				raw, _ := json.Marshal(env)
				return jsonResponse(r, string(raw)), nil
			}
			switch method {
			case key("torrent-add", "torrent_add"):
				var paused bool
				json.Unmarshal(args["paused"], &paused)
				if !paused {
					t.Fatal("TR add not paused")
				}
				json.Unmarshal(args["labels"], &w.labels)
				w.acceptAdd(t)
				if w.loss == "add" {
					return nil, context.DeadlineExceeded
				}
				return reply(map[string]any{key("torrent-added", "torrent_added"): map[string]any{key("hashString", "hash_string"): w.hash}})
			case key("torrent-get", "torrent_get"):
				var ids []string
				json.Unmarshal(args["ids"], &ids)
				if len(ids) != 1 || ids[0] != w.hash {
					t.Fatal("widened RPC scope", args)
				}
				if !w.present {
					return reply(map[string]any{"torrents": []any{}})
				}
				files, stats := []any{}, []any{}
				for i, f := range w.files {
					size := f.Size
					if w.changeManifest && i == 0 {
						size++
					}
					files = append(files, map[string]any{"name": f.Path, "length": size})
					stats = append(stats, map[string]any{"wanted": f.Selected})
				}
				status := 4
				if w.paused {
					status = 0
				}
				row := map[string]any{key("hashString", "hash_string"): w.hash, key("downloadDir", "download_dir"): "/downloads", "labels": w.labels, "status": status, key("metadataPercentComplete", "metadata_percent_complete"): 1, "files": files, key("fileStats", "file_stats"): stats, key("uploadLimit", "upload_limit"): 0, key("uploadLimited", "upload_limited"): false, key("downloadLimit", "download_limit"): 0, key("downloadLimited", "download_limited"): false, key("seedRatioLimit", "seed_ratio_limit"): w.ratio, key("seedRatioMode", "seed_ratio_mode"): w.ratioMode, key("seedIdleLimit", "seed_idle_limit"): 0, key("seedIdleMode", "seed_idle_mode"): w.idleMode}
				return reply(map[string]any{"torrents": []any{row}})
			case key("torrent-set", "torrent_set"):
				if raw, ok := args[key("files-wanted", "files_wanted")]; ok {
					if !w.paused {
						t.Fatal("TR selecting while running")
					}
					w.operations = append(w.operations, "want")
					w.priorities++
					var wanted, unwanted []int
					json.Unmarshal(raw, &wanted)
					if v, ok := args[key("files-unwanted", "files_unwanted")]; ok {
						json.Unmarshal(v, &unwanted)
						if len(unwanted) == 0 {
							t.Fatal("empty unwanted array means all")
						}
					}
					if len(wanted) == 0 {
						t.Fatal("empty wanted array")
					}
					if !w.ignoreWrite {
						for _, n := range unwanted {
							w.files[n].Selected = false
						}
						for _, n := range wanted {
							w.files[n].Selected = true
						}
					}
					if w.afterSelection != nil {
						w.afterSelection()
					}
					if w.loss == "want" {
						return nil, context.DeadlineExceeded
					}
				} else {
					w.settings++
					w.operations = append(w.operations, "settings")
					json.Unmarshal(args[key("seedRatioLimit", "seed_ratio_limit")], &w.ratio)
					json.Unmarshal(args[key("seedRatioMode", "seed_ratio_mode")], &w.ratioMode)
					json.Unmarshal(args[key("seedIdleMode", "seed_idle_mode")], &w.idleMode)
					if w.loss == "settings" {
						return nil, context.DeadlineExceeded
					}
				}
				return reply(map[string]any{})
			case key("torrent-start", "torrent_start"):
				w.acceptStart(t)
				if w.loss == "start" {
					return nil, context.DeadlineExceeded
				}
				return reply(map[string]any{})
			default:
				t.Fatal("unexpected TR RPC", method)
				return nil, context.Canceled
			}
		}
		return previous.RoundTrip(r)
	})
	if !strings.HasPrefix(kind, "qb") {
		item, _ := base.runner.download.service.downloaders.Get(t.Context(), 1)
		item.Type = "transmission"
		item.Config = `{"host":"tr.local","port":9091}`
		if _, err := base.runner.download.service.downloaders.Upsert(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	var runner *rssRunAPI
	base.handler, err = buildHandler(config.Config{ApplicationConfigPath: base.path, DisableLegacy: true}, base.transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	base.runner = runner
	return w
}
func (w *subscriptionFilesFixture) acceptAdd(t *testing.T) {
	t.Helper()
	w.adds++
	w.present = true
	w.paused = true
	w.operations = append(w.operations, "add")
	rows, err := w.runner.download.service.downloaders.FileSelections(t.Context(), 1)
	if err != nil || len(rows) != 1 || rows[0].State != "adding" || !slices.Contains(w.labels, rows[0].Definition.Marker()) {
		t.Fatal("missing durable file selection before add", rows, err)
	}
	var n int
	if err := w.db.QueryRow(`SELECT COUNT(*) FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS WHERE PAYLOAD LIKE '%fileSelectionId%'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("shared intent not committed", n, err)
	}
}
func (w *subscriptionFilesFixture) acceptStart(t *testing.T) {
	t.Helper()
	expected := w.expectedPaths
	if expected == nil {
		expected = []string{"Show.S01/Show.S01E02.mkv", "Show.S01/Show.S01E03.mkv", "Show.S01/Show.S01E02.zh.srt"}
	}
	if !slices.Equal(w.wantedPaths(), expected) {
		t.Fatal("started whole/incorrect pack", w.files)
	}
	w.starts++
	w.paused = false
	w.operations = append(w.operations, "start")
}
func (w *subscriptionFilesFixture) wantedPaths() []string {
	out := []string{}
	for _, f := range w.files {
		if f.Selected {
			out = append(out, f.Path)
		}
	}
	return out
}
func (w *subscriptionFilesFixture) record(t *testing.T) downloaderconfig.FileSelection {
	t.Helper()
	rows, err := w.runner.download.service.downloaders.FileSelections(t.Context(), 1)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	return rows[0]
}
func (w *subscriptionFilesFixture) review(t *testing.T) subscriptionFilesReview {
	t.Helper()
	p := w.record(t)
	return recoveryData[subscriptionFilesReview](t, nativeJSONRequest(w.handler, "POST", "/api/v1/subscription-file-selections/"+p.Definition.ID+"/review", w.token, `{}`))
}
func (w *subscriptionFilesFixture) recover(t *testing.T, r subscriptionFilesReview, all bool) int {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"confirm": true, "fingerprint": r.Fingerprint, "confirmFiles": all, "confirmSettings": all, "confirmStart": all, "confirmRecord": all, "confirmSeasons": all})
	res := nativeJSONRequest(w.handler, "POST", "/api/v1/subscription-file-selections/"+r.Receipt.ID+"/recover", w.token, string(raw))
	if res.Code != 200 && all {
		t.Fatal("recovery", res.Code, res.Body.String(), r.Next)
	}
	return res.Code
}
func TestSubscriptionPartialFilesActuallySelectedBeforeStartAndSourceConsumption(t *testing.T) {
	for _, kind := range []string{"qb4", "qb5", "tr-old", "tr-new"} {
		t.Run(kind, func(t *testing.T) {
			w := newSubscriptionFilesFixture(t, kind)
			status, body := w.run("TV", "2")
			if status != 200 || !strings.Contains(body, `"completed":true`) || w.adds != 1 || w.starts != 1 {
				t.Fatal(status, body, w.operations)
			}
			p := w.record(t)
			if p.State != "ready" || !p.Committed || !slices.Equal(p.Definition.Episodes, []int{2, 3}) {
				t.Fatal(p)
			}
			var history int
			w.db.QueryRow(`SELECT COUNT(*) FROM RSS_HISTORY WHERE TYPE='TV'`).Scan(&history)
			if history != 1 {
				t.Fatal(history)
			}
			if strings.Contains(body, "torrent.fixture") || strings.Contains(body, "private") {
				t.Fatal("private proof leaked", body)
			}
			r := nativeJSONRequest(w.handler, "GET", "/api/v1/subscription-file-selections/"+p.Definition.ID, w.token, "")
			if r.Code != 200 || strings.Contains(r.Body.String(), "torrent.fixture") || strings.Contains(r.Body.String(), "options") {
				t.Fatal(r.Code, r.Body.String())
			}
		})
	}
}
func TestSubscriptionWholeSeasonManifestIsRequiredBeforeSourceCompletion(t *testing.T) {
	for _, kind := range []string{"qb4", "qb5", "tr-old", "tr-new"} {
		t.Run(kind, func(t *testing.T) {
			w := newSubscriptionFilesFixture(t, kind)
			if _, err := w.db.Exec(`UPDATE RSS_TVS SET LACK=3;UPDATE RSS_TV_EPISODES SET EPISODES='1,2,3'`); err != nil {
				t.Fatal(err)
			}
			w.expectedPaths = []string{"Show.S01/Show.S01E01.mkv", "Show.S01/Show.S01E02.mkv", "Show.S01/Show.S01E03.mkv", "Show.S01/Show.S01E02.zh.srt"}
			code, body := w.run("TV", "2")
			if code != 200 || !strings.Contains(body, `"completed":true`) || w.adds != 1 || w.starts != 1 {
				t.Fatal(code, body, w.operations)
			}
			p := w.record(t)
			if p.State != "ready" || !p.Committed || !slices.Equal(p.Definition.Episodes, []int{1, 2, 3}) {
				t.Fatal("whole season bypassed actual file selection", p)
			}
		})
	}
}

func TestSubscriptionWholeSeasonIncompleteManifestCannotForgeMissingProgress(t *testing.T) {
	for _, wash := range []bool{false, true} {
		t.Run(fmt.Sprint(wash), func(t *testing.T) {
			w := newSubscriptionFilesFixture(t, "qb5")
			w.onlyEpisodeTwo(t)
			w.expectedPaths = []string{"Show.S01/Show.S01E01.mkv", "Show.S01/Show.S01E02.mkv", "Show.S01/Show.S01E02.zh.srt"}
			sql := `UPDATE RSS_TVS SET LACK=3;UPDATE RSS_TV_EPISODES SET EPISODES='1,2,3'`
			if wash {
				sql = `INSERT INTO CONFIG_FILTER_GROUP(ID,GROUP_NAME,IS_DEFAULT) VALUES(777,'quality','N');INSERT INTO CONFIG_FILTER_RULES(ID,GROUP_ID,ROLE_NAME,PRIORITY,INCLUDE) VALUES(7771,'777','best','1','1080p');UPDATE RSS_TVS SET OVER_EDITION=1,FILTER_RULE=777,CURRENT_EP=1,LACK=0;UPDATE RSS_TV_EPISODES SET EPISODES=''`
			}
			if _, err := w.db.Exec(sql); err != nil {
				t.Fatal(err)
			}
			code, body := w.run("TV", "2")
			if code != 200 || !strings.Contains(body, `"remaining":[3]`) || strings.Contains(body, `"completed":true`) || w.adds != 1 || w.starts != 1 {
				t.Fatal("title invented absent episode", code, body, w.operations)
			}
			p := w.record(t)
			if !p.Committed || !slices.Equal(p.Definition.Episodes, []int{1, 2}) {
				t.Fatal(p)
			}
			var lack int
			var missing string
			var order any
			if err := w.db.QueryRow(`SELECT LACK,FILTER_ORDER FROM RSS_TVS WHERE ID=2`).Scan(&lack, &order); err != nil {
				t.Fatal(err)
			}
			if err := w.db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='2'`).Scan(&missing); err != nil {
				t.Fatal(err)
			}
			if wash {
				if lack != 0 || missing != "" || order != nil {
					t.Fatal("partial wash forged ordinary progress or final quality", lack, missing, order)
				}
			} else if lack != 1 || missing != "3" {
				t.Fatal("whole-season title replaced actual coverage", lack, missing)
			}
		})
	}
}

func TestSubscriptionFileLostRepliesRecoverOriginalStagesWithoutReplayingRequests(t *testing.T) {
	for _, kind := range []string{"qb5", "tr-old", "tr-new"} {
		for _, loss := range []string{"add", "want", "start"} {
			t.Run(kind+"/"+loss, func(t *testing.T) {
				w := newSubscriptionFilesFixture(t, kind)
				w.loss = loss
				status, body := w.run("TV", "2")
				if status != 502 || w.adds != 1 {
					t.Fatal(status, body, w.operations)
				}
				var lack int
				w.db.QueryRow(`SELECT LACK FROM RSS_TVS WHERE ID=2`).Scan(&lack)
				if lack != 2 {
					t.Fatal("unknown selected pack consumed episodes", lack)
				}
				if code, _ := w.run("TV", "2"); code != 409 || w.adds != 1 {
					t.Fatal("repeated add", code, w.adds)
				}
				if r := performFormRequest(w.handler, "/api/v1/subscriptions/TV/2/reconcile", w.token, url.Values{}); r.Code != 409 {
					t.Fatal("mere presence adopted", r.Code, r.Body.String())
				}
				w.loss = ""
				for n := 0; n < 8 && !w.record(t).Committed; n++ {
					r := w.review(t)
					if r.Next == "select" || r.Next == "start" || r.Next == "record" {
						if w.recover(t, r, false) != 409 {
							t.Fatal("missing independent consent accepted")
						}
					}
					w.recover(t, r, true)
				}
				if !w.record(t).Committed || w.adds != 1 || w.starts != 1 {
					t.Fatal("lost response replayed", w.operations)
				}
				wantWrites := 1
				if kind == "qb5" {
					wantWrites = 2
				}
				if w.priorities != wantWrites {
					t.Fatal("file write replayed", w.priorities, w.operations)
				}
			})
		}
	}
}
func TestSubscriptionFileLostUnwantedOnlyAdoptsPositiveOriginalReceipt(t *testing.T) {
	w := newSubscriptionFilesFixture(t, "qb5")
	w.loss = "unwant"
	if code, _ := w.run("TV", "2"); code != 502 {
		t.Fatal(code)
	}
	r := w.review(t)
	if r.Next != "adopt-unwanted" {
		t.Fatal(r.Next)
	}
	w.loss = ""
	w.recover(t, r, true)
	if w.priorities != 1 {
		t.Fatal("unwanted replayed")
	}
	for n := 0; n < 6 && !w.record(t).Committed; n++ {
		w.recover(t, w.review(t), true)
	}
	if w.priorities != 2 || w.starts != 1 || w.adds != 1 {
		t.Fatal(w.operations)
	}
}
func TestSubscriptionFileScopeChangesAndIgnoredRemoteWritesNeverStartOrFakeSuccess(t *testing.T) {
	for _, mode := range []string{"ignore", "manifest", "source", "rule"} {
		t.Run(mode, func(t *testing.T) {
			w := newSubscriptionFilesFixture(t, "qb5")
			switch mode {
			case "ignore":
				w.ignoreWrite = true
			case "manifest":
				w.changeManifest = true
			case "source":
				w.afterSelection = func() { w.db.Exec(`UPDATE RSS_TVS SET NAME='edited' WHERE ID=2`) }
			case "rule":
				w.afterSelection = func() { w.runner.download.system.Set(t.Context(), "UserIndexerSites", `[]`) }
			}
			code, body := w.run("TV", "2")
			if code != 502 || w.starts != 0 || w.adds != 1 {
				t.Fatal(code, body, w.operations)
			}
			if r := nativeJSONRequest(w.handler, "POST", "/api/v1/subscription-file-selections/"+w.record(t).Definition.ID+"/review", w.token, `{}`); r.Code != 409 {
				t.Fatal("unsafe recovery permitted", r.Code, r.Body.String())
			}
		})
	}
}
func TestSubscriptionFileSourceCommitFailureOnlyReconcilesOriginalSelectedTask(t *testing.T) {
	w := newSubscriptionFilesFixture(t, "qb5")
	// Use the new selected-source commit trigger: even a silent ignored receipt
	// update must roll back all ordinary progress/history/accepted event writes.
	w.afterSelection = func() {
		if _, err := w.db.Exec(`CREATE TRIGGER ignore_file_commit BEFORE UPDATE OF COMMITTED ON GO_TORRENT_FILE_SELECTIONS WHEN NEW.COMMITTED=1 BEGIN SELECT RAISE(IGNORE); END`); err != nil {
			t.Fatal(err)
		}
	}
	code, body := w.run("TV", "2")
	if code != 502 || w.record(t).Committed || w.starts != 1 {
		t.Fatal(code, body)
	}
	w.db.Exec(`DROP TRIGGER ignore_file_commit`)
	r := w.review(t)
	if r.Next != "record" {
		t.Fatal(r.Next)
	}
	w.recover(t, r, true)
	if !w.record(t).Committed || w.adds != 1 || w.priorities != 2 || w.starts != 1 {
		t.Fatal("reconciliation replayed", w.operations)
	}
}
func TestSubscriptionFileTransmissionPresetsAndPTSettingsPrecedeFileSelection(t *testing.T) {
	for _, kind := range []string{"qb5", "tr-old", "tr-new"} {
		t.Run(kind, func(t *testing.T) {
			w := newSubscriptionFilesFixture(t, kind)
			store := w.runner.download.service.downloaders
			item, _ := store.Get(t.Context(), 1)
			if _, err := store.SetSeedingAdmission(t.Context(), item, "fixture", true, bus.Hash((*downloaderconfig.SeedingAdmission)(nil))); err != nil {
				t.Fatal(err)
			}
			seconds := int64(900)
			meta, _ := mediameta.Parse("Show.S01.1080p.WEB-DL", "")
			provider := func(context.Context, map[string]any, string, string) ([]identifiedSubscriptionResource, *recognitionFailure) {
				return []identifiedSubscriptionResource{{resource: externalindexer.Resource{Title: "Show.S01.1080p.WEB-DL", Indexer: "Selected-prowlarr", DownloadURL: "http://torrent.fixture/bundle.torrent", Size: 39, MinimumSeedTime: &seconds}, meta: meta, revised: "Show.S01.1080p.WEB-DL"}}, nil
			}
			result, failure := w.runner.search.executeResources(t.Context(), "TV", 2, "", provider)
			if failure != nil || result.Submitted != 1 || w.settings != 1 || w.starts != 1 || !w.record(t).Committed {
				t.Fatal(result, failure, w.operations)
			}
			if slices.Index(w.operations, "settings") > slices.Index(w.operations, "want") {
				t.Fatal("settings applied after start", w.operations)
			}
		})
	}
}
func TestSubscriptionFileUnknownPresetSettingsCannotBeBypassedByOldPresetRecovery(t *testing.T) {
	w := newSubscriptionFilesFixture(t, "tr-new")
	setting, err := w.runner.download.service.downloaders.UpsertSetting(t.Context(), downloaderconfig.DownloadSetting{Name: "fixture", RatioLimit: 150, DownloaderID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	w.db.Exec(`UPDATE RSS_TVS SET DOWNLOAD_SETTING=? WHERE ID=2`, strconv.FormatInt(setting.ID, 10))
	w.loss = "settings"
	code, body := w.run("TV", "2")
	if code != 502 || w.priorities != 0 || w.starts != 0 {
		t.Fatal(code, body, w.operations)
	}
	profile, err := w.runner.download.service.downloaders.FindTransmissionProfile(t.Context(), 1, w.hash)
	if err != nil {
		t.Fatal(err)
	}
	old := nativeJSONRequest(w.handler, "POST", "/api/v1/downloads/transmission-profiles/"+profile.ID+"/review", w.token, `{}`)
	if old.Code != 409 {
		t.Fatal("old preset flow bypassed pending file selection", old.Code, old.Body.String())
	}
	r := w.review(t)
	if r.Next != "adopt-add" {
		t.Fatal(r.Next)
	}
	w.recover(t, r, true)
	r = w.review(t)
	if r.Next != "adopt-settings" {
		t.Fatal(r.Next)
	}
	w.loss = ""
	w.recover(t, r, true)
	for n := 0; n < 7 && !w.record(t).Committed; n++ {
		w.recover(t, w.review(t), true)
	}
	if w.settings != 1 || w.adds != 1 || w.starts != 1 {
		t.Fatal("preset replayed", w.operations)
	}
}
func TestSubscriptionFileInspectionIsReadonlyRootOnlyAndStableAfterSourceRemoval(t *testing.T) {
	w := newSubscriptionFilesFixture(t, "qb5")
	if r := nativeJSONRequest(w.handler, "GET", "/api/v1/subscription-file-selections", w.token, ""); r.Code != 200 || w.adds != 0 {
		t.Fatal(r.Code)
	}
	var n int
	w.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_TORRENT_FILE_SELECTIONS'`).Scan(&n)
	if n != 0 {
		t.Fatal("read created tables")
	}
	viewer, err := w.runner.download.service.auth.service.IssueToken("viewer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", viewer} {
		want := 401
		if token != "" {
			want = 403
		}
		if r := nativeJSONRequest(w.handler, "GET", "/api/v1/subscription-file-selections", token, ""); r.Code != want {
			t.Fatal(r.Code)
		}
	}
	if code, _ := w.run("TV", "2"); code != 200 {
		t.Fatal(code)
	}
	p := w.record(t)
	r := w.review(t)
	if r.Next != "none" || r.Current != nil {
		t.Fatal(r)
	}
	public := recoveryData[subscriptionFilesPublic](t, nativeJSONRequest(w.handler, "GET", "/api/v1/subscription-file-selections/"+p.Definition.ID, w.token, ""))
	if !public.Recorded || len(public.Files) != 5 {
		t.Fatal(public)
	}
}
func TestSubscriptionSelectiveMagnetMissingMetadataNeverAddsWholeResource(t *testing.T) {
	w := newSubscriptionFilesFixture(t, "qb5")
	t.Setenv("NASTOOL_ARIA2_BINARY", filepath.Join(t.TempDir(), "missing"))
	previous := w.transport
	w.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "indexer.local" && r.URL.Path == "/api/v1/search" {
			return jsonResponse(r, `[{"indexerId":42,"title":"Show.S01.1080p.WEB-DL","size":39,"downloadUrl":"magnet:?xt=urn:btih:`+w.hash+`"}]`), nil
		}
		return previous.RoundTrip(r)
	})
	h, err := newHandler(config.Config{ApplicationConfigPath: w.path, DisableLegacy: true}, w.transport)
	if err != nil {
		t.Fatal(err)
	}
	w.handler = h
	code, body := w.run("TV", "2")
	if code != 422 || w.adds != 0 || w.starts != 0 {
		t.Fatal(code, body, w.operations)
	}
}

func (w *subscriptionFilesFixture) onlyEpisodeTwo(t *testing.T) {
	t.Helper()
	entries := []any{}
	for _, f := range []struct {
		name string
		size int
	}{{"Show.S01E01.mkv", 11}, {"Show.S01E02.mkv", 12}, {"Show.S01E02.zh.srt", 2}, {"readme.exe", 1}} {
		entries = append(entries, map[string]any{"length": f.size, "path": []any{f.name}})
	}
	w.raw = []byte(filesBencode(map[string]any{"info": map[string]any{"files": entries, "name": "Show.S01", "piece length": 16384, "pieces": strings.Repeat("x", 20)}}))
	meta, files, err := torrentmeta.Manifest(w.raw)
	if err != nil {
		t.Fatal(err)
	}
	w.hash = meta.InfoHash
	w.files = files
	for i := range w.files {
		w.files[i].Selected = true
		w.files[i].Index = i*2 + 2
	}
	w.expectedPaths = []string{"Show.S01/Show.S01E02.mkv", "Show.S01/Show.S01E02.zh.srt"}
}
func TestSubscriptionFileActualCoverageNotTitleCountDrivesMissingAndOldURLIsNotReplayed(t *testing.T) {
	w := newSubscriptionFilesFixture(t, "qb5")
	w.onlyEpisodeTwo(t)
	code, body := w.run("TV", "2")
	if code != 200 || !strings.Contains(body, `"remaining":[3]`) || strings.Contains(body, `"completed":true`) || w.adds != 1 {
		t.Fatal(code, body, w.operations)
	}
	p := w.record(t)
	if !slices.Equal(p.Definition.Episodes, []int{2}) {
		t.Fatal(p)
	}
	var lack int
	var missing string
	w.db.QueryRow(`SELECT LACK FROM RSS_TVS WHERE ID=2`).Scan(&lack)
	w.db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='2'`).Scan(&missing)
	if lack != 1 || missing != "3" {
		t.Fatal(lack, missing)
	}
	code, body = w.run("TV", "2")
	if code != 200 || w.adds != 1 || w.starts != 1 || !strings.Contains(body, `"remaining":[3]`) {
		t.Fatal("old pack prevented future missing search or replayed", code, body, w.operations)
	}
}
func TestSubscriptionFileQuietlyIgnoredBusinessWritesRollBackSelectedSourceCommit(t *testing.T) {
	for _, target := range []string{"torrent", "lack", "episodes", "delete"} {
		t.Run(target, func(t *testing.T) {
			w := newSubscriptionFilesFixture(t, "qb5")
			sql := `CREATE TRIGGER ignore_selected_business BEFORE INSERT ON RSS_TORRENTS BEGIN SELECT RAISE(IGNORE); END`
			switch target {
			case "lack":
				w.onlyEpisodeTwo(t)
				sql = `CREATE TRIGGER ignore_selected_business BEFORE UPDATE OF LACK ON RSS_TVS BEGIN SELECT RAISE(IGNORE); END`
			case "episodes":
				w.onlyEpisodeTwo(t)
				sql = `CREATE TRIGGER ignore_selected_business BEFORE INSERT ON RSS_TV_EPISODES BEGIN SELECT RAISE(IGNORE); END`
			case "delete":
				sql = `CREATE TRIGGER ignore_selected_business BEFORE DELETE ON RSS_TVS BEGIN SELECT RAISE(IGNORE); END`
			}
			w.afterSelection = func() {
				if _, err := w.db.Exec(sql); err != nil {
					t.Fatal(err)
				}
			}
			code, body := w.run("TV", "2")
			if code != 502 || w.record(t).Committed || w.starts != 1 || w.adds != 1 {
				t.Fatal(code, body, w.operations)
			}
			if _, err := w.db.Exec(`DROP TRIGGER ignore_selected_business`); err != nil {
				t.Fatal(err)
			}
			review := w.review(t)
			if review.Next != "record" {
				t.Fatal(review.Next)
			}
			w.recover(t, review, true)
			if !w.record(t).Committed || w.adds != 1 || w.starts != 1 || w.priorities != 2 {
				t.Fatal(w.operations)
			}
		})
	}
}
func TestSubscriptionFileWashingUsesSelectedEpisodesAndOriginalHighestQualityCycle(t *testing.T) {
	w := newSubscriptionFilesFixture(t, "qb5")
	_, err := w.db.Exec(`INSERT INTO CONFIG_FILTER_GROUP(ID,GROUP_NAME,IS_DEFAULT) VALUES(777,'quality','N');INSERT INTO CONFIG_FILTER_RULES(ID,GROUP_ID,ROLE_NAME,PRIORITY,INCLUDE) VALUES(7771,'777','best','1','1080p');UPDATE RSS_TVS SET OVER_EDITION=1,FILTER_RULE=777,CURRENT_EP=2,LACK=0;UPDATE RSS_TV_EPISODES SET EPISODES=''`)
	if err != nil {
		t.Fatal(err)
	}
	code, body := w.run("TV", "2")
	if code != 200 || !strings.Contains(body, `"completed":true`) || !strings.Contains(body, `"order":99`) || w.adds != 1 {
		t.Fatal(code, body, w.operations)
	}
	p := w.record(t)
	if !p.Committed || !slices.Equal(p.Definition.Episodes, []int{2, 3}) {
		t.Fatal(p)
	}
	var payload string
	w.db.QueryRow(`SELECT PAYLOAD FROM GO_SUBSCRIPTION_EDITION_ATTEMPTS`).Scan(&payload)
	proof, _, err := decodeSubscriptionProof(payload)
	if err != nil || proof.Version != 4 || proof.Edition == nil || proof.Edition.Cycle.Order != 99 || !slices.Equal(proof.Edition.Cycle.Target, []int{2, 3}) {
		t.Fatal(proof, err)
	}
}
func TestSubscriptionFileReviewRejectsContradictoryPrivateFileSelectionProof(t *testing.T) {
	w := newSubscriptionFilesFixture(t, "qb5")
	w.loss = "add"
	if code, _ := w.run("TV", "2"); code != 502 {
		t.Fatal(code)
	}
	p := w.record(t)
	d := p.Definition
	d.Files[0].Selected = true
	raw, _ := json.Marshal(d)
	if _, err := w.db.Exec(`UPDATE GO_TORRENT_FILE_SELECTIONS SET DOCUMENT=?,FINGERPRINT=? WHERE ID=?`, string(raw), bus.Hash(d), d.ID); err != nil {
		t.Fatal(err)
	}
	r := nativeJSONRequest(w.handler, "POST", "/api/v1/subscription-file-selections/"+d.ID+"/review", w.token, `{}`)
	if r.Code != 409 || w.priorities != 0 || w.starts != 0 {
		t.Fatal(r.Code, r.Body.String(), w.operations)
	}
}
func TestSubscriptionFileMixedNamespacesNeverInvokeLegacyBackend(t *testing.T) {
	w := newSubscriptionFilesFixture(t, "qb5")
	h, err := newHandler(config.Config{ApplicationConfigPath: w.path, LegacyBackendURL: "http://legacy.test"}, w.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/subscription-file-selections", "/api/v1/subscription-file-selections/" + strings.Repeat("a", 32), "/api/v1/subscription-file-selections/" + strings.Repeat("a", 32) + "/review", "/api/v1/subscription-file-selections/unknown/other"} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			r := nativeJSONRequest(h, method, path, w.token, `{}`)
			if r.Code != 501 {
				t.Fatal(method, path, r.Code, r.Body.String())
			}
		}
	}
}

func TestSubscriptionFileIgnoredSharedClaimsAndPrivateProofCannotIssuePausedAdd(t *testing.T) {
	for _, table := range []string{"GO_SUBSCRIPTION_DOWNLOAD_CLAIMS", "GO_SUBSCRIPTION_SUBMISSION_PROOFS"} {
		t.Run(table, func(t *testing.T) {
			w := newSubscriptionFilesFixture(t, "qb5")
			db, err := w.runner.search.planner.runner.recognition.service.openNativeSubscriptionWriteDatabase()
			if err != nil {
				t.Fatal(err)
			}
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := ensureSubscriptionSubmissionSchema(t.Context(), tx); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if _, err := w.db.Exec(`CREATE TRIGGER ignore_selected_shared BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(IGNORE); END`); err != nil {
				t.Fatal(err)
			}
			code, body := w.run("TV", "2")
			if code != 409 || w.adds != 0 || w.starts != 0 {
				t.Fatal(code, body, w.operations)
			}
			var n int
			if err := w.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS`).Scan(&n); err != nil || n != 0 {
				t.Fatal("partial reservation", n, err)
			}
		})
	}
}
