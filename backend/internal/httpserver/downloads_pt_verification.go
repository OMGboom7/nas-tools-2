package httpserver

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

func (s downloadService) verificationActorCurrent(ctx context.Context, actor string) bool {
	parts := strings.SplitN(actor, ":", 2)
	if len(parts) != 2 || s.auth == nil || !s.auth.service.IsAdministrator(parts[1]) {
		return false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	u, err := s.auth.service.FindUser(ctx, parts[1])
	if err != nil || u.ID != id {
		return false
	}
	for _, p := range u.Permissions {
		if p == "下载管理" {
			return true
		}
	}
	return false
}
func (s downloadService) seedingActionDecision(ctx context.Context, item downloaderconfig.Downloader, hash, action, digest string, policies []downloaderconfig.SeedingPolicy, e seeding.Evidence, state downloaderconfig.PTVerificationState) seeding.Decision {
	rules := []seeding.Requirements{}
	for _, p := range policies {
		if p.ConfigDigest != bus.Hash(item) {
			return seeding.Decision{Allowed: false, Reasons: []string{"downloader configuration changed"}}
		}
		rules = append(rules, p.Requirements)
	}
	d := seeding.Evaluate(hash, rules, e)
	if d.Allowed {
		return d
	}
	v := state.Record
	if v == nil || state.Revoked || !v.Actions.Allows(action) || !v.Current(bus.Hash(item), digest, time.Now()) || !s.verificationActorCurrent(ctx, v.Actor) {
		return d
	}
	d = seeding.EvaluateAttested(hash, rules, e, v.Observed)
	d.Basis = "operator-attestation"
	return d
}

type ptVerificationInput struct {
	DownloaderID               int64                                `json:"downloaderId"`
	TaskID                     string                               `json:"taskId"`
	Mode                       string                               `json:"mode"`
	Minutes                    int                                  `json:"minutes"`
	Note                       string                               `json:"note"`
	Actions                    downloaderconfig.VerificationActions `json:"actions"`
	Fingerprint                string                               `json:"fingerprint"`
	Confirm                    bool                                 `json:"confirm"`
	ConfirmAccountReviewed     bool                                 `json:"confirmAccountReviewed"`
	ConfirmAutomationScope     bool                                 `json:"confirmAutomationScope"`
	ConfirmOldExecutorsStopped bool                                 `json:"confirmOldExecutorsStopped"`
}
type ptVerificationReview struct {
	DownloaderID  int64                                `json:"downloaderId"`
	Name          string                               `json:"name"`
	TaskID        string                               `json:"taskId"`
	Mode          string                               `json:"mode"`
	Minutes       int                                  `json:"minutes"`
	Note          string                               `json:"note"`
	Actions       downloaderconfig.VerificationActions `json:"actions"`
	Policies      []downloaderconfig.SeedingPolicy     `json:"policies"`
	Evidence      *seeding.Evidence                    `json:"evidence"`
	Current       downloaderconfig.PTVerificationState `json:"current"`
	Fingerprint   string                               `json:"fingerprint"`
	ReviewExpires string                               `json:"reviewExpires"`
	Warning       string                               `json:"warning"`
	item          downloaderconfig.Downloader
	digest        string
}

func (s downloadService) reviewPTVerification(ctx context.Context, actor string, in ptVerificationInput) (ptVerificationReview, error) {
	r := ptVerificationReview{DownloaderID: in.DownloaderID, TaskID: in.TaskID, Mode: in.Mode, Minutes: in.Minutes, Note: in.Note, Actions: in.Actions}
	item, err := s.downloaders.Get(ctx, in.DownloaderID)
	if err != nil {
		return r, err
	}
	r.item = item
	r.Name = item.Name
	r.Current, err = s.downloaders.PTVerification(ctx, item.ID, in.TaskID)
	if err != nil {
		return r, err
	}
	r.Policies, r.digest, err = s.downloaders.SeedingPolicies(ctx, item.ID, in.TaskID)
	if err != nil {
		return r, err
	}
	if in.Mode == "attest" {
		if !in.Actions.Valid() || in.Minutes < 1 || in.Minutes > 30 || !downloaderconfig.VerificationNoteValid(in.Note) || item.Enabled == 0 || item.Type != "qbittorrent" && item.Type != "transmission" || len(r.Policies) == 0 {
			return r, downloaderconfig.ErrSeedingPolicy
		}
		rules := []seeding.Requirements{}
		for _, p := range r.Policies {
			if p.ConfigDigest != bus.Hash(item) {
				return r, downloaderconfig.ErrSeedingPolicy
			}
			rules = append(rules, p.Requirements)
		}
		e, err := s.seedingEvidence(ctx, item, in.TaskID)
		if err != nil || !seeding.EvaluateAttested(in.TaskID, rules, e, e).Allowed {
			return r, downloaderconfig.ErrSeedingPolicy
		}
		r.Evidence = &e
	} else if in.Mode != "revoke" || r.Current.Record == nil || r.Current.Revoked {
		return r, downloaderconfig.ErrSeedingPolicy
	}
	bucket := time.Now().Unix() / 300
	r.ReviewExpires = time.Unix((bucket+1)*300, 0).UTC().Format(time.RFC3339)
	var epoch *int64
	if r.Evidence != nil {
		epoch = r.Evidence.AddedAt
	}
	// Mutable counters are reread at record time, not editable client minima.
	// A five-minute review bucket bounds stale human confirmations.
	r.Fingerprint = bus.Hash([]any{actor, in.DownloaderID, in.TaskID, bus.Hash(item), r.digest, r.Current, in.Mode, in.Minutes, in.Note, in.Actions, epoch, bucket})
	r.Warning = "这是管理员在站点账户中自行核实的声明，不是站点自动验证；原约束不删除、不降低，已知最低做种条件必须仍然满足。仅对当前下载器配置、准确任务新增时间及整组来源生效，最长 30 分钟，动作分别授权。记录本身不停止、删除或移动，但已启用且具有原权限/确认的自动流程可能立即继续所选动作（删除可涉及文件）。计数回退、重新添加、过期、撤销、操作者权限或来源变化都会使声明失效。不要填写私密链接、Cookie 或口令。"
	return r, nil
}
func (s downloadService) servePTVerification(w http.ResponseWriter, req *http.Request) {
	actor, ok := s.authorizeManualReceipts(w, req)
	if !ok {
		return
	}
	if _, err := s.manualActor(req.Context(), req.Header.Get("Authorization")); err != nil {
		manualSubmissionError(w, err)
		return
	}
	var in ptVerificationInput
	if req.Method == http.MethodGet {
		var err error
		in.DownloaderID, err = strconv.ParseInt(req.URL.Query().Get("downloaderId"), 10, 64)
		if err != nil {
			writeAPIError(w, 400, 400, "exact downloader required")
			return
		}
		in.TaskID = req.URL.Query().Get("taskId")
	} else if !decodeServiceRequest(w, req, &in, "invalid PT verification request") {
		return
	}
	in.TaskID = strings.ToLower(strings.TrimSpace(in.TaskID))
	in.Note = strings.TrimSpace(in.Note)
	if in.DownloaderID < 1 || !seeding.ValidHash(in.TaskID) {
		writeAPIError(w, 400, 400, "exact downloader and hash required")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	if req.Method == http.MethodGet {
		state, err := s.downloaders.PTVerification(ctx, in.DownloaderID, in.TaskID)
		if err != nil {
			writeAPIError(w, 503, 503, "PT verification state unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": state})
		return
	}
	if s.manualGuards == nil || *s.manualGuards == nil {
		writeAPIError(w, 503, 503, "PT task guard unavailable")
		return
	}
	ctx, close, err := s.seedingGuard(ctx, in.DownloaderID, in.TaskID)
	if err != nil {
		manualSubmissionError(w, err)
		return
	}
	defer close()
	r, err := s.reviewPTVerification(ctx, actor, in)
	if err != nil {
		writeAPIError(w, 409, 409, "exact task identity, known minima or verification scope could not be established; no operation authorized")
		return
	}
	if req.PathValue("action") == "review" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": r})
		return
	}
	if !in.Confirm || in.Fingerprint != r.Fingerprint {
		writeAPIError(w, 409, 409, "fresh scope review and explicit confirmation required")
		return
	}
	switch req.PathValue("action") {
	case "record":
		if in.Mode != "attest" || !in.ConfirmAccountReviewed || !in.ConfirmAutomationScope || !in.ConfirmOldExecutorsStopped {
			writeAPIError(w, 409, 409, "independent account evidence, automation scope and old executor confirmations required")
			return
		}
		ids := []string{}
		for _, p := range r.Policies {
			ids = append(ids, p.ID)
		}
		v, err := s.downloaders.RecordPTVerification(ctx, downloaderconfig.PTVerification{DownloaderID: in.DownloaderID, InfoHash: in.TaskID, ConfigDigest: bus.Hash(r.item), PolicyDigest: r.digest, PolicyIDs: ids, Actor: actor, Note: in.Note, Actions: in.Actions, Observed: *r.Evidence}, in.Minutes, bus.Hash(r.Current))
		if err != nil {
			writeAPIError(w, 409, 409, "PT verification scope changed; readonly refresh before retrying")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
	case "revoke":
		if in.Mode != "revoke" {
			writeAPIError(w, 409, 409, "explicit revocation scope required")
			return
		}
		if err := s.downloaders.RevokePTVerification(ctx, in.DownloaderID, in.TaskID, actor, bus.Hash(r.Current)); err != nil {
			writeAPIError(w, 409, 409, "PT verification changed; readonly refresh required")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true})
	default:
		writeAPIError(w, 404, 404, "unknown PT verification action")
	}
}
