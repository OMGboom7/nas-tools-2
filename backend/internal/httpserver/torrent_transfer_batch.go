package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

// The manifest is sealed before the first submission. After interruption we
// revisit exactly these receipts, including tasks already visible at the target.
type transferBatch struct {
	Follow    []transferFollowup `json:"follow,omitempty"`
	Completed int                `json:"completed"`
	Review    int                `json:"review"`
	ID        string             `json:"id"`
	Receipts  []string           `json:"receipts"`
	Existing  int                `json:"existing"`
	Failed    int                `json:"failed"`
	Submitted int                `json:"submitted"`
	Unknown   int                `json:"unknown"`
	Cursor    int                `json:"cursor"`
}

func (api torrentTransferAPI) pendingScope(ctx context.Context, expected transferSchedule) (transferReviewScope, error) {
	var empty transferReviewScope
	values, err := api.system.ReadMany(ctx, []string{transferScheduleKey})
	if err != nil {
		return empty, err
	}
	current, err := parseTransferSchedule(values[transferScheduleKey])
	if err != nil || current.Epoch != expected.Epoch || current.Scope != expected.Scope || current.Actor != expected.Actor || current.Pending == nil || current.Pending.ID != expected.Pending.ID {
		return empty, torrenttransfer.ErrState
	}
	scope, err := api.reviewScope(ctx, current.Actor)
	if err != nil {
		return empty, err
	}
	loc, err := rssWorkerLocation()
	if err != nil || loc.String() != current.Timezone || scope.fingerprint != current.Scope {
		return empty, torrenttransfer.ErrState
	}
	return scope, nil
}

