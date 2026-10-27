package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

func rebuildSeasonFixture(t *testing.T, w *subscriptionFilesFixture) {
	t.Helper()
	var runner *rssRunAPI
	var err error
	w.handler, err = buildHandler(config.Config{ApplicationConfigPath: w.path, DisableLegacy: true}, w.transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	w.runner = runner
}
func setSeasonManifest(t *testing.T, w *subscriptionFilesFixture, omitSecond bool) {
	t.Helper()
	entries := []any{}
	for _, name := range []string{"S01/Show.S01E01.mkv", "S01/Show.S01E02.mkv", "S01/Show.S01E03.mkv", "S02/Show.S02E01.mkv", "S02/Show.S02E02.mkv", "S02/Show.S02E01.zh.srt", "E01.mkv", "readme.exe"} {
		if omitSecond && name == "S02/Show.S02E02.mkv" {
			continue
		}
		parts := []any{}
		for _, part := range strings.Split(name, "/") {
			parts = append(parts, part)
		}
		entries = append(entries, map[string]any{"length": 12, "path": parts})
	}
	w.raw = []byte(filesBencode(map[string]any{"info": map[string]any{"name": "Show.S01-S02", "files": entries, "piece length": 16384, "pieces": strings.Repeat("x", 20)}}))
	meta, files, err := torrentmeta.Manifest(w.raw)
	if err != nil {
		t.Fatal(err)
	}
	w.hash, w.files = meta.InfoHash, files
	w.expectedPaths = nil
	for i := range w.files {
		w.files[i].Selected = true
		if strings.HasPrefix(w.kind, "qb") {
			w.files[i].Index = i*3 + 2
		}
		path := w.files[i].Path
		if strings.Contains(path, "S01E02.") || strings.Contains(path, "S01E03.") || strings.Contains(path, "S02E01.") || strings.Contains(path, "S02E02.") {
			w.expectedPaths = append(w.expectedPaths, path)
		}
	}
}
func newSeasonFixture(t *testing.T, kind string) *subscriptionFilesFixture {
	t.Helper()
	w := newSubscriptionFilesFixture(t, kind)
	if _, err := w.db.Exec(`DELETE FROM RSS_MOVIES;UPDATE RSS_TVS SET STATE='R';INSERT INTO RSS_TVS(ID,NAME,YEAR,TMDBID,SEASON,STATE,FUZZY_MATCH,OVER_EDITION,FILTER_RULE,SEARCH_SITES,TOTAL,LACK) VALUES(3,'Show','2026','200','S02','R',0,0,-1,'["Selected-prowlarr"]',2,2);INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES('3','1,2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec(`CREATE TABLE IF NOT EXISTS RSS_HISTORY(ID INTEGER PRIMARY KEY,TYPE TEXT,RSSID TEXT,NAME TEXT,YEAR TEXT,TMDBID TEXT,SEASON TEXT,IMAGE TEXT,DESC TEXT,TOTAL INTEGER,START INTEGER,FINISH_TIME TEXT)`); err != nil {
		t.Fatal(err)
	}
	setSeasonManifest(t, w, false)
	previous := w.transport
	w.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "tmdb.test" && strings.HasPrefix(r.URL.Path, "/3/tv/") {
			return jsonResponse(r, `{"id":200,"name":"Show","first_air_date":"2026-01-01","seasons":[{"season_number":1,"episode_count":3},{"season_number":2,"episode_count":2}]}`), nil
		}
		if r.URL.Hostname() == "indexer.local" && r.URL.Path == "/api/v1/search" {
			w.queries++
			raw, _ := json.Marshal([]map[string]any{{"indexerId": 42, "title": "Show.S01-S02.1080p.WEB-DL", "size": 96, "downloadUrl": "http://torrent.fixture/bundle.torrent"}})
			return jsonResponse(r, string(raw)), nil
		}
		return previous.RoundTrip(r)
	})
	rebuildSeasonFixture(t, w)
	return w
}
func seasonCount(t *testing.T, w *subscriptionFilesFixture, sql string) int {
	t.Helper()
	if strings.Contains(sql, "FROM RSS_HISTORY") {
		var exists bool
		if err := w.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='RSS_HISTORY')`).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			return 0
		}
	}
	var n int
	if err := w.db.QueryRow(sql).Scan(&n); err != nil {
		t.Fatal(sql, err)
	}
	return n
}
func decodeSeasonRun(t *testing.T, code int, body string) subscriptionSearchRunResult {
	t.Helper()
	var out struct {
		Data subscriptionSearchRunResult `json:"data"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &out) != nil {
		t.Fatal(code, body)
	}
	return out.Data
}
func recoverSeasonToReceipt(t *testing.T, w *subscriptionFilesFixture) {
	t.Helper()
	for range 10 {
		r := w.review(t)
		if r.Next == "none" {
			return
		}
		w.recover(t, r, true)
	}
	t.Fatal("joint receipt did not terminate", w.record(t))
}

func TestSubscriptionSeasonCohortOneTaskAndAtomicActualProgressAcrossProtocols(t *testing.T) {
	for _, kind := range []string{"qb4", "qb5", "tr-old", "tr-new"} {
		t.Run(kind, func(t *testing.T) {
			w := newSeasonFixture(t, kind)
			code, body := w.run("TV", "2")
			out := decodeSeasonRun(t, code, body)
			p := w.record(t)
			if !out.Completed || out.Submitted != 1 || len(out.Seasons) != 2 || w.adds != 1 || w.starts != 1 || p.Definition.Version != 2 || len(p.Definition.Sources) != 2 || !p.Committed {
				t.Fatal(out, w.operations, p)
			}
			for _, s := range out.Seasons {
				if !s.Completed || len(s.Remaining) != 0 {
					t.Fatal(s)
				}
			}
			for _, q := range []struct {
				sql  string
				want int
			}{
				{`SELECT COUNT(*) FROM RSS_TVS`, 0}, {`SELECT COUNT(*) FROM RSS_HISTORY`, 2},
				{`SELECT COUNT(*) FROM RSS_TORRENTS`, 1}, {`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE STATE='submitted'`, 1},
				{`SELECT COUNT(*) FROM GO_SUBSCRIPTION_SEASON_MEMBERS WHERE STATE='submitted'`, 2},
				{`SELECT COUNT(*) FROM GO_SUBSCRIPTION_EDITION_ARCHIVES`, 2},
				{`SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='download_start'`, 1},
				{`SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='rss_finished'`, 2},
			} {
				if n := seasonCount(t, w, q.sql); n != q.want {
					t.Fatal(q, n)
				}
			}
			if strings.Contains(body, "torrent.fixture") || strings.Contains(body, "private") {
				t.Fatal("private cohort leaked", body)
			}
			got := recoveryData[subscriptionFilesPublic](t, nativeJSONRequest(w.handler, "GET", "/api/v1/subscription-file-selections/"+p.Definition.ID, w.token, ""))
			if !got.Recorded || len(got.Seasons) != 2 || got.Seasons[1].SubscriptionID != 3 {
				t.Fatal(got)
			}
		})
	}
}

func TestSubscriptionSeasonCohortActualIncompleteSeasonStaysMissingAndNeverReaddsURL(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	setSeasonManifest(t, w, true)
	code, body := w.run("TV", "2")
	out := decodeSeasonRun(t, code, body)
	if !out.Completed || len(out.Seasons) != 2 || out.Seasons[1].Completed || !slices.Equal(out.Seasons[1].Remaining, []int{2}) {
		t.Fatal(out)
	}
	var lack int
	var missing string
	w.db.QueryRow(`SELECT LACK FROM RSS_TVS WHERE ID=3`).Scan(&lack)
	w.db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='3'`).Scan(&missing)
	if lack != 1 || missing != "2" || !w.record(t).Committed {
		t.Fatal(lack, missing)
	}
	code, body = w.run("TV", "3")
	again := decodeSeasonRun(t, code, body)
	if again.Submitted != 0 || again.Completed || !slices.Equal(again.Remaining, []int{2}) || w.adds != 1 || w.starts != 1 {
		t.Fatal(again, w.operations)
	}
}

