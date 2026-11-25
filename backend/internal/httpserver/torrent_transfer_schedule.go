package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

const transferScheduleKey = "GoTorrentTransferSchedule"

var transferScheduleKeys = []string{transferScheduleKey, torrenttransfer.ConfigKey, "UserInstalledPlugins"}

type transferTrigger struct {
	ID   string    `json:"id"`
	Kind string    `json:"kind"`
	Due  time.Time `json:"due"`
}
type transferSchedule struct {
	Epoch    string               `json:"epoch"`
	Actor    string               `json:"actor"`
	Scope    string               `json:"scope"`
	Timezone string               `json:"timezone"`
	Once     *time.Time           `json:"once,omitempty"`
	Next     *time.Time           `json:"next,omitempty"`
	Pending  *transferTrigger     `json:"pending,omitempty"`
	Latest   *transferBatchResult `json:"latest,omitempty"`
}

func parseTransferSchedule(raw string) (transferSchedule, error) {
	var st transferSchedule
	if raw != "" && (json.Unmarshal([]byte(raw), &st) != nil || !torrenttransfer.Hex(st.Epoch, 64)) {
		return st, torrenttransfer.ErrState
	}
	return st, nil
}

func (api torrentTransferAPI) schedule(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.login.actor(w, r, true)
	if !ok {
		return
	}
	if api.system == nil {
		writeAPIError(w, 503, 503, "转种配置库不可用")
		return
	}
	ctx := r.Context()
	values, err := api.system.ReadMany(ctx, transferScheduleKeys)
	if err != nil {
		writeAPIError(w, 503, 503, "无法读取转种调度")
		return
	}
	st, err := parseTransferSchedule(values[transferScheduleKey])
	if err != nil {
		writeAPIError(w, 409, 409, "调度记录无法读取")
		return
	}
	scope, scopeErr := api.reviewScope(ctx, actor)
	loc, locErr := rssWorkerLocation()
	if r.Method == "GET" {
		ready := scopeErr == nil && locErr == nil && (scope.settings.OnlyOnce || scope.settings.Cron != "")
		var fingerprint string
		if ready {
			fingerprint = torrenttransfer.Hash([]string{scope.fingerprint, loc.String(), st.Epoch})
		}
		writeJSON(w, 200, map[string]any{"code": 0, "result": map[string]any{"control": st, "ready": ready, "fingerprint": fingerprint, "source": scope.from, "target": scope.to, "cron": scope.settings.Cron, "onlyOnce": scope.settings.OnlyOnce, "autoStart": scope.settings.AutoStart, "deleteSource": scope.settings.DeleteSource}})
		return
	}
	var in struct {
		Epoch       string `json:"epoch"`
		Fingerprint string `json:"fingerprint"`
		Confirm     bool   `json:"confirm"`
	}
	if !siteLoginJSON(w, r, &in) {
		return
	}
	if !in.Confirm || in.Epoch != st.Epoch {
		writeAPIError(w, 409, 409, "请刷新并确认当前调度")
		return
	}
	next := transferSchedule{Epoch: sitelogin.Random()}
	if !torrenttransfer.Hex(next.Epoch, 64) {
		writeAPIError(w, 503, 503, "生成调度编号失败")
		return
	}
	changes := map[string]string{}
	switch r.PathValue("control") {
	case "activate":
		if scopeErr != nil || locErr != nil || st.Actor != "" || scope.raw != values[torrenttransfer.ConfigKey] || (!scope.settings.OnlyOnce && scope.settings.Cron == "") || in.Fingerprint != torrenttransfer.Hash([]string{scope.fingerprint, loc.String(), st.Epoch}) {
			writeAPIError(w, 409, 409, "请先核对转种周期、下载器及当前调度")
			return
		}
		now := time.Now()
		next.Actor, next.Scope, next.Timezone = actor, scope.fingerprint, loc.String()
		if scope.settings.Cron != "" {
			due, err := nextSyncTimer(organization.MonitorTimer{Cron: scope.settings.Cron, Timezone: next.Timezone}, now)
			if err != nil {
				writeAPIError(w, 400, 400, "无效的转种周期")
				return
			}
			next.Next = &due
		}
		if scope.settings.OnlyOnce {
			due := now.Add(3 * time.Second)
			next.Once = &due
			raw, err := torrenttransfer.Merge(scope.raw, map[string]json.RawMessage{"onlyonce": json.RawMessage("false")})
			if err != nil {
				writeAPIError(w, 409, 409, "无法登记单次运行")
				return
			}
			changes[torrenttransfer.ConfigKey] = raw
			next.Scope = scope.digest(raw)
		}
	case "stop":
	default:
		writeAPIError(w, 404, 404, "未知调度操作")
		return
	}
	raw, err := json.Marshal(next)
	if err == nil {
		changes[transferScheduleKey] = string(raw)
		err = api.system.UpdateMany(ctx, transferScheduleKeys, func(current map[string]string) (map[string]string, error) {
			for _, k := range transferScheduleKeys {
				if current[k] != values[k] {
					return nil, torrenttransfer.ErrState
				}
			}
			return changes, nil
		})
	}
	if err != nil {
		writeAPIError(w, 409, 409, "调度已变化，请重新核对")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "result": next})
}

