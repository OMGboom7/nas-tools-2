package httpserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/torrentremover"
)

type torrentRemovalAPI struct {
	store     *torrentremover.Store
	login     *siteLoginAPI
	organizer *organizationAPI
}
type torrentRemovalScope struct {
	settings         torrentremover.Settings
	actor, authority string
	targets          []downloaderconfig.Downloader
}

func removalDownloaderDigest(d downloaderconfig.Downloader) string {
	return torrentremover.DownloaderDigest(d.ID, d.Name, d.Enabled, d.Type, d.Transfer, d.OnlyNastool, d.MatchPath, d.RmtMode, d.Config, d.DownloadDir)
}
func (s torrentRemovalScope) fingerprint() string {
	return torrentremover.Hash([]any{"torrent-remover-v1", s.actor, s.authority, s.settings.Raw, s.settings.Installed, s.targets})
}
func (api *torrentRemovalAPI) actor(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	if api == nil || api.store == nil || api.organizer == nil {
		writeAPIError(w, 503, 503, "下载任务联动删除不可用")
		return "", false
	}
	actor, ok := api.login.actor(w, r, false)
	if ok && write && !api.login.pureGo {
		writeAPIError(w, 501, 501, "请在 Go 独立运行模式操作联动删除")
		return "", false
	}
	return actor, ok
}
func (api *torrentRemovalAPI) scope(ctx context.Context, actor string) (torrentRemovalScope, error) {
	s := torrentRemovalScope{actor: actor}
	var err error
	s.authority, _, err = api.login.authority(actor)
	if err != nil {
		return s, err
	}
	s.settings, err = api.store.Settings(ctx)
	if err != nil {
		return s, err
	}
	if _, err := torrentremover.Enabled(s.settings.Raw); err != nil {
		return s, err
	}
	items, err := api.organizer.service.downloaders.List(ctx)
	if err != nil {
		return s, err
	}
	for _, d := range items {
		if d.Enabled == 1 && (d.Type == "qbittorrent" || d.Type == "transmission" || d.Type == "aria2") {
			s.targets = append(s.targets, d)
		}
	}
	if len(s.targets) > 50 {
		return s, torrentremover.ErrState
	}
	return s, nil
}
func (api *torrentRemovalAPI) valid(ctx context.Context, r torrentremover.Receipt, running bool) error {
	if !api.login.pureGo || ctx.Err() != nil {
		return torrentremover.ErrState
	}
	s, err := api.scope(ctx, r.Actor)
	if err != nil || !s.settings.Installed || s.fingerprint() != r.Definition.Control.Scope {
		return torrentremover.ErrState
	}
	if running {
		return api.store.Check(ctx, r.ID)
	}
	return nil
}
func removalError(w http.ResponseWriter) {
	writeAPIError(w, 409, 409, "原范围、历史或执行状态已变化，请只读刷新并核对记录")
}
func (api *torrentRemovalAPI) serve(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, r.Method != "GET")
	if !ok {
		return
	}
	ctx := r.Context()
	action := r.PathValue("action")
	var input struct {
		Fingerprint    string `json:"fingerprint"`
		Epoch          string `json:"epoch"`
		Confirm        bool   `json:"confirm"`
		WritersStopped bool   `json:"writersStopped"`
		OldStopped     bool   `json:"oldExecutorsStopped"`
		PreserveFiles  bool   `json:"preserveFiles"`
		ID             string `json:"id"`
	}
	if r.Method != "GET" && !siteLoginJSON(w, r, &input) {
		return
	}
	if action == "stop" {
		settings, err := api.store.Settings(ctx)
		if err != nil || !input.Confirm || settings.Control.Epoch != input.Epoch {
			removalError(w)
			return
		}
		if api.store.Change(ctx, settings, nil, nil, torrentremover.Control{}) != nil {
			removalError(w)
			return
		}
		signinJSON(w, map[string]bool{"stopped": true})
		return
	}
	if r.Method == "GET" && r.PathValue("id") != "" {
		receipt, err := api.store.Get(ctx, r.PathValue("id"))
		if err != nil || receipt.Actor != actor {
			removalError(w)
			return
		}
		signinJSON(w, map[string]any{"receipt": receipt, "event": receipt.Definition.Event})
		return
	}
	s, err := api.scope(ctx, actor)
	if err != nil {
		removalError(w)
		return
	}
	if r.Method == "GET" {
		receipts, err := api.store.List(ctx, "")
		if err != nil {
			removalError(w)
			return
		}
		public := []map[string]any{}
		for _, receipt := range receipts {
			if receipt.Actor == actor {
				public = append(public, map[string]any{"receipt": receipt, "event": receipt.Definition.Event})
			}
		}
		targets := []map[string]string{}
		for _, d := range s.targets {
			var cfg map[string]any
			_ = json.Unmarshal([]byte(d.Config), &cfg)
			host := text(cfg["host"])
			if !strings.Contains(host, "://") {
				host = "http://" + host
			}
			u, _ := url.Parse(host)
			receiver := ""
			if u != nil && u.User == nil {
				if port := text(cfg["port"]); port != "" {
					if n, e := strconv.Atoi(port); e == nil && n > 0 && n <= 65535 {
						u.Host = net.JoinHostPort(u.Hostname(), port)
					}
				}
				receiver = u.Scheme + "://" + u.Host
			}
			targets = append(targets, map[string]string{"id": strconv.FormatInt(d.ID, 10), "name": d.Name, "type": d.Type, "receiver": receiver})
		}
		active := s.settings.Installed && s.settings.Control.Valid() && s.settings.Control.Scope == s.fingerprint() && api.login.pureGo
		enabled, _ := torrentremover.Enabled(s.settings.Raw)
		signinJSON(w, map[string]any{"fingerprint": s.fingerprint(), "goOnly": api.login.pureGo, "installed": s.settings.Installed, "enabled": enabled, "active": active, "epoch": s.settings.Control.Epoch, "targets": targets, "records": public})
		return
	}
	if !input.Confirm || input.Fingerprint != s.fingerprint() {
		removalError(w)
		return
	}
	switch action {
	case "activate":
		enabled, _ := torrentremover.Enabled(s.settings.Raw)
		if !s.settings.Installed || !enabled || !input.OldStopped || !input.WritersStopped || !input.PreserveFiles || len(s.targets) == 0 {
			removalError(w)
			return
		}
		c := torrentremover.Control{Exclusive: true, Actor: actor, Epoch: sitelogin.Random(), Scope: s.fingerprint(), Config: torrentremover.Hash(s.settings.Raw), Targets: []torrentremover.Target{}}
		for _, d := range s.targets {
			c.Targets = append(c.Targets, torrentremover.Target{ID: strconv.FormatInt(d.ID, 10), Name: d.Name, Type: d.Type, Digest: removalDownloaderDigest(d)})
		}
		if api.store.Change(ctx, s.settings, nil, nil, c) != nil {
			removalError(w)
			return
		}
		signinJSON(w, map[string]bool{"activated": true})
	case "reconcile":
		receipt, err := api.store.Get(ctx, input.ID)
		if err != nil || receipt.Actor != actor {
			removalError(w)
			return
		}
		if api.reconcile(ctx, receipt, s.fingerprint()) != nil {
			removalError(w)
			return
		}
		fresh, err := api.store.Get(ctx, receipt.ID)
		if err != nil {
			removalError(w)
			return
		}
		signinJSON(w, map[string]any{"receipt": fresh, "event": fresh.Definition.Event})
	default:
		writeAPIError(w, 404, 404, "未找到联动删除操作")
	}
}
func (api *torrentRemovalAPI) acquire(ctx context.Context) (*organization.JobGuard, error) {
	return api.organizer.jobs.AcquireJob(ctx, "native-torrent-remover")
}
func startTorrentRemovalWorker(ctx context.Context, api *torrentRemovalAPI) func() {
	if api == nil || api.store == nil || !api.login.pureGo {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		api.tick(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				api.tick(ctx)
			}
		}
	}()
	return func() { <-done }
}
