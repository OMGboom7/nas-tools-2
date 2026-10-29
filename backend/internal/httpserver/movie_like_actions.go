package httpserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/movielike"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type movieLikeReview struct {
	ID          string           `json:"id"`
	Fingerprint string           `json:"fingerprint"`
	Created     string           `json:"created"`
	MediaID     string           `json:"mediaId"`
	HTTP        bool             `json:"http"`
	Spec        movielike.Spec   `json:"spec"`
	Roots       []movielike.Root `json:"roots"`
	Categories  []string         `json:"categories"`
}
type movieLikeConsents struct {
	Confirm       bool `json:"confirm"`
	Move          bool `json:"move"`
	Whole         bool `json:"whole"`
	SourceRemoval bool `json:"sourceRemoval"`
	CloseRelated  bool `json:"closeRelated"`
	LegacyStopped bool `json:"legacyStopped"`
	Refresh       bool `json:"refresh"`
}

func (c movieLikeConsents) valid() bool {
	return c.Confirm && c.Move && c.Whole && c.SourceRemoval && c.CloseRelated && c.LegacyStopped && c.Refresh
}
func (api *movieLikeAPI) review(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		Fingerprint string   `json:"fingerprint"`
		ConfirmRead bool     `json:"confirmRead"`
		HTTP        bool     `json:"http"`
		Roots       []string `json:"rootIds"`
	}
	if !decodeOrganizationRequest(w, r, &in, "无效只读电影精选审核") {
		return
	}
	status, err := api.status(r.Context(), actor)
	if err != nil || !status.Ready || !in.ConfirmRead || in.Fingerprint != status.Fingerprint || len(in.Roots) == 0 || len(in.Roots) > 128 {
		writeAPIError(w, 400, 400, "先保存有效原配置，选择完整电影库范围并确认只读审核")
		return
	}
	snapshot, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		movieLikeError(w, err)
		return
	}
	http := plainHTTP(text(objectValue(snapshot["emby"])["host"]))
	if http && !in.HTTP {
		writeAPIError(w, 400, 400, "先明确允许向原明文 Emby 连接传送已有凭据")
		return
	}
	authority, err := api.authority(actor)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	app, err := api.appDigest()
	if err != nil {
		movieLikeError(w, err)
		return
	}
	category, names, err := api.categories()
	if err != nil {
		movieLikeError(w, err)
		return
	}
	raw, err := api.system.Get(r.Context(), movielike.ConfigKey)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	epoch, err := refreshNonce()
	if err != nil {
		movieLikeError(w, err)
		return
	}
	id, err := refreshNonce()
	if err != nil {
		movieLikeError(w, err)
		return
	}
	c := movielike.Control{Epoch: epoch, Actor: actor, Authority: authority, AppDigest: app, CategoryDigest: category, Categories: names, ConfigDigest: movielike.Hash(raw), Created: movielike.Now(), HTTP: http, Roots: []movielike.Root{}}
	seen := map[string]bool{}
	for _, id := range in.Roots {
		if seen[id] {
			movieLikeError(w, movielike.ErrState)
			return
		}
		seen[id] = true
		found := false
		for _, root := range status.Roots {
			if root.ID != id {
				continue
			}
			current, err := libraryRootSnapshot(root)
			if err != nil {
				movieLikeError(w, err)
				return
			}
			c.Roots = append(c.Roots, movielike.Root{ID: current.ID, Path: current.Path, Identity: current.Identity, ConfiguredPath: root.Path})
			found = true
		}
		if !found {
			movieLikeError(w, movielike.ErrState)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	client, err := api.client()
	if err != nil {
		movieLikeError(w, err)
		return
	}
	c.MediaID, err = client.RefreshIdentity(ctx)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	p := movielike.Review{ID: id, Basis: status.Basis, Created: c.Created, Control: c, Spec: status.Spec}
	if err := api.queue.SaveReview(ctx, p, func() error { return api.validate(c) }); err != nil {
		movieLikeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": movieLikeReview{ID: id, Fingerprint: movielike.Hash(p), Created: p.Created, MediaID: c.MediaID, HTTP: http, Spec: p.Spec, Roots: c.Roots, Categories: c.Categories}})
}
func (api *movieLikeAPI) activate(w http.ResponseWriter, r *http.Request, actor string) {
	var in struct {
		ID          string `json:"id"`
		Fingerprint string `json:"fingerprint"`
		movieLikeConsents
	}
	if !decodeOrganizationRequest(w, r, &in, "无效电影精选授权") {
		return
	}
	p, err := api.queue.Review(r.Context(), in.ID)
	if err != nil || p.Control.Actor != actor || movielike.Hash(p) != in.Fingerprint || !in.movieLikeConsents.valid() {
		writeAPIError(w, 400, 400, "需要完整原审核及各项独立移动、源清理、目录、旧写入权和刷新确认")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	guard, err := api.organizer.jobs.AcquireJob(ctx, movieLikeGuard)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	if err := api.validate(p.Control); err != nil {
		movieLikeError(w, err)
		return
	}
	client, err := api.client()
	if err != nil {
		movieLikeError(w, err)
		return
	}
	identity, err := client.RefreshIdentity(ctx)
	if err != nil || identity != p.Control.MediaID {
		movieLikeError(w, movielike.ErrState)
		return
	}
	if err := api.queue.Activate(ctx, p, func() error { return api.validate(p.Control) }); err != nil {
		movieLikeError(w, err)
		return
	}
	status, err := api.status(ctx, actor)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": status})
}

type movieLikeExecution struct {
	Receipt movielike.Receipt               `json:"receipt"`
	Move    organization.FavoriteMoveReview `json:"move"`
}

func (api *movieLikeAPI) scopeCurrent(ctx context.Context, s organization.RecoveryScope) error {
	if err := api.organizer.recoveryScopeCurrent(ctx, s); err != nil {
		return err
	}
	roots, err := api.movieRoots(ctx)
	if err != nil {
		return err
	}
	for _, root := range roots {
		if root.ID != s.Root.ID {
			continue
		}
		current, err := libraryRootSnapshot(root)
		if err != nil || current.Path != s.Root.Path || current.Identity != s.Root.Identity {
			return movielike.ErrState
		}
		return nil
	}
	return movielike.ErrState
}
func (api *movieLikeAPI) receiptAction(w http.ResponseWriter, r *http.Request, actor string) {
	id, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/v1/movie-like/receipts/"), "/")
	p, err := api.queue.Receipt(r.Context(), id)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	if r.Method == "GET" && action == "" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": p})
		return
	}
	if r.Method != "POST" || (action != "review" && action != "continue" && action != "ignore") {
		writeAPIError(w, 405, 405, "仅支持原回执只读审核、独立继续或明确停止")
		return
	}
	var in struct {
		movieLikeConsents
		ConfirmRead bool   `json:"confirmRead"`
		Fingerprint string `json:"fingerprint"`
		Reason      string `json:"reason"`
	}
	if !decodeOrganizationRequest(w, r, &in, "无效电影精选原回执操作") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	guard, err := api.organizer.jobs.AcquireJob(ctx, movieLikeGuard)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	authority, err := api.authority(actor)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	p, err = api.queue.Receipt(ctx, id)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	currentActor := func() error {
		now, err := api.authority(actor)
		if err != nil || now != authority {
			return movielike.ErrState
		}
		return nil
	}
	if action == "ignore" {
		if !in.Confirm {
			writeAPIError(w, 400, 400, "需要确认停止这份原移动并保留现有对象")
			return
		}
		if err := api.organizer.jobs.IgnoreFavoriteMove(ctx, id, actor, in.Reason, currentActor); err != nil {
			movieLikeError(w, err)
			return
		}
		p, err = api.queue.Receipt(ctx, id)
		if err != nil {
			movieLikeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": p})
		return
	}
	if !in.ConfirmRead || len(p.Work) == 0 {
		writeAPIError(w, 400, 400, "先确认读取同一完整原目录及已有移动凭证")
		return
	}
	v, err := api.organizer.jobs.ReviewFavoriteMove(ctx, id)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	closeGuards, err := api.organizer.recoveryGuards(ctx, organization.RecoveryDefinition{Owners: v.Definition.Owners})
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer closeGuards()
	v, err = api.organizer.jobs.ReviewFavoriteMove(ctx, id)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	validate := func() error {
		if err := currentActor(); err != nil {
			return err
		}
		return api.scopeCurrent(ctx, v.Definition.Scope)
	}
	if err := validate(); err != nil {
		movieLikeError(w, err)
		return
	}
	if action == "review" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": movieLikeExecution{p, v}})
		return
	}
	if !in.movieLikeConsents.valid() || in.Fingerprint != v.Fingerprint || !v.CanContinue {
		writeAPIError(w, 400, 400, "重新核对原完整范围并逐项确认本次独立继续")
		return
	}
	if err := api.organizer.jobs.ExecuteFavoriteMove(ctx, v, actor, false, validate); err != nil {
		writeOrganizationError(w, err)
		return
	}
	p, err = api.queue.Receipt(ctx, id)
	if err != nil {
		movieLikeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": p})
}
