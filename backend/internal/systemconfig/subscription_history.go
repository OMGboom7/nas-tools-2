package systemconfig

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

var ErrHistoryNotFound = errors.New("plugin history not found")
var ErrHistoryConflict = errors.New("plugin history identity is inconsistent")

// These three plugins store subscription history in the same legacy format.
func SubscriptionHistoryTitle(plugin string) string {
	switch plugin {
	case "DoubanRank":
		return "订阅历史"
	case "DoubanSync":
		return "同步历史"
	case "MovieRandom":
		return "随机历史"
	default:
		return ""
	}
}

type SubscriptionHistoryRecord struct {
	RowID  int64
	Key    string
	Fields map[string]any
}

func historyFields(raw []byte) map[string]any {
	if !json.Valid(raw) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var fields map[string]any
	if decoder.Decode(&fields) != nil {
		return nil
	}
	// Keep only display scalars after decoding a row. A saved image or
	// private extension object can be much larger than the entire table page.
	for key, value := range fields {
		switch key {
		case "id", "name", "type", "year", "rating", "state", "add_time":
			switch v := value.(type) {
			case string:
				if len(v) > 2048 {
					fields[key] = strings.ToValidUTF8(strings.Clone(v[:2048]), "")
				}
				continue
			case json.Number:
				if len(v) <= 100 {
					continue
				}
			}
		}
		delete(fields, key)
	}
	return fields
}

// HistoryKey checks the original JSON ID without converting it to float64.
func (row SubscriptionHistoryRecord) HistoryKey() string {
	var id string
	switch value := row.Fields["id"].(type) {
	case json.Number:
		id = value.String()
	case string:
		id = value
	}
	if id != row.Key || len(id) == 0 || len(id) > 30 {
		return ""
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return id
}

func installedHistory(ctx context.Context, tx *sql.Tx, plugin string) (bool, error) {
	if SubscriptionHistoryTitle(plugin) == "" {
		return false, ErrHistoryNotFound
	}
	settings, err := readMany(ctx, tx, []string{"UserInstalledPlugins"})
	if err != nil {
		return false, err
	}
	var installed []string
	raw := settings["UserInstalledPlugins"]
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &installed); err != nil {
			return false, err
		}
	}
	if !slices.Contains(installed, plugin) {
		return false, ErrHistoryNotFound
	}
	var exists bool
	err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='PLUGIN_HISTORY' COLLATE NOCASE)").Scan(&exists)
	return exists, err
}

const HistoryPageSize = 200

func (store *Store) SubscriptionHistory(ctx context.Context, plugin string, after int64) ([]SubscriptionHistoryRecord, bool, error) {
	tx, err := store.database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	exists, err := installedHistory(ctx, tx, plugin)
	if err != nil || !exists {
		return nil, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT ID,COALESCE(KEY,''),CASE WHEN length(CAST(VALUE AS BLOB)) <= 1048576 THEN VALUE END
		FROM PLUGIN_HISTORY WHERE PLUGIN_ID=? AND ID>? ORDER BY ID LIMIT ?`, plugin, after, HistoryPageSize+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var records []SubscriptionHistoryRecord
	for rows.Next() {
		if len(records) == HistoryPageSize {
			return records, true, nil
		}
		var row SubscriptionHistoryRecord
		var raw []byte
		if err := rows.Scan(&row.RowID, &row.Key, &raw); err != nil {
			return nil, false, err
		}
		row.Fields = historyFields(raw)
		records = append(records, row)
	}
	return records, false, rows.Err()
}

// The Python plugins delete by (PLUGIN_ID, KEY), including duplicate historical
// rows for that key. Validate every matching payload before changing any row.
func (store *Store) DeleteSubscriptionHistory(ctx context.Context, plugin, key string) error {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exists, err := installedHistory(ctx, tx, plugin)
	if err != nil {
		return err
	}
	if !exists {
		return ErrHistoryNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN length(CAST(VALUE AS BLOB)) <= 1048576 THEN VALUE END FROM PLUGIN_HISTORY WHERE PLUGIN_ID=? AND KEY=?`, plugin, key)
	if err != nil {
		return err
	}
	defer rows.Close()
	var count int64
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		row := SubscriptionHistoryRecord{Key: key, Fields: historyFields(raw)}
		if key == "" || row.HistoryKey() != key {
			return ErrHistoryConflict
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		return ErrHistoryNotFound
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM PLUGIN_HISTORY WHERE PLUGIN_ID=? AND KEY=?", plugin, key)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != count {
		return ErrHistoryConflict
	}
	var remaining bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM PLUGIN_HISTORY WHERE PLUGIN_ID=? AND KEY=?)", plugin, key).Scan(&remaining); err != nil {
		return err
	}
	if remaining {
		return ErrHistoryConflict
	}
	return tx.Commit()
}
