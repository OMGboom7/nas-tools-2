package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

type reservationQuery struct{ table, where, order string }

func reservationQueries(kind string) []reservationQuery {
	switch kind {
	case "organization":
		return []reservationQuery{
			{"GO_ORGANIZATION_JOBS", "ID=?", "ID"}, {"GO_ORGANIZATION_ITEMS", "JOB_ID=?", "ORDINAL"},
			{"GO_ORGANIZATION_TARGETS", "JOB_ID=?", "TARGET_KEY"}, {"GO_ORGANIZATION_DISC_TARGETS", "JOB_ID=?", "ROOT_ID,TARGET"}, {"GO_ORGANIZATION_REMOTE_TARGETS", "JOB_ID=?", "TARGET"},
			{"GO_ORGANIZATION_UNKNOWN_LINKS", "JOB_ID=?", "JOB_ID"}, {"GO_ORGANIZATION_UNKNOWN_DETACH", "JOB_ID=?", "JOB_ID"},
			{"GO_ORGANIZATION_SOURCE_RESTORE", "JOB_ID=?", "JOB_ID"}, {"GO_ORGANIZATION_REMOTE_CLOSURES", "JOB_ID=?", "JOB_ID"},
			{"GO_SCRAPE_JOBS", "JOB_ID=?", "ID"}, {"GO_SUBTITLE_JOBS", "JOB_ID=?", "ID"},
		}
	case "library":
		return []reservationQuery{
			{"GO_LIBRARY_SCRAPE_JOBS", "ID=?", "ID"}, {"GO_LIBRARY_SCRAPE_ITEMS", "JOB_ID=?", "ORDINAL"}, {"GO_LIBRARY_SCRAPE_OUTPUTS", "JOB_ID=?", "ID"},
			{"GO_LIBRARY_SCRAPE_CLAIMS", "OUTPUT_ID IN(SELECT ID FROM GO_LIBRARY_SCRAPE_OUTPUTS WHERE JOB_ID=?)", "TARGET_KEY"},
		}
	case "recovery":
		return []reservationQuery{{"GO_RECOVERY_DISPOSALS", "ID=?", "ID"}, {"GO_RECOVERY_DISPOSAL_ITEMS", "DISPOSAL_ID=?", "ORDINAL"}}
	case "media-removal":
		return []reservationQuery{{"GO_MEDIA_REMOVALS", "ID=?", "ID"}, {"GO_MEDIA_REMOVAL_UNITS", "REMOVAL_ID=?", "ORDINAL"}}
	case "intake-scraper":
		return []reservationQuery{{"GO_SCRAPE_JOBS", "ID=?", "ID"}}
	}
	return nil
}

