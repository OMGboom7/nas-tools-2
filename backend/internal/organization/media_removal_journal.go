package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediasyncdel"
)

func mediaRemovalSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_MEDIA_REMOVALS(ID TEXT PRIMARY KEY,DEFINITION TEXT NOT NULL,STATE TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL,REASON TEXT NOT NULL,LAST_REVIEW TEXT NOT NULL DEFAULT '',FROZEN TEXT NOT NULL DEFAULT '[]')`,
		`CREATE TABLE IF NOT EXISTS GO_MEDIA_REMOVAL_UNITS(REMOVAL_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,CHILD_ID TEXT NOT NULL,STATE TEXT NOT NULL,PRIMARY KEY(REMOVAL_ID,ORDINAL))`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return recoverySchema(ctx, tx)
}

func mediaRemovalClaimed(ctx context.Context, r recoveryReader, locations []string, historyIDs []int64, except string) error {
	if err := favoriteClaims(ctx, r, locations, except); err != nil {
		return err
	}
	yes, err := recoveryTable(ctx, r, "GO_MEDIA_REMOVALS")
	if err != nil || !yes {
		return err
	}
	rows, err := recoveryRows(ctx, r, `SELECT ID,DEFINITION FROM GO_MEDIA_REMOVALS WHERE STATE!='completed' ORDER BY ID`)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row[0] == except {
			continue
		}
		released, err := reservationReleased(ctx, r, "media-removal", row[0])
		if err != nil {
			return err
		}
		if released {
			continue
		}
		var d MediaRemovalDefinition
		if json.Unmarshal([]byte(row[1]), &d) != nil {
			return ErrState
		}
		for _, h := range d.Histories {
			for _, id := range historyIDs {
				if h.Record.ID == id {
					return ErrClaimed
				}
			}
		}
		for _, u := range d.Units {
			a := path.Join(u.Definition.Scope.Root.Path, u.Definition.Path)
			for _, b := range locations {
				if recoveryOverlaps(a, b) {
					return ErrClaimed
				}
			}
		}
	}
	return nil
}
func (s *Store) CreateMediaRemoval(ctx context.Context, d MediaRemovalDefinition, fingerprint string) (string, error) {
	historyLimit, unitLimit := 100, 1024
	if d.MediaSyncDelete != nil {
		if !mediasyncdel.Hex(d.MediaSyncDelete.Receipt, 64) {
			return "", ErrState
		}
		historyLimit, unitLimit = mediasyncdel.HistoryLimit, RecoveryObjectLimit
	}
	if fingerprint != Digest(d) || d.Actor == "" || !recoveryHex(d.RequestID, 32) || !recoveryHex(d.InputDigest, 64) || len(d.Histories) > historyLimit || len(d.Units) > unitLimit || len(d.Histories) == 0 && len(d.Units) == 0 {
		return "", ErrState
	}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 64<<20 {
		return "", ErrLimit
	}
	id := MediaRemovalID(d.Actor, d.RequestID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := mediaRemovalSchema(ctx, tx); err != nil {
		return "", err
	}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_MEDIA_REMOVALS WHERE ID=?`, id).Scan(&previous)
	if err == nil {
		if previous != string(raw) {
			return "", ErrState
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err := verifyMediaRemovalHistories(ctx, tx, d.Histories); err != nil {
		return "", err
	}
	owners, err := mediaRemovalOwners(ctx, tx, d)
	if err != nil {
		return "", err
	}
	if Digest(owners) != Digest(d.Owners) {
		return "", ErrState
	}
	locations := []string{}
	ids := []int64{}
	for _, u := range d.Units {
		locations = append(locations, path.Join(u.Definition.Scope.Root.Path, u.Definition.Path))
	}
	for _, h := range d.Histories {
		ids = append(ids, h.Record.ID)
	}
	if err := mediaRemovalClaimed(ctx, tx, locations, ids, ""); err != nil {
		return "", err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_MEDIA_REMOVALS(ID,DEFINITION,STATE,CREATED,UPDATED,REASON)VALUES(?,?,'ready',?,?,'review saved; no filesystem action or automatic worker')`, id, string(raw), now, now); err != nil {
		return "", err
	}
	for i, u := range d.Units {
		child := ""
		if u.Definition.Removal == nil || u.Definition.Removal.Parent != id || u.Definition.RequestID != mediaRemovalChildRequest(id, i) {
			return "", ErrState
		}
		if !u.Absent {
			child, err = createRecoveryDisposal(ctx, tx, u.Definition, Digest(u.Definition))
			if err != nil {
				return "", err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_MEDIA_REMOVAL_UNITS VALUES(?,?,?, 'pending')`, id, i, child); err != nil {
			return "", err
		}
	}
	if d.MediaSyncDelete != nil {
		if err := mediasyncdel.BindParent(ctx, tx, d.MediaSyncDelete.Receipt, id, Digest(d), ids, d.MediaSyncDelete.Image); err != nil {
			return "", err
		}
	}
	return id, tx.Commit()
}

