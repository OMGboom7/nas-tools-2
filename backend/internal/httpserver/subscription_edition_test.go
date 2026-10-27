package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

type editionFixture struct {
	*subscriptionRunFixture
	kind    string
	id      int64
	titles  []string
	hashes  []int
	present map[string]bool
	feeds   int
}

func newEditionFixture(t *testing.T, tv, rss bool) *editionFixture {
	t.Helper()
	f := &editionFixture{kind: "MOV", id: 1, present: map[string]bool{}}
	if tv {
		f.kind, f.id = "TV", 2
	}
	if rss {
		f.subscriptionRunFixture = newSubscriptionFeedFixture(t, tv, `{"rule":"-1","parse":"N"}`, func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/rss" {
				t.Fatal("unexpected edition detail request", r.URL.Path)
			}
			f.feeds++
			body := "<rss><channel>"
			for i, title := range f.titles {
				body += subscriptionFeedItem(title, fmt.Sprintf("magnet:?xt=urn:btih:%040x", f.hashes[i]), "", "")
			}
			return jsonResponse(r, body+"</channel></rss>"), nil
		})
	} else {
		f.subscriptionRunFixture = newSubscriptionRunFixture(t)
	}
	previous := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "emby.test":
			t.Fatal("edition planning queried inventory instead of its quality receipt")
			return nil, context.Canceled
		case "indexer.local":
			if r.URL.Path == "/api/v1/indexerstats" {
				return previous.RoundTrip(r)
			}
			f.queries++
			if f.afterSearch != nil {
				f.afterSearch()
			}
			rows := []map[string]any{}
			for i, title := range f.titles {
				rows = append(rows, map[string]any{"indexerId": 42, "title": title, "size": 1024, "downloadUrl": fmt.Sprintf("magnet:?xt=urn:btih:%040x", f.hashes[i])})
			}
			raw, _ := json.Marshal(rows)
			return jsonResponse(r, string(raw)), nil
		case "qb.local":
			if strings.HasSuffix(r.URL.Path, "/torrents/info") {
				hash := r.URL.Query().Get("hashes")
				if f.present[hash] {
					return jsonResponse(r, `[{"hash":"`+hash+`"}]`), nil
				}
				return jsonResponse(r, "[]"), nil
			}
			if strings.HasSuffix(r.URL.Path, "/torrents/add") {
				if r.ParseForm() != nil {
					t.Fatal("invalid add form")
				}
				f.present[magnetInfoHash(r.Form.Get("urls"))] = true
			}
		}
		return previous.RoundTrip(r)
	})
	var runner *rssRunAPI
	var err error
	f.handler, err = buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true, SiteCatalogPath: f.runner.recognition.service.catalogPath}, f.transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	f.runner = runner
	other := "RSS_TVS"
	if tv {
		other = "RSS_MOVIES"
	}
	if _, err := f.db.Exec("DELETE FROM " + other + `;INSERT INTO CONFIG_FILTER_GROUP(ID,GROUP_NAME,IS_DEFAULT) VALUES(777,'Edition','N');INSERT INTO CONFIG_FILTER_RULES(ID,GROUP_ID,ROLE_NAME,PRIORITY,INCLUDE) VALUES(7771,'777','best','1','1080p'),(7772,'777','fallback','2','720p');UPDATE ` + subscriptionTable(f.kind) + ` SET OVER_EDITION=1,FUZZY_MATCH=0,FILTER_RULE=777,FILTER_ORDER=NULL`); err != nil {
		t.Fatal(err)
	}
	if tv {
		if _, err := f.db.Exec(`UPDATE RSS_TVS SET LACK=0;UPDATE RSS_TV_EPISODES SET EPISODES=''`); err != nil {
			t.Fatal(err)
		}
	}
	f.titles, f.hashes = []string{"Movie.2026.1080p.WEB-DL"}, []int{71}
	if tv {
		// Quality-only fixtures use an explicit episode range. Season-pack
		// completeness is covered with real metainfo in subscription_files_test.
		f.titles = []string{"Show.S01E01-E03.1080p.WEB-DL"}
	}
	return f
}
func (f *editionFixture) source() string { return "/api/v1/subscriptions/" + f.kind + "/" + text(f.id) }
func (f *editionFixture) execute() *httptest.ResponseRecorder {
	return performFormRequest(f.handler, f.source()+"/refresh", f.token, url.Values{})
}
func editionScopeForTest(t *testing.T, f *editionFixture) map[string]json.RawMessage {
	t.Helper()
	return recoveryData[map[string]json.RawMessage](t, nativeJSONRequest(f.handler, "GET", f.source()+"/upgrade", f.token, ""))
}
func editionViewForTest(t *testing.T, f *editionFixture) subscriptionEditionView {
	t.Helper()
	raw := editionScopeForTest(t, f)
	var v subscriptionEditionView
	if json.Unmarshal(raw["upgrade"], &v) != nil {
		t.Fatal("bad progress view")
	}
	return v
}
func editionReceiptForTest(t *testing.T, f *editionFixture, id string) editionReceipt {
	t.Helper()
	return recoveryData[editionReceipt](t, nativeJSONRequest(f.handler, "GET", "/api/v1/subscription-editions/"+id, f.token, ""))
}
func editionRunForTest(t *testing.T, f *editionFixture) subscriptionSearchRunResult {
	t.Helper()
	return recoveryData[subscriptionSearchRunResult](t, f.execute())
}
func editionAbandonForTest(t *testing.T, f *editionFixture) {
	t.Helper()
	scope := editionScopeForTest(t, f)
	var revision string
	if json.Unmarshal(scope["revision"], &revision) != nil {
		t.Fatal("bad revision")
	}
	raw, _ := json.Marshal(map[string]any{"revision": revision, "confirm": true, "reason": "fixture fresh full quality cycle"})
	r := nativeJSONRequest(f.handler, "POST", f.source()+"/upgrade/reset", f.token, string(raw))
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
}
func TestEditionReadOnlyInspectionAndPlanHaveNoNewControlTablesOrDownloads(t *testing.T) {
	f := newEditionFixture(t, false, false)
	for _, path := range []string{f.source() + "/upgrade", "/api/v1/subscription-editions"} {
		r := nativeJSONRequest(f.handler, "GET", path, f.token, "")
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if f.queries != 0 || f.adds != 0 {
		t.Fatal("local inspection used external services")
	}
	p := recoveryData[subscriptionSearchPlan](t, performFormRequest(f.handler, "/api/v1/subscriptions/search/plan", f.token, url.Values{"type": {"MOV"}, "id": {"1"}}))
	if p.Upgrade == nil || p.Upgrade.Order != 99 || len(p.Candidates) != 1 || p.LibraryComplete || f.adds != 0 {
		t.Fatal(p, f.adds)
	}
	if p.Upgrade.CycleID != "" {
		t.Fatal("unsaved readonly plan returned a nonexistent persisted cycle")
	}
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'GO_SUBSCRIPTION_EDITION_%'`).Scan(&count)
	if count != 0 {
		t.Fatal("readonly plan initialized edition controls", count)
	}
}
func TestEditionMovieAdvancesLowerQualityThenOnlyFinishesAtTop(t *testing.T) {
	f := newEditionFixture(t, false, false)
	f.titles = []string{"Movie.2026.720p.WEB-DL"}
	f.hashes = []int{81}
	v := editionRunForTest(t, f)
	if v.Completed || v.Upgrade == nil || !v.Upgrade.CycleComplete || v.Upgrade.Order != 98 || f.adds != 1 {
		t.Fatal(v, f.adds)
	}
	first := v.Upgrade.CycleID
	var order int
	if err := f.db.QueryRow(`SELECT FILTER_ORDER FROM RSS_MOVIES WHERE ID=1`).Scan(&order); err != nil || order != 98 {
		t.Fatal(order, err)
	}
	if again := editionRunForTest(t, f); again.Submitted != 0 || again.Completed || f.adds != 1 {
		t.Fatal("equal quality replayed", again, f.adds)
	}
	f.titles = []string{"Movie.2026.1080p.WEB-DL"}
	f.hashes = []int{82}
	v = editionRunForTest(t, f)
	if !v.Completed || v.Upgrade.Order != 99 || f.adds != 2 {
		t.Fatal(v, f.adds)
	}
	receipt := editionReceiptForTest(t, f, first)
	if receipt.State != "completed" || len(receipt.Attempts) != 1 || receipt.Attempts[0].State != "accepted" {
		t.Fatal(receipt)
	}
	var histories, events int
	f.db.QueryRow(`SELECT COUNT(*) FROM RSS_HISTORY`).Scan(&histories)
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='rss_finished'`).Scan(&events)
	if histories != 1 || events != 1 {
		t.Fatal("lower cycle emitted completion", histories, events)
	}
}
func TestEditionTVPartialQualityNeverMixesOrConsumesOrdinaryMissingLedger(t *testing.T) {
	f := newEditionFixture(t, true, false)
	f.titles = []string{"Show.S01E01.1080p.WEB-DL", "Show.S01E02.720p.WEB-DL", "Show.S01E03.720p.WEB-DL"}
	f.hashes = []int{91, 92, 93}
	v := editionRunForTest(t, f)
	if v.Completed || v.Submitted != 1 || !slices.Equal(v.Remaining, []int{2, 3}) || f.adds != 1 {
		t.Fatal(v, f.adds)
	}
	id := v.Upgrade.CycleID
	var total, lack int
	var oldEpisodes string
	var order any
	f.db.QueryRow(`SELECT TOTAL,LACK,FILTER_ORDER FROM RSS_TVS WHERE ID=2`).Scan(&total, &lack, &order)
	f.db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='2'`).Scan(&oldEpisodes)
	if total != 3 || lack != 0 || order != nil || oldEpisodes != "" {
		t.Fatal("wash forged ordinary progress", total, lack, order, oldEpisodes)
	}
	f.titles = f.titles[1:]
	f.hashes = f.hashes[1:]
	if v := editionRunForTest(t, f); v.Submitted != 0 || v.Completed || f.adds != 1 {
		t.Fatal("mixed lower quality into active cycle", v, f.adds)
	}
	f.titles = []string{"Show.S01E02.1080p.WEB-DL", "Show.S01E03.1080p.WEB-DL"}
	f.hashes = []int{94, 95}
	v = editionRunForTest(t, f)
	if !v.Completed || v.Submitted != 2 || v.Upgrade.CycleID != id || f.adds != 3 {
		t.Fatal(v, f.adds)
	}
	receipt := editionReceiptForTest(t, f, id)
	if receipt.State != "completed" || len(receipt.Attempts) != 3 || len(receipt.Progress.Remaining) != 0 {
		t.Fatal(receipt)
	}
}
func TestEditionExplicitAbandonPreservesAcceptedAttemptsAndDoesNotStartNewCycle(t *testing.T) {
	f := newEditionFixture(t, true, false)
	f.titles = []string{"Show.S01E01.720p.WEB-DL"}
	f.hashes = []int{101}
	v := editionRunForTest(t, f)
	old := v.Upgrade.CycleID
	f.titles = []string{"Show.S01E01-E03.1080p.WEB-DL"}
	f.hashes = []int{102}
	if v := editionRunForTest(t, f); v.Submitted != 0 || f.adds != 1 {
		t.Fatal("replaced active quality without explicit choice", v, f.adds)
	}
	editionAbandonForTest(t, f)
	if receipt := editionReceiptForTest(t, f, old); receipt.State != "abandoned" || receipt.Attempts[0].State != "accepted" || f.adds != 1 {
		t.Fatal(receipt, f.adds)
	}
	v = editionRunForTest(t, f)
	if !v.Completed || v.Upgrade.CycleID == old || f.adds != 2 {
		t.Fatal(v, f.adds)
	}
}
func TestEditionUncertainReceiptKeepsCycleAndPositiveVerificationDoesNotRepeatAdd(t *testing.T) {
	for _, tv := range []bool{false, true} {
		t.Run(fmt.Sprint(tv), func(t *testing.T) {
			f := newEditionFixture(t, tv, false)
			if tv {
				f.titles = []string{"Show.S01E01.1080p.WEB-DL"}
			}
			f.addError = true
			r := f.execute()
			if r.Code != 502 || f.adds != 1 {
				t.Fatal(r.Code, r.Body.String(), f.adds)
			}
			view := editionViewForTest(t, f)
			if view.CycleID == "" || view.CycleComplete {
				t.Fatal(view)
			}
			if again := f.execute(); again.Code != 409 || f.adds != 1 {
				t.Fatal("uncertain add repeated", again.Code, f.adds)
			}
			scope := editionScopeForTest(t, f)
			var revision string
			_ = json.Unmarshal(scope["revision"], &revision)
			raw, _ := json.Marshal(map[string]any{"revision": revision, "confirm": true, "reason": "must not bypass unknown"})
			if denied := nativeJSONRequest(f.handler, "POST", f.source()+"/upgrade/reset", f.token, string(raw)); denied.Code != 409 {
				t.Fatal("abandon bypassed pending claim", denied.Code)
			}
			f.addError = false
			v := recoveryData[subscriptionSearchRunResult](t, performFormRequest(f.handler, f.source()+"/reconcile", f.token, url.Values{}))
			if v.Uncertain || v.Completed == tv || f.adds != 1 {
				t.Fatal(v, f.adds)
			}
			if tv && (!slices.Equal(v.Remaining, []int{2, 3}) || v.Upgrade.CycleComplete) {
				t.Fatal("first episode ended wash", v)
			}
		})
	}
}
func TestEditionDefinitiveRejectionRetainsAttemptAndAllowsOnlyNewNativeAttempt(t *testing.T) {
	f := newEditionFixture(t, false, false)
	f.loginFailure = true
	if r := f.execute(); r.Code != 422 || f.adds != 0 {
		t.Fatal(r.Code, r.Body.String(), f.adds)
	}
	view := editionViewForTest(t, f)
	receipt := editionReceiptForTest(t, f, view.CycleID)
	if len(receipt.Attempts) != 1 || receipt.Attempts[0].State != "rejected" {
		t.Fatal(receipt)
	}
	f.loginFailure = false
	v := editionRunForTest(t, f)
	receipt = editionReceiptForTest(t, f, view.CycleID)
	if !v.Completed || f.adds != 1 || len(receipt.Attempts) != 2 || receipt.Attempts[0].State != "rejected" || receipt.Attempts[1].State != "accepted" {
		t.Fatal(v, receipt, f.adds)
	}
}
func TestEditionIgnoredWritesCannotFakeSuccessAndAcceptedFactsNeverAutoReplay(t *testing.T) {
	for _, boundary := range []string{"intent", "cycle", "priority", "history", "delete"} {
		t.Run(boundary, func(t *testing.T) {
			f := newEditionFixture(t, false, false)
			tx, err := f.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := editionSchema(t.Context(), tx); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			triggers := map[string]string{
				"intent":   `CREATE TRIGGER reject_edition BEFORE INSERT ON GO_SUBSCRIPTION_EDITION_ATTEMPTS BEGIN SELECT RAISE(IGNORE);END`,
				"cycle":    `CREATE TRIGGER reject_edition BEFORE UPDATE ON GO_SUBSCRIPTION_EDITION_CYCLES WHEN NEW.STATE='completed' BEGIN SELECT RAISE(IGNORE);END`,
				"priority": `CREATE TRIGGER reject_edition BEFORE UPDATE ON RSS_MOVIES WHEN NEW.FILTER_ORDER IS NOT NULL BEGIN SELECT RAISE(IGNORE);END`,
				"history":  `CREATE TRIGGER reject_edition BEFORE INSERT ON RSS_HISTORY BEGIN SELECT RAISE(IGNORE);END`,
				"delete":   `CREATE TRIGGER reject_edition BEFORE DELETE ON RSS_MOVIES BEGIN SELECT RAISE(IGNORE);END`,
			}
			if boundary == "history" {
				if _, err := f.db.Exec(`CREATE TABLE RSS_HISTORY(ID INTEGER PRIMARY KEY,TYPE TEXT,RSSID TEXT,NAME TEXT,YEAR TEXT,TMDBID TEXT,SEASON TEXT,IMAGE TEXT,DESC TEXT,TOTAL INTEGER,START INTEGER,FINISH_TIME TEXT)`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.db.Exec(triggers[boundary]); err != nil {
				t.Fatal(err)
			}
			r := f.execute()
			expected := 1
			if boundary == "intent" {
				expected = 0
			}
			if r.Code == 200 || f.adds != expected {
				t.Fatal(boundary, r.Code, r.Body.String(), f.adds)
			}
			var source int
			f.db.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES`).Scan(&source)
			if source != 1 {
				t.Fatal("partial completion removed source")
			}
			if boundary != "intent" {
				if again := f.execute(); again.Code != 409 || f.adds != 1 {
					t.Fatal("accepted task replayed", again.Code, f.adds)
				}
			}
			if _, err := f.db.Exec(`DROP TRIGGER reject_edition`); err != nil {
				t.Fatal(err)
			}
			if boundary == "intent" {
				if v := editionRunForTest(t, f); !v.Completed || f.adds != 1 {
					t.Fatal(v, f.adds)
				}
			} else {
				if r := performFormRequest(f.handler, f.source()+"/reconcile", f.token, url.Values{}); r.Code != 200 || f.adds != 1 {
					t.Fatal(r.Code, r.Body.String(), f.adds)
				}
			}
		})
	}
}
func TestEditionChangedSourceAndDownloaderBeforeIntentPreventOutboundSubmission(t *testing.T) {
	for _, statement := range []string{`UPDATE RSS_MOVIES SET FILTER_INCLUDE='new rule'`, `UPDATE DOWNLOADER SET CONFIG=replace(CONFIG,'qb.local','different-account.local')`, `UPDATE CONFIG_FILTER_RULES SET INCLUDE='different rule' WHERE ID=7771`} {
		t.Run(statement, func(t *testing.T) {
			f := newEditionFixture(t, false, false)
			f.afterSearch = func() {
				if _, err := f.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if r := f.execute(); r.Code != 409 || f.adds != 0 {
				t.Fatal(r.Code, r.Body.String(), f.adds)
			}
		})
	}
}
func TestEditionHistoryRestorePreservesTypedUnknownSettingsAndStableRequest(t *testing.T) {
	f := newEditionFixture(t, true, false)
	if _, err := f.db.Exec(`ALTER TABLE RSS_TVS ADD COLUMN EXTRA_REAL REAL;ALTER TABLE RSS_TVS ADD COLUMN EXTRA_BLOB BLOB;ALTER TABLE RSS_TVS ADD COLUMN EXTRA_NULL TEXT;UPDATE RSS_TVS SET EXTRA_REAL=7.0,EXTRA_BLOB=x'00ff00',EXTRA_NULL=NULL`); err != nil {
		t.Fatal(err)
	}
	v := editionRunForTest(t, f)
	if !v.Completed {
		t.Fatal(v)
	}
	var historyID int64
	if err := f.db.QueryRow(`SELECT ID FROM RSS_HISTORY`).Scan(&historyID); err != nil {
		t.Fatal(err)
	}
	// No restore may ask TMDB or pick fresh default subscription settings.
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("history restore performed external lookup %s", r.URL.Hostname())
		return nil, context.Canceled
	})
	h, err := newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = h
	requestID := strings.Repeat("a", 32)
	var created string
	for range 2 {
		r := performFormRequest(f.handler, "/api/v1/subscriptions/history/TV/"+text(historyID)+"/redo", f.token, url.Values{"request_id": {requestID}})
		out := recoveryData[map[string]string](t, r)
		if created != "" && out["id"] != created {
			t.Fatal("duplicate stable restore created another rule")
		}
		created = out["id"]
	}
	var state, blob, realKind, nullKind string
	var wash, filter int
	if err := f.db.QueryRow(`SELECT STATE,OVER_EDITION,FILTER_RULE,typeof(EXTRA_REAL),hex(EXTRA_BLOB),typeof(EXTRA_NULL) FROM RSS_TVS WHERE ID=?`, created).Scan(&state, &wash, &filter, &realKind, &blob, &nullKind); err != nil {
		t.Fatal(err)
	}
	if state != "D" || wash != 1 || filter != 777 || realKind != "real" || blob != "00FF00" || nullKind != "null" {
		t.Fatal(state, wash, filter, realKind, blob, nullKind)
	}
	if receipt := editionReceiptForTest(t, f, v.Upgrade.CycleID); receipt.State != "completed" || receipt.SourceMatches {
		t.Fatal("restored ID reused old cycle authority", receipt)
	}
}
func TestEditionRuleEditAndDeleteAbandonActiveCycleWithoutErasingFacts(t *testing.T) {
	for _, action := range []string{"edit", "modern-delete", "legacy-delete"} {
		t.Run(action, func(t *testing.T) {
			f := newEditionFixture(t, true, false)
			f.titles = []string{"Show.S01E01.720p.WEB-DL"}
			v := editionRunForTest(t, f)
			cycle := v.Upgrade.CycleID
			var r *httptest.ResponseRecorder
			if action == "edit" {
				r = nativeJSONRequest(f.handler, "POST", "/api/v1/subscriptions", f.token, `{"id":"2","name":"Show","year":"2026","type":"TV","mediaId":"200","season":"S01","overEdition":true,"filterRule":"777"}`)
			} else if action == "modern-delete" {
				r = performFormRequest(f.handler, f.source()+"/remove", f.token, url.Values{})
			} else {
				r = performFormRequest(f.handler, "/api/v1/subscribe/delete", f.token, url.Values{"type": {"TV"}, "rssid": {"2"}})
			}
			if r.Code != 200 {
				t.Fatal(r.Code, r.Body.String())
			}
			receipt := editionReceiptForTest(t, f, cycle)
			if receipt.State != "abandoned" || len(receipt.Attempts) != 1 || receipt.Attempts[0].State != "accepted" || f.adds != 1 {
				t.Fatal(receipt, f.adds)
			}
		})
	}
}
func TestEditionRSSAndScheduledSearchUseTheSameCycleSemantics(t *testing.T) {
	for _, rss := range []bool{false, true} {
		t.Run(fmt.Sprint(rss), func(t *testing.T) {
			f := newEditionFixture(t, false, rss)
			if rss {
				r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
				if r.Code != 200 || !strings.Contains(r.Body.String(), `"completed":1`) || f.adds != 1 || f.feeds != 1 {
					t.Fatal(r.Code, r.Body.String(), f.adds, f.feeds)
				}
			} else {
				f.runner.search.executeSearchBatch(t.Context(), []string{"D"}, 0)
				if f.adds != 1 {
					t.Fatal("scheduler did not execute edition source", f.adds)
				}
			}
			var cycles int
			f.db.QueryRow(`SELECT COUNT(*) FROM GO_SUBSCRIPTION_EDITION_CYCLES WHERE STATE='completed'`).Scan(&cycles)
			if cycles != 1 {
				t.Fatal(cycles)
			}
		})
	}
}
func TestEditionInspectionsAreRootOnlyAndMixedNamespacesNeverReachPython(t *testing.T) {
	f := newEditionFixture(t, false, false)
	viewer, err := f.runner.download.service.auth.service.IssueToken("viewer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", viewer} {
		want := 401
		if token == viewer {
			want = 403
		}
		for _, path := range []string{f.source() + "/upgrade", "/api/v1/subscription-editions", "/api/v1/subscription-editions/" + strings.Repeat("a", 64)} {
			r := nativeJSONRequest(f.handler, "GET", path, token, "")
			if r.Code != want {
				t.Fatal(path, r.Code, r.Body.String())
			}
		}
	}
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("edition namespace fell back to Python: %s", r.URL.Path)
	}))
	defer python.Close()
	h, err := newHandler(config.Config{ApplicationConfigPath: f.path, LegacyBackendURL: python.URL}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.source() + "/upgrade", f.source() + "/upgrade/reset", f.source() + "/upgrade/unknown", "/api/v1/subscription-editions", "/api/v1/subscription-editions/" + strings.Repeat("a", 64), "/api/v1/subscription-editions/unknown/action"} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			r := nativeJSONRequest(h, method, path, f.token, `{}`)
			if r.Code != 501 {
				t.Fatal(method, path, r.Code, r.Body.String())
			}
		}
	}
}
func TestEditionUnverifiedCrossSeasonAndOverlapCannotAuthorizeWholeResources(t *testing.T) {
	f := newEditionFixture(t, true, false)
	t.Setenv("NASTOOL_ARIA2_BINARY", filepath.Join(t.TempDir(), "missing"))
	f.titles = []string{"Show.S01-S02.1080p.WEB-DL", "Show.S02E01.1080p.WEB-DL"}
	f.hashes = []int{111, 112}
	if response := f.execute(); response.Code != 422 || f.adds != 0 {
		t.Fatal("unverified cross-season magnet authorized a whole download", response.Code, response.Body.String(), f.adds)
	}
	f.titles = []string{"Show.S01E01.1080p.WEB-DL"}
	f.hashes = []int{113}
	v := editionRunForTest(t, f)
	if v.Submitted != 1 {
		t.Fatal(v)
	}
	f.titles = []string{"Show.S01E01-E03.1080p.WEB-DL"}
	f.hashes = []int{114}
	// The overlap now enters real file selection, not a whole-pack submission.
	// Keep this legacy fixture entirely offline when its magnet has no manifest.
	t.Setenv("NASTOOL_ARIA2_BINARY", filepath.Join(t.TempDir(), "missing"))
	if response := f.execute(); response.Code != 422 || f.adds != 1 {
		t.Fatal("unverified magnet authorized whole overlap", response.Code, response.Body.String(), f.adds)
	}
}