func TestSubscriptionSeasonCohortOnlySelectsSubscribedAndIndependentlyMatchingSeasons(t *testing.T) {
	for _, excluded := range []string{"absent", "filter"} {
		t.Run(excluded, func(t *testing.T) {
			w := newSeasonFixture(t, "qb5")
			sql := `DELETE FROM RSS_TVS WHERE ID=3;DELETE FROM RSS_TV_EPISODES WHERE RSSID='3'`
			if excluded == "filter" {
				sql = `UPDATE RSS_TVS SET FILTER_EXCLUDE='Show' WHERE ID=3`
			}
			if _, err := w.db.Exec(sql); err != nil {
				t.Fatal(err)
			}
			w.expectedPaths = []string{"Show.S01-S02/S01/Show.S01E02.mkv", "Show.S01-S02/S01/Show.S01E03.mkv"}
			code, body := w.run("TV", "2")
			out := decodeSeasonRun(t, code, body)
			if !out.Completed || len(w.record(t).Definition.Sources) != 1 || w.adds != 1 || w.starts != 1 {
				t.Fatal(out, w.operations)
			}
			for _, f := range w.files {
				if strings.Contains(f.Path, "/S02/") && f.Selected {
					t.Fatal("unsubscribed or excluded season selected", f)
				}
			}
		})
	}
}

