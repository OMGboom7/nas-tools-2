package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

const fuzzyEndpoint = "/api/v1/fuzzy-subscriptions"

type fuzzyFixture struct {
	*subscriptionRunFixture
	feed    string
	fetches atomic.Int32
	present map[string]bool
	kind    string
	id      int64
}

func newFuzzyFixture(t *testing.T, tv bool) *fuzzyFixture {
	t.Helper()
	v := &fuzzyFixture{present: map[string]bool{}, kind: "MOV", id: 1}
	if tv {
		v.kind, v.id = "TV", 2
	}
	base := newSubscriptionFeedFixture(t, tv, `{"rule":"-1"}`, func(r *http.Request) (*http.Response, error) {
		v.fetches.Add(1)
		if r.URL.Path != "/rss" {
			t.Fatalf("unexpected fuzzy site request %s", r.URL.Path)
		}
		return jsonResponse(r, v.feed), nil
	})
	v.subscriptionRunFixture = base
	if err := config.NewStore(v.path).Update(map[string]any{"app.rmt_tmdbkey": ""}); err != nil {
		t.Fatal(err)
	}
	previous := v.transport
	v.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "tracker.test":
			return previous.RoundTrip(r)
		case "qb.local":
			if strings.HasSuffix(r.URL.Path, "/torrents/info") {
				hash := r.URL.Query().Get("hashes")
				if v.present[hash] {
					return jsonResponse(r, `[{"hash":"`+hash+`"}]`), nil
				}
				return jsonResponse(r, `[]`), nil
			}
			if strings.HasSuffix(r.URL.Path, "/torrents/add") {
				if err := r.ParseForm(); err != nil {
					t.Fatal(err)
				}
				v.present[magnetInfoHash(r.Form.Get("urls"))] = true
			}
			return previous.RoundTrip(r)
		default:
			t.Fatalf("fuzzy lookup must not require TMDB, media inventory, indexer or Python: %s", r.URL.Hostname())
			return nil, context.Canceled
		}
	})
	var runner *rssRunAPI
	var err error
	v.handler, err = buildHandler(config.Config{ApplicationConfigPath: v.path, DisableLegacy: true, SiteCatalogPath: base.runner.recognition.service.catalogPath}, v.transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	v.runner = runner
	query := `UPDATE RSS_MOVIES SET NAME='Movie|Other',YEAR='',TMDBID=NULL,FUZZY_MATCH=1,OVER_EDITION=0,FILTER_RULE=-1,STATE='R'`
	if tv {
		query = `UPDATE RSS_TVS SET NAME='Show',YEAR='',TMDBID=NULL,FUZZY_MATCH=1,OVER_EDITION=0,FILTER_RULE=-1,SEASON='S00',STATE='R',TOTAL=0,LACK=0`
	}
	if _, err := v.db.Exec(query); err != nil {
		t.Fatal(err)
	}
	v.feed = `<rss><channel>` + subscriptionFeedItem("Movie.2026.1080p.WEB-DL", fmt.Sprintf("magnet:?xt=urn:btih:%040x", 1), "", "") + subscriptionFeedItem("Other.2026.1080p.WEB-DL", fmt.Sprintf("magnet:?xt=urn:btih:%040x", 2), "", "") + `</channel></rss>`
	return v
}
func fuzzyHTTP(t *testing.T, f *fuzzyFixture, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	return nativeJSONRequest(f.handler, method, path, f.token, raw)
}
func fuzzySourcePath(f *fuzzyFixture) string { return fuzzyEndpoint + "/" + f.kind + "/" + text(f.id) }
func fuzzyGrantForTest(t *testing.T, f *fuzzyFixture) fuzzyGrantReview {
	t.Helper()
	response := recoveryData[map[string]json.RawMessage](t, fuzzyHTTP(t, f, "GET", fuzzySourcePath(f), nil))
	var view fuzzyGrantReview
	if err := json.Unmarshal(response["review"], &view); err != nil {
		t.Fatal(err)
	}
	r := fuzzyHTTP(t, f, "POST", fuzzySourcePath(f)+"/authorize", fuzzyRequest{Fingerprint: view.Fingerprint, Confirm: true, ConfirmWhole: true, ConfirmOldExecutorsStopped: true})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	return view
}
func fuzzyReviewForTest(t *testing.T, f *fuzzyFixture) fuzzyReview {
	t.Helper()
	return recoveryData[fuzzyReview](t, fuzzyHTTP(t, f, "POST", fuzzySourcePath(f)+"/preview", fuzzyRequest{ConfirmRead: true}))
}
func fuzzySaveForTest(t *testing.T, f *fuzzyFixture, v fuzzyReview) fuzzyBatch {
	t.Helper()
	return recoveryData[fuzzyBatch](t, fuzzyHTTP(t, f, "POST", fuzzySourcePath(f)+"/save", fuzzyRequest{Fingerprint: v.Fingerprint, RequestID: v.RequestID, ConfirmRead: true, Confirm: true, ConfirmWhole: true, ConfirmOldExecutorsStopped: true}))
}
func fuzzyGetForTest(t *testing.T, f *fuzzyFixture, id string) fuzzyBatch {
	t.Helper()
	j, err := f.runner.search.fuzzy.get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func fuzzyContinueForTest(t *testing.T, f *fuzzyFixture, j fuzzyBatch) {
	t.Helper()
	path := fuzzyEndpoint + "/jobs/" + j.ID
	v := recoveryData[fuzzyContinuationReview](t, fuzzyHTTP(t, f, "POST", path+"/review", fuzzyRequest{ConfirmRead: true}))
	if !v.CanResume {
		t.Fatal("known remaining targets were not reviewable", v)
	}
	r := fuzzyHTTP(t, f, "POST", path+"/resume", fuzzyRequest{Revision: v.Batch.Revision, Fingerprint: v.Fingerprint, ConfirmRead: true, Confirm: true, ConfirmWhole: true, ConfirmOldExecutorsStopped: true})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
}
func TestFuzzySubscriptionOldFlagHasNoNewAuthorityAndReadonlyDoesNotCreateTables(t *testing.T) {
	f := newFuzzyFixture(t, false)
	for _, path := range []string{fuzzyEndpoint, fuzzySourcePath(f)} {
		if r := fuzzyHTTP(t, f, "GET", path, nil); r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"skipped":1`) || f.fetches.Load() != 0 || f.adds != 0 {
		t.Fatal(r.Code, r.Body.String(), f.fetches.Load(), f.adds)
	}
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'GO_FUZZY_%'`).Scan(&count)
	if count != 0 {
		t.Fatal("readonly inspection initialized fuzzy runtime tables", count)
	}
	view := recoveryData[FuzzySourceForTest](t, fuzzyHTTP(t, f, "GET", fuzzySourcePath(f), nil))
	for _, missing := range []string{"confirm", "confirmWhole", "confirmOldExecutorsStopped"} {
		body := map[string]any{"fingerprint": view.Review.Fingerprint, "confirm": true, "confirmWhole": true, "confirmOldExecutorsStopped": true}
		body[missing] = false
		if r := fuzzyHTTP(t, f, "POST", fuzzySourcePath(f)+"/authorize", body); r.Code != 400 {
			t.Fatal(missing, r.Code, r.Body.String())
		}
	}
}

type FuzzySourceForTest struct {
	Review fuzzyGrantReview
	Jobs   []fuzzyBatch
}

func TestFuzzySubscriptionWholeMovieBatchClosesOnlyAfterEveryAcceptedReceipt(t *testing.T) {
	f := newFuzzyFixture(t, false)
	fuzzyGrantForTest(t, f)
	v := fuzzyReviewForTest(t, f)
	if len(v.Candidates) != 2 || f.adds != 0 {
		t.Fatal(v, f.adds)
	}
	j := fuzzySaveForTest(t, f, v)
	f.afterAdd = func() {
		var count int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES WHERE ID=1`).Scan(&count); err != nil || count != 1 {
			t.Fatal("first accepted target prematurely closed whole rule", count, err)
		}
	}
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	out := fuzzyGetForTest(t, f, j.ID)
	if out.State != "completed" || out.SourceResult != "finished" || len(out.Items) != 2 || f.adds != 2 {
		t.Fatal(out, f.adds)
	}
	var history, original, downloads, finished int
	f.db.QueryRow(`SELECT COUNT(*) FROM RSS_HISTORY`).Scan(&history)
	f.db.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES`).Scan(&original)
	f.db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&downloads)
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='rss_finished'`).Scan(&finished)
	if history != 1 || original != 0 || downloads != 2 || finished != 1 {
		t.Fatal(history, original, downloads, finished)
	}
	fetched := f.fetches.Load()
	repeated := fuzzySaveForTest(t, f, v)
	if repeated.ID != j.ID || repeated.State != "completed" || f.adds != 2 || f.fetches.Load() != fetched {
		t.Fatal("saved request re-read feeds or resubmitted", repeated)
	}
	b, _ := json.Marshal(out)
	for _, secret := range []string{"magnet:", "private-site", "private-downloader", "passkey", "ConfigHash", "Native", "DOCUMENT"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("private definition leaked", secret)
		}
	}
}
func TestFuzzySubscriptionWildcardTVUsesWholeTorrentsWithoutInventedInventory(t *testing.T) {
	f := newFuzzyFixture(t, true)
	f.feed = `<rss><channel>` + subscriptionFeedItem("Show.S01E01.1080p.WEB-DL", fmt.Sprintf("magnet:?xt=urn:btih:%040x", 11), "", "") + subscriptionFeedItem("Show.S02E02.1080p.WEB-DL", fmt.Sprintf("magnet:?xt=urn:btih:%040x", 12), "", "") + subscriptionFeedItem("Movie.2026.1080p.WEB-DL", fmt.Sprintf("magnet:?xt=urn:btih:%040x", 13), "", "") + `</channel></rss>`
	fuzzyGrantForTest(t, f)
	v := fuzzyReviewForTest(t, f)
	if len(v.Candidates) != 2 || v.Candidates[0].Kind != "TV" {
		t.Fatal(v)
	}
	j := fuzzySaveForTest(t, f, v)
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	out := fuzzyGetForTest(t, f, j.ID)
	if out.SourceResult != "finished" || f.adds != 2 {
		t.Fatal(out, f.adds)
	}
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_FUZZY_SOURCE_ARCHIVES WHERE BATCH_ID=? AND TABLE_NAME='RSS_TV_EPISODES'`, j.ID).Scan(&count)
	if count != 1 {
		t.Fatal("original auxiliary episode ledger not archived")
	}
}
func TestFuzzySubscriptionUncertainSubmissionNeverReplaysAndPositiveVerificationDoesNotAutoContinue(t *testing.T) {
	f := newFuzzyFixture(t, false)
	fuzzyGrantForTest(t, f)
	j := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
	f.addError = true
	if err := f.runner.search.fuzzy.run(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	out := fuzzyGetForTest(t, f, j.ID)
	if out.State != "uncertain" || out.Items[0].State != "uncertain" || f.adds != 1 {
		t.Fatal(out, f.adds)
	}
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil || f.adds != 1 {
		t.Fatal(err, f.adds)
	}
	f.addError = false
	r := fuzzyHTTP(t, f, "POST", fuzzyEndpoint+"/jobs/"+j.ID+"/verify", fuzzyRequest{Revision: out.Revision, Index: 0, ConfirmRead: true, Confirm: true})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	out = fuzzyGetForTest(t, f, j.ID)
	if out.Items[0].State != "accepted" || out.State != "uncertain" || f.adds != 1 {
		t.Fatal("verification implicitly resumed", out, f.adds)
	}
	fuzzyContinueForTest(t, f, out)
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if out := fuzzyGetForTest(t, f, j.ID); out.SourceResult != "finished" || f.adds != 2 {
		t.Fatal(out, f.adds)
	}
}
func TestFuzzySubscriptionChangedConfigurationAndNewGrantCannotTransferOldBatch(t *testing.T) {
	for _, change := range []string{"rule", "site", "downloader", "password"} {
		t.Run(change, func(t *testing.T) {
			f := newFuzzyFixture(t, false)
			fuzzyGrantForTest(t, f)
			j := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
			switch change {
			case "rule":
				_, _ = f.db.Exec(`UPDATE RSS_MOVIES SET FILTER_INCLUDE='Other' WHERE ID=1`)
			case "site":
				_, _ = f.db.Exec(`UPDATE CONFIG_SITE SET COOKIE='new-private-site'`)
			case "downloader":
				_, _ = f.db.Exec(`UPDATE DOWNLOADER SET CONFIG=replace(CONFIG,'qb.local','new-peer.local')`)
			case "password":
				if err := config.NewStore(f.path).Update(map[string]any{"app.login_password": "new-native-password"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.runner.search.fuzzy.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if out := fuzzyGetForTest(t, f, j.ID); out.State != "blocked" || f.adds != 0 || f.fetches.Load() != 2 {
				t.Fatal(out, f.adds, f.fetches.Load())
			}
			if change == "password" {
				fuzzyGrantForTest(t, f)
				if err := f.runner.search.fuzzy.tick(t.Context()); err != nil || f.adds != 0 {
					t.Fatal("new grant implicitly adopted old batch", err, f.adds)
				}
				fuzzyContinueForTest(t, f, fuzzyGetForTest(t, f, j.ID))
				if err := f.runner.search.fuzzy.tick(t.Context()); err != nil || f.adds != 2 {
					t.Fatal(err, f.adds)
				}
			}
		})
	}
}
func TestFuzzySubscriptionIgnoredBoundaryWritesNeverFakeSuccessOrRepeatAcceptedTargets(t *testing.T) {
	for _, point := range []string{"intent", "accepted", "completion"} {
		t.Run(point, func(t *testing.T) {
			f := newFuzzyFixture(t, false)
			fuzzyGrantForTest(t, f)
			j := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
			if point == "intent" {
				tx, err := f.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := ensureSubscriptionSubmissionSchema(t.Context(), tx); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			statements := map[string]string{
				"intent":     `CREATE TRIGGER reject_fuzzy BEFORE UPDATE ON GO_SUBSCRIPTION_SUBMISSION_PROOFS BEGIN SELECT RAISE(IGNORE);END`,
				"accepted":   `CREATE TRIGGER reject_fuzzy BEFORE UPDATE ON GO_FUZZY_SUBSCRIPTION_ITEMS WHEN NEW.STATE='accepted' BEGIN SELECT RAISE(IGNORE);END`,
				"completion": `CREATE TRIGGER reject_fuzzy BEFORE DELETE ON RSS_MOVIES BEGIN SELECT RAISE(IGNORE);END`,
			}
			if _, err := f.db.Exec(statements[point]); err != nil {
				t.Fatal(err)
			}
			_ = f.runner.search.fuzzy.run(t.Context(), j.ID)
			out := fuzzyGetForTest(t, f, j.ID)
			var originals, histories int
			f.db.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES`).Scan(&originals)
			var exists bool
			f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='RSS_HISTORY')`).Scan(&exists)
			if exists {
				f.db.QueryRow(`SELECT COUNT(*) FROM RSS_HISTORY`).Scan(&histories)
			}
			if originals != 1 || histories != 0 || out.State == "completed" {
				t.Fatal("partial transaction reported success", out, originals, histories)
			}
			expected := map[string]int{"intent": 0, "accepted": 1, "completion": 2}[point]
			if f.adds != expected {
				t.Fatal(point, f.adds)
			}
			if err := f.runner.search.fuzzy.tick(t.Context()); err != nil || f.adds != expected {
				t.Fatal("interrupted work replayed", err, f.adds)
			}
			if _, err := f.db.Exec(`DROP TRIGGER reject_fuzzy`); err != nil {
				t.Fatal(err)
			}
			if point == "accepted" {
				if err := f.runner.search.fuzzy.verify(t.Context(), j.ID, 0); err != nil {
					t.Fatal(err)
				}
				out = fuzzyGetForTest(t, f, j.ID)
			}
			fuzzyContinueForTest(t, f, out)
			if err := f.runner.search.fuzzy.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if out := fuzzyGetForTest(t, f, j.ID); out.SourceResult != "finished" || f.adds != 2 {
				t.Fatal(out, f.adds)
			}
		})
	}
}
func TestFuzzySubscriptionStopKeepsReceiptsAndPausesMatchingRule(t *testing.T) {
	f := newFuzzyFixture(t, false)
	fuzzyGrantForTest(t, f)
	j := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
	f.afterAdd = func() {
		r := fuzzyHTTP(t, f, "POST", fuzzyEndpoint+"/jobs/"+j.ID+"/stop", fuzzyRequest{Confirm: true})
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	_ = f.runner.search.fuzzy.run(t.Context(), j.ID)
	out := fuzzyGetForTest(t, f, j.ID)
	if out.State != "stopped" || f.adds != 1 {
		t.Fatal(out, f.adds)
	}
	source := recoveryData[FuzzySourceForTest](t, fuzzyHTTP(t, f, "GET", fuzzySourcePath(f), nil))
	if source.Review.Active {
		t.Fatal("stopped rule can silently re-propose unsent targets")
	}
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil || f.adds != 1 {
		t.Fatal(err, f.adds)
	}
}
func TestFuzzySubscriptionRestorePreservesTypedUnknownFieldsAndDoesNotRestoreAuthority(t *testing.T) {
	f := newFuzzyFixture(t, true)
	f.feed = `<rss><channel>` + subscriptionFeedItem("Show.S01E01.1080p.WEB-DL", fmt.Sprintf("magnet:?xt=urn:btih:%040x", 31), "", "") + `</channel></rss>`
	if _, err := f.db.Exec(`ALTER TABLE RSS_TVS ADD COLUMN EXTRA_REAL REAL;ALTER TABLE RSS_TVS ADD COLUMN EXTRA_BLOB BLOB;ALTER TABLE RSS_TVS ADD COLUMN EXTRA_NULL TEXT;UPDATE RSS_TVS SET EXTRA_REAL=7.0,EXTRA_BLOB=x'00ff00',EXTRA_NULL=NULL`); err != nil {
		t.Fatal(err)
	}
	fuzzyGrantForTest(t, f)
	j := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	out := fuzzyGetForTest(t, f, j.ID)
	r := fuzzyHTTP(t, f, "POST", fuzzyEndpoint+"/jobs/"+j.ID+"/restore", fuzzyRequest{Revision: out.Revision, RequestID: strings.Repeat("d", 32), Confirm: true})
	var restored struct {
		ID                  string
		ExecutionAuthorized bool
	}
	restored = recoveryData[struct {
		ID                  string
		ExecutionAuthorized bool
	}](t, r)
	if restored.ID == "" || restored.ExecutionAuthorized || f.adds != 1 {
		t.Fatal(restored, f.adds)
	}
	var realKind, blobKind, blobValue, nullKind string
	if err := f.db.QueryRow(`SELECT typeof(EXTRA_REAL),typeof(EXTRA_BLOB),hex(EXTRA_BLOB),typeof(EXTRA_NULL) FROM RSS_TVS WHERE ID=?`, restored.ID).Scan(&realKind, &blobKind, &blobValue, &nullKind); err != nil {
		t.Fatal(err)
	}
	if realKind != "real" || blobKind != "blob" || blobValue != "00FF00" || nullKind != "null" {
		t.Fatal(realKind, blobKind, blobValue, nullKind)
	}
	var grantCount int
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_FUZZY_SUBSCRIPTION_GRANTS WHERE KIND='TV' AND SUB_ID=?`, restored.ID).Scan(&grantCount)
	if grantCount != 0 {
		t.Fatal("restored old authority")
	}
	r = fuzzyHTTP(t, f, "POST", fuzzyEndpoint+"/jobs/"+j.ID+"/restore", fuzzyRequest{Revision: out.Revision, RequestID: strings.Repeat("d", 32), Confirm: true})
	if r.Code != 200 {
		t.Fatal("stable restore did not return original receipt", r.Code, r.Body.String())
	}
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil || f.adds != 1 {
		t.Fatal(err, f.adds)
	}
}
func TestFuzzySubscriptionWashCombinationAdvancesOnlyByOriginalWholeMediaPriority(t *testing.T) {
	for _, tv := range []bool{false, true} {
		t.Run(fmt.Sprintf("tv=%v", tv), func(t *testing.T) {
			f := newFuzzyFixture(t, tv)
			if _, err := f.db.Exec(`INSERT INTO CONFIG_FILTER_GROUP(ID,GROUP_NAME,IS_DEFAULT) VALUES(777,'Wash','N');INSERT INTO CONFIG_FILTER_RULES(ID,GROUP_ID,ROLE_NAME,PRIORITY,INCLUDE) VALUES(7771,'777','best','1','1080p'),(7772,'777','fallback','2','720p');UPDATE ` + subscriptionTable(f.kind) + ` SET FILTER_RULE=777,OVER_EDITION=1`); err != nil {
				t.Fatal(err)
			}
			title := "Movie.2026.720p.WEB-DL"
			if tv {
				title = "Show.S01E01.720p.WEB-DL"
			}
			f.feed = `<rss><channel>` + subscriptionFeedItem(title, fmt.Sprintf("magnet:?xt=urn:btih:%040x", 41), "", "") + `</channel></rss>`
			fuzzyGrantForTest(t, f)
			j := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
			if err := f.runner.search.fuzzy.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			out := fuzzyGetForTest(t, f, j.ID)
			if out.State != "completed" || out.SourceResult != "retained" || f.adds != 1 {
				t.Fatal(out, f.adds)
			}
			var order sqlNullIntForFuzzy
			if err := f.db.QueryRow("SELECT FILTER_ORDER FROM " + subscriptionTable(f.kind)).Scan(&order); err != nil {
				t.Fatal(err)
			}
			if tv && order.Valid || !tv && (!order.Valid || order.Value != 98) {
				t.Fatal("episodic wash or priority semantics changed", order)
			}
			if !tv {
				f.feed = `<rss><channel>` + subscriptionFeedItem("Movie.2026.1080p.WEB-DL", fmt.Sprintf("magnet:?xt=urn:btih:%040x", 42), "", "") + `</channel></rss>`
				source := recoveryData[FuzzySourceForTest](t, fuzzyHTTP(t, f, "GET", fuzzySourcePath(f), nil))
				if !source.Review.Active {
					t.Fatal("native priority progress revoked its own scoped grant")
				}
				next := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
				if err := f.runner.search.fuzzy.tick(t.Context()); err != nil {
					t.Fatal(err)
				}
				if out := fuzzyGetForTest(t, f, next.ID); out.SourceResult != "finished" || f.adds != 2 {
					t.Fatal(out, f.adds)
				}
			}
		})
	}
}

type sqlNullIntForFuzzy struct {
	Value int64
	Valid bool
}

func (v *sqlNullIntForFuzzy) Scan(raw any) error {
	if raw == nil {
		v.Valid = false
		return nil
	}
	v.Valid = true
	switch x := raw.(type) {
	case int64:
		v.Value = x
	default:
		return fmt.Errorf("unexpected priority type %T", raw)
	}
	return nil
}
func TestFuzzySubscriptionMatcherRegexYearSeasonAndCandidateDedup(t *testing.T) {
	ctx := t.Context()
	m, err := newFuzzyMatcher(map[string]any{"NAME": `(?P<n>Movie|Other)`, "YEAR": "2026", "FUZZY_MATCH": 1, "FILTER_RULE": -1}, "MOV", "seeder", nil)
	if err != nil {
		t.Fatal(err)
	}
	site, _ := normalizeFuzzySite(siteconfig.Site{ID: 1, Name: "Tracker"})
	seed1, seed9 := int64(1), int64(9)
	resources := []identifiedSubscriptionResource{}
	for i, title := range []string{"Movie.2026.1080p.WEB-DL", "Movie.2026.720p.WEB-DL", "Other.2026.1080p.WEB-DL", "Movie.2025.1080p.WEB-DL", "Show.S01E01.WEB-DL"} {
		meta, err := mediameta.ParseWithOptions(ctx, title, "", mediameta.LabelOptions{})
		if err != nil {
			t.Fatal(err)
		}
		r := identifiedSubscriptionResource{meta: meta, revised: title, site: &site, siteOrder: 1}
		r.resource.Title, r.resource.Indexer, r.resource.DownloadURL = title, "Tracker", fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1)
		r.resource.Seeders = &seed1
		if i == 1 {
			r.resource.Seeders = &seed9
		}
		resources = append(resources, r)
	}
	got, err := m.selectCandidates(ctx, resources)
	if err != nil || len(got) != 2 {
		t.Fatal(got, err)
	}
	for _, v := range got {
		if v.View.Name == "Movie" && !strings.Contains(v.View.Title, "720p") {
			t.Fatal("seeder ordering lost", got)
		}
	}
	if _, err := newFuzzyMatcher(map[string]any{"NAME": "[", "FUZZY_MATCH": 1}, "MOV", "", nil); err == nil {
		t.Fatal("invalid regex silently became literal")
	}
}
func TestFuzzySubscriptionNativeNamespaceCannotFallBackToPython(t *testing.T) {
	f := newFuzzyFixture(t, false)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("native fuzzy namespace reached Python: %s", r.URL.Path)
	}))
	defer fallback.Close()
	handler, err := newHandler(config.Config{ApplicationConfigPath: f.path, LegacyBackendURL: fallback.URL}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fuzzyEndpoint, fuzzySourcePath(f), fuzzySourcePath(f) + "/preview", fuzzyEndpoint + "/jobs/" + strings.Repeat("a", 64), fuzzyEndpoint + "/unsupported/deeper/action"} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			r := nativeJSONRequest(handler, method, path, f.token, `{}`)
			if r.Code != 501 {
				t.Fatal(method, path, r.Code, r.Body.String())
			}
		}
	}
}
func TestFuzzySubscriptionRuntimeWorkerOnlyTakesFreshPendingBatches(t *testing.T) {
	f := newFuzzyFixture(t, false)
	fuzzyGrantForTest(t, f)
	j := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
	if f.runner.search.fuzzy.workers.Load() != 0 || f.adds != 0 {
		t.Fatal("handler-only constructor dispatched")
	}
	ctx, cancel := context.WithCancel(t.Context())
	wait := startFuzzySubscriptionWorker(ctx, f.runner.search.fuzzy)
	deadline := time.Now().Add(14 * time.Second)
	var completed bool
	for time.Now().Before(deadline) {
		v, err := f.runner.search.fuzzy.get(t.Context(), j.ID)
		if err != nil {
			cancel()
			wait()
			t.Fatal(err)
		}
		if v.State == "completed" {
			completed = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	wait()
	if !completed || f.adds != 2 || f.runner.search.fuzzy.workers.Load() != 0 {
		t.Fatal(completed, f.adds)
	}
}

func TestFuzzySubscriptionRootOnlyStaleReviewAndNoImplicitNetworkAuthority(t *testing.T) {
	f := newFuzzyFixture(t, false)
	viewer, err := f.runner.download.service.auth.service.IssueToken("viewer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "invalid", viewer} {
		for _, path := range []string{fuzzyEndpoint, fuzzySourcePath(f), fuzzySourcePath(f) + "/preview", fuzzyEndpoint + "/jobs/" + strings.Repeat("a", 64) + "/restore"} {
			method := "POST"
			if path == fuzzyEndpoint || path == fuzzySourcePath(f) {
				method = "GET"
			}
			r := nativeJSONRequest(f.handler, method, path, token, `{}`)
			want := 401
			if token == viewer {
				want = 403
			}
			if r.Code != want {
				t.Fatal(path, r.Code, r.Body.String())
			}
		}
	}
	view := recoveryData[FuzzySourceForTest](t, fuzzyHTTP(t, f, "GET", fuzzySourcePath(f), nil))
	if _, err := f.db.Exec(`UPDATE RSS_MOVIES SET FILTER_INCLUDE='Other'`); err != nil {
		t.Fatal(err)
	}
	r := fuzzyHTTP(t, f, "POST", fuzzySourcePath(f)+"/authorize", fuzzyRequest{Fingerprint: view.Review.Fingerprint, Confirm: true, ConfirmWhole: true, ConfirmOldExecutorsStopped: true})
	if r.Code != 409 || f.fetches.Load() != 0 || f.adds != 0 {
		t.Fatal(r.Code, r.Body.String(), f.fetches.Load(), f.adds)
	}
	if r := fuzzyHTTP(t, f, "POST", fuzzySourcePath(f)+"/preview", fuzzyRequest{ConfirmRead: true}); r.Code != 409 || f.fetches.Load() != 0 {
		t.Fatal("read consent became execution authority", r.Code, r.Body.String())
	}
}
func TestFuzzySubscriptionPositiveVerificationNeverUsesAbsentEvidenceOrEditedDownloader(t *testing.T) {
	for _, change := range []string{"absent", "peer"} {
		t.Run(change, func(t *testing.T) {
			f := newFuzzyFixture(t, false)
			fuzzyGrantForTest(t, f)
			j := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
			f.addError = true
			if err := f.runner.search.fuzzy.run(t.Context(), j.ID); err != nil {
				t.Fatal(err)
			}
			out := fuzzyGetForTest(t, f, j.ID)
			if change == "peer" {
				if _, err := f.db.Exec(`UPDATE DOWNLOADER SET CONFIG=replace(CONFIG,'qb.local','not-the-original.local')`); err != nil {
					t.Fatal(err)
				}
			} else {
				f.present = map[string]bool{}
			}
			r := fuzzyHTTP(t, f, "POST", fuzzyEndpoint+"/jobs/"+j.ID+"/verify", fuzzyRequest{Revision: out.Revision, Index: 0, ConfirmRead: true, Confirm: true})
			if r.Code != 409 || f.adds != 1 {
				t.Fatal(r.Code, r.Body.String(), f.adds)
			}
			out = fuzzyGetForTest(t, f, j.ID)
			var claims int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE STATE='pending'`).Scan(&claims); err != nil || claims != 1 || out.Items[0].State != "uncertain" {
				t.Fatal(out, claims, err)
			}
		})
	}
}
func TestFuzzySubscriptionDefinitiveRejectionRequiresExplicitContinuationAndKeepsAttempts(t *testing.T) {
	f := newFuzzyFixture(t, false)
	fuzzyGrantForTest(t, f)
	j := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
	f.loginFailure = true
	if err := f.runner.search.fuzzy.run(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	out := fuzzyGetForTest(t, f, j.ID)
	if out.State != "blocked" || out.Items[0].State != "rejected" || f.adds != 0 {
		t.Fatal(out, f.adds)
	}
	var claims int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS`).Scan(&claims); err != nil || claims != 0 {
		t.Fatal(claims, err)
	}
	f.loginFailure = false
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil || f.adds != 0 {
		t.Fatal("rejected item auto-retried", err, f.adds)
	}
	fuzzyContinueForTest(t, f, out)
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var attempts, rejected int
	f.db.QueryRow(`SELECT COUNT(*),SUM(CASE WHEN STATE='rejected' THEN 1 ELSE 0 END) FROM GO_FUZZY_ITEM_ATTEMPTS WHERE BATCH_ID=?`, j.ID).Scan(&attempts, &rejected)
	if out := fuzzyGetForTest(t, f, j.ID); out.SourceResult != "finished" || f.adds != 2 || attempts != 3 || rejected != 1 {
		t.Fatal(out, f.adds, attempts, rejected)
	}
}
func TestFuzzySubscriptionRSSRunOnlyQueuesAuthorizedWholeBatchWithoutInlineAdd(t *testing.T) {
	f := newFuzzyFixture(t, false)
	fuzzyGrantForTest(t, f)
	for i := range 2 {
		r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
		if r.Code != 200 || !strings.Contains(r.Body.String(), `"queued":1`) || f.adds != 0 || f.fetches.Load() != 1 {
			t.Fatal(i, r.Code, r.Body.String(), f.fetches.Load(), f.adds)
		}
	}
	jobs, err := f.runner.search.fuzzy.list(t.Context(), "MOV", 1, false)
	if err != nil || len(jobs) != 1 || len(jobs[0].Items) != 2 {
		t.Fatal(jobs, err)
	}
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil || f.adds != 2 {
		t.Fatal(err, f.adds)
	}
}
func TestFuzzySubscriptionDeletedAndIdenticallyReusedIDDoesNotInheritOldGrant(t *testing.T) {
	f := newFuzzyFixture(t, false)
	fuzzyGrantForTest(t, f)
	if _, err := f.db.Exec(`CREATE TABLE SAVED_TEST_RULE AS SELECT * FROM RSS_MOVIES WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	r := performFormRequest(f.handler, "/api/v1/subscriptions/MOV/1/remove", f.token, url.Values{})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if _, err := f.db.Exec(`INSERT INTO RSS_MOVIES SELECT * FROM SAVED_TEST_RULE`); err != nil {
		t.Fatal(err)
	}
	view := recoveryData[FuzzySourceForTest](t, fuzzyHTTP(t, f, "GET", fuzzySourcePath(f), nil))
	if view.Review.Active {
		t.Fatal("old authority transferred through identical row ID reuse")
	}
	if r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{}); r.Code != 200 || f.fetches.Load() != 0 || f.adds != 0 {
		t.Fatal(r.Code, r.Body.String(), f.adds)
	}
}
func TestFuzzySubscriptionHistoryRedoRestoresOriginalRegexNotTMDBAndKeepsAuditHistory(t *testing.T) {
	f := newFuzzyFixture(t, false)
	fuzzyGrantForTest(t, f)
	j := fuzzySaveForTest(t, f, fuzzyReviewForTest(t, f))
	if err := f.runner.search.fuzzy.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var historyID int64
	if err := f.db.QueryRow(`SELECT ID FROM RSS_HISTORY`).Scan(&historyID); err != nil {
		t.Fatal(err)
	}
	requestID := strings.Repeat("a", 32)
	var restored string
	for range 2 {
		r := performFormRequest(f.handler, "/api/v1/subscriptions/history/MOV/"+text(historyID)+"/redo", f.token, url.Values{"request_id": {requestID}})
		out := recoveryData[map[string]string](t, r)
		if restored != "" && out["id"] != restored {
			t.Fatal("same restore request did not return original receipt", out)
		}
		restored = out["id"]
	}
	var name string
	var fuzzy, histories int
	if err := f.db.QueryRow(`SELECT NAME,FUZZY_MATCH FROM RSS_MOVIES WHERE ID=?`, restored).Scan(&name, &fuzzy); err != nil || name != "Movie|Other" || fuzzy != 1 {
		t.Fatal(name, fuzzy, err)
	}
	f.db.QueryRow(`SELECT COUNT(*) FROM RSS_HISTORY`).Scan(&histories)
	if histories != 1 || f.adds != 2 || fuzzyGetForTest(t, f, j.ID).SourceResult != "finished" {
		t.Fatal(histories, f.adds)
	}
	if _, err := f.db.Exec(`UPDATE RSS_HISTORY SET NAME='Different history' WHERE ID=?`, historyID); err != nil {
		t.Fatal(err)
	}
	if batch, err := f.runner.search.fuzzy.service().fuzzyHistoryBatch(t.Context(), "MOV", historyID); err != nil || batch != "" {
		t.Fatal("reused/changed history selected old private archive", batch, err)
	}
}
func TestFuzzySubscriptionMatcherFixedSeasonLiteralFallbackAndWholeOverflow(t *testing.T) {
	site, err := normalizeFuzzySite(siteconfig.Site{ID: 1, Name: "Tracker"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Show", "Show$"} {
		matcher, err := newFuzzyMatcher(map[string]any{"NAME": name, "YEAR": "2026", "SEASON": "S02", "FUZZY_MATCH": 1, "FILTER_RULE": -1}, "TV", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, season := range []int{1, 2} {
			meta, err := mediameta.ParseWithOptions(t.Context(), fmt.Sprintf("Show.2026.S%02dE01.1080p.WEB-DL", season), "", mediameta.LabelOptions{})
			if err != nil {
				t.Fatal(err)
			}
			meta.Year = "2026"
			item := identifiedSubscriptionResource{meta: meta, revised: name + " 2026 S02E01", site: &site}
			matched, err := matcher.identity(t.Context(), item)
			if err != nil || matched != (season == 2) {
				t.Fatal(name, season, matched, err)
			}
		}
	}
	matcher, err := newFuzzyMatcher(map[string]any{"NAME": "Item", "FUZZY_MATCH": 1, "FILTER_RULE": -1}, "MOV", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resources := []identifiedSubscriptionResource{}
	for i := range fuzzyCandidateLimit + 1 {
		name := fmt.Sprintf("Item%03d", i)
		item := identifiedSubscriptionResource{meta: mediameta.Metadata{Title: name, Year: "2026"}, revised: name, site: &site}
		item.resource.Title, item.resource.Indexer, item.resource.DownloadURL = name, "Tracker", fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1)
		resources = append(resources, item)
	}
	if candidates, err := matcher.selectCandidates(t.Context(), resources); err == nil || len(candidates) != 0 {
		t.Fatal("overflow silently truncated whole batch", len(candidates), err)
	}
}

func TestFuzzySubscriptionPrivateCandidatesRetainLocatorsAndBindTheirFingerprints(t *testing.T) {
	c := fuzzyCandidateRecord{View: fuzzyCandidateView{Title: "Movie", Name: "Movie", Kind: "MOV", Site: "Tracker"}, SiteID: 1, SiteDigest: strings.Repeat("a", 64)}
	c.Resource.Title, c.Resource.Indexer, c.Resource.DownloadURL, c.Resource.DownloadResolver = "Movie", "Tracker", "https://tracker.test/private-download?passkey=private", "mteam"
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var restored fuzzyCandidateRecord
	if err := decodeFuzzyDocument(string(b), &restored, 3<<20); err != nil || restored.Resource.DownloadURL != c.Resource.DownloadURL || restored.Resource.DownloadResolver != c.Resource.DownloadResolver || fuzzyDigest(restored) != fuzzyDigest(c) {
		t.Fatal("private candidate lost its locator or resolver", err)
	}
	changed := c
	changed.Resource.DownloadURL += "-changed"
	if fuzzyDigest(changed) == fuzzyDigest(c) {
		t.Fatal("changed private URL did not invalidate review fingerprint")
	}
	public, err := json.Marshal(c.View)
	if err != nil || strings.Contains(string(public), "private") || strings.Contains(string(public), "mteam") {
		t.Fatal("public candidate leaked private locator", err)
	}
}
