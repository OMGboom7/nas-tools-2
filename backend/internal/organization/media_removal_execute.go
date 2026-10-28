package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediasyncdel"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func mediaRemovalUnitAbsent(ctx context.Context, v MediaRemovalReview, i int, c *remotestore.Client) error {
	u := v.Definition.Units[i]
	present, err := MediaRemovalPresence(ctx, u.Definition.Scope, u.Definition.Path, c)
	if err != nil {
		return err
	}
	if present {
		return ErrState
	}
	if c == nil && !u.Absent {
		present, err = MediaRemovalPresence(ctx, u.Definition.Scope, recoveryCapture(mediaRemovalChildID(v.Definition, i), u.Definition.Path), nil)
		if err != nil {
			return err
		}
		if present {
			return ErrState
		}
	}
	return nil
}
func (s *Store) ReviewMediaRemoval(ctx context.Context, v MediaRemovalReview, clients []*remotestore.Client) (MediaRemovalReview, error) {
	if mediaRemovalTerminal(v.State) {
		v.Fingerprint = Digest([]any{v.ID, v.State, v.Updated})
		return v, nil
	}
	if len(clients) != len(v.Definition.Units) {
		return v, ErrState
	}
	v.CanContinue = true
	if verifyMediaRemovalHistories(ctx, s.db, v.Definition.Histories) != nil {
		v.CanContinue = false
		v.Reason = "history or exact dedupe rows changed; no further deletion authorized"
	}
	owners, err := mediaRemovalOwners(ctx, s.db, v.Definition)
	if err != nil {
		return v, err
	}
	expected := v.Frozen
	if v.State == "ready" {
		expected = v.Definition.Owners
	}
	if Digest(owners) != Digest(expected) {
		v.CanContinue = false
		v.Reason = "related jobs changed; obtain a new independent review"
	}
	for i, u := range v.Definition.Units {
		if err := recoveryClient(u.Definition.Scope, clients[i]); err != nil {
			return v, err
		}
		if u.Absent {
			if err := mediaRemovalUnitAbsent(ctx, v, i, clients[i]); err != nil {
				if ctx.Err() != nil {
					return v, ctx.Err()
				}
				v.CanContinue = false
				v.Reason = "an originally absent selection is no longer proven absent"
			}
			continue
		}
		child, err := s.RecoveryDisposal(ctx, mediaRemovalChildID(v.Definition, i))
		if err != nil {
			return v, err
		}
		v.Units[i].Disposal = &child
		review, err := s.ReviewRecoveryDisposal(ctx, child, clients[i])
		if err != nil {
			return v, err
		}
		v.Units[i].Review = &review
		if child.State == "completed" {
			if err := mediaRemovalUnitAbsent(ctx, v, i, clients[i]); err != nil {
				v.CanContinue = false
				v.Reason = "new object appeared at a completed selection; never adopt or delete it"
			}
		} else if !review.CanContinue {
			v.CanContinue = false
			v.Reason = "a selected unit has changed or incomplete evidence; all remaining objects retained"
		}
	}
	if ctx.Err() != nil {
		return v, ctx.Err()
	}
	v.Fingerprint = Digest([]any{v.ID, v.State, v.Updated, v.LastReview, v.Units, owners, Digest(v.Definition.Histories), v.CanContinue, v.Reason})
	return v, nil
}

func (s *Store) receiptMediaRemovalUnit(ctx context.Context, v MediaRemovalReview, i int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := receiptMediaRemovalUnitTx(ctx, tx, v, i); err != nil {
		return err
	}
	return tx.Commit()
}