func TestSubscriptionSeasonCohortIncompatibleOrUnavailableSourceSettingsCannotBeBorrowed(t *testing.T) {
	for _, sql := range []string{
		`UPDATE RSS_TVS SET DOWNLOAD_SETTING='-2' WHERE ID=3`,
		`UPDATE RSS_TVS SET SAVE_PATH='/different-scope' WHERE ID=3`,
		`UPDATE RSS_TVS SET SEARCH_SITES='["unavailable-prowlarr"]' WHERE ID=3`,
	} {
		t.Run(sql, func(t *testing.T) {
			w := newSeasonFixture(t, "qb5")
			if _, err := w.db.Exec(sql); err != nil {
				t.Fatal(err)
			}
			code, body := w.run("TV", "2")
			if code != 422 || w.adds != 0 || w.starts != 0 {
				t.Fatal("borrowed leader settings/sites", code, body, w.operations)
			}
		})
	}
}

func TestSubscriptionSeasonCohortLostStageRepliesRecoverOnceAndRequireAllSeasonConsent(t *testing.T) {
	for _, kind := range []string{"qb5", "tr-old", "tr-new"} {
		for _, loss := range []string{"add", "want", "start"} {
			t.Run(kind+"/"+loss, func(t *testing.T) {
				w := newSeasonFixture(t, kind)
				w.loss = loss
				code, body := w.run("TV", "2")
				if code != 502 || w.adds != 1 || w.record(t).Committed || seasonCount(t, w, `SELECT COUNT(*) FROM RSS_HISTORY`) != 0 {
					t.Fatal(code, body, w.operations)
				}
				r := w.review(t)
				noConsent, _ := json.Marshal(map[string]any{"confirm": true, "fingerprint": r.Fingerprint, "confirmFiles": true, "confirmSettings": true, "confirmStart": true, "confirmRecord": true})
				if got := nativeJSONRequest(w.handler, "POST", "/api/v1/subscription-file-selections/"+r.Receipt.ID+"/recover", w.token, string(noConsent)); got.Code != 409 {
					t.Fatal("missing joint scope consent", got.Code, got.Body.String())
				}
				rebuildSeasonFixture(t, w)
				if w.record(t).Committed || seasonCount(t, w, `SELECT COUNT(*) FROM RSS_TVS`) != 2 {
					t.Fatal("restart consumed source")
				}
				recoverSeasonToReceipt(t, w)
				if !w.record(t).Committed || w.adds != 1 || w.starts != 1 || seasonCount(t, w, `SELECT COUNT(*) FROM RSS_HISTORY`) != 2 {
					t.Fatal(w.operations)
				}
				wanted := 1
				if strings.HasPrefix(kind, "qb") {
					wanted = 2
				}
				if w.priorities != wanted {
					t.Fatal("replayed original file write", w.operations)
				}
			})
		}
	}
}

func TestSubscriptionSeasonCohortPartnerReconcileUsesOriginalTaskNotAnotherAdd(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	w.afterSelection = func() {
		_, err := w.db.Exec(`CREATE TRIGGER reject_joint_history BEFORE INSERT ON RSS_HISTORY WHEN NEW.RSSID='3' BEGIN SELECT RAISE(IGNORE);END`)
		if err != nil {
			t.Fatal(err)
		}
	}
	code, body := w.run("TV", "2")
	if code != 502 || w.starts != 1 || w.record(t).Committed {
		t.Fatal(code, body, w.operations)
	}
	if _, err := w.db.Exec(`DROP TRIGGER reject_joint_history`); err != nil {
		t.Fatal(err)
	}
	got := nativeJSONRequest(w.handler, "POST", "/api/v1/subscriptions/TV/3/reconcile", w.token, `{}`)
	out := recoveryData[subscriptionSearchRunResult](t, got)
	if !out.Completed || len(out.Seasons) != 2 || !w.record(t).Committed || w.adds != 1 || w.starts != 1 {
		t.Fatal(out, w.operations)
	}
}