func (s *Store) MediaRemoval(ctx context.Context, id string) (MediaRemovalReview, error) {
	empty := MediaRemovalReview{}
	yes, err := recoveryTable(ctx, s.db, "GO_MEDIA_REMOVALS")
	if err != nil {
		return empty, err
	}
	if !yes {
		return empty, ErrNotFound
	}
	var raw, state, created, updated, reason, last, frozen string
	err = s.db.QueryRowContext(ctx, `SELECT DEFINITION,STATE,CREATED,UPDATED,REASON,LAST_REVIEW,FROZEN FROM GO_MEDIA_REMOVALS WHERE ID=?`, id).Scan(&raw, &state, &created, &updated, &reason, &last, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	var d MediaRemovalDefinition
	if json.Unmarshal([]byte(raw), &d) != nil || MediaRemovalID(d.Actor, d.RequestID) != id {
		return empty, ErrState
	}
	v := mediaRemovalProject(d)
	v.State, v.Created, v.Updated, v.Reason, v.LastReview = state, created, updated, reason, last
	if json.Unmarshal([]byte(frozen), &v.Frozen) != nil {
		return empty, ErrState
	}
	rows, err := recoveryRows(ctx, s.db, `SELECT CAST(ORDINAL AS TEXT),CHILD_ID,STATE FROM GO_MEDIA_REMOVAL_UNITS WHERE REMOVAL_ID=? ORDER BY ORDINAL`, id)
	if err != nil {
		return empty, err
	}
	if len(rows) != len(d.Units) {
		return empty, ErrState
	}
	for i, row := range rows {
		if row[0] != strconv.Itoa(i) || row[2] != "pending" && row[2] != "done" {
			return empty, ErrState
		}
		if d.Units[i].Absent {
			if row[1] != "" {
				return empty, ErrState
			}
			continue
		}
		if row[1] != mediaRemovalChildID(d, i) {
			return empty, ErrState
		}
		child, err := s.RecoveryDisposal(ctx, row[1])
		if err != nil {
			return empty, err
		}
		if Digest(child.Definition) != Digest(d.Units[i].Definition) {
			return empty, ErrState
		}
		v.Units[i].Disposal = &child
	}
	return v, nil
}
func (s *Store) ListMediaRemovals(ctx context.Context) ([]MediaRemovalReview, error) {
	yes, err := recoveryTable(ctx, s.db, "GO_MEDIA_REMOVALS")
	if err != nil {
		return nil, err
	}
	if !yes {
		return []MediaRemovalReview{}, nil
	}
	rows, err := recoveryRows(ctx, s.db, `SELECT ID FROM GO_MEDIA_REMOVALS ORDER BY STATE NOT IN('completed','closed') DESC,UPDATED DESC,ID LIMIT 30`)
	if err != nil {
		return nil, err
	}
	out := []MediaRemovalReview{}
	for _, row := range rows {
		v, err := s.MediaRemoval(ctx, row[0])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *Store) CheckMediaRemovalOpen(ctx context.Context, id string) error {
	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT STATE FROM GO_MEDIA_REMOVALS WHERE ID=?`, id).Scan(&state); err != nil {
		return err
	}
	if mediaRemovalTerminal(state) {
		return ErrState
	}
	return nil
}
func (s *Store) CheckMediaRemovalRunning(ctx context.Context, id string) error {
	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT STATE FROM GO_MEDIA_REMOVALS WHERE ID=?`, id).Scan(&state); err != nil {
		return err
	}
	if state != "running" {
		return ErrState
	}
	return nil
}
func (s *Store) CloseMediaRemoval(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_REMOVALS SET STATE='closed',UPDATED=?,REASON='explicitly stopped; remaining objects, original history, dedupe rows and claims retained; sent deletes may still complete' WHERE ID=? AND STATE NOT IN('closed','completed')`, time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_RECOVERY_DISPOSALS SET STATE='closed',UPDATED=?,REASON='whole media-removal coordinator stopped; no replay' WHERE ID IN(SELECT CHILD_ID FROM GO_MEDIA_REMOVAL_UNITS WHERE REMOVAL_ID=?) AND STATE NOT IN('completed','returned','closed')`, time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) beginMediaRemoval(ctx context.Context, v MediaRemovalReview, fingerprint string, guards ...func(context.Context, *sql.Tx) error) (bool, error) {
	if !recoveryHex(fingerprint, 64) || mediaRemovalTerminal(v.State) {
		return false, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var state, last, raw, frozen string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,LAST_REVIEW,DEFINITION,FROZEN FROM GO_MEDIA_REMOVALS WHERE ID=?`, v.ID).Scan(&state, &last, &raw, &frozen); err != nil {
		return false, err
	}
	if last == fingerprint {
		return false, nil
	}
	var saved MediaRemovalDefinition
	if json.Unmarshal([]byte(raw), &saved) != nil || Digest(saved) != Digest(v.Definition) || state != v.State {
		return false, ErrState
	}
	for _, guard := range guards {
		if guard == nil {
			return false, ErrState
		}
		if err := guard(ctx, tx); err != nil {
			return false, err
		}
	}
	if err := verifyMediaRemovalHistories(ctx, tx, saved.Histories); err != nil {
		return false, err
	}
	owners, err := mediaRemovalOwners(ctx, tx, saved)
	if err != nil {
		return false, err
	}
	if state == "ready" {
		if Digest(owners) != Digest(saved.Owners) {
			return false, ErrState
		}
		if err := freezeRecoveryOwners(ctx, tx, owners, v.ID); err != nil {
			return false, err
		}
		owners, err = mediaRemovalOwners(ctx, tx, saved)
		if err != nil {
			return false, err
		}
		for i, u := range saved.Units {
			if u.Absent {
				continue
			}
			childOwners, err := recoveryDefinitionOwners(ctx, tx, u.Definition)
			if err != nil {
				return false, err
			}
			b, err := json.Marshal(childOwners)
			if err != nil {
				return false, err
			}
			phase := "deleting"
			if u.Definition.Scope.Remote == nil {
				phase = "capturing"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE GO_RECOVERY_DISPOSALS SET STATE=?,FROZEN=?,UPDATED=? WHERE ID=? AND STATE='ready'`, phase, string(b), time.Now().UTC().Format(time.RFC3339Nano), mediaRemovalChildID(saved, i)); err != nil {
				return false, err
			}
		}
	} else {
		var expected []RecoveryOwner
		if json.Unmarshal([]byte(frozen), &expected) != nil || Digest(expected) != Digest(owners) {
			return false, ErrState
		}
	}
	b, err := json.Marshal(owners)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_REMOVALS SET STATE='running',FROZEN=?,LAST_REVIEW=?,UPDATED=?,REASON='explicit single reviewed attempt; no automatic retry' WHERE ID=?`, string(b), fingerprint, time.Now().UTC().Format(time.RFC3339Nano), v.ID); err != nil {
		return false, err
	}
	if saved.MediaSyncDelete != nil {
		if err := mediasyncdel.StartParent(ctx, tx, saved.MediaSyncDelete.Receipt, v.ID, Digest(saved)); err != nil {
			return false, err
		}
	}
	for _, guard := range guards {
		if err := guard(ctx, tx); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}