func (api torrentTransferAPI) processTransferBatch(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	values, err := api.system.ReadMany(ctx, []string{transferScheduleKey})
	if err != nil {
		return err
	}
	raw := values[transferScheduleKey]
	st, err := parseTransferSchedule(raw)
	if err != nil || st.Pending == nil {
		return err
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, "native-torrent-transfer-batch")
	if err != nil {
		return err
	}
	defer guard.Close()
	// Recheck the active batch before each downloader request, including
	// requests made while enumerating a long source list.
	original := api
	organizer := *api.organizer
	base := organizer.service.client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	organizer.service.client = &http.Client{Transport: markTransport(func(req *http.Request) (*http.Response, error) {
		if _, err := original.pendingScope(req.Context(), st); err != nil {
			return nil, err
		}
		return base.RoundTrip(req)
	})}
	api.organizer = &organizer
	scope, err := api.pendingScope(ctx, st)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: api.database}).String()+"?mode=rw")
	if err != nil {
		return err
	}
	defer db.Close()
	ledger, err := torrenttransfer.NewLedger(ctx, db)
	if err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_TORRENT_TRANSFER_BATCHES(id TEXT PRIMARY KEY NOT NULL, manifest TEXT NOT NULL)`); err != nil {
		return err
	}
	batch := transferBatch{ID: st.Pending.ID}
	var saved string
	err = db.QueryRowContext(ctx, "SELECT manifest FROM GO_TORRENT_TRANSFER_BATCHES WHERE id=?", batch.ID).Scan(&saved)
	if errors.Is(err, sql.ErrNoRows) {
		root, err := torrenttransfer.OpenMetainfoRoot(scope.settings.TorrentPath)
		if err != nil {
			return err
		}
		items, existing, failed, err := collectTransferPlans(ctx, scope, root, ledger)
		root.Close()
		if err != nil {
			return err
		}
		batch.Existing, batch.Failed = existing, failed
		for _, item := range items {
			batch.Receipts = append(batch.Receipts, item.Receipt)
		}
		data, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, "INSERT INTO GO_TORRENT_TRANSFER_BATCHES(id,manifest) VALUES(?,?)", batch.ID, string(data)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if json.Unmarshal([]byte(saved), &batch) != nil || batch.ID != st.Pending.ID || batch.Cursor < 0 || batch.Cursor > len(batch.Receipts) {
		return torrenttransfer.ErrState
	}
	resolve := func(ctx context.Context) (torrenttransfer.Settings, torrenttransfer.SourceClient, torrenttransfer.ReceiverClient, string, error) {
		current, err := api.pendingScope(ctx, st)
		return current.settings, current.source, current.target, current.fingerprint, err
	}
	for batch.Cursor < len(batch.Receipts) {
		if _, err := api.pendingScope(ctx, st); err != nil {
			return err
		}
		id := batch.Receipts[batch.Cursor]
		state, err := api.submitBatchItem(ctx, ledger, scope, id, resolve, st.Exclusive)
		if err != nil {
			return err
		}
		switch state {
		case "added":
			batch.Submitted++
			if st.Exclusive {
				batch.Follow = append(batch.Follow, transferFollowup{Receipt: id, Since: time.Now().UTC()})
			}
		case "review":
			batch.Submitted++
			batch.Review++
		case "duplicate":
			batch.Existing++
		case "unknown":
			batch.Unknown++
		default:
			batch.Failed++
		}
		batch.Cursor++
		data, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, "UPDATE GO_TORRENT_TRANSFER_BATCHES SET manifest=? WHERE id=?", string(data), batch.ID); err != nil {
			return err
		}
	}
	if len(batch.Follow) > 0 {
		for range len(batch.Follow) {
			work := batch.Follow[0]
			if _, err := api.pendingScope(ctx, st); err != nil {
				return err
			}
			state, err := api.followTransfer(ctx, db, ledger, scope, work, resolve)
			if err != nil {
				return err
			}
			batch.Follow = batch.Follow[1:]
			switch state {
			case "complete":
				batch.Completed++
			case "review":
				batch.Review++
			default:
				batch.Follow = append(batch.Follow, work)
			}
			data, err := json.Marshal(batch)
			if err != nil {
				return err
			}
			if _, err = db.ExecContext(ctx, "UPDATE GO_TORRENT_TRANSFER_BATCHES SET manifest=? WHERE id=?", string(data), batch.ID); err != nil {
				return err
			}
		}
		if len(batch.Follow) > 0 {
			return nil
		}
	}
	st.Latest = &transferBatchResult{Completed: batch.Completed, Review: batch.Review, ID: batch.ID, Submitted: batch.Submitted, Existing: batch.Existing, Unknown: batch.Unknown, Failed: batch.Failed}
	st.Pending = nil
	if st.Once == nil && st.Next == nil {
		st.Actor = ""
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	total := batch.Submitted + batch.Existing + batch.Failed + batch.Unknown
	if scope.settings.Notify && total > 0 {
		message := fmt.Sprintf("提交阶段共 %d 项，暂停添加已应答 %d 项，目标已有 %d 项，失败 %d 项，待核实 %d 项。批次编号：%s", total, batch.Submitted, batch.Existing, batch.Failed, batch.Unknown, batch.ID)
		if st.Exclusive {
			message += fmt.Sprintf(" 后续操作已完成 %d 项，需人工核对 %d 项。", batch.Completed, batch.Review)
		}
		if err := bus.EmitData(ctx, tx, "plugin_message", batch.ID, batch.ID, "【移转做种任务提交结果】", message, map[string]string{"plugin": torrenttransfer.Plugin}); err != nil {
			return err
		}
	}
	err = systemconfig.UpdateManyTx(ctx, tx, transferScheduleKeys, func(current map[string]string) (map[string]string, error) {
		if current[transferScheduleKey] != raw || current[torrenttransfer.ConfigKey] != scope.raw || current["UserInstalledPlugins"] != scope.installedRaw {
			return nil, torrenttransfer.ErrState
		}
		return map[string]string{transferScheduleKey: string(data)}, nil
	})
	if err != nil {
		return err
	}
	return tx.Commit()
}

type transferBatchResult struct {
	Completed int    `json:"completed"`
	Review    int    `json:"review"`
	ID        string `json:"id"`
	Submitted int    `json:"submitted"`
	Existing  int    `json:"existing"`
	Unknown   int    `json:"unknown"`
	Failed    int    `json:"failed"`
}

func (api torrentTransferAPI) submitBatchItem(ctx context.Context, ledger *torrenttransfer.Ledger, scope transferReviewScope, id string, resolve torrenttransfer.ResolveScope, exclusive bool) (string, error) {
	plan, err := ledger.LoadPlan(ctx, id)
	if err != nil {
		return "", err
	}
	if torrenttransfer.Hash(plan) != id || plan.GrantDigest != scope.fingerprint {
		return "", torrenttransfer.ErrState
	}
	target, err := syncConfigID(scope.settings.To[0])
	if err != nil {
		return "", err
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, torrentLabelGuard(target, plan.WireID))
	if err != nil {
		return "", err
	}
	defer guard.Close()
	key := torrenttransfer.ActionKey{Receiver: plan.Receiver, WireID: plan.WireID, Action: "add"}
	action, err := ledger.Lookup(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		reply, sendErr := ledger.SubmitPlan(ctx, id, resolve)
		if sendErr == nil {
			if reply.Duplicate {
				return "duplicate", nil
			}
			if exclusive {
				_, err := ledger.ContinuePlan(ctx, id, resolve, func(_ context.Context, added torrenttransfer.AddReceipt, t torrenttransfer.Task) error {
					if added != reply || added.Duplicate || t.WireID != plan.WireID {
						return torrenttransfer.ErrState
					}
					return nil
				}, nil)
				if err != nil {
					return "review", nil
				}
			}
			return "added", nil
		}
		action, err = ledger.Lookup(ctx, key)
		if errors.Is(err, sql.ErrNoRows) {
			return "failed", nil
		}
	}
	if err != nil {
		return "", err
	}
	if action.State != "acknowledged" {
		return "unknown", nil
	}
	reply, err := ledger.AddReply(ctx, key, action.Receipt)
	if err != nil {
		return "", err
	}
	if action.Receipt != id || reply.Duplicate {
		return "duplicate", nil
	}
	return "added", nil
}

func (api torrentTransferAPI) batchRecord(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.login.actor(w, r, false); !ok {
		return
	}
	id := r.PathValue("id")
	if !torrenttransfer.Hex(id, 64) {
		writeAPIError(w, 400, 400, "批次编号无效")
		return
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: api.database}).String()+"?mode=ro")
	if err != nil {
		writeAPIError(w, 503, 503, "转种记录库不可用")
		return
	}
	defer db.Close()
	var raw string
	err = db.QueryRowContext(r.Context(), "SELECT manifest FROM GO_TORRENT_TRANSFER_BATCHES WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		writeAPIError(w, 404, 404, "未找到转种批次")
		return
	}
	var batch transferBatch
	if err != nil || json.Unmarshal([]byte(raw), &batch) != nil || batch.ID != id {
		writeAPIError(w, 503, 503, "无法读取批次清单")
		return
	}
	if batch.Receipts == nil {
		batch.Receipts = []string{}
	}
	writeJSON(w, 200, map[string]any{"code": 0, "result": batch})
}
