package torrentremover

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
)

func current(ctx context.Context, q reader, r Receipt) error {
	settings, err := readSettings(ctx, q)
	if err != nil {
		return err
	}
	enabled, err := Enabled(settings.Raw)
	if err != nil || !settings.Installed || !enabled || Hash(settings.Control) != Hash(r.Definition.Control) || Hash(settings.Raw) != r.Definition.Control.Config {
		return ErrState
	}
	for _, target := range r.Definition.Control.Targets {
		var id int64
		var name, kind, mode, config, dirs string
		var enabled, transfer, onlyNastool, matchPath int
		err := q.QueryRowContext(ctx, `SELECT ID,COALESCE(NAME,''),COALESCE(ENABLED,0),COALESCE(TYPE,''),COALESCE(TRANSFER,0),COALESCE(ONLY_NASTOOL,0),COALESCE(MATCH_PATH,0),COALESCE(RMT_MODE,''),COALESCE(CONFIG,''),COALESCE(DOWNLOAD_DIR,'') FROM DOWNLOADER WHERE ID=?`, target.ID).Scan(&id, &name, &enabled, &kind, &transfer, &onlyNastool, &matchPath, &mode, &config, &dirs)
		if err != nil || enabled != 1 || kind != target.Type || DownloaderDigest(id, name, enabled, kind, transfer, onlyNastool, matchPath, mode, config, dirs) != target.Digest {
			return ErrState
		}
	}
	for _, d := range r.Definition.Downloads {
		var item Download
		err := q.QueryRowContext(ctx, `SELECT CAST(ID AS TEXT),COALESCE(TITLE,''),COALESCE(DOWNLOADER,''),COALESCE(DOWNLOAD_ID,''),COALESCE(SAVE_PATH,'') FROM DOWNLOAD_HISTORY WHERE ID=?`, d.ID).Scan(&item.ID, &item.Title, &item.Downloader, &item.Task, &item.SavePath)
		if err != nil || item != d {
			return ErrState
		}
	}
	for _, h := range r.Definition.Histories {
		current, err := historyFor(ctx, q, h.Plugin, h.Key)
		if err != nil || current == nil || *current != h {
			return ErrState
		}
	}
	return nil
}
func (s *Store) Check(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := readReceipt(ctx, tx, id)
	if err != nil {
		return err
	}
	if r.State != "running" {
		return ErrState
	}
	if err := current(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Claim(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := readReceipt(ctx, tx, id)
	if err != nil {
		return err
	}
	if r.State != "pending" {
		return ErrState
	}
	if err := current(ctx, tx, r); err != nil {
		return err
	}
	if err := writeReceiptState(ctx, tx, id, "pending", "running", "正在核对原下载器任务"); err != nil {
		return err
	}
	return tx.Commit()
}
func writeReceiptState(ctx context.Context, tx *sql.Tx, id, from, to, reason string) error {
	result, err := tx.ExecContext(ctx, `UPDATE GO_TORRENT_REMOVAL_EVENTS SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE=?`, to, reason, timestamp(), id, from)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrState
	}
	var state, stored string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,REASON FROM GO_TORRENT_REMOVAL_EVENTS WHERE ID=?`, id).Scan(&state, &stored); err != nil || state != to || stored != reason {
		return ErrState
	}
	return nil
}
func taskFor(r Receipt, id string) (Task, bool) {
	for _, t := range r.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}
func writeTaskState(ctx context.Context, tx *sql.Tx, id, task, from, to, reason, proof string) error {
	result, err := tx.ExecContext(ctx, `UPDATE GO_TORRENT_REMOVAL_TASKS SET STATE=?,REASON=?,PROOF=? WHERE RECEIPT_ID=? AND ID=? AND STATE=?`, to, reason, proof, id, task, from)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrState
	}
	var state, stored, actualProof string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,REASON,PROOF FROM GO_TORRENT_REMOVAL_TASKS WHERE RECEIPT_ID=? AND ID=?`, id, task).Scan(&state, &stored, &actualProof); err != nil || state != to || stored != reason || actualProof != proof {
		return ErrState
	}
	return nil
}