func TestEditionReceiptRejectsCrossScopePrivateProofAndOldPrioritySnapshot(t *testing.T) {
	f := newEditionFixture(t, true, false)
	f.titles = []string{"Show.S01E01.1080p.WEB-DL"}
	v := editionRunForTest(t, f)
	cycle := v.Upgrade.CycleID
	var raw string
	if err := f.db.QueryRow(`SELECT DOCUMENT FROM GO_SUBSCRIPTION_EDITION_CYCLES WHERE ID=?`, cycle).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var c subscriptionEditionCycle
	if decodeFuzzyDocument(raw, &c, 1<<20) != nil {
		t.Fatal("invalid initial cycle")
	}
	c.Name = "Different scope"
	if _, err := f.db.Exec(`UPDATE GO_SUBSCRIPTION_EDITION_CYCLES SET DOCUMENT=? WHERE ID=?`, string(mustFuzzyJSON(c)), cycle); err != nil {
		t.Fatal(err)
	}
	r := nativeJSONRequest(f.handler, "GET", "/api/v1/subscription-editions/"+cycle, f.token, "")
	if r.Code != 409 {
		t.Fatal("cross-scope proof projected as original receipt", r.Code, r.Body.String())
	}
	if _, err := f.db.Exec(`UPDATE GO_SUBSCRIPTION_EDITION_CYCLES SET DOCUMENT=? WHERE ID=?`, raw, cycle); err != nil {
		t.Fatal(err)
	}
	plan, failure := f.runner.search.planner.plan(t.Context(), "TV", 2)
	if failure != nil {
		t.Fatal(failure)
	}
	plan.edition.PreviousOrder = 100
	if err := f.runner.search.planner.runner.recognition.service.checkEditionSnapshot(t.Context(), f.db, "TV", 2, plan); err == nil {
		t.Fatal("changed recorded-priority snapshot accepted")
	}
}
