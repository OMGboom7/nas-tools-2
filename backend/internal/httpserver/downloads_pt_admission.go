package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

var errPTAdmissionBlocked = errors.New("PT source admission requires a configuration-scoped grant and verified unlimited seeding")

type sourceSeedingConstraint struct {
	Requirements seeding.Requirements `json:"requirements"`
	SourceDigest string               `json:"sourceDigest"`
	AdmissionID  string               `json:"admissionId"`
	Title        string               `json:"title"`
	Source       string               `json:"source"`
	Actor        string               `json:"actor"`
}

func (p sourceSeedingConstraint) policy(item downloaderconfig.Downloader, hash string) downloaderconfig.SeedingPolicy {
	return downloaderconfig.SeedingPolicy{DownloaderID: item.ID, DownloaderName: item.Name, DownloaderType: item.Type, ConfigDigest: bus.Hash(item), InfoHash: strings.ToLower(hash), Title: p.Title, Source: p.Source, SourceDigest: p.SourceDigest, Requirements: p.Requirements, Actor: "pt-admission:" + p.Actor}
}

func (s downloadService) preparePTSource(ctx context.Context, item downloaderconfig.Downloader, r externalindexer.Resource, o downloadAddOptions) (downloadAddOptions, error) {
	// A detail-page absence of the resource HR marker is not proof of account
	// clearance. Keep this unknown until a tracker-specific verifier exists.
	rules := seeding.Requirements{Seconds: r.MinimumSeedTime, Ratio: r.MinimumRatio, HR: nil}
	if !rules.Valid() {
		return o, errPTAdmissionBlocked
	}
	if r.HitAndRun != nil && *r.HitAndRun {
		return o, errPTAdmissionBlocked
	}
	if (r.MinimumSeedTime == nil || *r.MinimumSeedTime <= 0) && (r.MinimumRatio == nil || *r.MinimumRatio <= 0) {
		return o, nil
	}
	if item.Type != "transmission" && item.Type != "qbittorrent" || !s.pureGo || s.downloaders == nil || s.manualGuards == nil || *s.manualGuards == nil || o.RatioLimit != 0 || o.SeedingTimeLimit != 0 {
		return o, errPTAdmissionBlocked
	}
	grant, err := s.downloaders.SeedingAdmission(ctx, item.ID)
	if err != nil || grant == nil || !grant.Enabled || grant.ConfigDigest != bus.Hash(item) {
		return o, errPTAdmissionBlocked
	}
	if rules.Seconds != nil {
		v := *rules.Seconds
		rules.Seconds = &v
	}
	if rules.Ratio != nil {
		v := *rules.Ratio
		rules.Ratio = &v
	}
	if rules.HR != nil {
		v := *rules.HR
		rules.HR = &v
	}
	o.Seeding = &sourceSeedingConstraint{Requirements: rules, SourceDigest: bus.Hash([]any{r, grant.ID}), AdmissionID: grant.ID, Title: bus.Bounded(r.Title, 600), Source: bus.Bounded(r.Indexer, 160), Actor: grant.Actor}
	o.ApplyTransmissionLimits = item.Type == "transmission"
	if !o.Seeding.Requirements.Valid() {
		return o, errPTAdmissionBlocked
	}
	return o, nil
}

func (s downloadService) validatePTAdmission(ctx context.Context, item downloaderconfig.Downloader, o downloadAddOptions) error {
	if o.Seeding == nil {
		return nil
	}
	p := o.Seeding
	if item.Type != "transmission" && item.Type != "qbittorrent" || item.Enabled == 0 || !s.pureGo || s.downloaders == nil || s.manualGuards == nil || *s.manualGuards == nil || item.Type == "transmission" && !o.ApplyTransmissionLimits || o.RatioLimit != 0 || o.SeedingTimeLimit != 0 || !p.Requirements.Valid() || p.Requirements.HR != nil && *p.Requirements.HR || len(p.SourceDigest) != 64 || !seeding.ValidHash(p.SourceDigest) {
		return errPTAdmissionBlocked
	}
	grant, err := s.downloaders.SeedingAdmission(ctx, item.ID)
	if err != nil || grant == nil || !grant.Enabled || grant.ID != p.AdmissionID || grant.Actor != p.Actor || grant.ConfigDigest != bus.Hash(item) {
		return errPTAdmissionBlocked
	}
	current, err := s.downloaders.Get(ctx, item.ID)
	if err != nil || bus.Hash(current) != bus.Hash(item) {
		return errPTAdmissionBlocked
	}
	return nil
}