// Only fixed internal query specifications reach this reader. Preserve SQLite
// value types and every column, including fields from a different app version.
func reservationReadRows(ctx context.Context, r recoveryReader, q reservationQuery, id string) ([]MediaRemovalRow, error) {
	yes, err := recoveryTable(ctx, r, q.table)
	if err != nil || !yes {
		return []MediaRemovalRow{}, err
	}
	rows, err := r.QueryContext(ctx, `SELECT * FROM `+removalQuote(q.table)+` WHERE `+q.where+` ORDER BY `+q.order, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if len(columns) > 256 {
		return nil, ErrLimit
	}
	result := []MediaRemovalRow{}
	used := 0
	for rows.Next() {
		if len(result) >= ReservationReleaseRowLimit {
			return nil, ErrLimit
		}
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := MediaRemovalRow{Table: q.table, Columns: append([]string{}, columns...), Values: []MediaRemovalValue{}}
		for _, value := range values {
			v, err := removalValue(value)
			if err != nil {
				return nil, err
			}
			row.Values = append(row.Values, v)
		}
		if err := mediaRemovalRowBudget([]MediaRemovalRow{row}, &used); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
func reservationOwnerRows(ctx context.Context, r recoveryReader, kind, id string) ([]MediaRemovalRow, error) {
	queries := reservationQueries(kind)
	if len(queries) == 0 {
		return nil, ErrState
	}
	result := []MediaRemovalRow{}
	used := 0
	for _, q := range queries {
		rows, err := reservationReadRows(ctx, r, q, id)
		if err != nil {
			return nil, err
		}
		if err := mediaRemovalRowBudget(rows, &used); err != nil {
			return nil, err
		}
		result = append(result, rows...)
		if len(result) > ReservationReleaseRowLimit {
			return nil, ErrLimit
		}
	}
	if len(result) == 0 {
		return nil, ErrNotFound
	}
	return result, nil
}
func reservationText(row MediaRemovalRow, column string) string {
	for i, c := range row.Columns {
		if c == column && i < len(row.Values) {
			v := row.Values[i]
			if v.Kind == "text" {
				return v.Text
			}
			if v.Kind == "integer" {
				b, _ := json.Marshal(v.Int)
				return string(b)
			}
		}
	}
	return ""
}
func reservationSignature(ctx context.Context, r recoveryReader, kind, id string) (string, error) {
	rows, err := reservationOwnerRows(ctx, r, kind, id)
	if err != nil {
		return "", err
	}
	if kind == "intake-scraper" {
		job := reservationText(rows[0], "JOB_ID")
		var state string
		if err := r.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job).Scan(&state); err != nil {
			return "", err
		}
		if reservationText(rows[0], "STATE") != "blocked" || state != RecoveryClosed {
			return "", nil
		}
		return Digest([]any{rows, state}), nil
	}
	state := reservationText(rows[0], "STATE")
	if (kind == "organization" || kind == "library") && state != RecoveryClosed || kind == "media-removal" && state != "closed" && state != "completed" || kind == "recovery" && !recoveryTerminal(state) {
		return "", nil
	}
	return Digest(rows), nil
}
func reservationReleased(ctx context.Context, r recoveryReader, kind, id string) (bool, error) {
	yes, err := recoveryTable(ctx, r, "GO_RESERVATION_RELEASE_MARKERS")
	if err != nil || !yes {
		return false, err
	}
	var signature, releaseID string
	err = r.QueryRowContext(ctx, `SELECT SIGNATURE,RELEASE_ID FROM GO_RESERVATION_RELEASE_MARKERS WHERE KIND=? AND OWNER_ID=?`, kind, id).Scan(&signature, &releaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	receipt, err := readReservationRelease(ctx, r, releaseID)
	if err != nil {
		return false, ErrState
	}
	bound := false
	for _, o := range receipt.Owners {
		bound = bound || o.Kind == kind && o.ID == id
	}
	if kind == "intake-scraper" {
		for _, row := range receipt.Document.Rows {
			if row.Table == "GO_SCRAPE_JOBS" && reservationText(row, "ID") == id {
				bound = true
				break
			}
		}
	}
	if !bound {
		return false, ErrState
	}
	actual, err := reservationSignature(ctx, r, kind, id)
	if err != nil {
		return false, err
	}
	return actual != "" && actual == signature, nil
}
func reservationClaims(rec reservationRecord, rows []MediaRemovalRow) ([]ReservationClaim, error) {
	claims := []ReservationClaim{}
	if (rec.ref.Kind == "organization" || rec.ref.Kind == "library") && rec.state != RecoveryClosed {
		claims = append(claims, ReservationClaim{"task-authority", rec.ref.ID, ""})
	}
	for _, row := range rows {
		key, p, kind := "", "", ""
		switch row.Table {
		case "GO_ORGANIZATION_TARGETS":
			kind, key = "target", reservationText(row, "TARGET_KEY")
		case "GO_ORGANIZATION_DISC_TARGETS":
			kind, key, p = "disc-target", reservationText(row, "ROOT_ID"), reservationText(row, "TARGET")
		case "GO_ORGANIZATION_REMOTE_TARGETS":
			kind, key, p = "remote-target", reservationText(row, "TARGET"), reservationText(row, "TARGET")
		case "GO_LIBRARY_SCRAPE_CLAIMS":
			kind, key = "library-output", reservationText(row, "TARGET_KEY")
		case "GO_ORGANIZATION_UNKNOWN_LINKS":
			if reservationText(row, "STATE") == "pending" {
				kind, key = "unknown-association", reservationText(row, "RECORD_ID")
			}
		case "GO_SCRAPE_JOBS":
			var outputs []SidecarRecord
			if json.Unmarshal([]byte(reservationText(row, "OUTPUTS")), &outputs) != nil {
				return nil, ErrState
			}
			for i, o := range outputs {
				if o.State == "writing" || o.State == "uncertain" {
					claims = append(claims, ReservationClaim{"intake-output", Digest([]any{reservationText(row, "ID"), i}), o.Proof.Target})
				}
			}
		case "GO_MEDIA_REMOVALS":
			var d MediaRemovalDefinition
			if json.Unmarshal([]byte(reservationText(row, "DEFINITION")), &d) != nil {
				return nil, ErrState
			}
			if rec.state != "completed" {
				for _, u := range d.Units {
					claims = append(claims, ReservationClaim{"media-range", Digest(u.Definition.Scope), u.Definition.Scope.Root.Path + "/" + u.Definition.Path})
				}
				for _, h := range d.Histories {
					if h.Record.ID < 1 || len(h.Rows) == 0 {
						return nil, ErrState
					}
					claims = append(claims, ReservationClaim{"history-association", strconv.FormatInt(h.Record.ID, 10), ""})
				}
			}
		case "GO_RECOVERY_DISPOSALS":
			if !recoveryTerminal(rec.state) {
				kind, key = "disposal-authority", rec.ref.ID
			}
		}
		if kind != "" {
			if key == "" {
				return nil, ErrState
			}
			claims = append(claims, ReservationClaim{kind, key, p})
		}
	}
	return claims, nil
}
func buildReservationRelease(ctx context.Context, r recoveryReader, seed ReservationOwnerRef, actor, request, decision string) (ReservationRelease, error) {
	if actor == "" || !recoveryHex(request, 32) || strings.TrimSpace(decision) == "" || len(decision) > 512 {
		return ReservationRelease{}, ErrState
	}
	records, err := reservationClosure(ctx, r, seed)
	if err != nil {
		return ReservationRelease{}, err
	}
	d := reservationReleaseDocument{Seed: seed, Actor: actor, RequestID: request, Decision: decision, Owners: []ReservationOwner{}, Rows: []MediaRemovalRow{}}
	used := 0
	seen := map[string]bool{}
	appendRows := func(rows []MediaRemovalRow) error {
		for _, row := range rows {
			key := Digest(row)
			if seen[key] {
				continue
			}
			seen[key] = true
			if err := mediaRemovalRowBudget([]MediaRemovalRow{row}, &used); err != nil {
				return err
			}
			d.Rows = append(d.Rows, row)
			if len(d.Rows) > ReservationReleaseRowLimit {
				return ErrLimit
			}
		}
		return nil
	}
	for _, rec := range records {
		rows, err := reservationOwnerRows(ctx, r, rec.ref.Kind, rec.ref.ID)
		if err != nil {
			return ReservationRelease{}, err
		}
		claims, err := reservationClaims(rec, rows)
		if err != nil {
			return ReservationRelease{}, err
		}
		if len(claims) > ReservationReleaseRowLimit {
			return ReservationRelease{}, ErrLimit
		}
		paths := append([]string{}, rec.paths...)
		sort.Strings(paths)
		paths = compactReservationPaths(paths)
		o := ReservationOwner{ReservationOwnerRef: rec.ref, State: rec.state, BatchID: rec.batch, Related: rec.ref != seed, PathCount: len(paths), Paths: paths, Claims: claims}
		if len(o.Paths) > 100 {
			o.Paths = o.Paths[:100]
		}
		d.Owners = append(d.Owners, o)
		if err := appendRows(rows); err != nil {
			return ReservationRelease{}, err
		}
		if rec.batch != "" {
			q := reservationQuery{"GO_LEGACY_ORGANIZATION_BATCHES", "ID=?", "ID"}
			if rec.ref.Kind == "library" {
				q = reservationQuery{"GO_LIBRARY_SCRAPE_RUNS", "ID=?", "ID"}
			}
			more, err := reservationReadRows(ctx, r, q, rec.batch)
			if err != nil {
				return ReservationRelease{}, err
			}
			if len(more) != 1 {
				return ReservationRelease{}, ErrState
			}
			if err := appendRows(more); err != nil {
				return ReservationRelease{}, err
			}
		}
	}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 64<<20 {
		return ReservationRelease{}, ErrLimit
	}
	v := reservationProjection(d)
	if v.ClaimCount == 0 {
		return ReservationRelease{}, ErrState
	}
	if v.ClaimCount > ReservationReleaseRowLimit {
		return ReservationRelease{}, ErrLimit
	}
	return v, nil
}
func compactReservationPaths(v []string) []string {
	out := []string{}
	for _, p := range v {
		if len(out) == 0 || out[len(out)-1] != p {
			out = append(out, p)
		}
	}
	return out
}
func (s *Store) ReviewReservationRelease(ctx context.Context, seed ReservationOwnerRef, actor, request, decision string) (ReservationRelease, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ReservationRelease{}, err
	}
	defer tx.Rollback()
	return buildReservationRelease(ctx, tx, seed, actor, request, decision)
}
func reservationReleaseSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_RESERVATION_RELEASES(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,CREATED TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_RESERVATION_RELEASE_MARKERS(KIND TEXT NOT NULL,OWNER_ID TEXT NOT NULL,RELEASE_ID TEXT NOT NULL,SIGNATURE TEXT NOT NULL,PRIMARY KEY(KIND,OWNER_ID))`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func readReservationRelease(ctx context.Context, r recoveryReader, id string) (ReservationRelease, error) {
	yes, err := recoveryTable(ctx, r, "GO_RESERVATION_RELEASES")
	if err != nil {
		return ReservationRelease{}, err
	}
	if !yes {
		return ReservationRelease{}, ErrNotFound
	}
	var raw, fp, created string
	err = r.QueryRowContext(ctx, `SELECT DOCUMENT,FINGERPRINT,CREATED FROM GO_RESERVATION_RELEASES WHERE ID=?`, id).Scan(&raw, &fp, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ReservationRelease{}, ErrNotFound
	}
	if err != nil {
		return ReservationRelease{}, err
	}
	var d reservationReleaseDocument
	if len(raw) > 64<<20 || json.Unmarshal([]byte(raw), &d) != nil || ReservationReleaseID(d.Actor, d.RequestID) != id || Digest(d) != fp {
		return ReservationRelease{}, ErrState
	}
	v := reservationProjection(d)
	v.State, v.Created = "released", created
	return v, nil
}
func (s *Store) ReservationRelease(ctx context.Context, id string) (ReservationRelease, error) {
	if !recoveryHex(id, 64) {
		return ReservationRelease{}, ErrPath
	}
	return readReservationRelease(ctx, s.db, id)
}

// All old authority is closed, exact rows are released and the original typed
// snapshots/receipt are persisted in this one transaction. No file or account
// API is called, and no incomplete copy is adopted as proof of success.
func (s *Store) ReleaseReservations(ctx context.Context, v ReservationRelease) (ReservationRelease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReservationRelease{}, err
	}
	defer tx.Rollback()
	if saved, e := readReservationRelease(ctx, tx, v.ID); e == nil {
		if saved.Fingerprint != v.Fingerprint {
			return ReservationRelease{}, ErrState
		}
		return saved, nil
	} else if !errors.Is(e, ErrNotFound) {
		return ReservationRelease{}, e
	}
	current, err := buildReservationRelease(ctx, tx, v.Seed, v.Actor, v.RequestID, v.Decision)
	if err != nil {
		return ReservationRelease{}, err
	}
	if current.ID != v.ID || current.Fingerprint != v.Fingerprint {
		return ReservationRelease{}, ErrState
	}
	if err := reservationReleaseSchema(ctx, tx); err != nil {
		return ReservationRelease{}, err
	}
	if err := closeReservationOwners(ctx, tx, current); err != nil {
		return ReservationRelease{}, err
	}
	for _, row := range current.Document.Rows {
		switch row.Table {
		case "GO_ORGANIZATION_TARGETS", "GO_ORGANIZATION_DISC_TARGETS", "GO_ORGANIZATION_REMOTE_TARGETS", "GO_LIBRARY_SCRAPE_CLAIMS":
		default:
			continue
		}
		where := []string{}
		args := []any{}
		for i, col := range row.Columns {
			value, e := removalSQLValue(row.Values[i])
			if e != nil {
				return ReservationRelease{}, e
			}
			where = append(where, removalQuote(col)+` IS ?`)
			args = append(args, value)
		}
		res, e := tx.ExecContext(ctx, `DELETE FROM `+removalQuote(row.Table)+` WHERE `+strings.Join(where, " AND "), args...)
		if e != nil {
			return ReservationRelease{}, e
		}
		n, e := res.RowsAffected()
		if e != nil || n != 1 {
			return ReservationRelease{}, ErrState
		}
	}
	for _, o := range current.Owners {
		if err := markReservationReleased(ctx, tx, o.Kind, o.ID, current.ID); err != nil {
			return ReservationRelease{}, err
		}
	}
	for _, row := range current.Document.Rows {
		if row.Table == "GO_SCRAPE_JOBS" {
			var outputs []SidecarRecord
			if json.Unmarshal([]byte(reservationText(row, "OUTPUTS")), &outputs) != nil {
				return ReservationRelease{}, ErrState
			}
			uncertain := false
			for _, o := range outputs {
				uncertain = uncertain || o.State == "writing" || o.State == "uncertain"
			}
			if uncertain {
				if err := markReservationReleased(ctx, tx, "intake-scraper", reservationText(row, "ID"), current.ID); err != nil {
					return ReservationRelease{}, err
				}
			}
		}
	}
	raw, err := json.Marshal(current.Document)
	if err != nil {
		return ReservationRelease{}, err
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_RESERVATION_RELEASES VALUES(?,?,?,?)`, current.ID, string(raw), current.Fingerprint, created); err != nil {
		return ReservationRelease{}, err
	}
	if err := ctx.Err(); err != nil {
		return ReservationRelease{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReservationRelease{}, err
	}
	current.State, current.Created = "released", created
	return current, nil
}
func markReservationReleased(ctx context.Context, tx *sql.Tx, kind, id, release string) error {
	signature, err := reservationSignature(ctx, tx, kind, id)
	if err != nil {
		return err
	}
	if signature == "" {
		return ErrState
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO GO_RESERVATION_RELEASE_MARKERS VALUES(?,?,?,?)`, kind, id, release, signature)
	return err
}
func closeReservationOwners(ctx context.Context, tx *sql.Tx, v ReservationRelease) error {
	batches := map[string]bool{}
	libraryBatches := map[string]bool{}
	reason := "independent reservation release " + v.ID + "; old operations permanently closed; files, proofs and original history retained"
	for _, o := range v.Owners {
		table, state := "", ""
		switch o.Kind {
		case "organization":
			table, state = "GO_ORGANIZATION_JOBS", RecoveryClosed
		case "library":
			table, state = "GO_LIBRARY_SCRAPE_JOBS", RecoveryClosed
		case "recovery":
			table, state = "GO_RECOVERY_DISPOSALS", "closed"
		case "media-removal":
			table, state = "GO_MEDIA_REMOVALS", "closed"
		default:
			return ErrState
		}
		if o.Kind == "recovery" && recoveryTerminal(o.State) || o.Kind == "media-removal" && o.State == "completed" {
			continue
		}
		res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET STATE=? WHERE ID=? AND STATE=?`, state, o.ID, o.State)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return ErrState
		}
		if o.Kind == "organization" {
			if o.BatchID != "" {
				batches[o.BatchID] = true
			}
			if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_UNKNOWN_LINKS SET STATE='cancelled' WHERE JOB_ID=? AND STATE='pending'`, o.ID); err != nil {
				return err
			}
			for _, t := range []string{"GO_SCRAPE_JOBS", "GO_SUBTITLE_JOBS"} {
				yes, err := recoveryTable(ctx, tx, t)
				if err != nil {
					return err
				}
				if yes {
					if _, err := tx.ExecContext(ctx, `UPDATE `+t+` SET STATE='blocked',REASON=? WHERE JOB_ID=? AND STATE NOT IN('completed','blocked')`, reason, o.ID); err != nil {
						return err
					}
				}
			}
		}
		if o.Kind == "library" && o.BatchID != "" {
			libraryBatches[o.BatchID] = true
		}
	}
	for batch := range batches {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_LEGACY_ORGANIZATION_BATCHES WHERE ID=?`, batch).Scan(&raw); err != nil {
			return err
		}
		var d legacyBatchDocument
		if json.Unmarshal([]byte(raw), &d) != nil || len(d.Jobs) < 2 || len(d.Jobs) > 128 {
			return ErrState
		}
		for _, job := range d.Jobs {
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job).Scan(&state); err != nil || state != RecoveryClosed {
				return ErrState
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE GO_LEGACY_ORGANIZATION_BATCHES SET STATE='closed',REASON=? WHERE ID=?`, reason, batch); err != nil {
			return err
		}
	}
	for batch := range libraryBatches {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_LIBRARY_SCRAPE_RUNS WHERE ID=?`, batch).Scan(&raw); err != nil {
			return err
		}
		var run LibraryPluginRun
		if json.Unmarshal([]byte(raw), &run) != nil || len(run.Jobs) < 1 || len(run.Jobs) > 16 {
			return ErrState
		}
		for _, id := range run.Jobs {
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, id).Scan(&state); err != nil || state != RecoveryClosed {
				return ErrState
			}
		}
		// The current run's children are closed. Its existing readonly status
		// calculation reports partial closure; future cron authorization is not
		// revoked or expanded by releasing this historical run's reservations.
	}
	return nil
}

