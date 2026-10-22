package organization

import (
	"context"
	"encoding/json"
)

// Adoption is a one-way transition of an already claimed, running item. A
// restart cannot hash a different object and silently re-enter a previous write.
func (s *Store) BindLibraryRemoteMedia(ctx context.Context, job string, ordinal int, previous, prepared LibraryMedia) error {
	if previous.Remote == nil || previous.Remote.Digest != "" || !LibraryRemotePrepared(prepared) {
		return ErrState
	}
	before, err := libraryMediaDocument(previous)
	if err != nil {
		return err
	}
	after, err := libraryMediaDocument(prepared)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var definition, state, media string
	if err := tx.QueryRowContext(ctx, `SELECT DEFINITION,STATE FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, job).Scan(&definition, &state); err != nil {
		return err
	}
	var d LibraryDefinition
	if state != "running" || json.Unmarshal([]byte(definition), &d) != nil || !LibraryRemoteApproved(d) {
		return ErrState
	}
	// Strip only newly adopted digests; every inventory/path/input binding must
	// remain exactly the reviewed document, including the complete disc scope.
	copy := prepared
	r := *prepared.Remote
	r.Digest = ""
	copy.Remote = &r
	if prepared.Disc != nil {
		disc := *prepared.Disc
		disc.Files = append([]LibraryMedia{}, prepared.Disc.Files...)
		copy.Disc = &disc
		for i, f := range disc.Files {
			remote := *f.Remote
			remote.Digest = ""
			disc.Files[i].Remote = &remote
		}
	}
	if Digest(copy) != Digest(previous) {
		return ErrState
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_ITEMS SET MEDIA=? WHERE JOB_ID=? AND ORDINAL=? AND STATE='running' AND MEDIA=?`, string(after), job, ordinal, string(before))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	if err := tx.QueryRowContext(ctx, `SELECT MEDIA FROM GO_LIBRARY_SCRAPE_ITEMS WHERE JOB_ID=? AND ORDINAL=? AND STATE='running'`, job, ordinal).Scan(&media); err != nil || media != string(after) {
		return ErrState
	}
	return tx.Commit()
}
