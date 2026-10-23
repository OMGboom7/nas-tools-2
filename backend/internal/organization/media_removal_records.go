package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Journal SQLite values without coalescing NULL, converting integer IDs to
// floating point, or discarding columns added by an older/newer installation.
type MediaRemovalValue struct {
	Kind string  `json:"kind"`
	Text string  `json:"text,omitempty"`
	Int  int64   `json:"int,string,omitempty"`
	Real float64 `json:"real,omitempty"`
	Blob []byte  `json:"blob,omitempty"`
}
type MediaRemovalRow struct {
	Table   string              `json:"table"`
	Columns []string            `json:"columns"`
	Values  []MediaRemovalValue `json:"values"`
}

// Marshal the private snapshot explicitly; public HTTP projections omit Rows.
type mediaRemovalHistoryDocument struct {
	Record HistoryRecord     `json:"record"`
	Rows   []MediaRemovalRow `json:"rows"`
}

func removalQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func removalValue(v any) (MediaRemovalValue, error) {
	r := MediaRemovalValue{}
	switch v := v.(type) {
	case nil:
		r.Kind = "null"
	case string:
		if !utf8.ValidString(v) {
			return r, ErrState
		}
		r.Kind, r.Text = "text", v
	case int64:
		r.Kind, r.Int = "integer", v
	case float64:
		r.Kind, r.Real = "real", v
	case []byte:
		r.Kind, r.Blob = "blob", append([]byte{}, v...)
	default:
		return r, ErrState
	}
	return r, nil
}
func removalSQLValue(v MediaRemovalValue) (any, error) {
	switch v.Kind {
	case "null":
		return nil, nil
	case "text":
		return v.Text, nil
	case "integer":
		return v.Int, nil
	case "real":
		return v.Real, nil
	case "blob":
		return v.Blob, nil
	}
	return nil, ErrState
}
func removalRows(ctx context.Context, r recoveryReader, table, where string, arg any) ([]MediaRemovalRow, error) {
	if table != "TRANSFER_HISTORY" && table != "TRANSFER_BLACKLIST" && table != "SYNC_HISTORY" {
		return nil, ErrState
	}
	yes, err := recoveryTable(ctx, r, table)
	if err != nil || !yes {
		return []MediaRemovalRow{}, err
	}
	rows, err := r.QueryContext(ctx, `SELECT * FROM `+removalQuote(table)+` WHERE `+where+` ORDER BY ID`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil || len(cols) > 256 {
		return nil, ErrLimit
	}
	result := []MediaRemovalRow{}
	var bytes int
	for rows.Next() {
		if len(result) >= 20000 {
			return nil, ErrLimit
		}
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := MediaRemovalRow{Table: table, Columns: append([]string{}, cols...), Values: []MediaRemovalValue{}}
		for _, v := range values {
			x, err := removalValue(v)
			if err != nil {
				return nil, err
			}
			row.Values = append(row.Values, x)
		}
		b, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		bytes += len(b)
		if bytes > 64<<20 {
			return nil, ErrLimit
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
func mediaRemovalHistories(ctx context.Context, r recoveryReader, ids []int64) ([]mediaRemovalHistoryDocument, error) {
	if len(ids) > 100 {
		return nil, ErrLimit
	}
	out := []mediaRemovalHistoryDocument{}
	bytes := 0
	seen := map[int64]bool{}
	for _, id := range ids {
		if id < 1 || seen[id] {
			return nil, ErrState
		}
		seen[id] = true
		record, err := readHistory(ctx, r, id)
		if err != nil {
			return nil, err
		}
		rows, err := removalRows(ctx, r, "TRANSFER_HISTORY", `ID=?`, id)
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 {
			return nil, ErrNotFound
		}
		if err := mediaRemovalRowBudget(rows, &bytes); err != nil {
			return nil, err
		}
		p := record.SourcePath + "/" + record.SourceFilename
		for _, table := range []string{"TRANSFER_BLACKLIST", "SYNC_HISTORY"} {
			more, err := removalRows(ctx, r, table, `PATH=?`, p)
			if err != nil {
				return nil, err
			}
			if err := mediaRemovalRowBudget(more, &bytes); err != nil {
				return nil, err
			}
			rows = append(rows, more...)
		}
		out = append(out, mediaRemovalHistoryDocument{Record: record, Rows: rows})
	}
	return out, nil
}
func mediaRemovalRowBudget(rows []MediaRemovalRow, used *int) error {
	if *used < 0 || *used > 64<<20 {
		return ErrLimit
	}
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if len(raw) > (64<<20)-*used {
			return ErrLimit
		}
		*used += len(raw)
	}
	return nil
}
func (s *Store) MediaRemovalHistories(ctx context.Context, ids []int64) ([]HistoryRecord, string, error) {
	docs, err := mediaRemovalHistories(ctx, s.db, ids)
	if err != nil {
		return nil, "", err
	}
	records := []HistoryRecord{}
	for _, d := range docs {
		records = append(records, d.Record)
	}
	return records, Digest(docs), nil
}
func verifyMediaRemovalHistories(ctx context.Context, r recoveryReader, docs []mediaRemovalHistoryDocument) error {
	ids := []int64{}
	for _, d := range docs {
		ids = append(ids, d.Record.ID)
	}
	current, err := mediaRemovalHistories(ctx, r, ids)
	if err != nil {
		return err
	}
	if Digest(current) != Digest(docs) {
		return ErrState
	}
	return nil
}
func deleteMediaRemovalHistoryRows(ctx context.Context, tx *sql.Tx, docs []mediaRemovalHistoryDocument) error {
	if err := verifyMediaRemovalHistories(ctx, tx, docs); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, doc := range docs {
		for _, row := range doc.Rows {
			if len(row.Columns) != len(row.Values) {
				return ErrState
			}
			idIndex := -1
			for i, c := range row.Columns {
				if strings.EqualFold(c, "ID") {
					idIndex = i
				}
			}
			if idIndex < 0 {
				return ErrState
			}
			value, err := removalSQLValue(row.Values[idIndex])
			if err != nil {
				return err
			}
			key := row.Table + ":" + fmt.Sprint(value)
			if seen[key] {
				continue
			}
			seen[key] = true
			res, err := tx.ExecContext(ctx, `DELETE FROM `+removalQuote(row.Table)+` WHERE ID=?`, value)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil || n != 1 {
				return ErrState
			}
		}
	}
	return nil
}
