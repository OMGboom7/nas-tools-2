package organization

import (
	"context"
	"database/sql"
	"errors"
)

func remoteMetadataKey(root, target string) string {
	return libraryTargetClaim(target, LibraryOutputProof{ParentIdentity: libraryRemoteParent(root, target)})
}

// Intake and existing-library enrichment share the logical target reservation
// even across providers/overlapping roots. An uncertain receipt is never erased
// or bypassed by switching entry points. Namespace operations are serialized by
// the API; these checks also remain inside the intent's SQLite transaction.
func checkRemoteMetadataClaims(ctx context.Context, tx *sql.Tx, root, target, libraryID, scrapeID string) error {
	key := remoteMetadataKey(root, target)
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_LIBRARY_SCRAPE_CLAIMS')`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		var holder string
		err := tx.QueryRowContext(ctx, `SELECT OUTPUT_ID FROM GO_LIBRARY_SCRAPE_CLAIMS WHERE TARGET_KEY=?`, key).Scan(&holder)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && holder != libraryID {
			return ErrClaimed
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_SCRAPE_JOBS') AND EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_ORGANIZATION_JOBS')`).Scan(&exists); err != nil || !exists {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.ID,json_extract(j.DEFINITION,'$.targetRoot'),json_extract(o.value,'$.Proof.Target') FROM GO_SCRAPE_JOBS s JOIN GO_ORGANIZATION_JOBS j ON j.ID=s.JOB_ID JOIN json_each(s.OUTPUTS) o WHERE json_type(j.DEFINITION,'$.remote')='object' AND json_extract(o.value,'$.State') IN('writing','uncertain')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, otherRoot, otherTarget string
		if err := rows.Scan(&id, &otherRoot, &otherTarget); err != nil {
			return err
		}
		if id != scrapeID && remoteMetadataKey(otherRoot, otherTarget) == key {
			return ErrClaimed
		}
	}
	return rows.Err()
}
