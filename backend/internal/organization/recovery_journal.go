package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"time"
)

func recoverySchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_RECOVERY_DISPOSALS(ID TEXT PRIMARY KEY,DEFINITION TEXT NOT NULL,STATE TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL,REASON TEXT NOT NULL,FROZEN TEXT NOT NULL DEFAULT '[]',LAST_REVIEW TEXT NOT NULL DEFAULT '',RETURN_NODE TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS GO_RECOVERY_DISPOSAL_ITEMS(DISPOSAL_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,STATE TEXT NOT NULL,PRIMARY KEY(DISPOSAL_ID,ORDINAL))`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func RecoveryDisposalID(actor, request string) string {
	return Digest([]string{"independent-recovery-disposal", actor, request})
}
func recoveryTerminal(state string) bool {
	return state == "completed" || state == "returned" || state == "closed"
}
func (s *Store) CreateRecoveryDisposal(ctx context.Context, d RecoveryDefinition, fingerprint string) (string, error) {
	if d.Removal != nil {
		return "", ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := createRecoveryDisposal(ctx, tx, d, fingerprint)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func createRecoveryDisposal(ctx context.Context, tx *sql.Tx, d RecoveryDefinition, fingerprint string) (string, error) {
	if err := favoriteClaims(ctx, tx, []string{path.Join(d.Scope.Root.Path, d.Path)}, ""); err != nil {
		return "", err
	}
	if fingerprint != Digest(d) || !recoveryScopeValid(d.Scope) || !recoveryDefinitionPath(d) || len(d.Nodes) < 1 || len(d.Nodes) > RecoveryObjectLimit || d.Actor == "" || !recoveryHex(d.RequestID, 32) {
		return "", ErrState
	}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 64<<20 {
		return "", ErrLimit
	}
	id := RecoveryDisposalID(d.Actor, d.RequestID)
	if err := recoverySchema(ctx, tx); err != nil {
		return "", err
	}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_RECOVERY_DISPOSALS WHERE ID=?`, id).Scan(&previous)
	if err == nil {
		if previous != string(raw) {
			return "", ErrState
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	owners, err := recoveryDefinitionOwners(ctx, tx, d)
	if err != nil || Digest(owners) != Digest(d.Owners) {
		return "", ErrState
	}
	others, err := recoveryRows(ctx, tx, `SELECT ID,DEFINITION FROM GO_RECOVERY_DISPOSALS WHERE STATE NOT IN('completed','returned','closed')`)
	if err != nil {
		return "", err
	}
	for _, row := range others {
		var other RecoveryDefinition
		if json.Unmarshal([]byte(row[1]), &other) != nil {
			return "", ErrState
		}
		a, b := path.Join(d.Scope.Root.Path, d.Path), path.Join(other.Scope.Root.Path, other.Path)
		if recoveryOverlaps(a, b) || recoveryOverlaps(a, path.Join(other.Scope.Root.Path, recoveryCapture(row[0], other.Path))) {
			return "", ErrClaimed
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_RECOVERY_DISPOSALS(ID,DEFINITION,STATE,CREATED,UPDATED,REASON)VALUES(?,?,'ready',?,?,'independently reviewed; no prior task proof adopted')`, id, string(raw), now, now); err != nil {
		return "", err
	}
	for i := range d.Nodes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_RECOVERY_DISPOSAL_ITEMS(DISPOSAL_ID,ORDINAL,STATE)VALUES(?,?,'pending')`, id, i); err != nil {
			return "", err
		}
	}
	return id, nil
}
func (s *Store) RecoveryDisposal(ctx context.Context, id string) (RecoveryDisposal, error) {
	r := RecoveryDisposal{ID: id, Items: []RecoveryDisposalItem{}}
	yes, err := recoveryTable(ctx, s.db, "GO_RECOVERY_DISPOSALS")
	if err != nil {
		return r, err
	}
	if !yes {
		return r, ErrNotFound
	}
	var raw, frozen, returned string
	err = s.db.QueryRowContext(ctx, `SELECT DEFINITION,STATE,CREATED,UPDATED,REASON,FROZEN,LAST_REVIEW,RETURN_NODE FROM GO_RECOVERY_DISPOSALS WHERE ID=?`, id).Scan(&raw, &r.State, &r.Created, &r.Updated, &r.Reason, &frozen, &r.LastReview, &returned)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if json.Unmarshal([]byte(raw), &r.Definition) != nil || json.Unmarshal([]byte(frozen), &r.Frozen) != nil || id != RecoveryDisposalID(r.Definition.Actor, r.Definition.RequestID) || !recoveryScopeValid(r.Definition.Scope) || !recoveryDefinitionPath(r.Definition) || len(r.Definition.Nodes) < 1 || len(r.Definition.Nodes) > RecoveryObjectLimit {
		return r, ErrState
	}
	if returned != "" {
		r.ReturnNode = &RecoveryNode{}
		if json.Unmarshal([]byte(returned), r.ReturnNode) != nil {
			return r, ErrState
		}
	}
	r.RootID = r.Definition.Scope.Root.ID
	r.Driver = recoveryDriver(r.Definition.Scope)
	r.Path = r.Definition.Path
	r.Actor = r.Definition.Actor
	r.Decision = r.Definition.Decision
	if r.Driver == "" {
		r.Capture = recoveryCapture(id, r.Path)
	}
	items, err := recoveryRows(ctx, s.db, `SELECT CAST(ORDINAL AS TEXT),STATE FROM GO_RECOVERY_DISPOSAL_ITEMS WHERE DISPOSAL_ID=? ORDER BY ORDINAL`, id)
	if err != nil {
		return r, err
	}
	if len(items) != len(r.Definition.Nodes) {
		return r, ErrState
	}
	for i, row := range items {
		if row[0] != strconv.Itoa(i) {
			return r, ErrState
		}
		r.Items = append(r.Items, RecoveryDisposalItem{Ordinal: i, State: row[1], Node: r.Definition.Nodes[i]})
		if row[1] == "done" {
			r.Done++
		} else if row[1] != "pending" && row[1] != "intent" {
			return r, ErrState
		}
	}
	r.Total = len(r.Items)
	return r, nil
}
func (s *Store) ListRecoveryDisposals(ctx context.Context) ([]RecoveryDisposal, error) {
	yes, err := recoveryTable(ctx, s.db, "GO_RECOVERY_DISPOSALS")
	if err != nil {
		return nil, err
	}
	if !yes {
		return []RecoveryDisposal{}, nil
	}
	rows, err := recoveryRows(ctx, s.db, `SELECT ID FROM GO_RECOVERY_DISPOSALS WHERE json_type(DEFINITION,'$.removal') IS NULL ORDER BY STATE NOT IN('completed','returned','closed') DESC,UPDATED DESC,ID LIMIT 50`)
	if err != nil {
		return nil, err
	}
	result := []RecoveryDisposal{}
	for _, row := range rows {
		r, err := s.RecoveryDisposal(ctx, row[0])
		if err != nil {
			return nil, err
		}
		if r.Definition.Removal != nil {
			continue
		}
		result = append(result, r)
	}
	return result, nil
}
func freezeRecoveryOwners(ctx context.Context, tx *sql.Tx, owners []RecoveryOwner, disposal string) error {
	batches := map[string]bool{}
	members := map[string]bool{}
	for _, o := range owners {
		table := "GO_ORGANIZATION_JOBS"
		if o.Kind == "library" {
			table = "GO_LIBRARY_SCRAPE_JOBS"
		} else if o.Kind != "organization" {
			return ErrState
		}
		if o.State != RecoveryClosed {
			res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET STATE=? WHERE ID=? AND STATE=?`, RecoveryClosed, o.ID, o.State)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return ErrState
			}
		}
		if o.Kind == "organization" {
			members[o.ID] = true
			if o.BatchID != "" {
				batches[o.BatchID] = true
			}
			for _, table := range []string{"GO_SCRAPE_JOBS", "GO_SUBTITLE_JOBS"} {
				yes, err := recoveryTable(ctx, tx, table)
				if err != nil {
					return err
				}
				if yes {
					if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET STATE='blocked',REASON='independent recovery disposal closed this transfer; prior content proof not reusable' WHERE JOB_ID=? AND STATE IN('pending','running','sending')`, o.ID); err != nil {
						return err
					}
				}
			}
		}
	}
	for id := range batches {
		var raw, state string
		if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT,STATE FROM GO_LEGACY_ORGANIZATION_BATCHES WHERE ID=?`, id).Scan(&raw, &state); err != nil {
			return err
		}
		var doc legacyBatchDocument
		if json.Unmarshal([]byte(raw), &doc) != nil || len(doc.Jobs) < 2 || len(doc.Jobs) > 128 {
			return ErrState
		}
		for _, job := range doc.Jobs {
			if !members[job] {
				return ErrState
			}
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job).Scan(&state); err != nil || state != RecoveryClosed {
				return ErrState
			}
		}
		// Historical successful/closed batch receipts are immutable. Pending
		// batches enter their existing negative disposition phase atomically
		// with all siblings, rather than leaving an executable parent behind.
		if state != "completed" && state != "closed" {
			if _, err := tx.ExecContext(ctx, `UPDATE GO_LEGACY_ORGANIZATION_BATCHES SET STATE='closing',REASON=? WHERE ID=?`, "independent recovery disposal "+disposal+" stopped every member; original history and association retained", id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Caller holds the disposal, enrichment, publication, namespace and every
// affected transfer guard. Closing related jobs and the first write intent are
// one transaction. Target claims, history, sources and legacy rows stay intact.
func (s *Store) BeginRecoveryAttempt(ctx context.Context, r RecoveryDisposal, review string) (bool, error) {
	if !recoveryHex(review, 64) || recoveryTerminal(r.State) || r.State == "returning" {
		return false, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var state, last, definition, frozen string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,LAST_REVIEW,DEFINITION,FROZEN FROM GO_RECOVERY_DISPOSALS WHERE ID=?`, r.ID).Scan(&state, &last, &definition, &frozen); err != nil {
		return false, err
	}
	if last == review {
		return false, nil
	}
	var saved RecoveryDefinition
	if json.Unmarshal([]byte(definition), &saved) != nil || Digest(saved) != Digest(r.Definition) || state != r.State {
		return false, ErrState
	}
	owners, err := recoveryDefinitionOwners(ctx, tx, r.Definition)
	if err != nil {
		return false, err
	}
	if state == "ready" {
		if Digest(owners) != Digest(r.Definition.Owners) {
			return false, ErrState
		}
		if err := freezeRecoveryOwners(ctx, tx, owners, r.ID); err != nil {
			return false, err
		}
		owners, err = recoveryDefinitionOwners(ctx, tx, r.Definition)
		if err != nil {
			return false, err
		}
	} else {
		var expected []RecoveryOwner
		if json.Unmarshal([]byte(frozen), &expected) != nil || Digest(expected) != Digest(owners) {
			return false, ErrState
		}
	}
	encoded, err := json.Marshal(owners)
	if err != nil {
		return false, err
	}
	started := false
	for _, item := range r.Items {
		started = started || item.State != "pending"
	}
	phase := "deleting"
	if r.Driver == "" && !started {
		phase = "capturing"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_RECOVERY_DISPOSALS SET STATE=?,FROZEN=?,LAST_REVIEW=?,UPDATED=?,REASON='explicit independent disposal attempt; no automatic replay' WHERE ID=?`, phase, string(encoded), review, time.Now().UTC().Format(time.RFC3339Nano), r.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
func (s *Store) CheckRecoveryRunning(ctx context.Context, id string) error {
	// Every related owner is pinned by the caller's existing job/publication
	// guards for the entire attempt. Do not rescan years of unrelated jobs for
	// each leaf. The complete owner document is compared in BeginRecoveryAttempt.
	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT STATE FROM GO_RECOVERY_DISPOSALS WHERE ID=?`, id).Scan(&state); err != nil {
		return err
	}
	if state != "capturing" && state != "deleting" && state != "returning" {
		return ErrState
	}
	return nil
}
func (s *Store) CheckRecoveryOpen(ctx context.Context, id string) error {
	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT STATE FROM GO_RECOVERY_DISPOSALS WHERE ID=?`, id).Scan(&state); err != nil {
		return err
	}
	if recoveryTerminal(state) {
		return ErrState
	}
	return nil
}
func (s *Store) recoveryPhase(ctx context.Context, id, phase, reason string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE GO_RECOVERY_DISPOSALS SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE NOT IN('completed','returned','closed')`, phase, reason, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrState
	}
	return nil
}
func (s *Store) recoveryItemIntent(ctx context.Context, id string, ordinal int) error {
	if err := s.CheckRecoveryRunning(ctx, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE GO_RECOVERY_DISPOSAL_ITEMS SET STATE='intent' WHERE DISPOSAL_ID=? AND ORDINAL=? AND STATE IN('pending','intent') AND EXISTS(SELECT 1 FROM GO_RECOVERY_DISPOSALS WHERE ID=? AND STATE='deleting')`, id, ordinal, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrState
	}
	return nil
}
func (s *Store) recoveryItemDone(ctx context.Context, id string, ordinal int) error {
	res, err := s.db.ExecContext(ctx, `UPDATE GO_RECOVERY_DISPOSAL_ITEMS SET STATE='done' WHERE DISPOSAL_ID=? AND ORDINAL=? AND STATE='intent' AND EXISTS(SELECT 1 FROM GO_RECOVERY_DISPOSALS WHERE ID=? AND STATE='deleting')`, id, ordinal, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrState
	}
	return nil
}
func (s *Store) finishRecovery(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE GO_RECOVERY_DISPOSALS SET STATE='completed',REASON='reviewed objects absent; original claims retained; coordinator owns any separate history disposition',UPDATED=? WHERE ID=? AND STATE='deleting' AND NOT EXISTS(SELECT 1 FROM GO_RECOVERY_DISPOSAL_ITEMS WHERE DISPOSAL_ID=? AND STATE!='done')`, time.Now().UTC().Format(time.RFC3339Nano), id, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrState
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_RECOVERY_DISPOSALS WHERE ID=?`, id).Scan(&raw); err != nil {
		return err
	}
	var d RecoveryDefinition
	if json.Unmarshal([]byte(raw), &d) != nil {
		return ErrState
	}
	if err := finishMediaRemovalChild(ctx, tx, id, d); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CloseRecoveryDisposal(ctx context.Context, id string) error {
	return s.recoveryPhase(ctx, id, "closed", "explicitly stopped; all remaining objects, captures, old histories and claims retained")
}