func receiptMediaRemovalUnitTx(ctx context.Context, tx *sql.Tx, v MediaRemovalReview, i int) error {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_MEDIA_REMOVALS WHERE ID=?`, v.ID).Scan(&state); err != nil {
		return err
	}
	if state != "running" {
		return ErrState
	}
	var unitState string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_MEDIA_REMOVAL_UNITS WHERE REMOVAL_ID=? AND ORDINAL=?`, v.ID, i).Scan(&unitState); err != nil {
		return err
	}
	if unitState == "done" {
		return nil
	}
	u := v.Definition.Units[i]
	if !u.Absent {
		if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_RECOVERY_DISPOSALS WHERE ID=?`, mediaRemovalChildID(v.Definition, i)).Scan(&state); err != nil {
			return err
		}
		if state != "completed" {
			return ErrState
		}
		for _, ref := range u.Events {
			var history *HistoryRecord
			for _, h := range v.Definition.Histories {
				if strconv.FormatInt(h.Record.ID, 10) == ref.HistoryID {
					copy := h.Record
					history = &copy
					break
				}
			}
			if history == nil || ref.Role != "source" && ref.Role != "library" {
				return ErrState
			}
			kind, title := "source_file_deleted", "源文件已删除"
			if ref.Role == "library" {
				kind, title = "library_file_deleted", "媒体库文件已删除"
			}
			if !validRelative(ref.Path) || ref.Path == "." || !sourceUnder(ref.Path, u.Definition.Path) {
				return ErrState
			}
			full := path.Join(u.Definition.Scope.Root.Path, ref.Path)
			payload := map[string]any{"media_info": map[string]string{"type": history.Type, "category": history.Category, "title": history.Title, "year": history.Year, "tmdbid": history.TMDBID, "season_episode": history.SeasonEpisode}, "path": path.Dir(full), "filename": path.Base(full), "removal_id": v.ID, "history_id": ref.HistoryID, "driver": recoveryDriver(u.Definition.Scope)}
			for _, node := range u.Definition.Nodes {
				if path.Join(u.Definition.Path, node.Path) == ref.Path && node.Kind == "directory" {
					payload["path"], payload["filename"] = full, ""
				}
			}
			key := Digest([]any{v.ID, i, ref})
			if err := bus.EmitData(ctx, tx, kind, key, v.ID, title, fmt.Sprintf("%s (%s) %s\n本次已核验删除：%s\n处置批次：%s", history.Title, history.Year, history.SeasonEpisode, full, v.ID), payload); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_REMOVAL_UNITS SET STATE='done' WHERE REMOVAL_ID=? AND ORDINAL=? AND STATE='pending'`, v.ID, i); err != nil {
		return err
	}
	return nil
}

