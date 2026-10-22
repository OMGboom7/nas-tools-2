package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type recoveryReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type recoveryOwnerRecord struct {
	kind, id, state, definition, batch string
	rows                               []string
	paths                              []string
	protected                          []string
}

func recoveryPathKey(value string) string {
	return cases.Fold().String(norm.NFD.String(path.Clean(value)))
}
func recoveryOverlaps(a, b string) bool {
	a, b = recoveryPathKey(a), recoveryPathKey(b)
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func recoveryTable(ctx context.Context, r recoveryReader, name string) (bool, error) {
	var yes bool
	err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, name).Scan(&yes)
	return yes, err
}
func recoveryRows(ctx context.Context, r recoveryReader, query string, args ...any) ([][]string, error) {
	rows, err := r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := [][]string{}
	bytes := 0
	for rows.Next() {
		if len(result) >= 50000 {
			return nil, ErrLimit
		}
		values := make([]string, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for _, v := range values {
			bytes += len(v)
		}
		if bytes > 64<<20 {
			return nil, ErrLimit
		}
		result = append(result, values)
	}
	return result, rows.Err()
}
func recoveryOwnerRecords(ctx context.Context, r recoveryReader) ([]recoveryOwnerRecord, error) {
	records := []recoveryOwnerRecord{}
	jobs, err := recoveryRows(ctx, r, `SELECT ID,STATE,DEFINITION FROM GO_ORGANIZATION_JOBS ORDER BY ID`)
	if err != nil {
		return nil, err
	}
	if len(jobs) > 5000 {
		return nil, ErrLimit
	}
	scrapes, err := recoveryTable(ctx, r, "GO_SCRAPE_JOBS")
	if err != nil {
		return nil, err
	}
	subtitles, err := recoveryTable(ctx, r, "GO_SUBTITLE_JOBS")
	if err != nil {
		return nil, err
	}
	for _, row := range jobs {
		rec := recoveryOwnerRecord{kind: "organization", id: row[0], state: row[1], definition: row[2], rows: []string{}, paths: []string{}}
		var d Definition
		if json.Unmarshal([]byte(rec.definition), &d) != nil || len(d.Entries) > 50000 {
			return nil, ErrState
		}
		if d.LegacyBatch != nil {
			rec.batch = d.LegacyBatch.ID
		}
		items, err := recoveryRows(ctx, r, `SELECT CAST(ORDINAL AS TEXT),STATE,REASON,PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? ORDER BY ORDINAL`, rec.id)
		if err != nil {
			return nil, err
		}
		if len(items) != len(d.Entries) {
			return nil, ErrState
		}
		for i, item := range items {
			if item[0] != fmt.Sprint(i) {
				return nil, ErrState
			}
			rec.rows = append(rec.rows, Digest(item))
			var p Proof
			if json.Unmarshal([]byte(item[3]), &p) != nil {
				return nil, ErrState
			}
			e := d.Entries[i]
			rec.protected = append(rec.protected, path.Join(d.SourceRoot, e.Source), path.Join(d.TargetRoot, e.Target))
			if d.Remote != nil {
				if p.Remote != nil {
					rec.paths = append(rec.paths, path.Join(d.TargetRoot, p.Remote.Stage))
				}
			} else {
				temp := p.Temp
				if temp == "" {
					temp = TempName(rec.id, i)
				}
				rec.paths = append(rec.paths, path.Join(d.TargetRoot, path.Dir(e.Target), temp))
			}
			hold := p.SourceHold
			if hold == "" {
				hold = fmt.Sprintf(".nastool-move-%s-%d.hold", rec.id, i)
			}
			rec.paths = append(rec.paths, path.Join(d.SourceRoot, path.Dir(e.Source), hold))
		}
		if scrapes {
			rows, err := recoveryRows(ctx, r, `SELECT ID,STATE,OUTPUTS FROM GO_SCRAPE_JOBS WHERE JOB_ID=? ORDER BY ID`, rec.id)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				rec.rows = append(rec.rows, Digest(row))
				var outputs []SidecarRecord
				if json.Unmarshal([]byte(row[2]), &outputs) != nil {
					return nil, ErrState
				}
				for _, o := range outputs {
					if o.Proof.Target != "" {
						rec.protected = append(rec.protected, path.Join(d.TargetRoot, o.Proof.Target))
					}
					if o.Proof.Temp == "" {
						continue
					}
					relative := path.Join(path.Dir(o.Proof.Target), o.Proof.Temp)
					if d.Remote != nil {
						relative = o.Proof.Temp
					}
					rec.paths = append(rec.paths, path.Join(d.TargetRoot, relative))
				}
			}
		}
		if subtitles {
			rows, err := recoveryRows(ctx, r, `SELECT ID,STATE,REASON,EXTERNAL_ID FROM GO_SUBTITLE_JOBS WHERE JOB_ID=? ORDER BY ID`, rec.id)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				rec.rows = append(rec.rows, Digest(row))
			}
		}
		records = append(records, rec)
	}
	yes, err := recoveryTable(ctx, r, "GO_LIBRARY_SCRAPE_JOBS")
	if err != nil {
		return nil, err
	}
	if !yes {
		return records, nil
	}
	libraries, err := recoveryRows(ctx, r, `SELECT ID,STATE,DEFINITION FROM GO_LIBRARY_SCRAPE_JOBS ORDER BY ID`)
	if err != nil {
		return nil, err
	}
	if len(libraries) > 5000 {
		return nil, ErrLimit
	}
	for _, row := range libraries {
		rec := recoveryOwnerRecord{kind: "library", id: row[0], state: row[1], definition: row[2], rows: []string{}, paths: []string{}}
		var d LibraryDefinition
		if json.Unmarshal([]byte(rec.definition), &d) != nil {
			return nil, ErrState
		}
		items, err := recoveryRows(ctx, r, `SELECT CAST(ORDINAL AS TEXT),STATE,REASON,MEDIA FROM GO_LIBRARY_SCRAPE_ITEMS WHERE JOB_ID=? ORDER BY ORDINAL`, rec.id)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			rec.rows = append(rec.rows, Digest(item))
		}
		outputs, err := recoveryRows(ctx, r, `SELECT ID,TARGET,STATE,REASON,PROOF FROM GO_LIBRARY_SCRAPE_OUTPUTS WHERE JOB_ID=? ORDER BY ID`, rec.id)
		if err != nil {
			return nil, err
		}
		for _, o := range outputs {
			rec.rows = append(rec.rows, Digest(o))
			if o[1] != "" {
				rec.protected = append(rec.protected, path.Join(d.Root.Path, o[1]))
			}
			var p LibraryOutputProof
			if json.Unmarshal([]byte(o[4]), &p) != nil {
				return nil, ErrState
			}
			if p.Stage == "" {
				continue
			}
			relative := path.Join(path.Dir(o[1]), p.Stage)
			if d.Remote != nil {
				relative = p.Stage
			}
			rec.paths = append(rec.paths, path.Join(d.Root.Path, relative))
		}
		records = append(records, rec)
	}
	return records, nil
}
func recoveryOwners(ctx context.Context, r recoveryReader, s RecoveryScope, selected string) ([]RecoveryOwner, error) {
	records, err := recoveryOwnerRecords(ctx, r)
	if err != nil {
		return nil, err
	}
	location := path.Join(s.Root.Path, selected)
	for _, rec := range records {
		for _, p := range rec.protected {
			if path.IsAbs(p) && p != "/" && recoveryOverlaps(location, p) {
				return nil, ErrPath
			}
		}
	}
	if err := recoveryHistoryProtection(ctx, r, location); err != nil {
		return nil, err
	}
	matches := map[string]bool{}
	batches := map[string]bool{}
	for _, rec := range records {
		for _, p := range rec.paths {
			if recoveryOverlaps(location, p) {
				matches[rec.kind+":"+rec.id] = true
				if rec.batch != "" {
					batches[rec.batch] = true
				}
				break
			}
		}
	}
	owners := []RecoveryOwner{}
	for _, rec := range records {
		direct := matches[rec.kind+":"+rec.id]
		if !direct && !(rec.kind == "organization" && rec.batch != "" && batches[rec.batch]) {
			continue
		}
		owners = append(owners, RecoveryOwner{BatchID: rec.batch, Kind: rec.kind, ID: rec.id, State: rec.state, Related: !direct, Fingerprint: Digest([]any{rec.kind, rec.id, rec.state, rec.definition, rec.rows})})
	}
	sort.Slice(owners, func(i, j int) bool {
		if owners[i].Kind == owners[j].Kind {
			return owners[i].ID < owners[j].ID
		}
		return owners[i].Kind < owners[j].Kind
	})
	return owners, nil
}
func recoveryHistoryProtection(ctx context.Context, r recoveryReader, location string) error {
	yes, err := recoveryTable(ctx, r, "TRANSFER_HISTORY")
	if err != nil || !yes {
		return err
	}
	rows, err := recoveryRows(ctx, r, `SELECT COALESCE(SOURCE_PATH,''),COALESCE(SOURCE_FILENAME,''),COALESCE(DEST_PATH,''),COALESCE(DEST_FILENAME,'') FROM TRANSFER_HISTORY`)
	if err != nil {
		return err
	}
	for _, row := range rows {
		for _, pair := range [][2]string{{row[0], row[1]}, {row[2], row[3]}} {
			if pair[1] == "" {
				continue
			}
			p := path.Join(pair[0], pair[1])
			if path.IsAbs(pair[1]) {
				p = path.Clean(pair[1])
			}
			if path.IsAbs(p) && p != "/" && recoveryOverlaps(location, p) {
				return ErrPath
			}
		}
	}
	return nil
}
func (s *Store) RecoveryObjectOwners(ctx context.Context, scope RecoveryScope, selected string) ([]RecoveryOwner, error) {
	return recoveryOwners(ctx, s.db, scope, selected)
}
func (s *Store) ReviewRecoveryObjects(ctx context.Context, d RecoveryDefinition) (RecoveryObjectReview, error) {
	// The content snapshot is assembled by the storage-specific reader. Never
	// accept an inventory or owner list from an HTTP request.
	if !recoveryScopeValid(d.Scope) || !RecoveryCandidate(d.Path) || len(d.Nodes) < 1 || len(d.Nodes) > RecoveryObjectLimit || d.Actor == "" || !recoveryHex(d.RequestID, 32) || strings.TrimSpace(d.Decision) == "" || len(d.Decision) > 512 {
		return RecoveryObjectReview{}, ErrState
	}
	owners, err := recoveryOwners(ctx, s.db, d.Scope, d.Path)
	if err != nil {
		return RecoveryObjectReview{}, err
	}
	d.Owners = owners
	review := RecoveryObjectReview{RootID: d.Scope.Root.ID, Driver: recoveryDriver(d.Scope), Path: d.Path, RequestID: d.RequestID, Unknown: len(owners) == 0, Owners: owners, Sample: []string{}, Definition: d}
	for _, n := range d.Nodes {
		switch n.Kind {
		case "directory":
			review.Directories++
		case "link":
			review.Links++
		case "file":
			review.Files++
			if n.Node.Size < 0 || n.Node.Size > RecoveryByteLimit-review.Bytes {
				return review, ErrLimit
			}
			review.Bytes += n.Node.Size
		default:
			return review, ErrMode
		}
		if len(review.Sample) < 100 {
			review.Sample = append(review.Sample, n.Path)
		}
	}
	review.Fingerprint = Digest(d)
	return review, nil
}
