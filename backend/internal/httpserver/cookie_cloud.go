package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/cookiecloud"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type cookieCloudAPI struct {
	store     *cookiecloud.Store
	login     *siteLoginAPI
	mu        sync.Mutex
	lifecycle context.Context
	cancel    context.CancelFunc
	wait      sync.WaitGroup
}

type cookieCloudScope struct {
	actor, raw, basis string
	installed         bool
	settings          cookiecloud.Settings
	settingsInvalid   bool
	catalog           indexercatalog.Catalog
	location          *time.Location
	app               map[string]any
}

func (s cookieCloudScope) fingerprint(raw string) string {
	return sitelogin.Hash([]string{"cookie-cloud-v1", s.basis, raw})
}

func (api *cookieCloudAPI) scope(ctx context.Context, actor string) (cookieCloudScope, error) {
	out := cookieCloudScope{actor: actor}
	authority, cfg, err := api.login.authority(actor)
	if err != nil {
		return out, err
	}
	out.raw, out.installed, err = api.store.Settings(ctx)
	if err != nil {
		return out, err
	}
	out.settings, err = cookiecloud.Parse(out.raw)
	if err != nil {
		out.settings = cookiecloud.Settings{Mode: "all_mode"}
		out.settingsInvalid = true
	}
	if api.login.catalogPath != "" {
		out.catalog, err = indexercatalog.Load(api.login.catalogPath)
		if err != nil {
			return out, err
		}
	}
	out.location, err = rssWorkerLocation()
	if err != nil {
		return out, err
	}
	out.app = objectValue(cfg["app"])
	out.basis = sitelogin.Hash([]any{actor, authority, out.installed, out.catalog, out.location.String(), out.app["proxies"], out.app["user_agent"]})
	return out, nil
}

func (api *cookieCloudAPI) actor(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	if api == nil || api.store == nil || api.login == nil {
		writeAPIError(w, 503, 503, "CookieCloud 服务不可用")
		return "", false
	}
	actor, ok := api.login.actor(w, r, false)
	if ok && write && !api.login.pureGo {
		writeAPIError(w, 501, 501, "请在 Go 独立运行模式修改或执行 CookieCloud")
		return "", false
	}
	return actor, ok
}

func cookieCloudError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeAPIError(w, 404, 404, "未找到原同步记录")
	case errors.Is(err, cookiecloud.ErrConfig):
		writeAPIError(w, 400, 400, "CookieCloud 设置无效，请核对地址、凭据、过滤方式和五段周期")
	case errors.Is(err, cookiecloud.ErrBusy), errors.Is(err, organization.ErrBusy):
		writeAPIError(w, 409, 409, "原同步正在执行，请查看记录")
	default:
		writeAPIError(w, 409, 409, "CookieCloud 设置或执行状态已变化，请重新读取")
	}
}

func cookieCloudSchedule(raw string, loc *time.Location, now time.Time) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if len(strings.Fields(raw)) != 5 {
		return time.Time{}, cookiecloud.ErrConfig
	}
	schedule, err := parseRSSSchedule(raw, loc)
	if err != nil {
		return time.Time{}, cookiecloud.ErrConfig
	}
	next := schedule.next(now)
	if next.IsZero() {
		return next, cookiecloud.ErrConfig
	}
	return next, nil
}

type cookieCloudPublicSettings struct {
	Server             string `json:"server"`
	Cron               string `json:"cron"`
	Mode               string `json:"synchronousMode"`
	BlackList          string `json:"black_list"`
	WhiteList          string `json:"white_list"`
	Notify             bool   `json:"notify"`
	OnlyOnce           bool   `json:"onlyonce"`
	KeyConfigured      bool   `json:"keyConfigured"`
	PasswordConfigured bool   `json:"passwordConfigured"`
}

func cookieCloudValues(s cookiecloud.Settings) cookieCloudPublicSettings {
	return cookieCloudPublicSettings{s.Server, s.Cron, s.Mode, s.BlackList, s.WhiteList, s.Notify, s.OnlyOnce, s.Key != "", s.Password != ""}
}

func (api *cookieCloudAPI) save(ctx context.Context, scope cookieCloudScope, changes map[string]json.RawMessage) error {
	next, err := cookiecloud.Merge(scope.raw, changes)
	if err != nil {
		return err
	}
	settings, err := cookiecloud.Parse(next)
	if err != nil {
		return err
	}
	if _, err = cookieCloudSchedule(settings.Cron, scope.location, time.Now()); err != nil {
		return err
	}
	return api.mutateControl(next != scope.raw, func() error {
		return api.store.Save(ctx, scope.raw, next)
	})
}

func (api *cookieCloudAPI) mutateControl(cancelRun bool, update func() error) error {
	api.mu.Lock()
	defer api.mu.Unlock()
	if err := update(); err != nil {
		return err
	}
	if cancelRun && api.cancel != nil {
		api.cancel()
	}
	return nil
}

