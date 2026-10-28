package httpserver

import (
	"context"
	"database/sql"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediasyncdel"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"net/http"
	"strings"
	"time"
)

type mediaSyncDeleteReview struct {
	ID          string              `json:"id"`
	Fingerprint string              `json:"fingerprint"`
	Created     string              `json:"created"`
	MediaID     string              `json:"mediaId"`
	HTTP        bool                `json:"http"`
	Spec        mediasyncdel.Spec   `json:"spec"`
	Roots       []mediasyncdel.Root `json:"roots"`
}

func (api *mediaSyncDeleteAPI) review(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		Fingerprint string   `json:"fingerprint"`
		ConfirmRead bool     `json:"confirmRead"`
		HTTP        bool     `json:"http"`
		Roots       []string `json:"rootIds"`
	}
	if !decodeOrganizationRequest(w, r, &in, "无效只读同步删除审核") {
		return
	}
	status, err := api.status(r.Context(), actor)
	if err != nil || !status.Ready || !in.ConfirmRead || status.Fingerprint != in.Fingerprint || len(in.Roots) > 256 {
		writeAPIError(w, 400, 400, "先保存有效原配置，再确认读取审核范围")
		return
	}
	snapshot, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	needsHTTP := plainHTTP(text(objectValue(snapshot["emby"])["host"]))
	if needsHTTP && !in.HTTP {
		writeAPIError(w, 400, 400, "先明确允许向原明文 Emby 连接传送已有凭据")
		return
	}
	authority, err := api.authority(actor)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	app, err := api.appDigest()
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	epoch, err := refreshNonce()
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	id, err := refreshNonce()
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	raw, err := api.system.Get(r.Context(), mediasyncdel.ConfigKey)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	c := mediasyncdel.Control{Epoch: epoch, Actor: actor, Authority: authority, AppDigest: app, ConfigDigest: mediasyncdel.ConfigDigest(raw), Created: time.Now().UTC().Format(time.RFC3339Nano), HTTP: in.HTTP, Roots: []mediasyncdel.Root{}}
	seen := map[string]bool{}
	if !status.Spec.DeleteSource && len(in.Roots) > 0 || status.Spec.DeleteSource && len(in.Roots) == 0 {
		writeAPIError(w, 400, 400, "按原删除源文件开关选择完整允许的源目录，历史记录模式不授权文件范围")
		return
	}
	for _, selected := range in.Roots {
		if seen[selected] {
			writeAPIError(w, 400, 400, "源目录重复")
			return
		}
		seen[selected] = true
		found := false
		for _, root := range status.Roots {
			if root.ID != selected {
				continue
			}
			current, err := libraryRootSnapshot(root)
			if err != nil {
				mediaSyncDeleteError(w, err)
				return
			}
			c.Roots = append(c.Roots, mediasyncdel.Root{ID: current.ID, Path: current.Path, Identity: current.Identity})
			found = true
		}
		if !found {
			writeAPIError(w, 400, 400, "所选源目录已变化")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	client, err := api.client()
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	c.MediaID, err = client.RefreshIdentity(ctx)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	p := mediasyncdel.Review{ID: id, Basis: status.Basis, Control: c, Spec: status.Spec, Created: c.Created}
	if err := api.queue.SaveReview(ctx, p, func() error { return api.validate(c) }); err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	view := mediaSyncDeleteReview{ID: id, Fingerprint: bus.Hash(p), Created: p.Created, MediaID: c.MediaID, HTTP: needsHTTP, Spec: p.Spec, Roots: c.Roots}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": view})
}

type mediaSyncDeleteConsents struct {
	Confirm       bool `json:"confirm"`
	History       bool `json:"history"`
	Dedupe        bool `json:"dedupe"`
	CloseRelated  bool `json:"closeRelated"`
	Source        bool `json:"source"`
	OnlyCopy      bool `json:"onlyCopy"`
	Expanded      bool `json:"expanded"`
	LegacyStopped bool `json:"legacyStopped"`
	Notify        bool `json:"notify"`
}

func (in mediaSyncDeleteConsents) valid(spec mediasyncdel.Spec, files bool) bool {
	return in.Confirm && in.History && in.Dedupe && in.CloseRelated && in.LegacyStopped && (!files || in.Source && in.OnlyCopy && in.Expanded) && (!spec.Notify || in.Notify)
}
func (api *mediaSyncDeleteAPI) activate(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		mediaSyncDeleteConsents
		ID          string `json:"id"`
		Fingerprint string `json:"fingerprint"`
	}
	if !decodeOrganizationRequest(w, r, &in, "无效同步删除授权") {
		return
	}
	p, err := api.queue.Review(r.Context(), in.ID)
	if err != nil || p.Control.Actor != actor || in.Fingerprint != bus.Hash(p) || !in.mediaSyncDeleteConsents.valid(p.Spec, p.Spec.DeleteSource) {
		writeAPIError(w, 400, 400, "重新读取审核并分别确认历史、去重、关联执行器、源文件范围与所选通知")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	guard, err := api.organizer.jobs.AcquireJob(ctx, mediaSyncDeleteGuard)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	if err := api.validate(p.Control); err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	client, err := api.client()
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	identity, err := client.RefreshIdentity(ctx)
	if err != nil || identity != p.Control.MediaID {
		mediaSyncDeleteError(w, mediasyncdel.ErrState)
		return
	}
	if err := api.queue.Activate(ctx, p, func() error { return api.validate(p.Control) }); err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	status, err := api.status(ctx, actor)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": status})
}

type mediaSyncDeleteExecutionReview struct {
	Receipt mediasyncdel.Receipt            `json:"receipt"`
	Parent  organization.MediaRemovalReview `json:"parent"`
}

func (api *mediaSyncDeleteAPI) receiptAction(w http.ResponseWriter, r *http.Request, actor string) {
	id, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/v1/media-sync-delete/receipts/"), "/")
	p, err := api.queue.Receipt(r.Context(), id)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	if r.Method == "GET" && action == "" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": p})
		return
	}
	if r.Method != "POST" || (action != "ignore" && action != "review" && action != "continue") {
		writeAPIError(w, 405, 405, "仅支持原回执只读审核、独立继续或明确忽略")
		return
	}
	var in struct {
		mediaSyncDeleteConsents
		ConfirmRead bool   `json:"confirmRead"`
		Reason      string `json:"reason"`
		Fingerprint string `json:"fingerprint"`
	}
	if !decodeOrganizationRequest(w, r, &in, "无效原同步删除处置") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	guard, err := api.organizer.jobs.AcquireJob(ctx, mediaSyncDeleteGuard)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	authority, err := api.authority(actor)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	validateActor := func() error {
		current, err := api.authority(actor)
		if err != nil || current != authority {
			return mediasyncdel.ErrState
		}
		return nil
	}
	if action == "ignore" {
		if !in.Confirm {
			writeAPIError(w, 400, 400, "需要确认忽略这份原未完成回执并停止后台")
			return
		}
		if err := api.queue.Ignore(ctx, id, actor, in.Reason, validateActor); err != nil {
			mediaSyncDeleteError(w, err)
			return
		}
		current, err := api.queue.Receipt(ctx, id)
		if err != nil {
			mediaSyncDeleteError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": current})
		return
	}
	if !in.ConfirmRead || p.Parent == "" {
		writeAPIError(w, 400, 400, "先确认只读读取已关联的原完整删除范围")
		return
	}
	v, err := api.organizer.jobs.MediaRemoval(ctx, p.Parent)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if v.Definition.MediaSyncDelete == nil || v.Definition.MediaSyncDelete.Receipt != id || organization.Digest(v.Definition) != p.ParentFingerprint {
		mediaSyncDeleteError(w, mediasyncdel.ErrState)
		return
	}
	closeGuards, err := api.organizer.recoveryGuards(ctx, organization.RecoveryDefinition{Owners: v.Definition.Owners})
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer closeGuards()
	clients, closeClients, err := api.organizer.mediaRemovalClients(ctx, v)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer closeClients()
	validate := func(call context.Context) error {
		if err := validateActor(); err != nil {
			return err
		}
		for _, u := range v.Definition.Units {
			if err := api.organizer.recoveryScopeCurrent(call, u.Definition.Scope); err != nil {
				return err
			}
		}
		return nil
	}
	call, stop := recoveryWatch(ctx, validate)
	defer stop()
	reviewed, err := api.organizer.jobs.ReviewMediaRemoval(call, v, clients)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if action == "review" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": mediaSyncDeleteExecutionReview{p, reviewed}})
		return
	}
	if !in.mediaSyncDeleteConsents.valid(p.Policy, len(v.Units) > 0) || in.Fingerprint != reviewed.Fingerprint || !reviewed.CanContinue {
		writeAPIError(w, 400, 400, "需要原完整范围的新审核与各项独立继续确认，不会自动重做")
		return
	}
	err = api.organizer.jobs.ExecuteMediaRemoval(call, reviewed, clients, reviewed.Fingerprint, func(scope organization.RecoveryScope) error {
		if err := validateActor(); err != nil {
			return err
		}
		return api.organizer.recoveryScopeCurrent(call, scope)
	}, func(call context.Context, tx *sql.Tx) error { return validateActor() })
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	current, err := api.queue.Receipt(ctx, id)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": current})
}
