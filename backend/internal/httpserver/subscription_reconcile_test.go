package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func pendingSubmissionFixture(t *testing.T, tv bool, verify func(*http.Request) (*http.Response, error)) *subscriptionRunFixture {
	t.Helper()
	f := newSubscriptionRunFixture(t)
	kind, id := "MOV", "1"
	if tv {
		kind, id = "TV", "2"
		if _, err := f.db.Exec(`DELETE FROM RSS_MOVIES`); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := f.db.Exec(`DELETE FROM RSS_TVS`); err != nil {
			t.Fatal(err)
		}
	}
	f.addError = true
	if status, body := f.run(kind, id); status != 502 || f.adds != 1 || !strings.Contains(body, `"uncertain":true`) {
		t.Fatal(status, body, f.adds)
	}
	f.addError = false
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "qb.local" && r.URL.Path == "/api/v2/torrents/info" {
			if r.URL.Query().Get("filter") != "all" || r.URL.Query().Get("hashes") == "" {
				t.Fatal("verification used an incomplete or unscoped list", r.URL)
			}
			return verify(r)
		}
		return base.RoundTrip(r)
	})
	var runner *rssRunAPI
	var err error
	f.handler, err = buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	f.runner = runner
	return f
}

func verifyPresent(r *http.Request) (*http.Response, error) {
	return jsonResponse(r, `[{"hash":"`+r.URL.Query().Get("hashes")+`"}]`), nil
}

func (f *subscriptionRunFixture) reconcileRequest(kind, id string) (int, string) {
	r := performFormRequest(f.handler, "/api/v1/subscriptions/"+kind+"/"+id+"/reconcile", f.token, url.Values{})
	return r.Code, r.Body.String()
}

func TestSubscriptionReconcileSurvivesRestartAndRestoresMovieHistory(t *testing.T) {
	f := pendingSubmissionFixture(t, false, verifyPresent)
	request := nativeJSONRequest(f.handler, "GET", "/api/v1/subscriptions", f.token, "")
	if request.Code != 200 || !strings.Contains(request.Body.String(), `"pendingSubmission":true`) || strings.Contains(request.Body.String(), "private-downloader") || strings.Contains(request.Body.String(), "magnet:") {
		t.Fatal(request.Code, request.Body.String())
	}
	status, body := f.reconcileRequest("MOV", "1")
	if status != 200 || !strings.Contains(body, `"completed":true`) || !strings.Contains(body, `"uncertain":false`) || f.adds != 1 || f.queries != 1 || strings.Contains(body, "private") || strings.Contains(body, "magnet:") {
		t.Fatal(status, body, f.adds, f.queries)
	}
	for query, want := range map[string]int{`SELECT COUNT(*) FROM RSS_MOVIES`: 0, `SELECT COUNT(*) FROM RSS_HISTORY`: 1, `SELECT COUNT(*) FROM RSS_TORRENTS`: 1, `SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE STATE='pending'`: 0} {
		var n int
		if err := f.db.QueryRow(query).Scan(&n); err != nil || n != want {
			t.Fatal(query, n, err)
		}
	}
	if status, _ := f.reconcileRequest("MOV", "1"); status != 404 || f.adds != 1 {
		t.Fatal("repeated verification consumed progress", status, f.adds)
	}
}

func TestSubscriptionReconcileTVConsumesOnlyAcceptedEpisode(t *testing.T) {
	f := pendingSubmissionFixture(t, true, verifyPresent)
	status, body := f.reconcileRequest("TV", "2")
	if status != 200 || !strings.Contains(body, `"remaining":[3]`) || !strings.Contains(body, `"completed":false`) || f.adds != 1 {
		t.Fatal(status, body, f.adds)
	}
	var episodes string
	var lack int
	if err := f.db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='2'`).Scan(&episodes); err != nil || episodes != "3" {
		t.Fatal(episodes, err)
	}
	if err := f.db.QueryRow(`SELECT LACK FROM RSS_TVS WHERE ID=2`).Scan(&lack); err != nil || lack != 1 {
		t.Fatal(lack, err)
	}
	status, body = f.run("TV", "2")
	if status != 200 || f.adds != 2 || !strings.Contains(body, `"completed":true`) {
		t.Fatal("remaining episodes did not resume", status, body, f.adds)
	}
}

func TestSubscriptionReconcileAbsentOrUnverifiableTasksStayPending(t *testing.T) {
	for _, response := range []string{`[]`, `null`, `{}`, `[{"hash":"wrong"}]`, `[{"hash":"0123456789abcdef0123456789abcdef01234567"},{"hash":"0123456789abcdef0123456789abcdef01234567"}]`} {
		t.Run(response, func(t *testing.T) {
			f := pendingSubmissionFixture(t, false, func(r *http.Request) (*http.Response, error) { return jsonResponse(r, response), nil })
			status, body := f.reconcileRequest("MOV", "1")
			want := 502
			if response == `[]` {
				want = 409
			}
			if status != want || f.adds != 1 || !strings.Contains(body, `"uncertain":true`) {
				t.Fatal(status, body, f.adds)
			}
			var n int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE STATE='pending'`).Scan(&n); err != nil || n != 1 {
				t.Fatal(n, err)
			}
			if status, _ := f.run("MOV", "1"); status != 409 || f.adds != 1 {
				t.Fatal("missing task incorrectly allowed resubmission", status, f.adds)
			}
		})
	}
}