func (api *cookieCloudAPI) serve(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	ctx := r.Context()
	if id := r.PathValue("id"); id != "" {
		if !sitelogin.Hex(id, 64) {
			cookieCloudError(w, sql.ErrNoRows)
			return
		}
		run, err := api.store.Run(ctx, id)
		if err != nil || run.Actor != actor {
			cookieCloudError(w, sql.ErrNoRows)
			return
		}
		signinJSON(w, run)
		return
	}
	var in struct {
		Fingerprint string                     `json:"fingerprint"`
		Epoch       string                     `json:"epoch"`
		RequestID   string                     `json:"requestId"`
		ActiveRunID string                     `json:"activeRunId"`
		Confirm     bool                       `json:"confirm"`
		Values      map[string]json.RawMessage `json:"values"`
	}
	if r.Method != http.MethodGet && !siteLoginJSON(w, r, &in) {
		return
	}
	action := r.PathValue("action")
	if action == "stop" {
		if !in.Confirm {
			cookieCloudError(w, cookiecloud.ErrScope)
			return
		}
		err := api.mutateControl(true, func() error { return api.store.Stop(ctx, in.Epoch, in.ActiveRunID) })
		if err != nil {
			cookieCloudError(w, err)
			return
		}
		signinJSON(w, map[string]bool{"stopped": true})
		return
	}
	scope, err := api.scope(ctx, actor)
	if err != nil {
		cookieCloudError(w, err)
		return
	}
	if !scope.installed {
		writeAPIError(w, 404, 404, "CookieCloud 插件尚未安装")
		return
	}
	fingerprint := scope.fingerprint(scope.raw)
	if r.Method == http.MethodGet {
		control, err := api.store.Control(ctx)
		if err != nil {
			cookieCloudError(w, err)
			return
		}
		runs, err := api.store.Runs(ctx)
		if err != nil {
			cookieCloudError(w, err)
			return
		}
		signinJSON(w, map[string]any{
			"goOnly": api.login.pureGo, "fingerprint": fingerprint, "settings": cookieCloudValues(scope.settings), "receiver": scope.settings.Receiver(),
			"control": control, "current": controlCurrent(control, scope), "runs": runs, "settingsInvalid": scope.settingsInvalid,
		})
		return
	}
	var runID string
	if action == "run" || action == "test" {
		if !in.Confirm || !sitelogin.Hex(in.RequestID, 64) {
			cookieCloudError(w, cookiecloud.ErrConfig)
			return
		}
		runID = sitelogin.Hash([]string{"cookie-cloud", actor, action, in.RequestID})
		if old, e := api.store.Run(ctx, runID); e == nil && old.Actor == actor {
			signinJSON(w, old)
			return
		}
	}
	if in.Fingerprint != fingerprint {
		cookieCloudError(w, cookiecloud.ErrScope)
		return
	}
	switch action {
	case "settings":
		err = api.save(ctx, scope, in.Values)
		if err == nil {
			signinJSON(w, map[string]bool{"saved": true})
			return
		}
	case "activate":
		if !in.Confirm || !scope.settings.Ready() {
			cookieCloudError(w, cookiecloud.ErrConfig)
			return
		}
		now := time.Now()
		next, err := cookieCloudSchedule(scope.settings.Cron, scope.location, now)
		if err != nil || next.IsZero() && !scope.settings.OnlyOnce {
			cookieCloudError(w, cookiecloud.ErrConfig)
			return
		}
		control := cookiecloud.Control{Epoch: sitelogin.Random(), Actor: actor}
		updated := scope.raw
		if !next.IsZero() {
			control.NextRun = next.UTC().Format(time.RFC3339Nano)
		}
		if scope.settings.OnlyOnce {
			updated, err = cookiecloud.Merge(scope.raw, map[string]json.RawMessage{"onlyonce": json.RawMessage("false")})
			if err != nil {
				cookieCloudError(w, err)
				return
			}
			control.NextOnce = now.Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
		}
		control.Scope = scope.fingerprint(updated)
		if err = api.mutateControl(true, func() error {
			return api.store.Activate(ctx, scope.raw, updated, in.Epoch, control)
		}); err != nil {
			cookieCloudError(w, err)
			return
		}
		signinJSON(w, control)
		return
	case "run", "test":
		if !scope.settings.Ready() {
			cookieCloudError(w, cookiecloud.ErrConfig)
			return
		}
		kind := "manual"
		if action == "test" {
			kind = "test"
		}
		var run cookiecloud.Run
		run, err = api.launch(ctx, scope, cookiecloud.Run{ID: runID, Kind: kind, Actor: actor, Scope: fingerprint}, cookiecloud.Control{}, cookiecloud.Control{})
		if err == nil {
			signinJSON(w, run)
			return
		}
	default:
		writeAPIError(w, 404, 404, "未找到 CookieCloud 操作")
		return
	}
	cookieCloudError(w, err)
}

func controlCurrent(c cookiecloud.Control, scope cookieCloudScope) bool {
	return scope.installed && scope.settings.Ready() && c.Epoch != "" && (c.NextOnce != "" || c.NextRun != "") &&
		c.Actor == scope.actor && c.Scope == scope.fingerprint(scope.raw)
}
