package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

func bindVerificationPolicy(t *testing.T, f *manualDownloadFixture) downloaderconfig.SeedingPolicy {
	t.Helper()
	item := mustDownloader(t, f)
	seconds, ratio := int64(900), 1.5
	_, digest, err := f.store.SeedingPolicies(t.Context(), 1, f.hash)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.store.BindSeedingPolicy(t.Context(), downloaderconfig.SeedingPolicy{DownloaderID: 1, DownloaderName: item.Name, DownloaderType: item.Type, ConfigDigest: bus.Hash(item), InfoHash: f.hash, SourceDigest: bus.Hash("verification source"), Actor: "0:admin", Requirements: seeding.Requirements{Seconds: &seconds, Ratio: &ratio}}, digest)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func verificationInput(f *manualDownloadFixture) ptVerificationInput {
	return ptVerificationInput{DownloaderID: 1, TaskID: f.hash, Mode: "attest", Minutes: 15, Note: "已在站点账户逐项核实本任务全部义务完成", Actions: downloaderconfig.VerificationActions{Stop: true}}
}
func verificationRequest(f *manualDownloadFixture, action string, in ptVerificationInput) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(in)
	return nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/pt-verification/"+action, f.token, string(raw))
}
func verificationReview(t *testing.T, f *manualDownloadFixture, in ptVerificationInput) ptVerificationReview {
	t.Helper()
	resp := verificationRequest(f, "review", in)
	if resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	var env struct{ Data ptVerificationReview }
	if json.Unmarshal(resp.Body.Bytes(), &env) != nil {
		t.Fatal(resp.Body.String())
	}
	return env.Data
}
func recordVerification(t *testing.T, f *manualDownloadFixture, in ptVerificationInput) ptVerificationInput {
	t.Helper()
	r := verificationReview(t, f, in)
	in.Fingerprint = r.Fingerprint
	in.Confirm = true
	in.ConfirmAccountReviewed = true
	in.ConfirmAutomationScope = true
	in.ConfirmOldExecutorsStopped = true
	resp := verificationRequest(f, "record", in)
	if resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	return in
}

func TestPTVerificationIsScopedHumanEvidenceNotAutomaticTrackerClearance(t *testing.T) {
	w := newSeedingFixture(t)
	w.seconds = 1800
	w.ratio = 2
	policy := bindVerificationPolicy(t, w.f)
	in := verificationInput(w.f)
	r := verificationReview(t, w.f, in)
	if r.Evidence == nil || r.Evidence.AddedAt == nil || r.Current.Record != nil || w.mutations != 0 {
		t.Fatal(r, w.mutations)
	}
	var exists bool
	_ = w.f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_PT_VERIFICATIONS')`).Scan(&exists)
	if exists {
		t.Fatal("review created tables")
	}
	in.Fingerprint = r.Fingerprint
	in.Confirm = true
	if resp := verificationRequest(w.f, "record", in); resp.Code != 409 {
		t.Fatal("missing separate confirmations", resp.Code)
	}
	in = recordVerification(t, w.f, in)
	if w.mutations != 0 {
		t.Fatal("record dispatched a task mutation", w.mutations)
	}
	if resp := verificationRequest(w.f, "record", in); resp.Code != 409 {
		t.Fatal("lost response could replay a record", resp.Code)
	}
	rows, _, err := w.f.store.SeedingPolicies(t.Context(), 1, w.f.hash)
	if err != nil || len(rows) != 1 || rows[0].ID != policy.ID || rows[0].Requirements.HR != nil {
		t.Fatal("original obligation changed", rows, err)
	}
	resp := nativeJSONRequest(w.f.handler, "GET", "/api/v1/downloads/seeding?downloaderId=1&taskId="+w.f.hash, w.f.token, "")
	if resp.Code != 200 || !strings.Contains(resp.Body.String(), `"basis":"operator-attestation"`) {
		t.Fatal(resp.Code, resp.Body.String())
	}
	if resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/"+w.f.hash+"/remove", w.f.token, `{}`); resp.Code != 409 || w.mutations != 0 {
		t.Fatal("stop statement authorized deletion", resp.Code, w.mutations)
	}
	if resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/"+w.f.hash+"/stop", w.f.token, `{}`); resp.Code != 200 || w.mutations != 1 {
		t.Fatal(resp.Code, resp.Body.String(), w.mutations)
	}
	w.f.rebuild(t)
	if err := w.f.runner.download.service.checkSeedingAction(t.Context(), mustDownloader(t, w.f), w.f.hash, "stop"); err != nil {
		t.Fatal("valid persisted record lost on restart", err)
	}
	if err := w.f.runner.download.service.checkSeedingPolicy(t.Context(), mustDownloader(t, w.f), w.f.hash); err == nil {
		t.Fatal("unscoped checker accepted a waiver")
	}
	revoke := ptVerificationInput{DownloaderID: 1, TaskID: w.f.hash, Mode: "revoke"}
	w.absent = true
	r = verificationReview(t, w.f, revoke)
	if r.Evidence != nil {
		t.Fatal("revoke unnecessarily depends on daemon")
	}
	revoke.Fingerprint = r.Fingerprint
	revoke.Confirm = true
	if resp := verificationRequest(w.f, "revoke", revoke); resp.Code != 200 || w.mutations != 1 {
		t.Fatal(resp.Code, resp.Body.String(), w.mutations)
	}
	w.absent = false
	if err := w.f.runner.download.service.checkSeedingAction(t.Context(), mustDownloader(t, w.f), w.f.hash, "stop"); !errors.Is(err, downloaderconfig.ErrSeedingPolicy) {
		t.Fatal("revoked record still authorized", err)
	}
}

