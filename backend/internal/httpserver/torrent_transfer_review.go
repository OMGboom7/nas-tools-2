package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

type transferReviewScope struct {
	raw, installedRaw     string
	digest                func(string) string
	settings              torrenttransfer.Settings
	source                torrenttransfer.ProtectedSource
	target                torrenttransfer.ReceiverClient
	from, to, fingerprint string
}

func (api torrentTransferAPI) reviewScope(ctx context.Context, actor string) (transferReviewScope, error) {
	var scope transferReviewScope
	if api.system == nil || api.organizer == nil || api.downloaders == nil || !api.login.pureGo {
		return scope, torrenttransfer.ErrState
	}
	authority, _, err := api.login.authority(actor)
	if err != nil {
		return scope, err
	}
	values, err := api.system.ReadMany(ctx, []string{"UserInstalledPlugins", torrenttransfer.ConfigKey})
	if err != nil {
		return scope, err
	}
	installed, err := decodeInstalledPlugins(values["UserInstalledPlugins"])
	if err != nil || !slices.Contains(installed, torrenttransfer.Plugin) {
		return scope, torrenttransfer.ErrState
	}
	raw := values[torrenttransfer.ConfigKey]
	scope.settings, err = torrenttransfer.Parse(raw)
	if err != nil || scope.settings.Ready() != nil {
		return scope, torrenttransfer.ErrState
	}
	load := func(id string) (downloaderconfig.Downloader, error) {
		number, err := syncConfigID(id)
		if err != nil {
			return downloaderconfig.Downloader{}, err
		}
		d, err := api.downloaders.Get(ctx, number)
		if err == nil && (d.Enabled != 1 || d.Type != "qbittorrent" && d.Type != "transmission") {
			err = torrenttransfer.ErrState
		}
		return d, err
	}
	from, err := load(scope.settings.From[0])
	if err != nil {
		return scope, err
	}
	to, err := load(scope.settings.To[0])
	if err != nil {
		return scope, err
	}
	scope.source, err = api.organizer.transferSource(from)
	if err != nil {
		return scope, err
	}
	c, err := api.organizer.clientFromDownloader(to)
	if err != nil {
		return scope, err
	}
	var ok bool
	scope.target, ok = c.transfer.(torrenttransfer.ReceiverClient)
	if !ok || scope.source.Address() == scope.target.Address() {
		return scope, torrenttransfer.ErrState
	}
	scope.from, scope.to = from.Name, to.Name
	scope.raw, scope.installedRaw = raw, values["UserInstalledPlugins"]
	scope.digest = func(raw string) string {
		return torrenttransfer.Hash([]string{"transfer-review-v1", actor, authority, raw, removalDownloaderDigest(from), removalDownloaderDigest(to)})
	}
	scope.fingerprint = scope.digest(raw)
	return scope, nil
}

type transferReviewItem struct {
	Receipt   string `json:"receipt"`
	Name      string `json:"name"`
	SourceID  string `json:"sourceId"`
	TargetID  string `json:"targetId"`
	Directory string `json:"directory"`
}

func (api torrentTransferAPI) review(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.login.actor(w, r, true)
	if !ok {
		return
	}
	var input struct {
		Confirm bool `json:"confirmRead"`
	}
	if !siteLoginJSON(w, r, &input) {
		return
	}
	if !input.Confirm {
		writeAPIError(w, 400, 400, "请确认读取下载任务和种子目录")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	scope, err := api.reviewScope(ctx, actor)
	if err != nil {
		writeAPIError(w, 409, 409, "请核对转种插件、源和目标下载器配置")
		return
	}
	root, err := torrenttransfer.OpenMetainfoRoot(scope.settings.TorrentPath)
	if err != nil {
		writeAPIError(w, 409, 409, "无法读取已配置的种子目录")
		return
	}
	defer root.Close()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: api.database}).String()+"?mode=rw")
	if err != nil {
		writeAPIError(w, 503, 503, "转种记录库不可用")
		return
	}
	defer db.Close()
	ledger, err := torrenttransfer.NewLedger(ctx, db)
	if err != nil {
		writeAPIError(w, 503, 503, "无法打开转种账本")
		return
	}
	items, existing, failed, err := collectTransferPlans(ctx, scope, root, ledger)
	if err != nil {
		writeAPIError(w, 502, 502, "无法完成源任务扫描或计划保存")
		return
	}
	current, err := api.reviewScope(ctx, actor)
	if err != nil || current.fingerprint != scope.fingerprint {
		writeAPIError(w, 409, 409, "配置已变更，请重新审核")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "result": map[string]any{"source": scope.from, "target": scope.to, "items": items, "existing": existing, "failed": failed}})
}

func collectTransferPlans(ctx context.Context, scope transferReviewScope, root *torrenttransfer.MetainfoRoot, ledger *torrenttransfer.Ledger) ([]transferReviewItem, int, int, error) {
	items := []transferReviewItem{}
	existing, failed := 0, 0
	for src, err := range torrenttransfer.ScanSources(ctx, scope.settings, scope.source, root) {
		if err != nil {
			if src.Task.WireID == "" {
				return nil, 0, 0, err
			}
			failed++
			continue
		}
		plan, err := torrenttransfer.PreparePlan(ctx, src, scope.target, scope.fingerprint)
		if err != nil {
			failed++
			continue
		}
		if _, err := scope.target.TransferTask(ctx, plan.WireID); err == nil {
			existing++
			continue
		} else if !errors.Is(err, torrenttransfer.ErrTaskAbsent) {
			failed++
			continue
		}
		id := torrenttransfer.Hash(plan)
		if err := ledger.SavePlan(ctx, id, plan); err != nil {
			return nil, 0, 0, err
		}
		items = append(items, transferReviewItem{id, src.Task.Name, src.Task.WireID, plan.WireID, src.Directory})
	}
	return items, existing, failed, ctx.Err()
}
