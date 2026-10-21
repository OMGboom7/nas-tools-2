package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/seeding"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

type seedingGuardKey struct{}

// Keep the same cross-process guard through binding and the entire destructive
// operation, including physical source removal. Nested checks reuse this scope.
func (s downloadService) seedingGuard(ctx context.Context, id int64, hash string) (context.Context, func(), error) {
	key := "pt-seeding:" + strconv.FormatInt(id, 10) + ":" + strings.ToLower(hash)
	if ctx.Value(seedingGuardKey{}) == key {
		return ctx, func() {}, nil
	}
	if s.manualGuards == nil || *s.manualGuards == nil {
		if s.downloaders != nil {
			p, _, err := s.downloaders.SeedingPolicies(ctx, id, hash)
			if err != nil || len(p) != 0 {
				return ctx, nil, downloaderconfig.ErrSeedingPolicy
			}
		}
		return ctx, func() {}, nil
	}
	g, err := (*s.manualGuards).AcquireJob(ctx, key)
	if err != nil {
		return ctx, nil, downloaderconfig.ErrSeedingPolicy
	}
	return context.WithValue(ctx, seedingGuardKey{}, key), func() { g.Close() }, nil
}

func (s downloadService) seedingEvidence(ctx context.Context, item downloaderconfig.Downloader, hash string) (seeding.Evidence, error) {
	var cfg map[string]any
	if json.Unmarshal([]byte(item.Config), &cfg) != nil || item.Enabled == 0 {
		return seeding.Evidence{}, downloaderconfig.ErrSeedingPolicy
	}
	var transport http.RoundTripper
	if s.client != nil {
		transport = s.client.Transport
	}
	switch item.Type {
	case "qbittorrent":
		c, err := qbittorrent.New(text(cfg["host"]), text(cfg["port"]), text(cfg["username"]), text(cfg["password"]), transport)
		if err != nil {
			return seeding.Evidence{}, err
		}
		return c.SeedingEvidence(ctx, hash)
	case "transmission":
		c, err := transmission.New(text(cfg["host"]), text(cfg["port"]), text(cfg["username"]), text(cfg["password"]), transport)
		if err != nil {
			return seeding.Evidence{}, err
		}
		return c.SeedingEvidence(ctx, hash)
	}
	return seeding.Evidence{}, downloaderconfig.ErrSeedingPolicy
}

func (s downloadService) checkSeedingPolicy(ctx context.Context, item downloaderconfig.Downloader, hash string) error {
	return s.checkSeedingAction(ctx, item, hash, "")
}
func (s downloadService) checkSeedingAction(ctx context.Context, item downloaderconfig.Downloader, hash, action string) error {
	if s.downloaders == nil {
		return nil
	}
	policies, digest, err := s.downloaders.SeedingPolicies(ctx, item.ID, hash)
	if err != nil {
		return downloaderconfig.ErrSeedingPolicy
	}
	if len(policies) == 0 {
		state, err := s.downloaders.PTVerification(ctx, item.ID, hash)
		if err != nil || state.Record != nil {
			return downloaderconfig.ErrSeedingPolicy
		}
		return nil
	} // Unregistered is not evidence of no tracker obligations.
	current, err := s.downloaders.Get(ctx, item.ID)
	if err != nil || bus.Hash(current) != bus.Hash(item) || current.Enabled == 0 {
		return downloaderconfig.ErrSeedingPolicy
	}
	e, err := s.seedingEvidence(ctx, current, hash)
	if err != nil {
		return downloaderconfig.ErrSeedingPolicy
	}
	fresh, err := s.downloaders.Get(ctx, item.ID)
	if err != nil || bus.Hash(fresh) != bus.Hash(current) {
		return downloaderconfig.ErrSeedingPolicy
	}
	state, err := s.downloaders.PTVerification(ctx, item.ID, hash)
	if err != nil || !s.seedingActionDecision(ctx, current, hash, action, digest, policies, e, state).Allowed {
		return downloaderconfig.ErrSeedingPolicy
	}
	return nil
}

func (api organizationAPI) seedingService() downloadService {
	return downloadService{downloaders: api.service.downloaders, client: api.service.client, manualGuards: &api.jobs, auth: api.auth}
}

func (api organizationAPI) protectSeedingClient(item downloaderconfig.Downloader, c completionOrganizationClient) completionOrganizationClient {
	wrap := func(action func(context.Context, string) error, kind string) func(context.Context, string) error {
		if action == nil {
			return nil
		}
		return func(ctx context.Context, hash string) error {
			s := api.seedingService()
			ctx, close, err := s.seedingGuard(ctx, item.ID, hash)
			if err != nil {
				return err
			}
			defer close()
			if err := s.checkSeedingAction(ctx, item, hash, kind); err != nil {
				return err
			}
			return action(ctx, hash)
		}
	}
	c.stop, c.remove = wrap(c.stop, "stop"), wrap(c.remove, "remove")
	return c
}