func (s downloadService) bindAdmissionSource(ctx context.Context, item downloaderconfig.Downloader, hash string, o downloadAddOptions) error {
	if o.Seeding == nil {
		return nil
	}
	if err := s.validatePTAdmission(ctx, item, o); err != nil {
		return err
	}
	_, digest, err := s.downloaders.SeedingPolicies(ctx, item.ID, hash)
	if err != nil {
		return err
	}
	_, err = s.downloaders.BindSeedingPolicy(ctx, o.Seeding.policy(item, hash), digest)
	return err
}

type ptAdmissionReview struct {
	DownloaderID int64                              `json:"downloaderId"`
	Name         string                             `json:"name"`
	Type         string                             `json:"type"`
	Grant        *downloaderconfig.SeedingAdmission `json:"grant"`
	Active       bool                               `json:"active"`
	Enabled      bool                               `json:"enabled"`
	Fingerprint  string                             `json:"fingerprint"`
	Warning      string                             `json:"warning"`
	item         downloaderconfig.Downloader
}

func (s downloadService) reviewPTAdmission(ctx context.Context, actor string, id int64, enabled bool) (ptAdmissionReview, error) {
	r := ptAdmissionReview{DownloaderID: id, Enabled: enabled}
	item, err := s.downloaders.Get(ctx, id)
	if err != nil {
		return r, err
	}
	grant, err := s.downloaders.SeedingAdmission(ctx, id)
	if err != nil {
		return r, err
	}
	r.item = item
	r.Name = item.Name
	r.Type = item.Type
	r.Grant = grant
	r.Active = grant != nil && grant.Enabled && grant.ConfigDigest == bus.Hash(item) && item.Enabled != 0 && (item.Type == "transmission" || item.Type == "qbittorrent")
	r.Warning = "仅允许此配置的 qBittorrent/Transmission 搜索、订阅搜索及订阅 RSS 新任务接管正值最低要求。先记录来源约束，再暂停添加、禁用并验证自动做种上限（qB 为分享率、累计及空闲时间，TR 为分享率和空闲时间），最后按原选择启动；所选停种上限必须为 0，不会静默覆盖。HR 标记拒绝下种，未知要求默认拦截停止/删除/移动；原约束不删改，限时人工核实须另行审核。撤销仅阻止新接管，不取消进行中的请求、不停止任务或清除原约束。其他客户端和来源不在此授权内。"
	r.Fingerprint = bus.Hash([]any{actor, id, bus.Hash(item), grant, enabled})
	return r, nil
}

func (s downloadService) servePTAdmission(w http.ResponseWriter, req *http.Request) {
	actor, ok := s.authorizeManualReceipts(w, req)
	if !ok {
		return
	}
	if _, err := s.manualActor(req.Context(), req.Header.Get("Authorization")); err != nil {
		manualSubmissionError(w, err)
		return
	}
	var in struct {
		DownloaderID               int64  `json:"downloaderId"`
		Enabled                    bool   `json:"enabled"`
		Fingerprint                string `json:"fingerprint"`
		Confirm                    bool   `json:"confirm"`
		ConfirmOldExecutorsStopped bool   `json:"confirmOldExecutorsStopped"`
		ConfirmUnknownHold         bool   `json:"confirmUnknownHold"`
		ConfirmUnlimitedSeeding    bool   `json:"confirmUnlimitedSeeding"`
	}
	if req.Method == http.MethodGet {
		var err error
		in.DownloaderID, err = strconv.ParseInt(req.URL.Query().Get("downloaderId"), 10, 64)
		if err != nil {
			writeAPIError(w, 400, 400, "exact downloader required")
			return
		}
	} else if !decodeServiceRequest(w, req, &in, "invalid PT admission request") {
		return
	}
	if in.DownloaderID < 1 {
		writeAPIError(w, 400, 400, "exact downloader required")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	r, err := s.reviewPTAdmission(ctx, actor, in.DownloaderID, in.Enabled)
	if err != nil {
		writeAPIError(w, 409, 409, "PT admission scope unavailable")
		return
	}
	if req.Method == http.MethodGet || req.PathValue("action") == "review" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": r})
		return
	}
	if req.PathValue("action") != "apply" {
		writeAPIError(w, 404, 404, "unknown PT admission action")
		return
	}
	if !in.Confirm || in.Fingerprint != r.Fingerprint || in.Enabled && (!in.ConfirmOldExecutorsStopped || !in.ConfirmUnknownHold || !in.ConfirmUnlimitedSeeding || r.item.Type != "transmission" && r.item.Type != "qbittorrent" || r.item.Enabled == 0) {
		writeAPIError(w, 409, 409, "fresh review and separate PT scope, old-executor, unknown hold and unlimited-seeding consents required")
		return
	}
	grant, err := s.downloaders.SetSeedingAdmission(ctx, r.item, actor, in.Enabled, bus.Hash(r.Grant))
	if err != nil {
		writeAPIError(w, 409, 409, "PT admission changed; readonly refresh before any retry")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": grant})
}
