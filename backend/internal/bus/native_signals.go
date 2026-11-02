package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// captureNativeSignal snapshots an explicitly enabled domain consumer before
// the optional external Webhook subscriber. Replacing a grant never rebinds an
// old event to a new executor, and no historic event scan authorizes new work.
func captureNativeSignal(ctx context.Context, tx *sql.Tx, kind, key, reference string, data any) error {
	if kind != "site.signin" {
		return nil
	}
	exists, e := eventHookTable(ctx, tx, "GO_SITE_SIGNIN_CONTROL")
	if e != nil || !exists {
		return e
	}
	var raw string
	var on bool
	e = tx.QueryRowContext(ctx, `SELECT ENABLED,DOCUMENT FROM GO_SITE_SIGNIN_CONTROL WHERE ID=1`).Scan(&on, &raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil || !on {
		return e
	}
	var c struct{ Scope json.RawMessage }
	if json.Unmarshal([]byte(raw), &c) != nil {
		return ErrState
	}
	var head struct {
		ID      string
		Options struct{ Remote bool }
	}
	if json.Unmarshal(c.Scope, &head) != nil || !eventHookHex(head.ID, 64) {
		return ErrState
	}
	if !head.Options.Remote {
		return nil
	}
	payload, e := canonicalSignalData(data)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_NATIVE_SIGNAL_JOBS(ID TEXT PRIMARY KEY,TYPE TEXT NOT NULL,REFERENCE TEXT NOT NULL,SCOPE_ID TEXT NOT NULL,SOURCE TEXT NOT NULL,DATA TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL,CREATED TEXT NOT NULL,BATCH_ID TEXT NOT NULL)`); e != nil {
		return e
	}
	id := Hash([]string{kind, key})
	_, e = tx.ExecContext(ctx, `INSERT INTO GO_NATIVE_SIGNAL_JOBS VALUES(?,?,?,?,?,?,'queued','',?,'') ON CONFLICT(ID) DO NOTHING`, id, kind, reference, head.ID, string(c.Scope), string(payload), time.Now().UTC().Format(time.RFC3339Nano))
	return e
}