func (api organizationAPI) guardSeedingMove(ctx context.Context, job organization.Job) (context.Context, func(), error) {
	o := job.Definition.Download
	if o == nil || !organization.MoveMode(job.Definition.Mode) {
		return ctx, func() {}, nil
	}
	id, err := syncConfigID(o.DownloaderID)
	if err != nil {
		return ctx, nil, err
	}
	s := api.seedingService()
	ctx, close, err := s.seedingGuard(ctx, id, o.Task.ID)
	if err != nil {
		return ctx, nil, err
	}
	item, err := s.downloaders.Get(ctx, id)
	if err == nil {
		err = s.checkSeedingAction(ctx, item, o.Task.ID, "move")
	}
	if err != nil {
		close()
		return ctx, nil, err
	}
	return ctx, close, nil
}

type seedingRequest struct {
	ResourceID                 string `json:"resourceId"`
	DownloaderID               int64  `json:"downloaderId"`
	TaskID                     string `json:"taskId"`
	Fingerprint                string `json:"fingerprint"`
	Confirm                    bool   `json:"confirm"`
	ConfirmOldExecutorsStopped bool   `json:"confirmOldExecutorsStopped"`
	ConfirmUnknownHold         bool   `json:"confirmUnknownHold"`
}
type seedingReview struct {
	Policy           downloaderconfig.SeedingPolicy   `json:"policy"`
	Policies         []downloaderconfig.SeedingPolicy `json:"policies"`
	Evidence         seeding.Evidence                 `json:"evidence"`
	Decision         seeding.Decision                 `json:"decision"`
	Fingerprint      string                           `json:"fingerprint"`
	NeedsUnknownHold bool                             `json:"needsUnknownHold"`
	Warning          string                           `json:"warning"`
	digest           string
}

// This API never accepts editable thresholds or HR clearance from the client.
func (s downloadService) reviewSeeding(ctx context.Context, actor string, in seedingRequest) (seedingReview, error) {
	r := seedingReview{}
	resource, err := s.resources.Read(actor, in.ResourceID)
	if err != nil {
		return r, downloaderconfig.ErrSeedingPolicy
	}
	item, err := s.downloaders.Get(ctx, in.DownloaderID)
	if err != nil || item.Enabled == 0 || item.Type != "qbittorrent" && item.Type != "transmission" {
		return r, downloaderconfig.ErrSeedingPolicy
	}
	magnet, torrent, err := s.prepareNativeSearchTorrent(ctx, resource)
	if err != nil {
		return r, downloaderconfig.ErrSeedingPolicy
	}
	hash := magnetInfoHash(magnet)
	if torrent != nil {
		meta, err := torrentmeta.Parse(torrent)
		if err != nil {
			return r, downloaderconfig.ErrSeedingPolicy
		}
		hash = meta.InfoHash
	}
	if !seeding.ValidHash(hash) || !strings.EqualFold(hash, in.TaskID) {
		return r, downloaderconfig.ErrSeedingPolicy
	}
	r.Evidence, err = s.seedingEvidence(ctx, item, hash)
	if err != nil {
		return r, downloaderconfig.ErrSeedingPolicy
	}
	r.Policies, r.digest, err = s.downloaders.SeedingPolicies(ctx, item.ID, hash)
	if err != nil {
		return r, err
	}
	// Search summaries cannot prove tracker account clearance. Until a dedicated
	// tracker verifier exists, retain a hold rather than treating absent HR as false.
	rules := seeding.Requirements{Seconds: resource.MinimumSeedTime, Ratio: resource.MinimumRatio, HR: nil}
	if resource.HitAndRun != nil && *resource.HitAndRun {
		rules.HR = resource.HitAndRun
	}
	r.Policy = downloaderconfig.SeedingPolicy{DownloaderID: item.ID, DownloaderName: item.Name, DownloaderType: item.Type, ConfigDigest: bus.Hash(item), InfoHash: strings.ToLower(hash), Title: bus.Bounded(resource.Title, 600), Source: bus.Bounded(resource.Indexer, 160), SourceDigest: bus.Hash(resource), Requirements: rules, Actor: actor}
	all := []seeding.Requirements{rules}
	for _, p := range r.Policies {
		all = append(all, p.Requirements)
	}
	r.Decision = seeding.Evaluate(hash, all, r.Evidence)
	r.NeedsUnknownHold = rules.HR == nil || *rules.HR || rules.Seconds == nil || rules.Ratio == nil
	r.Warning = "仅保护本版本 Go 的停止、删除及下载任务移动整理；不控制下载器自动停止或外部客户端。HR 未验证将持续拦截，本接口不提供解除或降低约束。未登记不代表没有 PT 义务。"
	r.Fingerprint = bus.Hash(struct {
		Actor, Resource, Config, Source, Previous string
		Policy                                    downloaderconfig.SeedingPolicy
	}{actor, in.ResourceID, r.Policy.ConfigDigest, r.Policy.SourceDigest, r.digest, r.Policy})
	return r, nil
}