type ReservationReleaseStatus struct {
	Owners   []ReservationOwner   `json:"owners"`
	Next     string               `json:"next"`
	Releases []ReservationRelease `json:"releases"`
}

func (s *Store) ReservationReleases(ctx context.Context, after string) (ReservationReleaseStatus, error) {
	out := ReservationReleaseStatus{Owners: []ReservationOwner{}, Releases: []ReservationRelease{}}
	if after != "" {
		parts := strings.SplitN(after, ":", 2)
		if len(parts) != 2 || !validReservationRef(ReservationOwnerRef{parts[0], parts[1]}) {
			return out, ErrPath
		}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	records, err := reservationRecords(ctx, tx)
	if err != nil {
		return out, err
	}
	for _, rec := range records {
		if reservationKey(rec.ref) <= after {
			continue
		}
		released, e := reservationReleased(ctx, tx, rec.ref.Kind, rec.ref.ID)
		if e != nil {
			return out, e
		}
		if released {
			continue
		}
		rows, e := reservationOwnerRows(ctx, tx, rec.ref.Kind, rec.ref.ID)
		if e != nil {
			return out, e
		}
		claims, e := reservationClaims(rec, rows)
		if e != nil {
			return out, e
		}
		if len(claims) > ReservationReleaseRowLimit {
			return out, ErrLimit
		}
		if len(claims) == 0 {
			continue
		}
		if len(out.Owners) == 30 {
			out.Next = reservationKey(out.Owners[29].ReservationOwnerRef)
			break
		}
		paths := append([]string{}, rec.paths...)
		sort.Strings(paths)
		paths = compactReservationPaths(paths)
		count := len(paths)
		if len(paths) > 100 {
			paths = paths[:100]
		}
		out.Owners = append(out.Owners, ReservationOwner{ReservationOwnerRef: rec.ref, State: rec.state, BatchID: rec.batch, PathCount: count, Paths: paths, Claims: claims})
	}
	yes, err := recoveryTable(ctx, tx, "GO_RESERVATION_RELEASES")
	if err != nil {
		return out, err
	}
	if yes {
		rows, e := recoveryRows(ctx, tx, `SELECT ID FROM GO_RESERVATION_RELEASES ORDER BY CREATED DESC,ID LIMIT 30`)
		if e != nil {
			return out, e
		}
		for _, row := range rows {
			v, e := readReservationRelease(ctx, tx, row[0])
			if e != nil {
				return out, e
			}
			out.Releases = append(out.Releases, v)
		}
	}
	return out, nil
}
