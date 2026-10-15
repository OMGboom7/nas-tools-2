package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// DownloadReservation is a snapshot of the immutable plan and only untouched
// saved jobs. It is never evidence that any file has been transferred.
type DownloadReservation struct {
	BatchID     string       `json:"batchId"`
	State       string       `json:"state"`
	Jobs        []string     `json:"jobs"`
	Missing     []int        `json:"missing"`
	Definitions []Definition `json:"-"`
	Digest      string       `json:"digest"`
}

func readDownloadReservation(ctx context.Context, tx *sql.Tx, id string) (DownloadReservation, error) {
	r := DownloadReservation{BatchID: id, Jobs: []string{}, Missing: []int{}}
	var raw, proof string
	if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT,STATE,RESULT_PROOF FROM GO_DOWNLOAD_ORGANIZATION_BATCHES WHERE ID=?`, id).Scan(&raw, &r.State, &proof); err != nil {
		return r, err
	}
	if (r.State != "reserving" && r.State != "needs_review") || proof != "" {
		return r, ErrState
	}
	var doc downloadBatchDocument
	if json.Unmarshal([]byte(raw), &doc) != nil || doc.Origin.BatchID != id || len(doc.Definitions) == 0 || len(doc.Definitions) > 128 {
		return r, ErrState
	}
	r.Definitions = doc.Definitions
	expected := map[string]int{}
	for i, d := range doc.Definitions {
		if d.Download == nil || Digest(*d.Download) != Digest(doc.Origin) {
			return r, ErrState
		}
		key := Digest(d)
		if _, exists := expected[key]; exists {
			return r, ErrState
		}
		expected[key] = i
	}
	rows, err := tx.QueryContext(ctx, `SELECT ID,FINGERPRINT,DEFINITION,STATE FROM GO_ORGANIZATION_JOBS WHERE json_extract(DEFINITION,'$.download.batchId')=? ORDER BY ID`, id)
	if err != nil {
		return r, err
	}
	type saved struct {
		id         string
		definition Definition
	}
	jobs := []saved{}
	seen := map[int]bool{}
	for rows.Next() {
		var jobID, fp, definition, state string
		if err := rows.Scan(&jobID, &fp, &definition, &state); err != nil {
			rows.Close()
			return r, err
		}
		var d Definition
		if json.Unmarshal([]byte(definition), &d) != nil || state != "active" || fp != Digest(d) {
			rows.Close()
			return r, ErrState
		}
		i, exists := expected[fp]
		if !exists || seen[i] {
			rows.Close()
			return r, ErrState
		}
		seen[i] = true
		jobs = append(jobs, saved{jobID, d})
		r.Jobs = append(r.Jobs, jobID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return r, err
	}
	for _, job := range jobs {
		items, err := tx.QueryContext(ctx, `SELECT ORDINAL,STATE,REASON,PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? ORDER BY ORDINAL`, job.id)
		if err != nil {
			return r, err
		}
		count := 0
		for items.Next() {
			var index int
			var state, reason, proof string
			if err := items.Scan(&index, &state, &reason, &proof); err != nil {
				items.Close()
				return r, err
			}
			if index != count || count >= len(job.definition.Entries) || state != "planned" || reason != "" || proof != "{}" {
				items.Close()
				return r, ErrState
			}
			count++
		}
		err = items.Err()
		items.Close()
		if err != nil {
			return r, err
		}
		if count != len(job.definition.Entries) {
			return r, ErrState
		}
		for _, e := range job.definition.Entries {
			var owner string
			if err := tx.QueryRowContext(ctx, `SELECT JOB_ID FROM GO_ORGANIZATION_TARGETS WHERE TARGET_KEY=?`, Digest(job.definition.TargetIdentity+":"+e.Target)).Scan(&owner); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return r, ErrState
				}
				return r, err
			}
			if owner != job.id {
				return r, ErrState
			}
		}
	}
	for i := range doc.Definitions {
		if !seen[i] {
			r.Missing = append(r.Missing, i)
		}
	}
	// A full but uncommitted reservation is recoverable too. Bind every saved
	// ID and the original document, not merely the number of missing jobs.
	r.Digest = Digest(struct {
		Document string
		State    string
		Jobs     []string
		Missing  []int
	}{raw, r.State, r.Jobs, r.Missing})
	return r, nil
}

func (s *Store) DownloadReservation(ctx context.Context, id string) (DownloadReservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DownloadReservation{}, err
	}
	defer tx.Rollback()
	return readDownloadReservation(ctx, tx, id)
}

// CompleteDownloadReservation only creates missing original job rows and target
// claims. All rows, the audit receipt and ready state commit together; it does
// not execute jobs, resume a monitor, or change downloader state.
func (s *Store) CompleteDownloadReservation(ctx context.Context, id, snapshot, actor, reason, fingerprint string) error {
	if actor == "" || reason == "" || len(reason) > 1000 || len(fingerprint) != 64 {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := readDownloadReservation(ctx, tx, id)
	if err != nil {
		return err
	}
	if r.Digest != snapshot {
		return ErrState
	}
	for _, d := range r.Definitions {
		if err := verifyDownloadDefinition(ctx, tx, d, true); err != nil {
			return err
		}
	}
	for _, i := range r.Missing {
		if _, err := createJobTx(ctx, tx, Digest(r.Definitions[i]), r.Definitions[i]); err != nil {
			return err
		}
	}
	complete, err := readDownloadReservation(ctx, tx, id)
	if err != nil {
		return err
	}
	if len(complete.Missing) != 0 || len(complete.Jobs) != len(r.Definitions) {
		return ErrState
	}
	for _, d := range r.Definitions {
		if err := verifyDownloadDefinition(ctx, tx, d, true); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_DOWNLOAD_RESERVATION_RECOVERY(BATCH_ID TEXT PRIMARY KEY,ACTOR TEXT NOT NULL,REASON TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,SNAPSHOT TEXT NOT NULL,CREATED TEXT NOT NULL)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_DOWNLOAD_RESERVATION_RECOVERY VALUES(?,?,?,?,?,?)`, id, actor, reason, fingerprint, snapshot, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_DOWNLOAD_ORGANIZATION_BATCHES SET STATE='ready',REASON='' WHERE ID=? AND STATE=? AND RESULT_PROOF=''`, id, r.State)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	return tx.Commit()
}
