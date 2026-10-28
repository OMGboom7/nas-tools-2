package httpserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
)

type speedLimiterReviewTarget struct {
	speedLimiterTarget
	Before  speedlimit.Rates `json:"before"`
	Desired speedlimit.Rates `json:"desired"`
}
type speedLimiterReview struct {
	ID          string                     `json:"id"`
	Fingerprint string                     `json:"fingerprint"`
	Created     string                     `json:"created"`
	Server      string                     `json:"server"`
	MediaID     string                     `json:"mediaId"`
	Cloud       bool                       `json:"cloud"`
	HTTP        bool                       `json:"http"`
	Spec        speedlimit.Spec            `json:"spec"`
	Observation speedlimit.Observation     `json:"observation"`
	Targets     []speedLimiterReviewTarget `json:"targets"`
}

func (api *speedLimiterAPI) review(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		Fingerprint string `json:"fingerprint"`
		Confirm     bool   `json:"confirm"`
		HTTP        bool   `json:"http"`
		PlexAccount bool   `json:"plexAccount"`
		PlexHTTP    bool   `json:"plexHTTP"`
	}
	if !decodeOrganizationRequest(w, r, &in, "无效限速只读审核") {
		return
	}
	status, err := api.status(r.Context(), actor)
	if err != nil || !status.Installed || !in.Confirm || in.Fingerprint != status.Fingerprint || !limiterReady(status.Spec) {
		writeAPIError(w, 400, 400, "先保存有效播放限速、非全放行来源范围和下载器，再审核当前设置")
		return
	}
	media, err := api.mediaSpec()
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	targets, err := api.queue.Targets(r.Context(), status.Spec)
	if err != nil || len(targets) == 0 {
		writeAPIError(w, 400, 400, "没有当前启用的原 qBittorrent 或 Transmission 下载器")
		return
	}
	requiresHTTP, err := api.needsHTTP(r.Context(), media, targets)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	if requiresHTTP && !in.HTTP || media.cloud && !in.PlexAccount || in.PlexHTTP && !in.PlexAccount {
		writeAPIError(w, 400, 400, "需要分别确认原明文连接或 Plex 账户登录，再进行只读连接审核")
		return
	}
	authority, err := api.authority(actor)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	epoch, err := refreshNonce()
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	id, err := refreshNonce()
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	c := speedlimit.Control{Epoch: epoch, Actor: actor, Authority: authority, ConfigDigest: "", MediaDigest: media.digest, Server: media.kind, Created: created, HTTP: in.HTTP, PlexAccount: media.cloud && in.PlexAccount, PlexHTTP: media.cloud && in.PlexHTTP, Baselines: []speedlimit.Baseline{}}
	raw, err := api.system.Get(r.Context(), speedlimit.ConfigKey)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	c.ConfigDigest = speedlimit.RawConfigDigest(raw)
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	client, err := api.connectMedia(ctx, c)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	c.MediaID, err = client.RefreshIdentity(ctx)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	observation, err := client.ObservePlaying(ctx, status.Spec)
	if err != nil {
		writeAPIError(w, 502, 502, "当前完整会话未核实，不能按无人播放审核")
		return
	}
	limits, err := status.Spec.Limits(observation, targets)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	view := speedLimiterReview{ID: id, Created: created, Server: c.Server, MediaID: c.MediaID, Cloud: media.cloud, HTTP: requiresHTTP, Spec: status.Spec, Observation: observation, Targets: []speedLimiterReviewTarget{}}
	for i, target := range targets {
		if blocked, err := api.queue.Unresolved(ctx, target.ID); err != nil || blocked {
			writeAPIError(w, 409, 409, "这个下载器有未确认的旧全局设置；先按原编号核对或明确忽略并停止")
			return
		}
		_, rates, err := api.openRates(ctx, target)
		if err != nil {
			speedLimiterError(w, err)
			return
		}
		desired, err := rates.Desired(target.Kind, limits[i][0], limits[i][1])
		if err != nil {
			writeAPIError(w, 409, 409, "当前下载器备用限速或目标数值不能按原普通限速语义执行，请先核对下载器")
			return
		}
		c.Baselines = append(c.Baselines, speedlimit.Baseline{Target: target, Rates: rates})
		view.Targets = append(view.Targets, speedLimiterReviewTarget{speedLimiterTarget{target.ID, target.Name, target.Kind, target.Weight}, rates, desired})
	}
	p := speedlimit.Review{ID: id, Basis: status.Basis, Control: c, Spec: status.Spec, Created: created}
	err = api.queue.SaveReview(ctx, p, func() error { return api.validate(c) })
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	view.Fingerprint = bus.Hash([]any{p.ID, p.Basis, p.Control, p.Spec, p.Created})
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": view})
}
func (api *speedLimiterAPI) activate(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		ID            string `json:"id"`
		Fingerprint   string `json:"fingerprint"`
		Confirm       bool   `json:"confirm"`
		Global        bool   `json:"global"`
		Unlimited     bool   `json:"unlimited"`
		LegacyStopped bool   `json:"legacyStopped"`
		Notify        bool   `json:"notify"`
	}
	if !decodeOrganizationRequest(w, r, &in, "无效限速授权") {
		return
	}
	p, err := api.queue.Review(r.Context(), in.ID)
	if err != nil || p.Control.Actor != actor || in.Fingerprint != bus.Hash([]any{p.ID, p.Basis, p.Control, p.Spec, p.Created}) || !in.Confirm || !in.Global || !in.Unlimited || !in.LegacyStopped || p.Spec.Notify && !in.Notify {
		writeAPIError(w, 400, 400, "重新审核并分别确认全局限速、0 为不限速、旧执行器停止及所选通知")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	guard, err := api.organizer.jobs.AcquireJob(ctx, speedLimiterGuard)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	if err := api.validate(p.Control); err != nil {
		speedLimiterError(w, err)
		return
	}
	client, err := api.connectMedia(ctx, p.Control)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	identity, err := client.RefreshIdentity(ctx)
	if err != nil || identity != p.Control.MediaID {
		speedLimiterError(w, speedlimit.ErrState)
		return
	}
	for _, b := range p.Control.Baselines {
		_, current, err := api.openRates(ctx, b.Target)
		if err != nil || current != b.Rates {
			speedLimiterError(w, speedlimit.ErrState)
			return
		}
	}
	err = api.queue.Activate(ctx, p, func() error { return api.validate(p.Control) })
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	status, err := api.status(ctx, actor)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": status})
}
func (api *speedLimiterAPI) jobAction(w http.ResponseWriter, r *http.Request, actor string) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/speed-limiter/jobs/")
	id, action, _ := strings.Cut(path, "/")
	job, err := api.queue.Job(r.Context(), id)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	if action == "" && r.Method == "GET" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": job})
		return
	}
	if r.Method != "POST" || (action != "verify" && action != "ignore") {
		writeAPIError(w, 405, 405, "限速回执只支持只读核对或明确忽略")
		return
	}
	var in struct {
		Confirm bool   `json:"confirm"`
		Ordinal *int   `json:"ordinal"`
		Reason  string `json:"reason"`
	}
	if !decodeOrganizationRequest(w, r, &in, "无效原限速处置") {
		return
	}
	if !in.Confirm {
		writeAPIError(w, 400, 400, "先明确确认原回执的处置范围")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	guard, err := api.organizer.jobs.AcquireJob(ctx, speedLimiterGuard)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	authority, err := api.authority(actor)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	validate := func() error {
		current, err := api.authority(actor)
		if err != nil || current != authority {
			return speedlimit.ErrState
		}
		return nil
	}
	if action == "ignore" {
		err = api.queue.IgnoreJob(ctx, job, actor, in.Reason, validate)
	} else {
		if in.Ordinal == nil || *in.Ordinal < 0 || *in.Ordinal >= len(job.Operations) {
			writeAPIError(w, 400, 400, "需要原回执中的设置序号")
			return
		}
		op := job.Operations[*in.Ordinal]
		if op.State != "intent" && op.State != "uncertain" {
			speedLimiterError(w, speedlimit.ErrState)
			return
		}
		target := job.Definition.Operations[*in.Ordinal].Target
		_, observed, readErr := api.openRates(ctx, target)
		if readErr != nil {
			speedLimiterError(w, readErr)
			return
		}
		err = api.queue.VerifyOperation(ctx, job, *in.Ordinal, observed, actor, validate)
	}
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	job, err = api.queue.Job(ctx, id)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": job})
}
