package organization

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"unicode/utf8"
)

type SyncRecord struct {
	ID            int64    `json:"id,string"`
	Source        string   `json:"source"`
	Dest          string   `json:"dest"`
	Unknown       string   `json:"unknown"`
	Mode          string   `json:"mode"`
	Compatibility int64    `json:"compatibility"`
	Rename        int64    `json:"rename"`
	Enabled       int64    `json:"enabled"`
	Note          string   `json:"note"`
	Locating      int64    `json:"locating"`
	NullFields    []string `json:"nullFields,omitempty"`
}
type SyncOrigin struct {
	Record        SyncRecord `json:"record"`
	Scope         string     `json:"scope"`
	ScopeIdentity string     `json:"scopeIdentity"`
	GraphDigest   string     `json:"graphDigest"`
	Actor         string     `json:"actor"`
}

const syncColumns = `ID,SOURCE,DEST,UNKNOWN,MODE,COMPATIBILITY,RENAME,ENABLED,NOTE,LOCATING`

func scanSync(row interface{ Scan(...any) error }) (SyncRecord, error) {
	r := SyncRecord{}
	var source, dest, unknown, mode, note sql.NullString
	var compatibility, rename, enabled, locating sql.NullInt64
	err := row.Scan(&r.ID, &source, &dest, &unknown, &mode, &compatibility, &rename, &enabled, &note, &locating)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.Source, r.Dest, r.Unknown, r.Mode, r.Note = source.String, dest.String, unknown.String, mode.String, note.String
	r.Compatibility, r.Rename, r.Enabled, r.Locating = compatibility.Int64, rename.Int64, enabled.Int64, locating.Int64
	for _, field := range []struct {
		name  string
		valid bool
	}{{"source", source.Valid}, {"dest", dest.Valid}, {"unknown", unknown.Valid}, {"mode", mode.Valid}, {"compatibility", compatibility.Valid}, {"rename", rename.Valid}, {"enabled", enabled.Valid}, {"note", note.Valid}, {"locating", locating.Valid}} {
		if !field.valid {
			r.NullFields = append(r.NullFields, field.name)
		}
	}
	for _, value := range []string{r.Source, r.Dest, r.Unknown, r.Mode, r.Note} {
		if !utf8.ValidString(value) {
			return r, ErrState
		}
	}
	return r, nil
}
func readSync(ctx context.Context, reader unknownReader, id int64) (SyncRecord, error) {
	return scanSync(reader.QueryRowContext(ctx, `SELECT `+syncColumns+` FROM CONFIG_SYNC_PATHS WHERE ID=?`, id))
}
func (s *Store) GetSync(ctx context.Context, id int64) (SyncRecord, error) {
	return readSync(ctx, s.db, id)
}

type syncReader interface {
	unknownReader
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func SyncGraphDigest(records []SyncRecord) string {
	enabled := []SyncRecord{}
	for _, record := range records {
		if record.Enabled == 1 {
			enabled = append(enabled, record)
		}
	}
	return Digest(enabled)
}
func (s *Store) ListSync(ctx context.Context) ([]SyncRecord, error) { return listSync(ctx, s.db) }
func listSync(ctx context.Context, reader syncReader) ([]SyncRecord, error) {
	items := []SyncRecord{}
	var exists bool
	if err := reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='CONFIG_SYNC_PATHS')`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return items, nil
	}
	rows, err := reader.QueryContext(ctx, `SELECT `+syncColumns+` FROM CONFIG_SYNC_PATHS ORDER BY ID LIMIT 129`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanSync(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, r)
		if len(items) > 128 {
			return nil, ErrLimit
		}
	}
	return items, rows.Err()
}

// Legacy rows suppress another direct sync, but are not evidence that a target
// exists today. Match only configured lexical roots and their bound physical
// aliases; never resolve arbitrary paths stored in old history.
func syncRecorded(ctx context.Context, reader unknownReader, d Definition, relative string) (bool, error) {
	var exists bool
	if err := reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='SYNC_HISTORY')`).Scan(&exists); err != nil || !exists {
		return false, err
	}
	r := d.Sync.Record
	err := reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM SYNC_HISTORY WHERE PATH IN (?,?) AND DEST IN (?,?))`, filepath.Join(r.Source, relative), filepath.Join(d.SourceRoot, relative), filepath.Clean(r.Dest), d.TargetRoot).Scan(&exists)
	return exists, err
}
func (s *Store) SyncRecorded(ctx context.Context, d Definition, relative string) (bool, error) {
	if d.Sync == nil {
		return false, ErrState
	}
	return syncRecorded(ctx, s.db, d, relative)
}
func verifySyncRecord(ctx context.Context, reader syncReader, d Definition) error {
	if d.Sync == nil {
		return nil
	}
	r, err := readSync(ctx, reader, d.Sync.Record.ID)
	if err != nil {
		return err
	}
	if r.Enabled != 1 || r.Rename != 0 || Digest(r) != Digest(d.Sync.Record) {
		return ErrState
	}
	records, err := listSync(ctx, reader)
	if err != nil {
		return err
	}
	if SyncGraphDigest(records) != d.Sync.GraphDigest {
		return ErrState
	}
	return nil
}
func (s *Store) VerifySync(ctx context.Context, job Job) error {
	if job.Definition.Sync == nil {
		return nil
	}
	if err := verifySyncRecord(ctx, s.db, job.Definition); err != nil {
		return err
	}
	current, err := s.Get(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, item := range current.Items {
		if item.State == "completed" {
			continue
		}
		exists, err := syncRecorded(ctx, s.db, current.Definition, item.Source)
		if err != nil {
			return err
		}
		if exists {
			return ErrState
		}
	}
	return nil
}