func finishMediaRemovalChild(ctx context.Context, tx *sql.Tx, id string, d RecoveryDefinition) error {
	if d.Removal == nil {
		return nil
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_MEDIA_REMOVALS WHERE ID=?`, d.Removal.Parent).Scan(&raw); err != nil {
		return err
	}
	var parent MediaRemovalDefinition
	if json.Unmarshal([]byte(raw), &parent) != nil || MediaRemovalID(parent.Actor, parent.RequestID) != d.Removal.Parent {
		return ErrState
	}
	var ordinal int
	if err := tx.QueryRowContext(ctx, `SELECT ORDINAL FROM GO_MEDIA_REMOVAL_UNITS WHERE REMOVAL_ID=? AND CHILD_ID=?`, d.Removal.Parent, id).Scan(&ordinal); err != nil {
		return err
	}
	if ordinal < 0 || ordinal >= len(parent.Units) || Digest(parent.Units[ordinal].Definition) != Digest(d) || mediaRemovalChildID(parent, ordinal) != id {
		return ErrState
	}
	return receiptMediaRemovalUnitTx(ctx, tx, mediaRemovalProject(parent), ordinal)
}
func (s *Store) finishMediaRemoval(ctx context.Context, v MediaRemovalReview, guards ...func(context.Context, *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_MEDIA_REMOVALS WHERE ID=?`, v.ID).Scan(&state); err != nil {
		return err
	}
	if state != "running" {
		return ErrState
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_MEDIA_REMOVAL_UNITS WHERE REMOVAL_ID=? AND STATE!='done'`, v.ID).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return ErrState
	}
	for _, guard := range guards {
		if guard == nil {
			return ErrState
		}
		if err := guard(ctx, tx); err != nil {
			return err
		}
	}
	if err := deleteMediaRemovalHistoryRows(ctx, tx, v.Definition.Histories); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_REMOVALS SET STATE='completed',UPDATED=?,REASON='all selected units verified absent; exact original history and dedupe rows removed atomically; journal retained' WHERE ID=? AND STATE='running'`, time.Now().UTC().Format(time.RFC3339Nano), v.ID); err != nil {
		return err
	}
	if v.Definition.MediaSyncDelete != nil {
		if err := mediasyncdel.CompleteParent(ctx, tx, v.Definition.MediaSyncDelete.Receipt, v.ID, Digest(v.Definition)); err != nil {
			return err
		}
	}
	// The source rows are now gone. Automatic guards check the same authority
	// after the commit projection without expecting the deleted rows to exist.
	for _, guard := range guards {
		if err := guard(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Full-batch preflight precedes the first write. Every child retains the same
// content/identity/delete-intent protocol; no media operation is scheduled.
func (s *Store) ExecuteMediaRemoval(ctx context.Context, v MediaRemovalReview, clients []*remotestore.Client, fingerprint string, validate func(RecoveryScope) error, guards ...func(context.Context, *sql.Tx) error) (err error) {
	if validate == nil || !v.CanContinue || len(clients) != len(v.Definition.Units) {
		return ErrState
	}
	for _, u := range v.Definition.Units {
		if err := validate(u.Definition.Scope); err != nil {
			return err
		}
	}
	started, err := s.beginMediaRemoval(ctx, v, fingerprint, guards...)
	if err != nil || !started {
		return err
	}
	defer func() {
		if err != nil {
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = s.db.ExecContext(persist, `UPDATE GO_MEDIA_REMOVALS SET STATE='needs_review',UPDATED=?,REASON='interrupted single attempt; retained history/dedupe and remaining objects; no automatic retry' WHERE ID=? AND STATE NOT IN('completed','closed')`, time.Now().UTC().Format(time.RFC3339Nano), v.ID)
		}
	}()
	current := func(scope RecoveryScope) error {
		if err := validate(scope); err != nil {
			return err
		}
		return s.CheckMediaRemovalRunning(ctx, v.ID)
	}
	for i, u := range v.Definition.Units {
		if err := current(u.Definition.Scope); err != nil {
			return err
		}
		if u.Absent {
			if err := mediaRemovalUnitAbsent(ctx, v, i, clients[i]); err != nil {
				return err
			}
		} else {
			child, err := s.RecoveryDisposal(ctx, mediaRemovalChildID(v.Definition, i))
			if err != nil {
				return err
			}
			if child.State != "completed" {
				review, err := s.ReviewRecoveryDisposal(ctx, child, clients[i])
				if err != nil {
					return err
				}
				if !review.CanContinue {
					return ErrState
				}
				if err := s.ExecuteRecoveryDisposal(ctx, child, clients[i], review.Fingerprint, func() error { return current(u.Definition.Scope) }); err != nil {
					return err
				}
			}
			if err := mediaRemovalUnitAbsent(ctx, v, i, clients[i]); err != nil {
				return err
			}
		}
		if err := current(u.Definition.Scope); err != nil {
			return err
		}
		if err := s.receiptMediaRemovalUnit(ctx, v, i); err != nil {
			return err
		}
	}
	for i, u := range v.Definition.Units {
		if err := current(u.Definition.Scope); err != nil {
			return err
		}
		if err := mediaRemovalUnitAbsent(ctx, v, i, clients[i]); err != nil {
			return err
		}
	}
	return s.finishMediaRemoval(ctx, v, guards...)
}

// Returned objects never reopen the original writer or reconstruct deleted
// siblings. The whole coordinator is then stopped, retaining its audit trail.
func (s *Store) ReturnMediaRemovalCapture(ctx context.Context, v MediaRemovalReview, ordinal int, validate func(RecoveryScope) error) error {
	if ordinal < 0 || ordinal >= len(v.Units) || v.Units[ordinal].Review == nil || !v.Units[ordinal].Review.CanReturn {
		return ErrState
	}
	u := v.Definition.Units[ordinal]
	if u.Absent || u.Definition.Scope.Remote != nil {
		return ErrState
	}
	review := v.Units[ordinal].Review
	if err := s.ReturnRecoveryCapture(ctx, review.Disposal, *review, func() error {
		if err := validate(u.Definition.Scope); err != nil {
			return err
		}
		return s.CheckMediaRemovalOpen(ctx, v.ID)
	}); err != nil {
		return err
	}
	return s.CloseMediaRemoval(ctx, v.ID)
}
