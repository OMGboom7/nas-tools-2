package httpserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

const recoveryEndpoint = "/api/v1/organization/recovery-objects"

type recoveryHTTP struct {
	f                      *libraryHTTPFixture
	remote                 *remoteLibraryHTTP
	root, driver, selected string
}

func recoveryHTTPFixture(t *testing.T, driver string) *recoveryHTTP {
	t.Helper()
	x := &recoveryHTTP{driver: driver, selected: ".nastool-copy-http-orphan.tmp"}
	if driver == "" {
		x.f = newLibraryHTTPFixture(t, false)
		x.root = x.f.root
	} else {
		x.remote = newRemoteLibraryHTTP(t, driver, false)
		x.f = x.remote.base
		x.root = x.remote.logical
		x.selected = ".nastool-remote/" + strings.Repeat("a", 32)
	}
	x.put(t, path.Join(x.selected, "payload/BDMV/index.bdmv"), "index")
	x.put(t, path.Join(x.selected, "payload/BDMV/STREAM/00001.m2ts"), "full disc")
	x.put(t, path.Join(x.selected, "unknown.txt"), "explicitly reviewed unknown")
	x.put(t, "published/keep.mkv", "published")
	return x
}
func (x *recoveryHTTP) put(t *testing.T, name, value string) {
	t.Helper()
	if x.remote != nil {
		x.remote.put(t, name, []byte(value))
	} else {
		libraryWriteHTTP(t, x.root, name, value)
	}
}
func libraryWriteHTTP(t *testing.T, root, name, value string) {
	t.Helper()
	file := path.Join(root, name)
	if err := os.MkdirAll(path.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func (x *recoveryHTTP) read(t *testing.T, name string) string {
	t.Helper()
	if x.remote != nil {
		data, yes := x.remote.read(t, name)
		if !yes {
			t.Fatal("missing", name)
		}
		return string(data)
	}
	data, err := os.ReadFile(path.Join(x.root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func (x *recoveryHTTP) input() map[string]any {
	return map[string]any{"rootId": x.f.input.RootID, "driver": x.driver, "path": x.selected, "requestId": strings.Repeat("b", 32), "decision": "independent cleanup of the complete obsolete recovery copy", "confirmRead": true}
}
func (x *recoveryHTTP) request(t *testing.T, method, url string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	return nativeJSONRequest(x.f.f.handler, method, recoveryEndpoint+url, x.f.f.token, raw)
}
func recoveryData[T any](t *testing.T, r *httptest.ResponseRecorder) T {
	t.Helper()
	var response struct{ Data T }
	if r.Code < 200 || r.Code >= 300 || json.Unmarshal(r.Body.Bytes(), &response) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return response.Data
}
func recoveryGrant() map[string]any {
	return map[string]any{"confirmRead": true, "confirm": true, "confirmPermanentDeletion": true, "confirmOnlyCopyRisk": true, "confirmOldExecutorsStopped": true, "confirmCloseRelated": true, "confirmRemoteExclusive": true}
}
func mergeRecovery(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
func (x *recoveryHTTP) create(t *testing.T) organization.RecoveryDisposal {
	t.Helper()
	preview := recoveryData[organization.RecoveryObjectReview](t, x.request(t, "POST", "/preview", x.input()))
	body := mergeRecovery(x.input(), recoveryGrant())
	body["requestId"], body["fingerprint"] = preview.RequestID, preview.Fingerprint
	return recoveryData[organization.RecoveryDisposal](t, x.request(t, "POST", "/disposals", body))
}
func (x *recoveryHTTP) review(t *testing.T, id string) organization.RecoveryDisposalReview {
	t.Helper()
	return recoveryData[organization.RecoveryDisposalReview](t, x.request(t, "GET", "/disposals/"+id, nil))
}

func TestRecoveryObjectsHTTPCompleteIndependentDisposalAcrossAllProvidersAndRestartNeverRunsIt(t *testing.T) {
	for _, driver := range []string{"", "minio", "rclone"} {
		t.Run(driver, func(t *testing.T) {
			x := recoveryHTTPFixture(t, driver)
			browse := recoveryData[struct {
				Entries []organization.RecoveryBrowseEntry
			}](t, x.request(t, "POST", "/browse", map[string]any{"rootId": x.f.input.RootID, "driver": driver, "path": "."}))
			if len(browse.Entries) == 0 {
				t.Fatal("empty browse")
			}
			created := x.create(t)
			if created.State != "ready" || x.read(t, path.Join(x.selected, "unknown.txt")) != "explicitly reviewed unknown" {
				t.Fatal("creation ran deletion")
			}
			if x.remote != nil {
				x.remote.restart(t)
			} else {
				var err error
				x.f.f.handler, err = buildHandler(config.Config{ApplicationConfigPath: x.f.f.path, DisableLegacy: true}, x.f.f.transport, &x.f.f.runner)
				if err != nil {
					t.Fatal(err)
				}
			}
			if x.read(t, path.Join(x.selected, "unknown.txt")) != "explicitly reviewed unknown" {
				t.Fatal("restart replayed a ready operation")
			}
			v := x.review(t, created.ID)
			if !v.CanContinue {
				t.Fatal(v.Evidence)
			}
			body := mergeRecovery(recoveryGrant(), map[string]any{"fingerprint": v.Fingerprint})
			for _, key := range []string{"confirmPermanentDeletion", "confirmOnlyCopyRisk", "confirmOldExecutorsStopped", "confirmCloseRelated"} {
				missing := mergeRecovery(body, map[string]any{key: false})
				r := x.request(t, "POST", "/disposals/"+created.ID+"/continue", missing)
				if r.Code < 400 {
					t.Fatal("missing grant accepted", key, r.Code)
				}
			}
			if driver != "" {
				r := x.request(t, "POST", "/disposals/"+created.ID+"/continue", mergeRecovery(body, map[string]any{"confirmRemoteExclusive": false}))
				if r.Code < 400 {
					t.Fatal("remote grant inferred")
				}
			}
			done := recoveryData[organization.RecoveryDisposal](t, x.request(t, "POST", "/disposals/"+created.ID+"/continue", body))
			if done.State != "completed" || done.Done != done.Total {
				t.Fatal(done)
			}
			if x.read(t, "published/keep.mkv") != "published" {
				t.Fatal("published media altered")
			}
			count := 0
			if x.remote != nil && x.remote.cloud != nil {
				count = len(x.remote.cloud.Requests())
			}
			again := recoveryData[organization.RecoveryDisposal](t, x.request(t, "POST", "/disposals/"+created.ID+"/continue", body))
			if again.State != "completed" {
				t.Fatal("lost response rerequest did not just return receipt")
			}
			if x.remote != nil && x.remote.cloud != nil {
				for _, r := range x.remote.cloud.Requests()[count:] {
					if r.Method == "DELETE" || r.Method == "PUT" {
						t.Fatal("old fingerprint replayed storage mutation")
					}
				}
			}
			if x.remote != nil {
				x.remote.assertNoHistory(t)
			}
		})
	}
}
func TestRecoveryObjectsHTTPRejectsSourceTargetAndAccountExpansionBeforeAnyDeletion(t *testing.T) {
	x := recoveryHTTPFixture(t, "")
	for _, p := range []string{".", "../other", "published/keep.mkv", "/"} {
		in := x.input()
		in["path"] = p
		r := x.request(t, "POST", "/preview", in)
		if r.Code < 400 {
			t.Fatal("broad scope accepted", p)
		}
	}
	in := x.input()
	in["confirmRead"] = false
	if r := x.request(t, "POST", "/preview", in); r.Code < 400 {
		t.Fatal("full reading consent omitted")
	}
	in = x.input()
	in["nodes"] = []string{"untrusted injected manifest"}
	if r := x.request(t, "POST", "/preview", in); r.Code != 400 {
		t.Fatal("client snapshot adopted", r.Code)
	}
	in = x.input()
	in["driver"] = "unconfigured"
	if r := x.request(t, "POST", "/preview", in); r.Code < 400 {
		t.Fatal("unknown storage silently fell back")
	}
	created := x.create(t)
	x.put(t, path.Join(x.selected, "late.txt"), "late arrival")
	v := x.review(t, created.ID)
	if v.CanContinue {
		t.Fatal("new file absorbed into old scope")
	}
	if r := x.request(t, "POST", "/disposals/"+created.ID+"/continue", mergeRecovery(recoveryGrant(), map[string]any{"fingerprint": v.Fingerprint})); r.Code < 400 {
		t.Fatal("changed scope deleted")
	}
	if x.read(t, path.Join(x.selected, "late.txt")) != "late arrival" {
		t.Fatal("late file lost")
	}
}
func TestRecoveryObjectsHTTPStopCancelsAnInFlightFullRemoteReadWithoutDeletingOrReplaying(t *testing.T) {
	x := recoveryHTTPFixture(t, "minio")
	created := x.create(t)
	entered := make(chan struct{})
	x.remote.cloud.BeforeNextRead(func(ctx context.Context) { close(entered); <-ctx.Done() })
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- nativeJSONRequest(x.f.f.handler, "GET", recoveryEndpoint+"/disposals/"+created.ID, x.f.f.token, "")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("full object read not started")
	}
	stopped := recoveryData[organization.RecoveryDisposal](t, x.request(t, "POST", "/disposals/"+created.ID+"/close", map[string]any{"confirm": true, "confirmClose": true}))
	if stopped.State != "closed" {
		t.Fatal(stopped)
	}
	select {
	case response := <-done:
		if response.Code < 400 {
			t.Fatal("cancelled review looked complete", response.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not interrupt current read")
	}
	for _, request := range x.remote.cloud.Requests() {
		if request.Method == "DELETE" {
			t.Fatal("stopping a read deleted data")
		}
	}
	x.remote.restart(t)
	if x.read(t, path.Join(x.selected, "unknown.txt")) != "explicitly reviewed unknown" {
		t.Fatal("restart replayed stopped operation")
	}
	// A closed receipt remains readable even after account revocation.
	if err := os.WriteFile(x.remote.account, []byte("revoked"), 0600); err != nil {
		t.Fatal(err)
	}
	view := x.review(t, created.ID)
	if view.Disposal.State != "closed" || view.CanContinue || view.CanReturn {
		t.Fatal(view)
	}
}
func TestRecoveryObjectsHTTPReadonlyReviewHonorsExistingWriterGuardAndConfigRevocation(t *testing.T) {
	x := recoveryHTTPFixture(t, "")
	guard, err := x.f.f.runner.organizer.jobs.AcquireJob(t.Context(), "library-scraper-publication")
	if err != nil {
		t.Fatal(err)
	}
	r := x.request(t, "POST", "/preview", x.input())
	guard.Close()
	if r.Code != 409 {
		t.Fatal("busy writer ignored", r.Code, r.Body.String())
	}
	created := x.create(t)
	if err := config.NewStore(x.f.f.path).Update(map[string]any{"media.nfo_poster": true}); err != nil {
		t.Fatal(err)
	}
	if r := x.request(t, "GET", "/disposals/"+created.ID, nil); r.Code < 400 {
		t.Fatal("changed full config accepted")
	}
	stopped := recoveryData[organization.RecoveryDisposal](t, x.request(t, "POST", "/disposals/"+created.ID+"/close", map[string]any{"confirm": true, "confirmClose": true}))
	if stopped.State != "closed" {
		t.Fatal(stopped)
	}
}

func TestRecoveryObjectsHTTPDisposalClosesEveryLegacySiblingWithoutMarkingOriginalUnknownSuccessful(t *testing.T) {
	f, api, source, _ := mixedLegacyFixture(t, "copy", true)
	batch := mixedLegacyReserve(t, api)
	first, err := api.jobs.Get(t.Context(), batch.Jobs[0])
	if err != nil {
		t.Fatal(err)
	}
	selected := path.Join(path.Dir(first.Items[0].Target), organization.TempName(first.ID, 0))
	x := &recoveryHTTP{f: &libraryHTTPFixture{f: f, root: first.TargetRoot, input: libraryPlanInput{RootID: first.Definition.TargetID}}, root: first.TargetRoot, selected: selected}
	x.put(t, path.Join(selected, "unrecorded-payload"), "explicit independent scope, not an original proof")
	preview := recoveryData[organization.RecoveryObjectReview](t, x.request(t, "POST", "/preview", x.input()))
	if len(preview.Owners) != len(batch.Jobs) {
		t.Fatal("incomplete sibling scope", preview.Owners, batch.Jobs)
	}
	var before int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	created := x.create(t)
	v := x.review(t, created.ID)
	done := recoveryData[organization.RecoveryDisposal](t, x.request(t, "POST", "/disposals/"+created.ID+"/continue", mergeRecovery(recoveryGrant(), map[string]any{"fingerprint": v.Fingerprint})))
	if done.State != "completed" {
		t.Fatal(done)
	}
	for _, id := range batch.Jobs {
		j, err := api.jobs.Get(t.Context(), id)
		if err != nil || j.State != organization.RecoveryClosed {
			t.Fatal("sibling left executable", j, err)
		}
		for _, item := range j.Items {
			if _, err := os.Stat(path.Join(source, item.Source)); err != nil {
				t.Fatal("original source removed", err)
			}
		}
	}
	var after int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS`).Scan(&after); err != nil || after != before {
		t.Fatal("target reservations released", before, after, err)
	}
	if unknownState(t, f, 7) != "N" {
		t.Fatal("unknown row falsely marked successful")
	}
	latest, err := api.jobs.GetLegacyBatch(t.Context(), batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, closed, body := legacyBatchAction(t, f, latest, "close", nil)
	if code != 200 || closed.State != "closed" || unknownState(t, f, 7) != "N" {
		t.Fatal(code, closed, body)
	}
}
func TestRecoveryObjectsHTTPClosingDecidedLibraryBackupDoesNotRevokeAuthorizedFutureCron(t *testing.T) {
	f := newLibraryHTTPFixture(t, false)
	libraryWriteHTTP(t, f.root, "Movie/Movie.2026.mkv", "readonly media")
	libraryWriteHTTP(t, f.root, "Movie/Movie.2026.nfo", "old metadata")
	libraryPluginInstall(t, f)
	control := libraryPluginSave(t, f, libraryPluginValues(f, true, "0 0 1 1 *", "force_nfo"))
	now := control.OnceAt.Add(time.Second)
	libraryPluginTick(t, f, now)
	queued := libraryPluginControlRead(t, f)
	if queued.State != "queued" || len(queued.Jobs) != 1 {
		t.Fatal(queued)
	}
	if err := f.f.runner.library.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	outputs, err := f.f.runner.organizer.jobs.LibraryOutputs(t.Context(), queued.Jobs[0], 1)
	if err != nil {
		t.Fatal(err)
	}
	selected := ""
	for _, out := range outputs {
		if out.State == "replaced" && out.Proof.Old.Identity != "" {
			selected = path.Join(path.Dir(out.Target), out.Proof.Stage)
			break
		}
	}
	if selected == "" {
		t.Fatal("missing complete local metadata backup")
	}
	x := &recoveryHTTP{f: f, root: f.root, selected: selected}
	created := x.create(t)
	review := x.review(t, created.ID)
	done := recoveryData[organization.RecoveryDisposal](t, x.request(t, "POST", "/disposals/"+created.ID+"/continue", mergeRecovery(recoveryGrant(), map[string]any{"fingerprint": review.Fingerprint})))
	if done.State != "completed" {
		t.Fatal(done)
	}
	libraryPluginTick(t, f, now.Add(time.Second))
	current := libraryPluginControlRead(t, f)
	if !current.Authorized || current.State != "scheduled" || current.Next.IsZero() {
		t.Fatal("old job closure revoked unrelated future authority", current)
	}
	libraryPluginTick(t, f, current.Next.Add(time.Second))
	fresh := libraryPluginControlRead(t, f)
	if !fresh.Authorized || fresh.State != "queued" || fresh.Batch == queued.Batch || len(fresh.Jobs) != 1 || fresh.Jobs[0] == queued.Jobs[0] {
		t.Fatal("future authorized cycle did not get a new independent worklist", fresh)
	}
}
