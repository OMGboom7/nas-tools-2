package organization

import (
	"context"
	"database/sql"
)

func libraryRecoveryAuthority(ctx context.Context, tx *sql.Tx, job string) error {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, job).Scan(&state); err != nil {
		return err
	}
	if state == RecoveryClosed {
		return ErrState
	}
	return nil
}
