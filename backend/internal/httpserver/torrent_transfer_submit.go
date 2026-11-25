package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

func (api torrentTransferAPI) transferAction(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.login.actor(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var in struct {
		Proof         string `json:"proof"`
		ConfirmOwner  bool   `json:"confirmOwner"`
		ConfirmAction bool   `json:"confirmAction"`
		ConfirmPaused bool   `json:"confirmPaused"`
		KeepFiles     bool   `json:"keepFiles"`
	}
	if !siteLoginJSON(w, r, &in) {
		return
	}
	action := r.PathValue("action")
	if !torrenttransfer.Hex(id, 64) || action == "submit" && !in.ConfirmPaused {
		writeAPIError(w, 400, 400, "请选择审核回执并确认暂停添加")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	scope, err := api.reviewScope(ctx, actor)
	if err != nil {
		writeAPIError(w, 409, 409, "请重新核对转种配置")
		return
	}
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
	plan, err := ledger.LoadPlan(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeAPIError(w, 404, 404, "未找到转种计划")
		return
	}
	if err != nil || torrenttransfer.Hash(plan) != id || plan.GrantDigest != scope.fingerprint {
		writeAPIError(w, 409, 409, "计划或下载器配置已改变，请重新审核")
		return
	}
	target, err := syncConfigID(scope.settings.To[0])
	if err != nil {
		writeAPIError(w, 409, 409, "目标下载器编号无效")
		return
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, torrentLabelGuard(target, plan.WireID))
	if err != nil {
		writeAPIError(w, 409, 409, "目标任务正在处理，请查看回执")
		return
	}
	defer guard.Close()
	key := torrenttransfer.ActionKey{Receiver: plan.Receiver, WireID: plan.WireID, Action: "add"}
	resolve := func(ctx context.Context) (torrenttransfer.Settings, torrenttransfer.SourceClient, torrenttransfer.ReceiverClient, string, error) {
		current, err := api.reviewScope(ctx, actor)
		return current.settings, current.source, current.target, current.fingerprint, err
	}
	if action != "submit" {
		if action != "inspect" && action != "bind" && action != "verify" && action != "start" && action != "remove-source" && action != "confirm-absence" {
			writeAPIError(w, 404, 404, "未知转种操作")
			return
		}
		reply, err := ledger.AddReply(ctx, key, id)
		if err != nil || reply.Duplicate {
			writeAPIError(w, 409, 409, "缺少可用的原添加回执")
			return
		}
		status, err := torrenttransfer.ReadStatus(ctx, db, id)
		if err != nil {
			writeAPIError(w, 503, 503, "无法读取绑定状态")
			return
		}
		task, err := scope.target.TransferTask(ctx, plan.WireID)
		if err != nil {
			writeAPIError(w, 409, 409, "无法核对目标任务")
			return
		}
		proofFor := func(task torrenttransfer.Task) string {
			return torrenttransfer.Hash([]any{id, scope.fingerprint, task})
		}
		proof := proofFor(task)
		source := plan.Source.Task
		removalState := ""
		started := false
		for _, a := range status.Actions {
			if a.Action == "remove-source" {
				removalState = a.State
			}
			if a.Action == "start" && a.State == "acknowledged" {
				started = true
			}
		}
		canRemove := scope.settings.DeleteSource && status.TargetStage == "verified" && task.Complete && !task.Error && (!scope.settings.AutoStart || started && task.SourceSeeding) && removalState == ""
		canConfirmAbsence := status.SourceAbsentAt == "" && (removalState == "acknowledged" || removalState == "pending" || removalState == "unknown")
		if action == "remove-source" || action == "confirm-absence" {
			if !in.ConfirmAction || in.Proof != proof || (action == "remove-source" && (!in.KeepFiles || !canRemove)) || (action == "confirm-absence" && !canConfirmAbsence) {
				writeAPIError(w, 409, 409, "请重新核对源任务、目标任务和移除回执")
				return
			}
			authorize := func(_ context.Context, receipt string, liveSource, liveTarget torrenttransfer.Task) error {
				if action != "remove-source" || receipt != id || !source.SameInstance(liveSource) || proofFor(liveTarget) != in.Proof {
					return torrenttransfer.ErrState
				}
				return nil
			}
			if action == "confirm-absence" {
				err = ledger.ConfirmSourceAbsent(ctx, id, resolve)
			} else {
				err = ledger.RemoveSource(ctx, id, resolve, authorize)
			}
			if err != nil {
				writeAPIError(w, 409, 409, "源移除尚未核实，请刷新回执并核对做种要求和原任务")
				return
			}
			writeJSON(w, 200, map[string]any{"code": 0, "result": map[string]string{"receipt": id, "state": "source-absent"}})
			return
		}
		if action == "inspect" {
			canStart := scope.settings.AutoStart && task.Paused && task.HasTracker && status.TargetStage == "verified"
			for _, a := range status.Actions {
				if a.Action == "start" {
					canStart = false
				}
			}
			writeJSON(w, 200, map[string]any{"code": 0, "result": map[string]any{"receipt": id, "proof": proof, "target": scope.to, "wireId": task.WireID, "directory": task.Directory, "name": task.Name, "addedAt": task.AddedAt, "state": task.State, "targetStage": status.TargetStage, "canStart": canStart, "sourceName": scope.from, "sourceId": source.WireID, "sourceDirectory": source.Directory, "sourceTask": source.Name, "canRemove": canRemove, "canConfirmAbsence": canConfirmAbsence}})
			return
		}
		if !in.ConfirmAction || in.Proof != proof || (action == "bind" && (!in.ConfirmOwner || status.TargetStage != "")) || (action == "verify" && status.TargetStage == "") || (action == "start" && status.TargetStage != "verified") {
			writeAPIError(w, 409, 409, "请重新核对当前目标任务及操作")
			return
		}
		var owner func(context.Context, torrenttransfer.AddReceipt, torrenttransfer.Task) error
		var start func(context.Context, string, torrenttransfer.Task) error
		if action == "bind" {
			owner = func(_ context.Context, reply torrenttransfer.AddReceipt, current torrenttransfer.Task) error {
				if reply.Duplicate || proofFor(current) != in.Proof {
					return torrenttransfer.ErrState
				}
				return nil
			}
		}
		if action == "start" {
			start = func(_ context.Context, receipt string, frozen torrenttransfer.Task) error {
				if receipt != id || !frozen.SameInstance(task) {
					return torrenttransfer.ErrState
				}
				return nil
			}
		}
		stage, err := ledger.ContinuePlan(ctx, id, resolve, owner, start)
		if err != nil {
			writeAPIError(w, 409, 409, "任务或授权已变化，请重新查看目标和回执")
			return
		}
		if !scope.settings.DeleteSource && (stage == "verified" && !scope.settings.AutoStart || action == "start" && stage == "acknowledged") {
			if err := ledger.CompleteTarget(ctx, id, target); err != nil {
				writeAPIError(w, 503, 503, "无法保存转种完成记录，请按原回执核对")
				return
			}
		}
		writeJSON(w, 200, map[string]any{"code": 0, "result": map[string]string{"receipt": id, "state": stage}})
		return
	}
	prior, err := ledger.Lookup(ctx, key)
	if err == nil {
		state := prior.State
		if state == "acknowledged" {
			reply, err := ledger.AddReply(ctx, key, prior.Receipt)
			if err != nil {
				writeAPIError(w, 503, 503, "无法读取原添加回执")
				return
			}
			if reply.Duplicate {
				state = "duplicate"
			}
		}
		writeJSON(w, 200, map[string]any{"code": 0, "result": map[string]string{"receipt": prior.Receipt, "state": state}})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeAPIError(w, 503, 503, "无法读取原提交记录")
		return
	}
	reply, err := ledger.SubmitPlan(ctx, id, resolve)
	if err != nil {
		writeAPIError(w, 409, 409, "未能确认添加结果，请按回执查看原提交")
		return
	}
	state := "acknowledged"
	if reply.Duplicate {
		state = "duplicate"
	}
	writeJSON(w, 200, map[string]any{"code": 0, "result": map[string]string{"receipt": id, "state": state}})
}
