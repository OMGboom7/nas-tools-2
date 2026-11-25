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

func (api torrentTransferAPI) submit(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.login.actor(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var in struct {
		ConfirmPaused bool `json:"confirmPaused"`
	}
	if !siteLoginJSON(w, r, &in) {
		return
	}
	if !torrenttransfer.Hex(id, 64) || !in.ConfirmPaused {
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
	resolve := func(ctx context.Context) (torrenttransfer.Settings, torrenttransfer.SourceClient, torrenttransfer.ReceiverClient, string, error) {
		current, err := api.reviewScope(ctx, actor)
		return current.settings, current.source, current.target, current.fingerprint, err
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