func TestPTVerificationCannotBypassMinimaUnknownCountersOrChangedScope(t *testing.T) {
	for _, mode := range []string{"minimum seconds", "minimum ratio", "unknown counters", "unknown epoch", "counter regression", "ratio regression", "re-added", "new policy", "configuration changed", "stale review"} {
		t.Run(mode, func(t *testing.T) {
			w := newSeedingFixture(t)
			w.seconds = 1800
			w.ratio = 2
			policy := bindVerificationPolicy(t, w.f)
			in := verificationInput(w.f)
			switch mode {
			case "minimum seconds":
				w.seconds = 899
			case "minimum ratio":
				w.ratio = 1.49
			case "unknown counters":
				w.unknown = true
			case "unknown epoch":
				w.addedAt = 0
			}
			if strings.HasPrefix(mode, "minimum") || strings.HasPrefix(mode, "unknown") {
				if resp := verificationRequest(w.f, "review", in); resp.Code != 409 || w.mutations != 0 {
					t.Fatal(mode, resp.Code, resp.Body.String())
				}
				return
			}
			if mode == "stale review" {
				r := verificationReview(t, w.f, in)
				in.Fingerprint = r.Fingerprint
				in.Confirm = true
				in.ConfirmAccountReviewed = true
				in.ConfirmAutomationScope = true
				in.ConfirmOldExecutorsStopped = true
				w.addedAt++
				if resp := verificationRequest(w.f, "record", in); resp.Code != 409 {
					t.Fatal("stale identity accepted", resp.Code)
				}
				return
			}
			recordVerification(t, w.f, in)
			switch mode {
			case "counter regression":
				w.seconds--
			case "ratio regression":
				w.ratio = 1.9
			case "re-added":
				w.addedAt++
			case "configuration changed":
				item := mustDownloader(t, w.f)
				item.Name = "changed"
				if _, err := w.f.store.Upsert(t.Context(), item); err != nil {
					t.Fatal(err)
				}
			case "new policy":
				_, digest, _ := w.f.store.SeedingPolicies(t.Context(), 1, w.f.hash)
				policy.SourceDigest = bus.Hash("another source")
				if _, err := w.f.store.BindSeedingPolicy(t.Context(), policy, digest); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.f.runner.download.service.checkSeedingAction(t.Context(), mustDownloader(t, w.f), w.f.hash, "stop"); !errors.Is(err, downloaderconfig.ErrSeedingPolicy) {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestPTVerificationMoveAndOrganizationRequireIndependentScopesAndSharedGuard(t *testing.T) {
	w := newSeedingFixture(t)
	w.seconds = 1800
	w.ratio = 2
	bindVerificationPolicy(t, w.f)
	in := verificationInput(w.f)
	recordVerification(t, w.f, in)
	api := *w.f.runner.organizer
	item := mustDownloader(t, w.f)
	calls := 0
	c := api.protectSeedingClient(item, completionOrganizationClient{stop: func(context.Context, string) error { calls++; return nil }, remove: func(context.Context, string) error { calls++; return nil }})
	if err := c.stop(t.Context(), w.f.hash); err != nil {
		t.Fatal(err)
	}
	if err := c.remove(t.Context(), w.f.hash); err == nil || calls != 1 {
		t.Fatal("scope expanded", err, calls)
	}
	job := organization.Job{Definition: organization.Definition{Mode: "move", Download: &organization.DownloadOrigin{DownloaderID: "1", Task: downloadcompletion.Task{ID: w.f.hash}}}}
	if _, _, err := api.guardSeedingMove(t.Context(), job); err == nil {
		t.Fatal("stop scope authorized source movement")
	}
	in.Actions = downloaderconfig.VerificationActions{Move: true}
	recordVerification(t, w.f, in)
	ctx, close, err := api.guardSeedingMove(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if resp := verificationRequest(w.f, "review", in); resp.Code != 409 {
		t.Fatal("record crossed active move guard", resp.Code)
	}
	if err := api.seedingService().checkSeedingAction(ctx, item, w.f.hash, "move"); err != nil {
		t.Fatal(err)
	}
	close()
	state, err := w.f.store.PTVerification(t.Context(), 1, w.f.hash)
	if err != nil {
		t.Fatal(err)
	}
	rows, digest, err := w.f.store.SeedingPolicies(t.Context(), 1, w.f.hash)
	if err != nil {
		t.Fatal(err)
	}
	e, err := api.seedingService().seedingEvidence(t.Context(), item, w.f.hash)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	state.Record.Created = now.Add(-31 * time.Minute).UTC().Format(time.RFC3339Nano)
	state.Record.Expires = now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if api.seedingService().seedingActionDecision(t.Context(), item, w.f.hash, "move", digest, rows, e, state).Allowed {
		t.Fatal("expired assertion authorized movement")
	}
	state, err = w.f.store.PTVerification(t.Context(), 1, w.f.hash)
	if err != nil {
		t.Fatal(err)
	}
	state.Record.Actor = "0:other-admin"
	if api.seedingService().seedingActionDecision(t.Context(), item, w.f.hash, "move", digest, rows, e, state).Allowed {
		t.Fatal("changed issuer accepted")
	}
}

func TestPTVerificationRequiresCurrentAdministratorAndReadOnlyRecovery(t *testing.T) {
	w := newSeedingFixture(t)
	bindVerificationPolicy(t, w.f)
	created := performFormRequest(w.f.handler, "/api/v1/user/manage", w.f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"下载管理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, w.f.handler, "viewer", "strong-password")
	for _, action := range []string{"review", "record", "revoke"} {
		resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/pt-verification/"+action, viewer, `{}`)
		if resp.Code != 403 {
			t.Fatal(action, resp.Code)
		}
	}
	resp := nativeJSONRequest(w.f.handler, "GET", "/api/v1/downloads/pt-verification?downloaderId=1&taskId="+w.f.hash, viewer, "")
	if resp.Code != 403 {
		t.Fatal(resp.Code)
	}
	in := verificationInput(w.f)
	recordVerification(t, w.f, in)
	resp = nativeJSONRequest(w.f.handler, "GET", "/api/v1/downloads/pt-verification?downloaderId=1&taskId="+w.f.hash, w.f.token, "")
	if resp.Code != 200 || !strings.Contains(resp.Body.String(), "operator-attestation") || strings.Contains(resp.Body.String(), "private-password") || w.mutations != 0 {
		t.Fatal(resp.Code, resp.Body.String(), w.mutations)
	}
}

func TestPTVerificationTransmissionEpochBothRPCVersions(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(map[bool]string{false: "old", true: "modern"}[modern], func(t *testing.T) {
			f := manualHTTPFixture(t, "transmission", modern)
			base := f.transport
			mutations := 0
			f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() != "client.local" || r.Header.Get("X-Transmission-Session-Id") == "" {
					return base.RoundTrip(r)
				}
				var req struct {
					Method    string
					Arguments map[string]json.RawMessage
					Params    map[string]json.RawMessage
				}
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					t.Fatal("invalid RPC")
				}
				args := req.Arguments
				if modern {
					args = req.Params
				}
				var ids []string
				_ = json.Unmarshal(args["ids"], &ids)
				if len(ids) != 1 || ids[0] != f.hash {
					t.Fatal(ids)
				}
				data := map[string]any{}
				if req.Method == "torrent-get" || req.Method == "torrent_get" {
					row := map[string]any{"hashString": f.hash, "status": 6, "percentDone": 1, "leftUntilDone": 0, "metadataPercentComplete": 1, "error": 0, "secondsSeeding": 1800, "uploadRatio": 2, "addedDate": 1700000000}
					if modern {
						row = map[string]any{"hash_string": f.hash, "status": 6, "percent_done": 1, "left_until_done": 0, "metadata_percent_complete": 1, "error": 0, "seconds_seeding": 1800, "upload_ratio": 2, "added_date": 1700000000}
					}
					data["torrents"] = []any{row}
				} else if req.Method == "torrent-stop" || req.Method == "torrent_stop" {
					mutations++
				} else {
					t.Fatal("unexpected mutation", req.Method)
				}
				env := map[string]any{"result": "success", "arguments": data}
				if modern {
					env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": data}
				}
				raw, _ := json.Marshal(env)
				return jsonResponse(r, string(raw)), nil
			})
			f.rebuild(t)
			bindVerificationPolicy(t, f)
			recordVerification(t, f, verificationInput(f))
			if mutations != 0 {
				t.Fatal(mutations)
			}
			resp := nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/"+f.hash+"/stop", f.token, `{}`)
			if resp.Code != 200 || mutations != 1 {
				t.Fatal(resp.Code, resp.Body.String(), mutations)
			}
		})
	}
}