func TestSubscriptionSeasonCohortAnyMemberEditStopsTheWholeOriginalAuthority(t *testing.T) {
	for _, sql := range []string{
		`UPDATE RSS_TVS SET FILTER_INCLUDE='changed' WHERE ID=3`,
		`UPDATE RSS_TV_EPISODES SET EPISODES='2' WHERE RSSID='3'`,
		`DELETE FROM RSS_TVS WHERE ID=3`,
	} {
		t.Run(sql, func(t *testing.T) {
			w := newSeasonFixture(t, "qb5")
			w.afterSelection = func() {
				if _, err := w.db.Exec(sql); err != nil {
					t.Fatal(err)
				}
			}
			code, body := w.run("TV", "2")
			if code != 502 || w.adds != 1 || w.starts != 0 || w.record(t).Committed {
				t.Fatal(code, body, w.operations)
			}
			p := w.record(t)
			r := nativeJSONRequest(w.handler, "POST", "/api/v1/subscription-file-selections/"+p.Definition.ID+"/review", w.token, `{}`)
			if r.Code != 409 {
				t.Fatal("adopted changed joint source", r.Code, r.Body.String())
			}
			if code, _ := w.run("TV", "2"); code != 409 || w.adds != 1 {
				t.Fatal("pending membership bypassed", code, w.operations)
			}
		})
	}
}