func TestSubscriptionReconcileRejectsEditsAndOldProofs(t *testing.T) {
	for _, change := range []string{`UPDATE RSS_MOVIES SET NAME='User edit'`, `DELETE FROM RSS_MOVIES`, `UPDATE DOWNLOADER SET ENABLED=0`, `UPDATE DOWNLOADER SET CONFIG='{}'`, `DELETE FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS`, `UPDATE GO_SUBSCRIPTION_SUBMISSION_PROOFS SET PAYLOAD='broken'`} {
		t.Run(change, func(t *testing.T) {
			var queried atomic.Int32
			f := pendingSubmissionFixture(t, false, func(r *http.Request) (*http.Response, error) { queried.Add(1); return verifyPresent(r) })
			if _, err := f.db.Exec(change); err != nil {
				t.Fatal(err)
			}
			status, body := f.reconcileRequest("MOV", "1")
			want := 409
			if strings.Contains(change, "PROOFS") {
				want = 501
			}
			if status != want || queried.Load() != 0 || f.adds != 1 {
				t.Fatal(status, body, queried.Load(), f.adds)
			}
			var n int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE STATE='pending'`).Scan(&n); err != nil || n != 1 {
				t.Fatal(n, err)
			}
		})
	}
}

func TestSubscriptionReconcileAcceptanceCommitRollbackRetainsEvidence(t *testing.T) {
	f := pendingSubmissionFixture(t, false, verifyPresent)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_reconcile BEFORE INSERT ON RSS_TORRENTS BEGIN SELECT RAISE(ABORT,'private-db-error'); END`); err != nil {
		t.Fatal(err)
	}
	status, body := f.reconcileRequest("MOV", "1")
	if status != 409 || f.adds != 1 || strings.Contains(body, "private-db-error") {
		t.Fatal(status, body)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE STATE='pending'`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_reconcile`); err != nil {
		t.Fatal(err)
	}
	if status, body := f.reconcileRequest("MOV", "1"); status != 200 || f.adds != 1 {
		t.Fatal(status, body, f.adds)
	}
}

func TestSubscriptionReconcileConcurrentStoresConsumeOnce(t *testing.T) {
	var queries atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	f := pendingSubmissionFixture(t, false, func(r *http.Request) (*http.Response, error) {
		if queries.Add(1) == 2 {
			close(started)
		}
		select {
		case <-release:
			return verifyPresent(r)
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	other, err := newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	codes := make(chan int, 2)
	for _, handler := range []http.Handler{f.handler, other} {
		group.Add(1)
		go func(handler http.Handler) {
			defer group.Done()
			r := performFormRequest(handler, "/api/v1/subscriptions/MOV/1/reconcile", f.token, url.Values{})
			codes <- r.Code
		}(handler)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		group.Wait()
		t.Fatal("both instances did not verify")
	}
	close(release)
	group.Wait()
	close(codes)
	var successes int
	for code := range codes {
		if code == 200 {
			successes++
		} else if code != 409 {
			t.Fatal(code)
		}
	}
	if successes != 1 || f.adds != 1 {
		t.Fatal(successes, f.adds)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_HISTORY`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestSubscriptionReconcilePermissionsAndCancellation(t *testing.T) {
	started := make(chan struct{})
	f := pendingSubmissionFixture(t, false, func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	path := "/api/v1/subscriptions/MOV/1/reconcile"
	if r := performFormRequest(f.handler, path, "", url.Values{}); r.Code != 401 {
		t.Fatal(r.Code)
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	if r := performFormRequest(f.handler, path, loginForTest(t, f.handler, "viewer", "strong-password"), url.Values{}); r.Code != 403 {
		t.Fatal(r.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, failure := f.runner.search.reconcile(ctx, "MOV", 1)
		if failure == nil || failure.status != 504 {
			t.Error(failure)
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("verification did not start")
	}
	if status, _ := f.reconcileRequest("MOV", "1"); status != 409 {
		t.Fatal("manual gate was not shared", status)
	}
	cancel()
	waitSubscriptionWorker(t, done)
	if f.adds != 1 {
		t.Fatal("verification resubmitted", f.adds)
	}
}

func TestSubscriptionReconcileGuardPreventsDownloaderEditAtCommit(t *testing.T) {
	f := pendingSubmissionFixture(t, false, verifyPresent)
	var payload, owner string
	if err := f.db.QueryRow(`SELECT p.PAYLOAD,p.OWNER FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS p`).Scan(&payload, &owner); err != nil {
		t.Fatal(err)
	}
	proof, plan, err := decodeSubscriptionProof(payload)
	if err != nil {
		t.Fatal(err)
	}
	candidate := subscriptionCandidate{Title: proof.Title, Episodes: proof.Episodes}
	candidate.resource.DownloadURL = proof.DownloadURL
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET CONFIG='{}'`); err != nil {
		t.Fatal(err)
	}
	service := f.runner.recognition.service
	if err := service.persistSubscriptionSubmission(t.Context(), "MOV", 1, &plan, &candidate, owner, subscriptionReconcileGuard{proof, payload}); err == nil {
		t.Fatal("changed downloader bypassed transactional guard")
	}
	if proof, _, err := decodeSubscriptionProof(payload + ` {}`); err == nil {
		t.Fatal(proof, "proof suffix accepted")
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(payload), &decoded) != nil {
		t.Fatal("invalid proof encoding")
	}
	if strings.Contains(payload, "private-downloader") {
		t.Fatal("proof copied downloader credentials")
	}
	if got := fmt.Sprint(plan.raw["ID"]); got != "1" {
		t.Fatal("SQL numeric identity was not preserved", got)
	}
}

func TestSubscriptionReconcileRejectsChangesDuringVerification(t *testing.T) {
	for _, change := range []string{
		`UPDATE RSS_MOVIES SET NAME='Edited during verification'`,
		`UPDATE DOWNLOADER SET CONFIG='{}'`,
		`UPDATE GO_SUBSCRIPTION_SUBMISSION_PROOFS SET PAYLOAD=PAYLOAD || ' '`,
	} {
		t.Run(change, func(t *testing.T) {
			var f *subscriptionRunFixture
			f = pendingSubmissionFixture(t, false, func(r *http.Request) (*http.Response, error) {
				if _, err := f.db.Exec(change); err != nil {
					t.Fatal(err)
				}
				return verifyPresent(r)
			})
			status, body := f.reconcileRequest("MOV", "1")
			if status != 409 || f.adds != 1 {
				t.Fatal(status, body, f.adds)
			}
			var pending int
			var historyTable bool
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS WHERE STATE='pending'`).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='RSS_HISTORY')`).Scan(&historyTable); err != nil || pending != 1 || historyTable {
				t.Fatal(pending, historyTable, err)
			}
		})
	}
}

func TestSubscriptionProofFailurePreventsSubmission(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	for _, statement := range []string{
		`CREATE TABLE GO_SUBSCRIPTION_SUBMISSION_PROOFS (RESOURCE_KEY TEXT PRIMARY KEY,OWNER TEXT NOT NULL,PAYLOAD TEXT NOT NULL)`,
		`CREATE TRIGGER reject_proof BEFORE INSERT ON GO_SUBSCRIPTION_SUBMISSION_PROOFS BEGIN SELECT RAISE(ABORT,'private-proof-error'); END`,
	} {
		if _, err := f.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	status, body := f.run("MOV", "1")
	if status != 409 || f.adds != 0 || strings.Contains(body, "private-proof-error") {
		t.Fatal(status, body, f.adds)
	}
	var historyTable bool
	if err := f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='RSS_HISTORY')`).Scan(&historyTable); err != nil || historyTable {
		t.Fatal(historyTable, err)
	}
	// Schema and reservation inserts belong to the same rolled-back transaction.
	var claimsTable bool
	if err := f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_SUBSCRIPTION_DOWNLOAD_CLAIMS')`).Scan(&claimsTable); err != nil || claimsTable {
		t.Fatal(claimsTable, err)
	}
}
