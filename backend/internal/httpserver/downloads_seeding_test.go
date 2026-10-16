package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

type seedingFixture struct {
	f               *manualDownloadFixture
	seconds         int64
	ratio           float64
	mutations       int
	unknown, absent bool
}

func newSeedingFixture(t *testing.T) *seedingFixture {
	t.Helper()
	w := &seedingFixture{f: manualHTTPFixture(t, "qbittorrent", false), seconds: 900, ratio: 1.5}
	original := w.f.transport
	w.f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "client.local" {
			return original.RoundTrip(r)
		}
		if strings.HasSuffix(r.URL.Path, "auth/login") {
			return jsonResponse(r, "Ok."), nil
		}
		if strings.HasSuffix(r.URL.Path, "torrents/info") {
			if r.Method != "GET" || r.URL.Query().Get("hashes") != w.f.hash || r.URL.Query().Get("filter") != "all" {
				t.Fatal("widened source", r.URL)
			}
			if w.absent {
				return jsonResponse(r, `[]`), nil
			}
			row := map[string]any{"hash": w.f.hash, "state": "pausedUP", "progress": 1, "amount_left": 0, "completion_on": 1}
			if !w.unknown {
				row["seeding_time"] = w.seconds
				row["ratio"] = w.ratio
			}
			raw, _ := json.Marshal([]any{row})
			return jsonResponse(r, string(raw)), nil
		}
		w.mutations++
		return jsonResponse(r, ""), nil
	})
	w.f.rebuild(t)
	return w
}

func (w *seedingFixture) source(t *testing.T) string {
	seconds, ratio := int64(900), 1.5
	ids, err := w.f.runner.download.service.resources.Put("0:admin", []externalindexer.Resource{{Title: "Exact source", Indexer: "Tracker", DownloadURL: testMagnet, MinimumSeedTime: &seconds, MinimumRatio: &ratio}})
	if err != nil {
		t.Fatal(err)
	}
	return ids[0]
}

func (w *seedingFixture) request(action string, in seedingRequest) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(in)
	return nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/seeding/"+action, w.f.token, string(raw))
}

func TestSeedingPolicyReviewBindAndRestartFailClosed(t *testing.T) {
	w := newSeedingFixture(t)
	in := seedingRequest{ResourceID: w.source(t), DownloaderID: 1, TaskID: w.f.hash}
	resp := w.request("review", in)
	if resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	var envelope struct{ Data seedingReview }
	if json.Unmarshal(resp.Body.Bytes(), &envelope) != nil {
		t.Fatal(resp.Body.String())
	}
	review := envelope.Data
	if review.Decision.Allowed || !review.NeedsUnknownHold || review.Policy.Requirements.HR != nil || review.Policy.Requirements.Seconds == nil || *review.Policy.Requirements.Seconds != 900 || w.mutations != 0 {
		t.Fatal(review, w.mutations)
	}
	var schema bool
	_ = w.f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_SEEDING_POLICIES')`).Scan(&schema)
	if schema {
		t.Fatal("review created policy table")
	}
	in.Fingerprint = review.Fingerprint
	in.Confirm = true
	in.ConfirmOldExecutorsStopped = true
	if resp := w.request("bind", in); resp.Code != 409 {
		t.Fatal("missing unknown-HR consent", resp.Code, resp.Body.String())
	}
	in.ConfirmUnknownHold = true
	resp = w.request("bind", in)
	if resp.Code != 200 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	for _, secret := range []string{"private-password", "private-token", "ConfigDigest", "SourceDigest", "DownloadURL"} {
		if strings.Contains(resp.Body.String(), secret) {
			t.Fatal("secret in receipt", resp.Body.String())
		}
	}
	for _, action := range []string{"stop", "remove"} {
		resp := nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/"+w.f.hash+"/"+action, w.f.token, `{}`)
		if resp.Code != 409 {
			t.Fatal(action, resp.Code, resp.Body.String())
		}
	}
	if w.mutations != 0 {
		t.Fatal("unsafe remote mutation", w.mutations)
	}
	w.f.rebuild(t)
	resp = nativeJSONRequest(w.f.handler, "GET", "/api/v1/downloads/seeding?downloaderId=1&taskId="+w.f.hash, w.f.token, "")
	if resp.Code != 200 || !strings.Contains(resp.Body.String(), `"registered":true`) || !strings.Contains(resp.Body.String(), `"allowed":false`) {
		t.Fatal(resp.Code, resp.Body.String())
	}
	if resp := w.request("bind", in); resp.Code != 409 {
		t.Fatal("expired source accepted after restart", resp.Code)
	}
}

