package notificationconfig

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const centerSchema = `CREATE TABLE IF NOT EXISTS GO_MESSAGE_CENTER (
	ID INTEGER PRIMARY KEY AUTOINCREMENT, TITLE TEXT NOT NULL, CONTENT TEXT NOT NULL, CREATED INTEGER NOT NULL)`

type SystemMessage struct {
	ID      int64  `json:"id,string"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Time    string `json:"time"`
}

// AppendSystemMessage belongs to the caller's business transaction. Old rows
// leave the display queue without changing notification delivery receipts.
func AppendSystemMessage(ctx context.Context, tx *sql.Tx, title, content string, at time.Time) error {
	if _, err := tx.ExecContext(ctx, centerSchema); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO GO_MESSAGE_CENTER(TITLE,CONTENT,CREATED) VALUES(?,?,?)",
		title, content, at.Unix()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM GO_MESSAGE_CENTER WHERE ID <=
		(SELECT ID FROM GO_MESSAGE_CENTER ORDER BY ID DESC LIMIT 1 OFFSET 50)`)
	return err
}

func (s *Store) AddSystemMessage(ctx context.Context, title, content string) error {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := AppendSystemMessage(ctx, tx, title, content, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SystemMessages(ctx context.Context, limit int, after int64, since string) ([]SystemMessage, error) {
	if limit < 0 || after < 0 {
		return nil, errors.New("invalid message query")
	}
	var seconds int64
	filterByTime := after == 0 && since != ""
	if filterByTime {
		at, err := time.ParseInLocation(time.DateTime, since, time.Local)
		if err != nil {
			return nil, err
		}
		seconds = at.Unix()
	}
	if after > 0 || filterByTime {
		limit = 50
	}
	rows, err := s.database.QueryContext(ctx, `SELECT ID,TITLE,CONTENT,CREATED FROM GO_MESSAGE_CENTER
		WHERE ID>? AND (?=0 OR CREATED>?) ORDER BY ID DESC LIMIT ?`, after, filterByTime, seconds, min(limit, 50))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SystemMessage{}
	for rows.Next() {
		var item SystemMessage
		var created int64
		if err = rows.Scan(&item.ID, &item.Title, &item.Content, &created); err != nil {
			return nil, err
		}
		item.Time = time.Unix(created, 0).Local().Format(time.DateTime)
		items = append(items, item)
	}
	return items, rows.Err()
}
