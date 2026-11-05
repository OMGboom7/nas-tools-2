package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/autobackup"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type autoBackupAPI struct {
	store      *autobackup.Store
	login      *siteLoginAPI
	sources    autobackup.Sources
	staticRoot string
	container  bool
	mu         sync.Mutex
	ctx        context.Context
	wait       sync.WaitGroup
	cancel     context.CancelFunc
}

type backupScope struct {
	state            autobackup.State
	settings         autobackup.Settings
	target           autobackup.Target
	location         *time.Location
	actor, authority string
}

func (s backupScope) fingerprint() string {
	return sitelogin.Hash([]any{"auto-backup-v1", s.actor, s.authority, s.state.Raw, s.state.Installed, s.target.Path, s.target.Identity, s.location.String()})
}

func (api *autoBackupAPI) readScope(ctx context.Context, actor string, legacy bool) (backupScope, error) {
	s := backupScope{actor: actor}
	var err error
	s.authority, _, err = api.login.authority(actor)
	if err != nil {
		return s, err
	}
	s.state, err = api.store.State(ctx)
	if err != nil {
		return s, err
	}
	if !legacy {
		s.settings, err = autobackup.Parse(s.state.Raw)
		if err != nil {
			return s, err
		}
	}
	s.location, err = rssWorkerLocation()
	if err != nil {
		return s, err
	}
	path, err := s.settings.Destination(api.sources.Config, api.container)
	if err != nil {
		return s, err
	}
	s.target, err = autobackup.Review(path)
	if err == nil && api.staticRoot != "" {
		rel, e := filepath.Rel(api.staticRoot, s.target.Path)
		if e != nil || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return s, autobackup.ErrScope
		}
	}
	return s, err
}

func (api *autoBackupAPI) actor(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	if api == nil || api.store == nil || api.login == nil {
		writeAPIError(w, 503, 503, "备份服务不可用")
		return "", false
	}
	actor, ok := api.login.actor(w, r, false)
	if ok && write && !api.login.pureGo {
		writeAPIError(w, 501, 501, "请在 Go 独立运行模式操作备份")
		return "", false
	}
	return actor, ok
}

func backupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeAPIError(w, 404, 404, "未找到备份记录")
	case errors.Is(err, autobackup.ErrSettings):
		writeAPIError(w, 400, 400, "请核对备份目录、保留数量和五段周期")
	case errors.Is(err, autobackup.ErrBusy), errors.Is(err, organization.ErrBusy):
		writeAPIError(w, 409, 409, "已有备份正在执行")
	default:
		writeAPIError(w, 409, 409, "备份配置、目录或记录已变化，请重新核对")
	}
}

func backupRunData(r autobackup.Run) map[string]any {
	return map[string]any{"id": r.ID, "kind": r.Kind, "state": r.State, "created": r.Created, "message": r.Message, "archive": r.Archive, "pruned": r.Pruned, "retired": r.Retired}
}

type backupInput struct {
	Fingerprint      string `json:"fingerprint"`
	Epoch            string `json:"epoch"`
	RequestID        string `json:"requestId"`
	ActiveID         string `json:"activeId"`
	Confirm          bool   `json:"confirm"`
	ConfirmRetention bool   `json:"confirmRetention"`
}

func (api *autoBackupAPI) serve(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	ctx := r.Context()
	if id := r.PathValue("id"); id != "" {
		run, err := api.store.Run(ctx, id)
		if err != nil || run.Actor != actor {
			backupError(w, sql.ErrNoRows)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/file") {
			api.send(w, r, run)
			return
		}
		signinJSON(w, backupRunData(run))
		return
	}
	var in backupInput
	action := r.PathValue("action")
	if r.Method != http.MethodGet && !siteLoginJSON(w, r, &in) {
		return
	}
	if action == "stop" {
		before, err := api.store.State(ctx)
		if err == nil && (in.Epoch != before.Control.Epoch || !in.Confirm) {
			err = autobackup.ErrScope
		}
		if err == nil {
			next := before
			// A new epoch invalidates an in-flight retention check in another process,
			// even if no periodic schedule was enabled.
			next.Control = autobackup.Control{Epoch: sitelogin.Random()}
			err = api.change(ctx, before, next, &in.ActiveID)
		}
		if err != nil {
			backupError(w, err)
			return
		}
		signinJSON(w, map[string]bool{"stopped": true})
		return
	}
	var id string
	if action == "run" {
		if !sitelogin.Hex(in.RequestID, 64) {
			backupError(w, autobackup.ErrSettings)
			return
		}
		id = sitelogin.Hash([]string{"auto-backup", actor, in.RequestID})
		if old, err := api.store.Run(ctx, id); err == nil && old.Actor == actor {
			signinJSON(w, backupRunData(old))
			return
		}
	}
	scope, err := api.readScope(ctx, actor, false)
	if err != nil {
		backupError(w, err)
		return
	}
	if !scope.state.Installed {
		backupError(w, sql.ErrNoRows)
		return
	}
	control := scope.state.Control
	if r.Method == http.MethodGet {
		runs, err := api.store.Runs(ctx, 50)
		if err != nil {
			backupError(w, err)
			return
		}
		items := []map[string]any{}
		for _, item := range runs {
			if item.Actor == actor {
				items = append(items, backupRunData(item))
			}
		}
		signinJSON(w, map[string]any{
			"goOnly": api.login.pureGo, "fingerprint": scope.fingerprint(), "target": scope.target.Path, "settings": scope.settings,
			"epoch": control.Epoch, "next": control.Next, "once": control.Once, "authorized": backupAuthorized(scope), "runs": items,
		})
		return
	}
	if !in.Confirm || in.Fingerprint != scope.fingerprint() || scope.settings.Limit() > 0 && !in.ConfirmRetention {
		backupError(w, autobackup.ErrScope)
		return
	}
	switch action {
	case "run":
		run, _, err := api.launch(ctx, scope, control, id, "manual")
		if err != nil {
			backupError(w, err)
			return
		}
		signinJSON(w, backupRunData(run))
	case "activate":
		if in.Epoch != control.Epoch {
			backupError(w, autobackup.ErrScope)
			return
		}
		if err := api.activate(ctx, scope); err != nil {
			backupError(w, err)
			return
		}
		signinJSON(w, map[string]bool{"active": true})
	default:
		backupError(w, sql.ErrNoRows)
	}
}

