package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/opensubtitles"
)

func openSubtitleRowDigest(p OpenSubtitleReceipt) string {
	p.Outputs = nil
	return Digest([]any{p, p.Definition, p.Fingerprint, p.Binding, p.NextCheck, p.Operations})
}
func (s *Store) WaitOpenSubtitle(ctx context.Context, expected OpenSubtitleReceipt) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	p, e := openSubtitleRead(ctx, tx, expected.ID)
	if e != nil || p.State != "pending" || openSubtitleRowDigest(p) != openSubtitleRowDigest(expected) {
		return ErrState
	}
	if !errors.Is(openSubtitleCurrent(ctx, tx, p.Definition), bus.ErrEventWaiting) {
		return ErrState
	}
	next := time.Now().UTC().Add(30 * time.Second).Format("2006-01-02T15:04:05.000000000Z")
	res, e := tx.ExecContext(ctx, `UPDATE GO_OPEN_SUBTITLE_REQUESTS SET NEXT_CHECK=? WHERE ID=? AND STATE='pending'`, next, p.ID)
	if e != nil {
		return e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	p.NextCheck = next
	saved, e := openSubtitleRead(ctx, tx, p.ID)
	if e != nil || openSubtitleRowDigest(saved) != openSubtitleRowDigest(p) {
		return ErrState
	}
	return tx.Commit()
}
func wakeOpenSubtitle(ctx context.Context, tx *sql.Tx, job string, ordinal int) error {
	yes, e := recoveryTable(ctx, tx, "GO_OPEN_SUBTITLE_REQUESTS")
	if e != nil || !yes {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE GO_OPEN_SUBTITLE_REQUESTS SET NEXT_CHECK='' WHERE KIND='intake' AND REFERENCE=? AND ORDINAL=? AND STATE='pending'`, job, ordinal); e != nil {
		return e
	}
	var remain int
	e = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_OPEN_SUBTITLE_REQUESTS WHERE KIND='intake' AND REFERENCE=? AND ORDINAL=? AND STATE='pending' AND NEXT_CHECK!=''`, job, ordinal).Scan(&remain)
	if e != nil {
		return e
	}
	if remain != 0 {
		return ErrState
	}
	return nil
}
func (s *Store) ClaimOpenSubtitle(ctx context.Context, expected OpenSubtitleReceipt, library LibraryDefinition, media LibraryMedia, current func() error) (OpenSubtitleReceipt, error) {
	zero := OpenSubtitleReceipt{}
	if !recoveryHex(expected.ID, 64) || current == nil || current() != nil || library.OpenSubtitle == nil || library.OpenSubtitle.Request != expected.ID || library.OpenSubtitle.Fingerprint != expected.Fingerprint || !library.OpenSubtitle.Query.Valid() || library.Actor != expected.Definition.Provider.Actor || library.Mode != "no_force" || library.Count != 1 || library.RequestID != expected.ID[:32] {
		return zero, ErrState
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return zero, e
	}
	defer tx.Rollback()
	p, e := openSubtitleRead(ctx, tx, expected.ID)
	if e != nil || p.State != "pending" || openSubtitleRowDigest(p) != openSubtitleRowDigest(expected) {
		return zero, ErrState
	}
	if e = openSubtitleCurrent(ctx, tx, p.Definition); e != nil {
		return zero, e
	}
	var denied bool
	if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_OPEN_SUBTITLE_AUTH_BLOCKS WHERE SCOPE=?)`, p.Definition.Provider.Epoch).Scan(&denied); e != nil {
		return zero, e
	}
	if denied {
		return zero, opensubtitles.ErrQuota
	}
	if library.Remote != nil && !p.Definition.RemoteWrite {
		return zero, ErrState
	}
	id, e := createLibraryJob(ctx, tx, Digest([]any{"open-subtitle-publication", p.Fingerprint, library, media}), library, []LibraryMedia{media})
	if e != nil {
		return zero, e
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, e := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_JOBS SET STATE='running',UPDATED=? WHERE ID=? AND STATE='ready'`, now, id)
	if e != nil {
		return zero, e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return zero, ErrState
	}
	res, e = tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_ITEMS SET STATE='running' WHERE JOB_ID=? AND ORDINAL=0 AND STATE='pending'`, id)
	if e != nil {
		return zero, e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return zero, ErrState
	}
	binding := Digest([]any{library, media})
	res, e = tx.ExecContext(ctx, `UPDATE GO_OPEN_SUBTITLE_REQUESTS SET STATE='running',LIBRARY_ID=?,BINDING=?,UPDATED=?,NEXT_CHECK='' WHERE ID=? AND STATE='pending'`, id, binding, now, p.ID)
	if e != nil {
		return zero, e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return zero, ErrState
	}
	p.State = "running"
	p.LibraryID = id
	p.Binding = binding
	p.Updated = now
	p.NextCheck = ""
	saved, e := openSubtitleRead(ctx, tx, p.ID)
	if e != nil || openSubtitleRowDigest(saved) != openSubtitleRowDigest(p) {
		return zero, ErrState
	}
	if e = openSubtitleCurrent(ctx, tx, p.Definition); e != nil {
		return zero, e
	}
	if current() != nil {
		return zero, ErrState
	}
	if e = tx.Commit(); e != nil {
		return zero, e
	}
	return saved, nil
}
func openSubtitleRunning(ctx context.Context, tx *sql.Tx, id, fp, library string) error {
	p, e := openSubtitleRead(ctx, tx, id)
	if e != nil || p.Fingerprint != fp || p.LibraryID != library || p.State != "running" {
		return ErrState
	}
	var state string
	if e = tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, library).Scan(&state); e != nil || state != "running" {
		return ErrState
	}
	return openSubtitleCurrent(ctx, tx, p.Definition)
}
func (s *Store) OpenSubtitleRunning(ctx context.Context, p OpenSubtitleReceipt) error {
	tx, e := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	saved, e := openSubtitleRead(ctx, tx, p.ID)
	if e != nil || saved.Binding != p.Binding {
		return ErrState
	}
	return openSubtitleRunning(ctx, tx, p.ID, p.Fingerprint, p.LibraryID)
}
func (s *Store) OpenSubtitleStep(ctx context.Context, id, fp, binding, kind, signature string, current func() error) (int, error) {
	if current == nil || current() != nil || !recoveryHex(signature, 64) {
		return 0, ErrState
	}
	switch kind {
	case "login", "search", "archive", "download-grant", "subtitle":
	default:
		return 0, ErrState
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	p, e := openSubtitleRead(ctx, tx, id)
	if e != nil || p.Fingerprint != fp || p.Binding != binding || p.State != "running" || len(p.Operations) >= 256 {
		return 0, ErrState
	}
	if e = openSubtitleRunning(ctx, tx, p.ID, fp, p.LibraryID); e != nil {
		return 0, e
	}
	if n := len(p.Operations); n > 0 && (p.Operations[n-1].State == "intent" || p.Operations[n-1].State == "uncertain" || p.Operations[n-1].State == "denied") {
		return 0, ErrState
	}
	for _, previous := range p.Operations {
		if previous.Kind == kind && previous.Signature == signature {
			return 0, ErrState
		}
	}
	index := len(p.Operations)
	p.Operations = append(p.Operations, OpenSubtitleOperation{Kind: kind, Signature: signature, State: "intent"})
	p.Requests = len(p.Operations)
	p.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	raw, _ := json.Marshal(p.Operations)
	res, e := tx.ExecContext(ctx, `UPDATE GO_OPEN_SUBTITLE_REQUESTS SET OPERATIONS=?,UPDATED=? WHERE ID=? AND STATE='running'`, string(raw), p.Updated, p.ID)
	if e != nil {
		return 0, e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, ErrState
	}
	saved, e := openSubtitleRead(ctx, tx, p.ID)
	if e != nil || openSubtitleRowDigest(saved) != openSubtitleRowDigest(p) {
		return 0, ErrState
	}
	if e = openSubtitleRunning(ctx, tx, p.ID, fp, p.LibraryID); e != nil {
		return 0, e
	}
	if current() != nil {
		return 0, ErrState
	}
	if e = tx.Commit(); e != nil {
		return 0, e
	}
	return index, nil
}
func (s *Store) FinishOpenSubtitleStep(ctx context.Context, id, fp, binding string, index int, state, digest string) error {
	if state != "received" && state != "denied" && state != "uncertain" || digest != "" && !recoveryHex(digest, 64) {
		return ErrState
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	p, e := openSubtitleRead(ctx, tx, id)
	if e != nil || p.Fingerprint != fp || p.Binding != binding || p.State != "running" || index < 0 || index != len(p.Operations)-1 || p.Operations[index].State != "intent" {
		return ErrState
	}
	p.Operations[index].State = state
	p.Operations[index].Digest = digest
	p.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	raw, _ := json.Marshal(p.Operations)
	res, e := tx.ExecContext(ctx, `UPDATE GO_OPEN_SUBTITLE_REQUESTS SET OPERATIONS=?,UPDATED=? WHERE ID=? AND STATE='running'`, string(raw), p.Updated, p.ID)
	if e != nil {
		return e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	if state == "denied" {
		if _, e = tx.ExecContext(ctx, `INSERT INTO GO_OPEN_SUBTITLE_AUTH_BLOCKS(SCOPE,REQUEST_ID,CREATED)VALUES(?,?,?) ON CONFLICT(SCOPE)DO NOTHING`, p.Definition.Provider.Epoch, p.ID, p.Updated); e != nil {
			return e
		}
		var blocked bool
		if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_OPEN_SUBTITLE_AUTH_BLOCKS WHERE SCOPE=?)`, p.Definition.Provider.Epoch).Scan(&blocked); e != nil || !blocked {
			return ErrState
		}
	}
	saved, e := openSubtitleRead(ctx, tx, p.ID)
	if e != nil || openSubtitleRowDigest(saved) != openSubtitleRowDigest(p) {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) SaveOpenSubtitleOutput(ctx context.Context, p OpenSubtitleReceipt, root LibraryRoot, target, owner, state, reason string, proof LibraryOutputProof, current func() error) error {
	if current == nil || state == "replaced" || owner != Digest([]string{"open-subtitle-output", p.ID, target}) || state == "writing" && current() != nil {
		return ErrState
	}
	return s.SaveLibraryOutput(ctx, p.LibraryID, root, target, owner, state, reason, proof, func(tx *sql.Tx) error {
		original, e := openSubtitleRead(ctx, tx, p.ID)
		if e != nil || original.Fingerprint != p.Fingerprint || original.LibraryID != p.LibraryID || original.Binding != p.Binding {
			return ErrState
		}
		var rawLibrary, rawMedia string
		if tx.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, p.LibraryID).Scan(&rawLibrary) != nil ||
			tx.QueryRowContext(ctx, `SELECT MEDIA FROM GO_LIBRARY_SCRAPE_ITEMS WHERE JOB_ID=? AND ORDINAL=0`, p.LibraryID).Scan(&rawMedia) != nil {
			return ErrState
		}
		var library LibraryDefinition
		var media LibraryMedia
		if opensubtitles.DecodeLimit(rawLibrary, &library, 1<<20) != nil || opensubtitles.DecodeLimit(rawMedia, &media, LibraryWorklistBytes) != nil || Digest(root) != Digest(library.Root) {
			return ErrState
		}
		allowed := false
		for _, extension := range []string{".srt", ".ass", ".ssa"} {
			fixed, err := LibraryTarget(library, media, "media", OpenSubtitleName(original.Definition, extension))
			if err == nil && fixed == target {
				allowed = true
			}
		}
		if !allowed {
			return ErrState
		}
		if state == "writing" {
			if e = openSubtitleRunning(ctx, tx, p.ID, p.Fingerprint, p.LibraryID); e != nil {
				return e
			}
			return current()
		}
		return nil
	})
}
func (s *Store) FinishOpenSubtitle(ctx context.Context, id, fp, state, reason string) error {
	if len(reason) > 1000 {
		return ErrState
	}
	switch state {
	case "completed", "not_found", "partial", "blocked", "uncertain":
	default:
		return ErrState
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	p, e := openSubtitleRead(ctx, tx, id)
	if e != nil || p.Fingerprint != fp || p.State != "pending" && p.State != "running" {
		return ErrState
	}
	if p.State == "pending" && state != "blocked" {
		return ErrState
	}
	if p.LibraryID != "" {
		var js string
		if e = tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, p.LibraryID).Scan(&js); e != nil {
			return e
		}
		if state == "completed" || state == "not_found" {
			var bad, total int
			if e = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(STATE NOT IN('created','preserved')),0) FROM GO_LIBRARY_SCRAPE_OUTPUTS WHERE JOB_ID=?`, p.LibraryID).Scan(&total, &bad); e != nil {
				return e
			}
			if bad != 0 || state == "completed" && total == 0 || state == "not_found" && total != 0 {
				return ErrState
			}
		}
		if js == "running" {
			ls := "needs_review"
			is := "blocked"
			if state == "completed" || state == "not_found" {
				ls = "completed"
				is = "completed"
			} else if state == "partial" {
				ls = "partial"
				is = "partial"
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			res, e := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_JOBS SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE='running'`, ls, reason, now, p.LibraryID)
			if e != nil {
				return e
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrState
			}
			res, e = tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_ITEMS SET STATE=?,REASON=? WHERE JOB_ID=? AND ORDINAL=0 AND STATE='running'`, is, reason, p.LibraryID)
			if e != nil {
				return e
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrState
			}
			var saved string
			if e = tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, p.LibraryID).Scan(&saved); e != nil || saved != ls {
				return ErrState
			}
			if e = tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_ITEMS WHERE JOB_ID=? AND ORDINAL=0`, p.LibraryID).Scan(&saved); e != nil || saved != is {
				return ErrState
			}
		}
	}
	p.State = state
	p.Reason = reason
	p.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	p.NextCheck = ""
	res, e := tx.ExecContext(ctx, `UPDATE GO_OPEN_SUBTITLE_REQUESTS SET STATE=?,REASON=?,UPDATED=?,NEXT_CHECK='' WHERE ID=? AND STATE IN('pending','running')`, p.State, p.Reason, p.Updated, p.ID)
	if e != nil {
		return e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	saved, e := openSubtitleRead(ctx, tx, p.ID)
	if e != nil || openSubtitleRowDigest(saved) != openSubtitleRowDigest(p) {
		return ErrState
	}
	return tx.Commit()
}
func stopOpenSubtitle(ctx context.Context, tx *sql.Tx, id, actor, reason string) (OpenSubtitleReceipt, error) {
	p, e := openSubtitleRead(ctx, tx, id)
	if e != nil {
		return p, e
	}
	if p.State != "pending" || p.LibraryID != "" || p.Requests != 0 || !subtitleRequestText(actor, 256) || !subtitleRequestText(reason, 600) {
		return p, ErrState
	}
	p.State = "stopped"
	p.Reason = actor + ": " + reason
	p.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	p.NextCheck = ""
	res, e := tx.ExecContext(ctx, `UPDATE GO_OPEN_SUBTITLE_REQUESTS SET STATE='stopped',REASON=?,UPDATED=?,NEXT_CHECK='' WHERE ID=? AND STATE='pending' AND LIBRARY_ID='' AND OPERATIONS='[]'`, p.Reason, p.Updated, p.ID)
	if e != nil {
		return p, e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return p, ErrState
	}
	saved, e := openSubtitleRead(ctx, tx, p.ID)
	if e != nil || openSubtitleRowDigest(saved) != openSubtitleRowDigest(p) {
		return p, ErrState
	}
	return saved, nil
}
func (s *Store) StopOpenSubtitle(ctx context.Context, id, actor, reason string, current func() error) (OpenSubtitleReceipt, error) {
	if current == nil || current() != nil {
		return OpenSubtitleReceipt{}, ErrState
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return OpenSubtitleReceipt{}, e
	}
	defer tx.Rollback()
	p, e := stopOpenSubtitle(ctx, tx, id, actor, reason)
	if e != nil {
		return p, e
	}
	if current() != nil {
		return p, ErrState
	}
	if e = tx.Commit(); e != nil {
		return p, e
	}
	return p, nil
}
func (s *Store) OpenSubtitleDenied(ctx context.Context, epoch string) (bool, error) {
	yes, e := recoveryTable(ctx, s.db, "GO_OPEN_SUBTITLE_AUTH_BLOCKS")
	if e != nil || !yes {
		return false, e
	}
	var denied bool
	e = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_OPEN_SUBTITLE_AUTH_BLOCKS WHERE SCOPE=?)`, epoch).Scan(&denied)
	return denied, e
}