func TestSeedingReviewRejectsIdentityOwnershipStaleScopesAndConcurrentBinding(t *testing.T) {
	w := newSeedingFixture(t)
	in := seedingRequest{ResourceID: w.source(t), DownloaderID: 1, TaskID: strings.Repeat("b", 40)}
	if resp := w.request("review", in); resp.Code != 409 {
		t.Fatal(resp.Code, resp.Body.String())
	}
	in.TaskID = w.f.hash
	ids, err := w.f.runner.download.service.resources.Put("other", []externalindexer.Resource{{Title: "other", DownloadURL: testMagnet}})
	if err != nil {
		t.Fatal(err)
	}
	owned := in.ResourceID
	in.ResourceID = ids[0]
	if resp := w.request("review", in); resp.Code != 409 {
		t.Fatal(resp.Code)
	}
	in.ResourceID = owned
	s := w.f.runner.download.service
	_, close, err := s.seedingGuard(t.Context(), 1, w.f.hash)
	if err != nil {
		t.Fatal(err)
	}
	if resp := w.request("review", in); resp.Code != 409 {
		t.Fatal("did not share guard", resp.Code)
	}
	close()
	resp := w.request("review", in)
	var e struct{ Data seedingReview }
	if resp.Code != 200 || json.Unmarshal(resp.Body.Bytes(), &e) != nil {
		t.Fatal(resp.Code, resp.Body.String())
	}
	in.Fingerprint = e.Data.Fingerprint
	in.Confirm = true
	in.ConfirmOldExecutorsStopped = true
	in.ConfirmUnknownHold = true
	item, err := s.downloaders.Get(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	item.Name = "scope changed"
	if _, err := s.downloaders.Upsert(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if resp := w.request("bind", in); resp.Code != 409 {
		t.Fatal("stale review accepted", resp.Code)
	}
	w.absent = true
	if resp := w.request("review", in); resp.Code != 409 {
		t.Fatal("absent task accepted", resp.Code)
	}
	if w.mutations != 0 {
		t.Fatal(w.mutations)
	}
}

func (w *seedingFixture) bindKnown(t *testing.T) downloaderconfig.Downloader {
	t.Helper()
	s := w.f.runner.download.service
	item, err := s.downloaders.Get(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	seconds, ratio, hr := int64(900), 1.5, false
	_, digest, err := s.downloaders.SeedingPolicies(t.Context(), 1, w.f.hash)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.downloaders.BindSeedingPolicy(t.Context(), downloaderconfig.SeedingPolicy{DownloaderID: 1, DownloaderName: item.Name, DownloaderType: item.Type, ConfigDigest: bus.Hash(item), InfoHash: w.f.hash, SourceDigest: bus.Hash("verified test source"), Requirements: seeding.Requirements{Seconds: &seconds, Ratio: &ratio, HR: &hr}, Actor: "0:admin"}, digest)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestSeedingExecutionGuardsNativeOrganizationAndMoveWithFreshEvidence(t *testing.T) {
	w := newSeedingFixture(t)
	item := w.bindKnown(t)
	s := w.f.runner.download.service
	api := *w.f.runner.organizer
	var calls int
	c := api.protectSeedingClient(item, completionOrganizationClient{stop: func(ctx context.Context, h string) error {
		calls++
		_, close, err := s.seedingGuard(t.Context(), 1, h)
		if err == nil {
			close()
			t.Error("remote mutation did not hold task guard")
		}
		return nil
	}, remove: func(ctx context.Context, h string) error { calls++; return nil }})
	job := organization.Job{Definition: organization.Definition{Mode: "move", Download: &organization.DownloadOrigin{DownloaderID: "1", Task: downloadcompletion.Task{ID: w.f.hash}}}}
	w.seconds = 899
	if err := c.stop(t.Context(), w.f.hash); !errors.Is(err, downloaderconfig.ErrSeedingPolicy) {
		t.Fatal(err)
	}
	if err := c.remove(t.Context(), w.f.hash); !errors.Is(err, downloaderconfig.ErrSeedingPolicy) {
		t.Fatal(err)
	}
	if err := api.runJob(t.Context(), job, false); !errors.Is(err, downloaderconfig.ErrSeedingPolicy) {
		t.Fatal("move bypassed", err)
	}
	if err := api.resumePublication(t.Context(), job); !errors.Is(err, downloaderconfig.ErrSeedingPolicy) {
		t.Fatal("publication recovery bypassed", err)
	}
	if err := api.cleanupMove(t.Context(), job, organization.JobItem{}); !errors.Is(err, downloaderconfig.ErrSeedingPolicy) {
		t.Fatal("cleanup bypassed", err)
	}
	if calls != 0 {
		t.Fatal(calls)
	}
	w.seconds = 900
	w.ratio = 1.49
	if _, err := s.nativeControl(t.Context(), w.f.hash, "remove"); !errors.Is(err, downloaderconfig.ErrSeedingPolicy) {
		t.Fatal(err)
	}
	w.ratio = 1.5
	w.unknown = true
	if err := c.stop(t.Context(), w.f.hash); !errors.Is(err, downloaderconfig.ErrSeedingPolicy) {
		t.Fatal("missing counters became zero", err)
	}
	w.unknown = false
	if err := c.stop(t.Context(), w.f.hash); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	ctx, close, err := api.guardSeedingMove(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	_, nested, err := s.seedingGuard(ctx, 1, w.f.hash)
	if err != nil {
		t.Fatal("nested scope blocked", err)
	}
	nested()
	if _, _, err := s.seedingGuard(t.Context(), 1, w.f.hash); err == nil {
		t.Fatal("move not serialized against first binding")
	}
	close()
	item.Name = "changed"
	if _, err := s.downloaders.Upsert(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if err := c.remove(t.Context(), w.f.hash); !errors.Is(err, downloaderconfig.ErrSeedingPolicy) {
		t.Fatal("configuration rebound implicitly", err)
	}
	if w.mutations != 0 {
		t.Fatal(w.mutations)
	}
}

func TestDownloadMutationsRecheckCurrentPermissionsAndRequireAdminForPolicy(t *testing.T) {
	w := newSeedingFixture(t)
	created := performFormRequest(w.f.handler, "/api/v1/user/manage", w.f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"探索"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, w.f.handler, "viewer", "strong-password")
	for _, path := range []string{"/api/v1/downloads/" + w.f.hash + "/stop", "/api/v1/downloads/" + w.f.hash + "/remove", "/api/v1/download/stop", "/api/v1/download/remove", "/api/v1/downloads/seeding/bind"} {
		resp := nativeJSONRequest(w.f.handler, "POST", path, viewer, `{}`)
		if resp.Code != 403 {
			t.Fatal(path, resp.Code, resp.Body.String())
		}
	}
	if _, err := w.f.db.Exec(`UPDATE CONFIG_USERS SET PRIS='下载管理' WHERE NAME='viewer'`); err != nil {
		t.Fatal(err)
	}
	resp := nativeJSONRequest(w.f.handler, "GET", "/api/v1/downloads/seeding?downloaderId="+strconv.Itoa(1)+"&taskId="+w.f.hash, viewer, "")
	if resp.Code != 403 {
		t.Fatal("nonadmin policy read", resp.Code)
	}
	if _, err := w.f.db.Exec(`UPDATE CONFIG_USERS SET PRIS='探索' WHERE NAME='viewer'`); err != nil {
		t.Fatal(err)
	}
	resp = nativeJSONRequest(w.f.handler, "POST", "/api/v1/downloads/"+w.f.hash+"/stop", viewer, `{}`)
	if resp.Code != 403 || w.mutations != 0 {
		t.Fatal("revoked permission allowed", resp.Code, w.mutations)
	}
}