func (api *autoBackupAPI) change(ctx context.Context, before, next autobackup.State, id *string) error {
	api.mu.Lock()
	defer api.mu.Unlock()
	if err := api.store.Change(ctx, before, next, id); err != nil {
		return err
	}
	if (before != next || id != nil) && api.cancel != nil {
		api.cancel()
	}
	return nil
}

func backupAuthorized(s backupScope) bool {
	c := s.state.Control
	return s.state.Installed && c.Epoch != "" && c.Actor == s.actor && c.Scope == s.fingerprint() && (c.Next != "" || c.Once != "")
}

func (api *autoBackupAPI) activate(ctx context.Context, scope backupScope) error {
	now := time.Now()
	before := scope.state
	next := before
	next.Control = autobackup.Control{Epoch: sitelogin.Random(), Actor: scope.actor}
	if scope.settings.Enabled {
		at, err := cookieCloudSchedule(scope.settings.Cron, scope.location, now)
		if err != nil || at.IsZero() {
			return autobackup.ErrSettings
		}
		next.Control.Next = at.UTC().Format(time.RFC3339Nano)
	}
	if scope.settings.OnlyOnce {
		var err error
		next.Raw, err = autobackup.Merge(before.Raw, map[string]json.RawMessage{"onlyonce": json.RawMessage("false")})
		if err != nil {
			return err
		}
		next.Control.Once = now.Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	}
	if next.Control.Next == "" && next.Control.Once == "" {
		return autobackup.ErrSettings
	}
	target, err := autobackup.Prepare(scope.target)
	if err != nil {
		return err
	}
	scope.target, scope.state = target, next
	next.Control.Scope = scope.fingerprint()
	return api.change(ctx, before, next, nil)
}

func (api *autoBackupAPI) send(w http.ResponseWriter, r *http.Request, run autobackup.Run) {
	if run.State != "complete" || run.Retired {
		backupError(w, sql.ErrNoRows)
		return
	}
	f, err := autobackup.Open(r.Context(), run.Target, run.Archive)
	if err != nil {
		backupError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+run.Archive.Name+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, run.Archive.Name, time.Time{}, f)
}

func (api *autoBackupAPI) legacy(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, true)
	if !ok {
		return
	}
	scope, err := api.readScope(r.Context(), actor, true)
	if err != nil {
		backupError(w, err)
		return
	}
	run, done, err := api.launch(r.Context(), scope, scope.state.Control, sitelogin.Random(), "legacy")
	if err != nil {
		backupError(w, err)
		return
	}
	select {
	case <-r.Context().Done():
		return
	case <-done:
	}
	run, err = api.store.Run(r.Context(), run.ID)
	if err != nil {
		backupError(w, err)
		return
	}
	api.send(w, r, run)
}

func newAutoBackupAPI(store *autobackup.Store, login *siteLoginAPI, configPath, defaultCategory, dbPath, frontendDist string) *autoBackupAPI {
	_, docker := os.Stat("/.dockerenv")
	configPath, _ = filepath.Abs(configPath)
	if defaultCategory != "" {
		defaultCategory, _ = filepath.Abs(defaultCategory)
	}
	if dbPath != "" {
		dbPath, _ = filepath.Abs(dbPath)
	}
	var staticRoot string
	if frontendDist != "" {
		if resolved, err := filepath.EvalSymlinks(frontendDist); err == nil {
			staticRoot, _ = filepath.Abs(resolved)
		}
	}
	return &autoBackupAPI{store: store, login: login, container: docker == nil, staticRoot: staticRoot, sources: autobackup.Sources{
		Config: configPath, Category: filepath.Join(filepath.Dir(configPath), "default-category.yaml"), FallbackCategory: defaultCategory, Database: dbPath,
	}}
}

func backupPluginID(id string) bool { return strings.TrimSpace(id) == autobackup.Plugin }