// Record the due slot and move the clock in one transaction. A pending batch
// survives restarts and holds its place until its executor finishes.
func (api torrentTransferAPI) queueDue(ctx context.Context, now time.Time) error {
	values, err := api.system.ReadMany(ctx, []string{transferScheduleKey})
	if err != nil {
		return err
	}
	raw := values[transferScheduleKey]
	st, err := parseTransferSchedule(raw)
	if err != nil || st.Actor == "" || st.Pending != nil {
		return err
	}
	once := st.Once != nil && !now.Before(*st.Once)
	periodic := st.Next != nil && !now.Before(*st.Next)
	if !once && !periodic {
		return nil
	}
	scope, err := api.reviewScope(ctx, st.Actor)
	if err != nil {
		return err
	}
	loc, err := rssWorkerLocation()
	if err != nil || loc.String() != st.Timezone || scope.fingerprint != st.Scope {
		return torrenttransfer.ErrState
	}
	job := transferTrigger{}
	if once && (!periodic || !st.Once.After(*st.Next)) {
		job.Kind, job.Due = "once", *st.Once
		st.Once = nil
	} else {
		job.Kind, job.Due = "cron", *st.Next
		next, err := nextSyncTimer(organization.MonitorTimer{Cron: scope.settings.Cron, Timezone: st.Timezone}, now)
		if err != nil {
			return err
		}
		st.Next = &next
	}
	job.ID = torrenttransfer.Hash([]any{st.Epoch, job.Kind, job.Due.UTC()})
	st.Pending = &job
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return api.system.UpdateMany(ctx, transferScheduleKeys, func(current map[string]string) (map[string]string, error) {
		if current[transferScheduleKey] != raw || current[torrenttransfer.ConfigKey] != scope.raw || current["UserInstalledPlugins"] != scope.installedRaw {
			return nil, torrenttransfer.ErrState
		}
		return map[string]string{transferScheduleKey: string(data)}, nil
	})
}

func startTransferScheduler(ctx context.Context, api *torrentTransferAPI) func() {
	if api == nil || api.system == nil || !api.login.pureGo {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if api.queueDue(ctx, now) == nil {
					_ = api.processTransferBatch(ctx)
				}
			}
		}
	}()
	return func() { <-done }
}

func (api torrentTransferAPI) scheduleSummary(ctx context.Context) (bool, bool) {
	values, err := api.system.ReadMany(ctx, []string{transferScheduleKey})
	if err != nil {
		return false, false
	}
	st, err := parseTransferSchedule(values[transferScheduleKey])
	if err != nil {
		return false, false
	}
	if st.Actor == "" {
		return false, true
	}
	scope, err := api.reviewScope(ctx, st.Actor)
	if err != nil {
		return false, false
	}
	loc, err := rssWorkerLocation()
	return err == nil && scope.fingerprint == st.Scope && loc.String() == st.Timezone && (st.Once != nil || st.Next != nil || st.Pending != nil), err == nil
}