func (s downloadService) serveSeedingPolicy(w http.ResponseWriter, req *http.Request) {
	actor, ok := s.authorizeManualReceipts(w, req)
	if !ok {
		return
	}
	if _, err := s.manualActor(req.Context(), req.Header.Get("Authorization")); err != nil {
		manualSubmissionError(w, err)
		return
	}
	if req.PathValue("action") != "review" && req.PathValue("action") != "bind" {
		writeAPIError(w, 404, 404, "unknown seeding policy action")
		return
	}
	if s.resources == nil || s.manualGuards == nil || *s.manualGuards == nil || !s.pureGo {
		writeAPIError(w, 503, 503, "native seeding policy service unavailable")
		return
	}
	var in seedingRequest
	if !decodeServiceRequest(w, req, &in, "invalid seeding policy request") {
		return
	}
	in.TaskID = strings.ToLower(strings.TrimSpace(in.TaskID))
	if in.DownloaderID < 1 || !seeding.ValidHash(in.TaskID) || len(in.ResourceID) > 128 {
		writeAPIError(w, 400, 400, "exact downloader, task hash and owned search resource required")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 45*time.Second)
	defer cancel()
	ctx, close, err := s.seedingGuard(ctx, in.DownloaderID, in.TaskID)
	if err != nil {
		manualSubmissionError(w, err)
		return
	}
	defer close()
	review, err := s.reviewSeeding(ctx, actor, in)
	if err != nil {
		writeAPIError(w, 409, 409, "source identity, task evidence or policy scope could not be verified")
		return
	}
	switch req.PathValue("action") {
	case "review":
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
	case "bind":
		if !in.Confirm || !in.ConfirmOldExecutorsStopped || in.Fingerprint != review.Fingerprint || review.NeedsUnknownHold && !in.ConfirmUnknownHold {
			writeAPIError(w, 409, 409, "fresh review and explicit immutable-policy, old-executor and unknown-HR hold consent required")
			return
		}
		p, err := s.downloaders.BindSeedingPolicy(ctx, review.Policy, review.digest)
		if err != nil {
			writeAPIError(w, 409, 409, "policy scope changed; inspect saved policies before retrying")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": p})
	default:
		writeAPIError(w, 404, 404, "unknown seeding policy action")
	}
}

func (s downloadService) serveSeedingEvidence(w http.ResponseWriter, req *http.Request) {
	if _, ok := s.authorizeManualReceipts(w, req); !ok {
		return
	}
	id, err := strconv.ParseInt(req.URL.Query().Get("downloaderId"), 10, 64)
	hash := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("taskId")))
	if err != nil || id < 1 || !seeding.ValidHash(hash) {
		writeAPIError(w, 400, 400, "exact downloader and task hash required")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 20*time.Second)
	defer cancel()
	item, err := s.downloaders.Get(ctx, id)
	if err != nil {
		writeAPIError(w, 404, 404, "downloader unavailable")
		return
	}
	policies, digest, err := s.downloaders.SeedingPolicies(ctx, id, hash)
	if err != nil {
		writeAPIError(w, 503, 503, "saved policies unavailable")
		return
	}
	e, err := s.seedingEvidence(ctx, item, hash)
	if err != nil {
		writeAPIError(w, 502, 502, "live seeding evidence unavailable; no operation authorized")
		return
	}
	rules := []seeding.Requirements{}
	configMatches := true
	for _, p := range policies {
		rules = append(rules, p.Requirements)
		configMatches = configMatches && p.ConfigDigest == bus.Hash(item)
	}
	decision := seeding.Evaluate(hash, rules, e)
	if !configMatches {
		decision.Allowed = false
		decision.Reasons = append(decision.Reasons, "downloader configuration changed")
	}
	verification, err := s.downloaders.PTVerification(ctx, id, hash)
	if err != nil {
		writeAPIError(w, 503, 503, "PT verification state unavailable")
		return
	}
	actions := map[string]seeding.Decision{}
	for _, action := range []string{"stop", "remove", "move"} {
		d := s.seedingActionDecision(ctx, item, hash, action, digest, policies, e, verification)
		if len(policies) == 0 && verification.Record != nil {
			d = seeding.Decision{Allowed: false, Reasons: []string{"verification policy set is missing"}}
		}
		actions[action] = d
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"policies": policies, "evidence": e, "decision": decision, "actions": actions, "manualVerification": verification, "registered": len(policies) > 0, "warning": "未登记不代表没有 PT 义务；管理员核实不等于站点自动验证。只读结果不是后续操作授权，执行时会重新检查。"}})
}
