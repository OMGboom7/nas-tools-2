package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func discPathOverlap(a, b string) bool {
	fold := func(s string) string { return cases.Fold().String(norm.NFD.String(filepath.ToSlash(s))) }
	a, b = fold(a), fold(b)
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func reserveDiscTargets(ctx context.Context, tx *sql.Tx, id string, d Definition) error {
	for i, a := range d.Entries {
		for _, b := range d.Entries[i+1:] {
			if (IsDisc(a) || IsDisc(b)) && discPathOverlap(a.Target, b.Target) {
				return ErrClaimed
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_ORGANIZATION_DISC_TARGETS(ROOT_ID TEXT NOT NULL,TARGET TEXT NOT NULL,JOB_ID TEXT NOT NULL,PRIMARY KEY(ROOT_ID,TARGET))`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT TARGET FROM GO_ORGANIZATION_DISC_TARGETS WHERE ROOT_ID=? AND JOB_ID<>?`, d.TargetIdentity, id)
	if err != nil {
		return err
	}
	existing := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		existing = append(existing, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range d.Entries {
		for _, root := range existing {
			if discPathOverlap(e.Target, root) {
				return ErrClaimed
			}
		}
	}
	newDiscs := []Entry{}
	for _, e := range d.Entries {
		if IsDisc(e) {
			newDiscs = append(newDiscs, e)
		}
	}
	if len(newDiscs) == 0 {
		return nil
	}
	rows, err = tx.QueryContext(ctx, `SELECT DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID<>? AND ID IN(SELECT JOB_ID FROM GO_ORGANIZATION_TARGETS) AND json_extract(DEFINITION,'$.targetIdentity')=?`, id, d.TargetIdentity)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var previous Definition
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		if json.Unmarshal([]byte(raw), &previous) != nil {
			return ErrState
		}
		for _, e := range previous.Entries {
			for _, disc := range newDiscs {
				if discPathOverlap(e.Target, disc.Target) {
					return ErrClaimed
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, disc := range newDiscs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_ORGANIZATION_DISC_TARGETS VALUES(?,?,?)`, d.TargetIdentity, disc.Target, id); err != nil {
			return err
		}
	}
	return nil
}
func releaseDiscTargets(ctx context.Context, tx *sql.Tx, id string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_ORGANIZATION_DISC_TARGETS' AND type='table')`).Scan(&exists); err != nil || !exists {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM GO_ORGANIZATION_DISC_TARGETS WHERE JOB_ID=?`, id)
	return err
}