// Intent is the only transition into a remotely writable state. The caller
// must hold the process/task guards and revalidate before every network request.
func (s *Store) Intent(ctx context.Context, id, task, proof string) error {
	if !Hex(proof, 64) {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := readReceipt(ctx, tx, id)
	if err != nil {
		return err
	}
	if r.State != "running" {
		return ErrState
	}
	if err := current(ctx, tx, r); err != nil {
		return err
	}
	t, ok := taskFor(r, task)
	if !ok || t.State != "pending" {
		return ErrState
	}
	if t.Parent != "" {
		p, ok := taskFor(r, t.Parent)
		if !ok || p.State != "completed" {
			return ErrState
		}
	}
	if err := writeTaskState(ctx, tx, id, task, "pending", "intent", "移除任务意图已保存，必须保留全部文件", proof); err != nil {
		return err
	}
	return tx.Commit()
}

// Resolve records evidence without making a request. An intent can never be
// changed back to pending, and an unknown result cannot be authorized to resend.
func (s *Store) Resolve(ctx context.Context, id, task, from, to, reason string) error {
	allowed := from == "pending" && slices.Contains([]string{"ignored", "blocked"}, to) || from == "intent" && slices.Contains([]string{"completed", "unknown"}, to) || from == "unknown" && to == "completed"
	if !allowed || !validText(reason, 600) {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := readReceipt(ctx, tx, id)
	if err != nil {
		return err
	}
	t, ok := taskFor(r, task)
	if !ok || t.State != from {
		return ErrState
	}
	if from == "pending" && r.State != "running" {
		return ErrState
	}
	if err := writeTaskState(ctx, tx, id, task, from, to, reason, t.Proof); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Finish(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := readReceipt(ctx, tx, id)
	if err != nil {
		return err
	}
	if r.State != "running" {
		return ErrState
	}
	state, reason := "completed", "原范围任务已核对，保留全部文件"
	for _, t := range r.Tasks {
		if t.State == "pending" || t.State == "blocked" {
			state, reason = "partial", "部分任务缺少原范围或做种证据，未执行移除"
		}
		if t.State == "intent" || t.State == "unknown" {
			state, reason = "unknown", "存在未确认的移除意图，仅可只读核对"
			break
		}
	}
	if state == "completed" {
		if err := current(ctx, tx, r); err != nil {
			return err
		}
		if err := cleanupHistories(ctx, tx, r); err != nil {
			return err
		}
	}
	if err := writeReceiptState(ctx, tx, id, "running", state, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// Recover must only run while holding the downloader-removal process lock.
// Unclaimed future events stay pending; interrupted executions are never replayed.
func (s *Store) Recover(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT ID FROM GO_TORRENT_REMOVAL_EVENTS WHERE STATE='running' ORDER BY ID LIMIT 501`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) > 500 {
		return ErrState
	}
	for _, id := range ids {
		r, err := readReceipt(ctx, tx, id)
		if err != nil {
			return err
		}
		for _, t := range r.Tasks {
			if t.State == "intent" {
				if err := writeTaskState(ctx, tx, id, t.ID, "intent", "unknown", "进程中断，原移除意图不重发", t.Proof); err != nil {
					return err
				}
			}
		}
		if err := writeReceiptState(ctx, tx, id, "running", "unknown", "原执行器中断，请只读核对原任务"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BlockPending closes work whose original authorization is no longer valid.
func (s *Store) BlockPending(ctx context.Context, id, reason string) error {
	if !validText(reason, 600) {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeReceiptState(ctx, tx, id, "pending", "blocked", reason); err != nil {
		return err
	}
	return tx.Commit()
}
func controlJSON(c Control) string { b, _ := json.Marshal(c); return string(b) }
