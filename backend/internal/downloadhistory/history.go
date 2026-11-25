// Package downloadhistory shares the original history table with classic and Go clients.
package downloadhistory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const schema = "CREATE TABLE IF NOT EXISTS DOWNLOAD_HISTORY(ID INTEGER PRIMARY KEY,TITLE TEXT,YEAR TEXT,TYPE TEXT,TMDBID TEXT,SE TEXT,VOTE TEXT,POSTER TEXT,OVERVIEW TEXT,TORRENT TEXT,ENCLOSURE TEXT,SITE TEXT,DESC TEXT,DOWNLOADER TEXT,DOWNLOAD_ID TEXT,SAVE_PATH TEXT,DATE TEXT)"
const mediaFields = "TITLE,YEAR,TYPE,TMDBID,SE,VOTE,POSTER,OVERVIEW,TORRENT,ENCLOSURE,SITE,DESC"
const LatestPerTitle = "ROW_NUMBER() OVER (PARTITION BY TITLE ORDER BY DATE DESC,ID DESC)"
const projection = "ID," + mediaFields + ",DOWNLOADER,DOWNLOAD_ID,SAVE_PATH,DATE"

var fields = strings.Split(mediaFields, ",")
var ErrInput = errors.New("invalid download history request")

// Media retains NULL and empty strings independently, including in old rows.
type Media map[string]*string

func (m Media) Valid() bool {
	if len(m) != len(fields) || m["TITLE"] == nil || *m["TITLE"] == "" || m["TMDBID"] == nil || *m["TMDBID"] == "" {
		return false
	}
	for _, key := range fields {
		if _, exists := m[key]; !exists {
			return false
		}
	}
	return true
}

// RecordTx keeps pre-existing row IDs. A shared enclosure can belong to more
// than one downloader or task. Old duplicate rows remain individually readable.
func RecordTx(ctx context.Context, tx *sql.Tx, m Media, downloader, task string, path *string) error {
	if !m.Valid() || downloader == "" || task == "" {
		return ErrInput
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT ID FROM DOWNLOAD_HISTORY WHERE DOWNLOADER=? AND DOWNLOAD_ID=?
		ORDER BY (ENCLOSURE IS ?) DESC, DATE DESC, ID DESC LIMIT 1`, downloader, task, m["ENCLOSURE"]).Scan(&id)
	switch {
	case err == nil:
		_, err = tx.ExecContext(ctx, `UPDATE DOWNLOAD_HISTORY SET TORRENT=?,ENCLOSURE=?,DESC=?,SITE=?,SAVE_PATH=?,SE=?,DATE=? WHERE ID=?`,
			m["TORRENT"], m["ENCLOSURE"], m["DESC"], m["SITE"], path, m["SE"], now, id)
	case errors.Is(err, sql.ErrNoRows):
		args := make([]any, 0, len(fields)+4)
		for _, key := range fields {
			args = append(args, m[key])
		}
		args = append(args, downloader, task, path, now)
		_, err = tx.ExecContext(ctx, "INSERT INTO DOWNLOAD_HISTORY("+mediaFields+",DOWNLOADER,DOWNLOAD_ID,SAVE_PATH,DATE) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", args...)
	}
	return err
}

type query struct {
	ID, Page, Num                                  int64
	Date, Title, Path, Downloader, Task, Enclosure *string
	Media                                          Media
}

func records(ctx context.Context, db *sql.DB, op string, q query) ([]map[string]any, error) {
	stmt := "SELECT " + projection + " FROM DOWNLOAD_HISTORY"
	var args []any
	switch op {
	case "id":
		stmt += " WHERE ID=?"
		args = []any{q.ID}
	case "title":
		stmt += " WHERE TITLE IS ? ORDER BY ID"
		args = []any{q.Title}
	case "path":
		stmt += " WHERE SAVE_PATH=? ORDER BY DATE DESC, ID DESC LIMIT 1"
		args = []any{q.Path}
	case "task":
		stmt += " WHERE DOWNLOADER=? AND DOWNLOAD_ID=? ORDER BY DATE DESC, ID DESC LIMIT 1"
		args = []any{q.Downloader, q.Task}
	case "list":
		stmt = "WITH ranked AS (SELECT " + projection + ", " + LatestPerTitle + " AS rn FROM DOWNLOAD_HISTORY) SELECT " + projection + " FROM ranked WHERE rn=1"
		if q.Date != nil {
			stmt += " AND DATE>? ORDER BY DATE DESC,ID DESC"
			args = []any{q.Date}
		} else {
			if q.Page < 1 || q.Num < 1 || q.Page-1 > (1<<63-1)/q.Num {
				return nil, ErrInput
			}
			stmt += " ORDER BY DATE DESC,ID DESC LIMIT ? OFFSET ?"
			args = []any{q.Num, (q.Page - 1) * q.Num}
		}
	default:
		return nil, ErrInput
	}
	rows, err := db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	names := strings.Split(projection, ",")
	values := make([]any, len(names))
	dest := make([]any, len(names))
	for i := range values {
		dest[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(names))
		for i, k := range names {
			row[k] = values[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
