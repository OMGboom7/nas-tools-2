// Package searchresults manages the existing shared results list used by the
// classic search page and chat links. The React search cache is user-scoped.
package searchresults

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

const fields = "TORRENT_NAME,ENCLOSURE,DESCRIPTION,TYPE,TITLE,YEAR,SEASON,EPISODE,ES_STRING,VOTE,IMAGE,POSTER,TMDBID,OVERVIEW,RES_TYPE,RES_ORDER,SIZE,SEEDERS,PEERS,SITE,SITE_ORDER,PAGEURL,OTHERINFO,UPLOAD_VOLUME_FACTOR,DOWNLOAD_VOLUME_FACTOR,NOTE"

var columns = strings.Split(fields, ",")
var errInput = errors.New("invalid search results")

type row map[string]any

func readRows(ctx context.Context, db *sql.DB, id *int64) ([]row, error) {
	query := "SELECT ID," + fields + " FROM SEARCH_RESULT_INFO"
	var args []any
	if id != nil {
		query += " WHERE ID=?"
		args = append(args, *id)
	}
	rows, err := db.QueryContext(ctx, query+" ORDER BY ID", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	values := make([]any, len(names))
	dest := make([]any, len(names))
	for i := range values {
		dest[i] = &values[i]
	}
	result := []row{}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		record := make(row, len(names))
		for i, k := range names {
			if _, blob := values[i].([]byte); blob {
				return nil, errors.New("unsupported binary search result field")
			}
			if k == "UPLOAD_VOLUME_FACTOR" || k == "DOWNLOAD_VOLUME_FACTOR" {
				record[k] = storedFactor(values[i])
			} else {
				record[k] = values[i]
			}
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// The old SIZE column has INTEGER affinity but holds formatted display text.
func displaySize(bytes int64) string {
	size, unit := float64(bytes), "B"
	for _, next := range []string{"K", "M", "G", "T"} {
		if size < 1024 {
			break
		}
		size /= 1024
		unit = next
	}
	label := strings.TrimRight(strconv.FormatFloat(size, 'f', 2, 64), "0")
	if strings.HasSuffix(label, ".") {
		label += "0"
	}
	return label + unit
}

func (r row) arguments(id int64) ([]any, error) {
	// Require the complete projection before replacing the previous list. Null
	// is valid for many legacy fields, in particular unknown promotion factors.
	if len(r) != len(columns) {
		return nil, errInput
	}
	args := make([]any, 1, len(columns)+1)
	args[0] = id
	for _, name := range columns {
		value, ok := r[name]
		if !ok {
			return nil, errInput
		}
		switch name {
		case "RES_TYPE":
			labels, ok := value.(map[string]any)
			if !ok {
				return nil, errInput
			}
			for _, label := range labels {
				if label != nil {
					if _, ok := label.(string); !ok {
						return nil, errInput
					}
				}
			}
			encoded, err := json.Marshal(labels)
			if err != nil {
				return nil, err
			}
			value = string(encoded)
		case "SIZE":
			n, ok := value.(json.Number)
			if !ok {
				return nil, errInput
			}
			bytes, err := n.Int64()
			if err != nil || bytes < 0 {
				return nil, errInput
			}
			value = displaySize(bytes)
		default:
			switch v := value.(type) {
			case nil, string, bool:
			case json.Number:
				if n, err := v.Int64(); err == nil {
					value = n
				} else {
					if !strings.ContainsAny(v.String(), ".eE") {
						return nil, errInput
					}
					n, err := v.Float64()
					if err != nil {
						return nil, errInput
					}
					value = n
				}
			default:
				return nil, errInput
			}
		}
		args = append(args, value)
	}
	return args, nil
}

func writeRows(ctx context.Context, db *sql.DB, rows []row, replace bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Remember the largest issued ID even across a clear, an empty search or
	// a restart. Older installations may already have rows with larger IDs.
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SEARCH_RESULT_SEQUENCE (
		id INTEGER PRIMARY KEY CHECK(id=1), last_id INTEGER NOT NULL)`); err != nil {
		return err
	}
	var high int64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(0, COALESCE((SELECT last_id FROM GO_SEARCH_RESULT_SEQUENCE WHERE id=1),0),
		COALESCE((SELECT MAX(ID) FROM SEARCH_RESULT_INFO),0))`).Scan(&high); err != nil {
		return err
	}
	// Some existing volumes use AUTOINCREMENT. Their current maximum row
	// can be lower than the last number SQLite issued before an earlier clear.
	var hasSequence bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='sqlite_sequence')").Scan(&hasSequence); err != nil {
		return err
	}
	if hasSequence {
		var last int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM sqlite_sequence WHERE name='SEARCH_RESULT_INFO' COLLATE NOCASE").Scan(&last); err != nil {
			return err
		}
		high = max(high, last)
	}
	if int64(len(rows)) > math.MaxInt64-high {
		return errInput
	}
	if replace {
		if _, err := tx.ExecContext(ctx, "DELETE FROM SEARCH_RESULT_INFO"); err != nil {
			return err
		}
	}
	if len(rows) > 0 {
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO SEARCH_RESULT_INFO(ID,"+fields+") VALUES (?"+strings.Repeat(",?", len(columns))+")")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rows {
			high++
			args, err := r.arguments(high)
			if err != nil {
				return err
			}
			if _, err := stmt.ExecContext(ctx, args...); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO GO_SEARCH_RESULT_SEQUENCE(id,last_id) VALUES(1,?)
		ON CONFLICT(id) DO UPDATE SET last_id=excluded.last_id`, high)
	if err != nil {
		return err
	}
	return tx.Commit()
}
