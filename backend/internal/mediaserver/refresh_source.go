package mediaserver

import (
	"context"
	"database/sql"
	"encoding/json"
)

type RefreshDependency struct{ ID, Origin string }

func refreshSource(ctx context.Context, r refreshReader, reference string) (string, string, []RefreshDependency, bool, error) {
	var raw, state string
	if err := r.QueryRowContext(ctx, `SELECT DEFINITION,STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, reference).Scan(&raw, &state); err != nil {
		return "", "", nil, false, err
	}
	if len(raw) > 64<<20 || !json.Valid([]byte(raw)) {
		return "", "", nil, false, ErrRefreshState
	}
	dependencies := []RefreshDependency{}
	yes, err := refreshTable(ctx, r, "GO_SCRAPE_JOBS")
	if err != nil || !yes {
		return raw, state, dependencies, true, err
	}
	rows, err := r.QueryContext(ctx, `SELECT ID,ORIGIN,STATE,OUTPUTS FROM GO_SCRAPE_JOBS WHERE JOB_ID=? ORDER BY ORDINAL`, reference)
	if err != nil {
		return "", "", nil, false, err
	}
	defer rows.Close()
	ready, used := true, 0
	for rows.Next() {
		var id, origin, status, outputs string
		if err := rows.Scan(&id, &origin, &status, &outputs); err != nil {
			return "", "", nil, false, err
		}
		if !refreshHex(id, 64) || !refreshHex(origin, 64) || len(dependencies) >= RefreshItemLimit {
			return "", "", nil, false, ErrRefreshState
		}
		used += len(outputs)
		if len(outputs) > 8<<20 || used > 64<<20 {
			return "", "", nil, false, ErrRefreshState
		}
		dependencies = append(dependencies, RefreshDependency{id, origin})
		switch status {
		case "pending", "running":
			ready = false
		case "completed", "partial", "blocked":
			var entries []struct{ State string }
			if json.Unmarshal([]byte(outputs), &entries) != nil || entries == nil {
				return "", "", nil, false, ErrRefreshState
			}
			for _, e := range entries {
				if e.State == "writing" || e.State == "uncertain" {
					return "", "", nil, false, ErrRefreshState
				}
			}
		default:
			return "", "", nil, false, ErrRefreshState
		}
	}
	return raw, state, dependencies, ready, rows.Err()
}
func bindRefreshSource(ctx context.Context, r refreshReader, d RefreshDefinition) (RefreshDefinition, error) {
	if d.Reference == "" {
		return d, nil
	}
	raw, _, deps, _, err := refreshSource(ctx, r, d.Reference)
	if err != nil {
		return d, err
	}
	d.OriginDigest, d.Enrichment = RefreshDigest(raw), deps
	return d, nil
}
func (s *RefreshStore) BindSource(ctx context.Context, d RefreshDefinition) (RefreshDefinition, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	return bindRefreshSource(ctx, tx, d)
}
func (s *RefreshStore) SourceReady(ctx context.Context, d RefreshDefinition) (bool, error) {
	if d.Reference == "" {
		return true, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	return refreshSourceReady(ctx, tx, d)
}
func refreshSourceReady(ctx context.Context, r refreshReader, d RefreshDefinition) (bool, error) {
	if d.Reference == "" {
		return true, nil
	}
	raw, state, deps, ready, err := refreshSource(ctx, r, d.Reference)
	if err != nil {
		return false, err
	}
	if RefreshDigest(raw) != d.OriginDigest || RefreshDigest(deps) != RefreshDigest(d.Enrichment) || d.Source == "transfer_finished" && state != "active" {
		return false, ErrRefreshState
	}
	var incomplete bool
	err = r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND (STATE!='completed' OR REASON!=''))`, d.Reference).Scan(&incomplete)
	if err != nil || incomplete {
		return false, ErrRefreshState
	}
	return ready, nil
}

// Only recheck a readonly prerequisite. Original due time and all request
// intents remain unchanged; metadata-waiting jobs cannot starve the whole FIFO.
func (s *RefreshStore) WaitForSource(ctx context.Context, id string, now string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE GO_MEDIA_REFRESH_JOBS SET NEXT_CHECK=?,REASON='waiting for the original NFO/image receipts; no scan request sent' WHERE ID=? AND STATE='pending'`, now, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrRefreshState
	}
	return nil
}