func TestSubscriptionSeasonCohortIgnoredMemberIntentRollsBackBeforeAnyRemoteAdd(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	tx, _ := w.db.BeginTx(t.Context(), nil)
	if err := seasonCohortSchema(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec(`CREATE TRIGGER ignore_joint_member BEFORE INSERT ON GO_SUBSCRIPTION_SEASON_MEMBERS WHEN NEW.SUB_ID=3 BEGIN SELECT RAISE(IGNORE);END`); err != nil {
		t.Fatal(err)
	}
	code, body := w.run("TV", "2")
	if code != 409 || w.adds != 0 || w.starts != 0 {
		t.Fatal(code, body, w.operations)
	}
	rows, err := w.runner.download.service.downloaders.FileSelections(t.Context(), 1)
	if err != nil || len(rows) != 0 || seasonCount(t, w, `SELECT COUNT(*) FROM GO_SUBSCRIPTION_SEASON_MEMBERS`) != 0 {
		t.Fatal(rows, err)
	}
}

func TestSubscriptionSeasonCohortIgnoredBusinessWritesRollBackEveryMemberAndRecoverOnlyReceipt(t *testing.T) {
	for _, target := range []string{"member", "history", "delete", "lack", "torrent", "cross-edit", "recreate", "late-member", "late-shared", "late-commit", "late-torrent", "late-identity"} {
		t.Run(target, func(t *testing.T) {
			w := newSeasonFixture(t, "qb5")
			trigger := `CREATE TRIGGER block_joint BEFORE UPDATE OF STATE ON GO_SUBSCRIPTION_SEASON_MEMBERS WHEN NEW.SUB_ID=3 BEGIN SELECT RAISE(IGNORE);END`
			switch target {
			case "history":
				trigger = `CREATE TRIGGER block_joint BEFORE INSERT ON RSS_HISTORY WHEN NEW.RSSID='3' BEGIN SELECT RAISE(IGNORE);END`
			case "delete":
				trigger = `CREATE TRIGGER block_joint BEFORE DELETE ON RSS_TVS WHEN OLD.ID=3 BEGIN SELECT RAISE(IGNORE);END`
			case "lack":
				setSeasonManifest(t, w, true)
				trigger = `CREATE TRIGGER block_joint BEFORE UPDATE OF LACK ON RSS_TVS WHEN NEW.ID=3 BEGIN SELECT RAISE(IGNORE);END`
			case "torrent":
				trigger = `CREATE TRIGGER block_joint BEFORE INSERT ON RSS_TORRENTS BEGIN SELECT RAISE(IGNORE);END`
			case "cross-edit":
				trigger = `CREATE TRIGGER block_joint AFTER DELETE ON RSS_TVS WHEN OLD.ID=2 BEGIN UPDATE RSS_TVS SET FILTER_INCLUDE='unexpected edit' WHERE ID=3;END`
			case "recreate":
				trigger = `CREATE TRIGGER block_joint AFTER DELETE ON RSS_TVS WHEN OLD.ID=3 BEGIN INSERT INTO RSS_TVS(ID,NAME,YEAR,TMDBID,SEASON,STATE,TOTAL,LACK) VALUES(2,'recreated','2026','200','S01','R',3,3);END`
			case "late-member":
				trigger = `CREATE TRIGGER block_joint AFTER UPDATE OF STATE ON GO_SUBSCRIPTION_SEASON_MEMBERS WHEN NEW.SUB_ID=3 AND NEW.STATE='submitted' BEGIN UPDATE GO_SUBSCRIPTION_SEASON_MEMBERS SET STATE='pending' WHERE SUB_ID=2;END`
			case "late-shared":
				trigger = `CREATE TRIGGER block_joint AFTER INSERT ON RSS_TORRENTS BEGIN UPDATE GO_RSS_DOWNLOAD_CLAIMS SET STATE='pending';END`
			case "late-commit":
				trigger = `CREATE TRIGGER block_joint AFTER INSERT ON RSS_TORRENTS BEGIN UPDATE GO_TORRENT_FILE_SELECTIONS SET COMMITTED=0;END`
			case "late-torrent":
				trigger = `CREATE TRIGGER block_joint AFTER INSERT ON RSS_TORRENTS BEGIN UPDATE RSS_TORRENTS SET ENCLOSURE='changed' WHERE rowid=NEW.rowid;END`
			case "late-identity":
				trigger = `CREATE TRIGGER block_joint AFTER INSERT ON RSS_TORRENTS BEGIN UPDATE GO_SUBSCRIPTION_DOWNLOAD_CLAIMS SET MEDIA_ID='another-media';END`
			}
			w.afterSelection = func() {
				if _, err := w.db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			code, body := w.run("TV", "2")
			if code != 502 || w.starts != 1 || w.record(t).Committed || seasonCount(t, w, `SELECT COUNT(*) FROM RSS_HISTORY`) != 0 || seasonCount(t, w, `SELECT COUNT(*) FROM RSS_TVS`) != 2 {
				t.Fatal("partial joint commit", code, body, w.operations)
			}
			if _, err := w.db.Exec(`DROP TRIGGER block_joint`); err != nil {
				t.Fatal(err)
			}
			recoverSeasonToReceipt(t, w)
			if !w.record(t).Committed || w.adds != 1 || w.starts != 1 || w.priorities != 2 {
				t.Fatal(w.operations)
			}
		})
	}
}

func TestSubscriptionSeasonCohortWashingKeepsEachSeasonsQualityAndOrdinaryLedgerIndependent(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	if _, err := w.db.Exec(`INSERT INTO CONFIG_FILTER_GROUP(ID,GROUP_NAME,IS_DEFAULT) VALUES(777,'one','N'),(778,'two','N');INSERT INTO CONFIG_FILTER_RULES(ID,GROUP_ID,ROLE_NAME,PRIORITY,INCLUDE) VALUES(7771,'777','best','1','1080p'),(7781,'778','best','1','720p'),(7782,'778','fallback','2','1080p');UPDATE RSS_TVS SET OVER_EDITION=1,CURRENT_EP=1,LACK=0,FILTER_RULE=CASE ID WHEN 2 THEN 777 ELSE 778 END;UPDATE RSS_TV_EPISODES SET EPISODES=''`); err != nil {
		t.Fatal(err)
	}
	w.expectedPaths = append([]string{"Show.S01-S02/S01/Show.S01E01.mkv"}, w.expectedPaths...)
	code, body := w.run("TV", "2")
	out := decodeSeasonRun(t, code, body)
	if !out.Completed || len(out.Seasons) != 2 || out.Seasons[1].Completed || w.adds != 1 {
		t.Fatal(out, w.operations)
	}
	var lack, order int
	var missing string
	if err := w.db.QueryRow(`SELECT LACK,FILTER_ORDER FROM RSS_TVS WHERE ID=3`).Scan(&lack, &order); err != nil {
		t.Fatal(err)
	}
	w.db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='3'`).Scan(&missing)
	if lack != 0 || order != 98 || missing != "" || seasonCount(t, w, `SELECT COUNT(*) FROM GO_SUBSCRIPTION_EDITION_ATTEMPTS WHERE STATE='accepted'`) != 2 {
		t.Fatal(lack, order, missing)
	}
}

func TestSubscriptionSeasonCohortConcurrentRequestsReserveOneCompleteJointIntent(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	previous := w.transport
	var mutex sync.Mutex
	w.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mutex.Lock()
		defer mutex.Unlock()
		return previous.RoundTrip(r)
	})
	rebuildSeasonFixture(t, w)
	var wait sync.WaitGroup
	for _, id := range []string{"2", "3"} {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			code, body := w.run("TV", id)
			if code != 200 && code != 409 && code != 404 && code != 422 {
				t.Errorf("unexpected concurrent result %d %s", code, body)
			}
		}(id)
	}
	wait.Wait()
	if w.adds != 1 || w.starts != 1 || seasonCount(t, w, `SELECT COUNT(*) FROM RSS_HISTORY`) != 2 {
		t.Fatal(w.operations)
	}
}

func TestSubscriptionSeasonCohortRSSBatchAccountsForBothSourcesWithoutFalseFailure(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	previous := w.transport
	w.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "tracker.test" {
			if r.URL.Path == "/bundle.torrent" {
				return jsonResponse(r, string(w.raw)), nil
			}
			return jsonResponse(r, `<rss><channel>`+subscriptionFeedItem("Show.S01-S02.1080p.WEB-DL", "https://tracker.test/bundle.torrent", "", "")+`</channel></rss>`), nil
		}
		return previous.RoundTrip(r)
	})
	rebuildSeasonFixture(t, w)
	if _, err := w.runner.download.sites.Upsert(t.Context(), siteconfig.Site{Name: "Tracker", Include: "D", RSSURL: "https://tracker.test/rss", SignURL: "https://tracker.test", Note: `{"rule":"-1"}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Exec(`UPDATE RSS_TVS SET RSS_SITES='["Tracker"]'`); err != nil {
		t.Fatal(err)
	}
	out, failure := w.runner.search.feeds.execute(t.Context())
	if failure != nil || out.Subscriptions != 2 || out.Submitted != 1 || out.Completed != 2 || out.Failed != 0 || w.adds != 1 || w.starts != 1 {
		t.Fatal(out, failure, w.operations)
	}
}

func TestSubscriptionSeasonCohortScheduledBatchDoesNotResubmitCompletedSibling(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	if _, err := w.db.Exec(`UPDATE RSS_TVS SET STATE='D'`); err != nil {
		t.Fatal(err)
	}
	w.runner.search.executeSearchBatch(t.Context(), []string{"D"}, 0)
	if w.adds != 1 || w.starts != 1 || seasonCount(t, w, `SELECT COUNT(*) FROM RSS_HISTORY`) != 2 {
		t.Fatal(w.operations)
	}
}

func TestSubscriptionSeasonCohortPrivateMemberAndUnionContradictionsCannotBeAdopted(t *testing.T) {
	for _, change := range []string{"row", "proof", "files"} {
		t.Run(change, func(t *testing.T) {
			w := newSeasonFixture(t, "qb5")
			w.loss = "add"
			if code, body := w.run("TV", "2"); code != 502 {
				t.Fatal(code, body)
			}
			p := w.record(t)
			switch change {
			case "row":
				if _, err := w.db.Exec(`DELETE FROM GO_SUBSCRIPTION_SEASON_MEMBERS WHERE SUB_ID=3`); err != nil {
					t.Fatal(err)
				}
			case "proof":
				if _, err := w.db.Exec(`UPDATE GO_SUBSCRIPTION_SEASON_MEMBERS SET PAYLOAD='{}' WHERE SUB_ID=3`); err != nil {
					t.Fatal(err)
				}
			case "files":
				p.Definition.Files[0].Selected = true
				if _, err := w.db.Exec(`UPDATE GO_TORRENT_FILE_SELECTIONS SET DOCUMENT=?,FINGERPRINT=? WHERE ID=?`, string(mustFuzzyJSON(p.Definition)), fuzzyDigest(p.Definition), p.Definition.ID); err != nil {
					t.Fatal(err)
				}
			}
			response := nativeJSONRequest(w.handler, "POST", "/api/v1/subscription-file-selections/"+p.Definition.ID+"/review", w.token, `{}`)
			if response.Code != 409 || w.starts != 0 || w.adds != 1 {
				t.Fatal(response.Code, response.Body.String(), w.operations)
			}
		})
	}
}

func TestSubscriptionSeasonCohortPTSettingsRemainOneOriginalObligationBeforeFiles(t *testing.T) {
	for _, kind := range []string{"qb5", "tr-old", "tr-new"} {
		t.Run(kind, func(t *testing.T) {
			w := newSeasonFixture(t, kind)
			store := w.runner.download.service.downloaders
			item, err := store.Get(t.Context(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetSeedingAdmission(t.Context(), item, "fixture", true, bus.Hash((*downloaderconfig.SeedingAdmission)(nil))); err != nil {
				t.Fatal(err)
			}
			seconds := int64(900)
			meta, err := mediameta.Parse("Show.S01-S02.1080p.WEB-DL", "")
			if err != nil {
				t.Fatal(err)
			}
			provider := func(context.Context, map[string]any, string, string) ([]identifiedSubscriptionResource, *recognitionFailure) {
				return []identifiedSubscriptionResource{{resource: externalindexer.Resource{Title: "Show.S01-S02.1080p.WEB-DL", Indexer: "Selected-prowlarr", DownloadURL: "http://torrent.fixture/bundle.torrent", Size: 96, MinimumSeedTime: &seconds}, meta: meta, revised: "Show.S01-S02.1080p.WEB-DL"}}, nil
			}
			result, failure := w.runner.search.executeResources(t.Context(), "TV", 2, "", provider)
			if failure != nil || result.Submitted != 1 || len(result.Seasons) != 2 || w.adds != 1 || w.settings != 1 || w.starts != 1 || !w.record(t).Committed {
				t.Fatal(result, failure, w.operations)
			}
			if slices.Index(w.operations, "settings") > slices.Index(w.operations, "want") {
				t.Fatal("PT configuration followed file choice", w.operations)
			}
		})
	}
}

func TestSubscriptionSeasonCohortHistoryRestoreRetainsTypedOriginalSettingsNotOldJointAuthority(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	if _, err := w.db.Exec(`ALTER TABLE RSS_TVS ADD COLUMN EXTRA_REAL REAL;ALTER TABLE RSS_TVS ADD COLUMN EXTRA_BLOB BLOB;ALTER TABLE RSS_TVS ADD COLUMN EXTRA_NULL TEXT;UPDATE RSS_TVS SET EXTRA_REAL=7.0,EXTRA_BLOB=x'00ff00',EXTRA_NULL=NULL`); err != nil {
		t.Fatal(err)
	}
	code, body := w.run("TV", "2")
	decodeSeasonRun(t, code, body)
	original := w.record(t)
	var history int64
	if err := w.db.QueryRow(`SELECT ID FROM RSS_HISTORY WHERE RSSID='3'`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	w.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("restore accessed upstream %s", r.URL.Hostname())
		return nil, context.Canceled
	})
	rebuildSeasonFixture(t, w)
	created := ""
	for range 2 {
		r := performFormRequest(w.handler, "/api/v1/subscriptions/history/TV/"+text(history)+"/redo", w.token, url.Values{"request_id": {strings.Repeat("d", 32)}})
		out := recoveryData[map[string]string](t, r)
		if created != "" && created != out["id"] {
			t.Fatal("duplicate restore", created, out)
		}
		created = out["id"]
	}
	var state, season, realKind, blob, nullKind string
	if err := w.db.QueryRow(`SELECT STATE,SEASON,typeof(EXTRA_REAL),hex(EXTRA_BLOB),typeof(EXTRA_NULL) FROM RSS_TVS WHERE ID=?`, created).Scan(&state, &season, &realKind, &blob, &nullKind); err != nil {
		t.Fatal(err)
	}
	if state != "D" || season != "S02" || realKind != "real" || blob != "00FF00" || nullKind != "null" || w.adds != 1 {
		t.Fatal(state, season, realKind, blob, nullKind, w.operations)
	}
	r := nativeJSONRequest(w.handler, "POST", "/api/v1/subscription-file-selections/"+original.Definition.ID+"/review", w.token, `{}`)
	view := recoveryData[subscriptionFilesReview](t, r)
	if view.Next != "none" || !view.Receipt.Recorded || len(view.Receipt.Seasons) != 2 {
		t.Fatal("restored rule adopted old cohort", view)
	}
}

func TestSubscriptionSeasonCohortPendingReadsDoNotCreateTables(t *testing.T) {
	w := newSubscriptionFilesFixture(t, "qb5")
	if yes, err := pendingSeasonCohort(t.Context(), w.db, "TV", 2, "200", 1); err != nil || yes {
		t.Fatal(yes, err)
	}
	if _, yes, err := pendingSeasonLeader(t.Context(), w.db, 2); err != nil || yes {
		t.Fatal(yes, err)
	}
	if count := seasonCount(t, w, `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'GO_SUBSCRIPTION_SEASON_%'`); count != 0 {
		t.Fatal("readonly query performed DDL", count)
	}
}

func TestSubscriptionSeasonCohortOldSourceIDDoesNotBecomeAnotherMediasBackupProvenance(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	code, body := w.run("TV", "2")
	decodeSeasonRun(t, code, body)
	tx, _ := w.db.BeginTx(t.Context(), nil)
	defer tx.Rollback()
	for _, target := range []struct {
		media  string
		season int
		want   bool
	}{{"200", 1, true}, {"300", 1, false}, {"200", 3, false}} {
		found, err := hasSeasonProvenance(t.Context(), tx, 2, target.media, target.season)
		if err != nil || found != target.want {
			t.Fatal("reused ID borrowed another source's provenance", target, found, err)
		}
	}
}

func TestSubscriptionSeasonCohortCreatesMissingHistoryOnlyInTheSuccessfulReceipt(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	if _, err := w.db.Exec(`DROP TABLE RSS_HISTORY`); err != nil {
		t.Fatal(err)
	}
	code, body := w.run("TV", "2")
	if !decodeSeasonRun(t, code, body).Completed || seasonCount(t, w, `SELECT COUNT(*) FROM RSS_HISTORY`) != 2 {
		t.Fatal(code, body)
	}
}

func TestSubscriptionSeasonCohortPartialSourceLaterOrdinaryFinishKeepsOriginalTypedBackup(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	if _, err := w.db.Exec(`ALTER TABLE RSS_TVS ADD COLUMN EXTRA_BLOB BLOB;UPDATE RSS_TVS SET EXTRA_BLOB=x'00ff00'`); err != nil {
		t.Fatal(err)
	}
	setSeasonManifest(t, w, true)
	code, body := w.run("TV", "2")
	decodeSeasonRun(t, code, body)
	original := w.record(t)
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("e", 40)
	previous := w.transport
	w.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "indexer.local" && r.URL.Path == "/api/v1/search" {
			raw, _ := json.Marshal([]map[string]any{{"indexerId": 42, "title": "Show.S02E02.1080p.WEB-DL", "size": 12, "downloadUrl": magnet}})
			return jsonResponse(r, string(raw)), nil
		}
		if r.URL.Hostname() == "qb.local" && strings.HasSuffix(r.URL.Path, "/torrents/add") && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			if r.ParseForm() != nil || r.Form.Get("urls") != magnet {
				t.Fatal("ordinary completion changed its resource")
			}
			w.adds++
			return jsonResponse(r, "Ok."), nil
		}
		return previous.RoundTrip(r)
	})
	rebuildSeasonFixture(t, w)
	code, body = w.run("TV", "3")
	out := decodeSeasonRun(t, code, body)
	if !out.Completed || out.Submitted != 1 || w.adds != 2 || w.starts != 1 {
		t.Fatal(out, w.operations)
	}
	var history int64
	if err := w.db.QueryRow(`SELECT ID FROM RSS_HISTORY WHERE RSSID='3'`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	tx, _ := w.db.BeginTx(t.Context(), nil)
	archive, err := readEditionArchive(t.Context(), tx, "TV", history)
	tx.Rollback()
	if err != nil || archive == nil || archive.Version != 2 || archive.FileSelectionID != original.Definition.ID {
		t.Fatal("later finish lost original file-flow provenance", archive, err)
	}
	cell, err := fuzzySQLValue(archive.Raw["EXTRA_BLOB"])
	if err != nil || string(cell.([]byte)) != string([]byte{0, 255, 0}) {
		t.Fatal("later finish lost typed source", cell, err)
	}
}
