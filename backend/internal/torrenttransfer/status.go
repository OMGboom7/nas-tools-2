package torrenttransfer

import (
	"context"
	"database/sql"
)

type ActionStatus struct {
	Action  string `json:"action"`
	State   string `json:"state"`
	Updated string `json:"updated"`
}

type PlanStatus struct {
	Receipt     string         `json:"receipt"`
	TargetStage string         `json:"targetStage,omitempty"`
	Actions     []ActionStatus `json:"actions"`
}

// ReadStatus observes a single SQLite snapshot without initializing a ledger,
// contacting a downloader or exposing task metadata and local paths.
func ReadStatus(ctx context.Context, db *sql.DB, receipt string) (PlanStatus, error) {
	status := PlanStatus{Receipt: receipt, Actions: []ActionStatus{}}
	if db == nil || !Hex(receipt, 64) {
		return status, ErrState
	}
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_TORRENT_TRANSFER_PLANS')").Scan(&exists); err != nil {
		return status, err
	}
	if !exists {
		return status, sql.ErrNoRows
	}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(t.stage,''), COALESCE(a.action,''), COALESCE(a.state,''),COALESCE(a.updated,'')
	FROM GO_TORRENT_TRANSFER_PLANS p
	LEFT JOIN GO_TORRENT_TRANSFER_TARGETS t ON t.receiver=json_extract(p.plan,'$.Receiver') AND t.wire_id=json_extract(p.plan,'$.WireID') AND t.receipt=p.receipt
	LEFT JOIN GO_TORRENT_TRANSFER_ACTIONS a ON a.receipt=p.receipt
	WHERE p.receipt=? ORDER BY a.created,a.action`, receipt)
	if err != nil {
		return status, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		found = true
		var action ActionStatus
		if err := rows.Scan(&status.TargetStage, &action.Action, &action.State, &action.Updated); err != nil {
			return status, err
		}
		if action.Action != "" {
			status.Actions = append(status.Actions, action)
		}
	}
	if err := rows.Err(); err != nil {
		return status, err
	}
	if !found {
		return status, sql.ErrNoRows
	}
	return status, nil
}
