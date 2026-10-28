package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

// Recreate the saved rule, not a TMDB subscription guessed from its regex.
// The original batch/history remain intact and no execution grant is copied.
func restoreFuzzyRule(ctx context.Context, service subscriptionService, batchID, requestID string, histories ...int64) (int64, error) {
	if !fuzzyHex(batchID, 64) || !fuzzyHex(requestID, 32) {
		return 0, errFuzzyScope
	}
	db, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	j, err := readFuzzyBatch(ctx, tx, batchID)
	if err != nil {
		return 0, err
	}
	if j.State != "completed" || j.SourceResult != "finished" {
		return 0, errFuzzyScope
	}
	var previousBatch string
	var restored int64
	err = tx.QueryRowContext(ctx, `SELECT BATCH_ID,NEW_ID FROM GO_FUZZY_RESTORES WHERE REQUEST_ID=?`, requestID).Scan(&previousBatch, &restored)
	if err == nil {
		if previousBatch != batchID || restored < 1 {
			return 0, errFuzzyScope
		}
		return restored, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if len(histories) > 1 {
		return 0, errFuzzyScope
	}
	if len(histories) == 1 {
		var linked, fingerprint string
		if err := tx.QueryRowContext(ctx, `SELECT BATCH_ID,HISTORY_FINGERPRINT FROM GO_FUZZY_HISTORY_LINKS WHERE HISTORY_ID=?`, histories[0]).Scan(&linked, &fingerprint); err != nil {
			return 0, err
		}
		row, err := readFuzzyTableRow(ctx, tx, "RSS_HISTORY", histories[0])
		if err != nil || row == nil || linked != batchID || text(row["TYPE"]) != j.Kind || fuzzyDigest(row) != fingerprint {
			return 0, errFuzzyScope
		}
	}
	// Compare duplicate semantics without changing NULL/REAL/BLOB snapshots.
	raw := j.definition.Raw
	name, err := fuzzySQLValue(raw["NAME"])
	if err != nil {
		return 0, err
	}
	year := text(raw["YEAR"])
	predicate, args := "NAME IS ? AND COALESCE(YEAR,'')=?", []any{name, year}
	if j.Kind == "TV" {
		season, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(text(raw["SEASON"])), "S"))
		if text(raw["SEASON"]) == "" {
			season, err = 0, nil
		}
		if err != nil {
			return 0, errFuzzyScope
		}
		predicate += " AND CAST(ltrim(upper(COALESCE(SEASON,'')),'S0') AS INTEGER)=?"
		args = append(args, season)
	}
	var duplicate bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+subscriptionTable(j.Kind)+" WHERE "+predicate+")", args...).Scan(&duplicate); err != nil {
		return 0, err
	}
	if duplicate {
		return 0, errSubscriptionAlreadyExists
	}
	copy := map[string]any{}
	for k, v := range raw {
		if k != "ID" {
			copy[k] = v
		}
	}
	copy["STATE"] = "R"
	restored, err = insertFuzzyClone(ctx, tx, subscriptionTable(j.Kind), copy)
	if err != nil {
		return 0, err
	}
	if err := revokeFuzzyGrantTx(ctx, tx, j.Kind, restored); err != nil {
		return 0, err
	}
	if j.Kind == "TV" {
		var payload string
		err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_FUZZY_SOURCE_ARCHIVES WHERE BATCH_ID=? AND TABLE_NAME='RSS_TV_EPISODES'`, batchID).Scan(&payload)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err == nil {
			var rows []map[string]any
			if decodeFuzzyDocument(payload, &rows, 8<<20) != nil || len(rows) > 10000 {
				return 0, errFuzzyScope
			}
			for _, row := range rows {
				delete(row, "ID")
				old, err := fuzzySQLValue(row["RSSID"])
				if err != nil {
					return 0, err
				}
				switch old.(type) {
				case string:
					row["RSSID"] = strconv.FormatInt(restored, 10)
				case float64:
					row["RSSID"] = float64(restored)
				default:
					row["RSSID"] = restored
				}
				if _, err := insertFuzzyClone(ctx, tx, "RSS_TV_EPISODES", row); err != nil {
					return 0, err
				}
			}
		}
	}
	if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_FUZZY_RESTORES(REQUEST_ID,BATCH_ID,NEW_ID,CREATED) VALUES(?,?,?,?)`, requestID, batchID, restored, time.Now().UTC().Format(time.RFC3339Nano))); err != nil {
		return 0, err
	}
	reference := "subscription:" + j.Kind + ":" + strconv.FormatInt(restored, 10)
	if err := bus.EmitData(ctx, tx, "rss_added", "fuzzy-restore:"+requestID, reference, text(raw["NAME"])+" 已恢复模糊订阅", "由原模糊批次恢复完整规则副本；执行授权保持关闭，旧历史、批次及已提交下载均不重放。", subscriptionEventData(raw, j.Kind, restored, true)); err != nil {
		return 0, err
	}
	return restored, tx.Commit()
}
func insertFuzzyClone(ctx context.Context, tx *sql.Tx, table string, raw map[string]any) (int64, error) {
	if table != "RSS_MOVIES" && table != "RSS_TVS" && table != "RSS_TV_EPISODES" {
		return 0, errFuzzyScope
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+fuzzyQuoted(table)+")")
	if err != nil {
		return 0, err
	}
	columns := map[string]bool{}
	idOK := false
	for rows.Next() {
		var ordinal, notnull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&ordinal, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			rows.Close()
			return 0, err
		}
		columns[strings.ToUpper(name)] = true
		if strings.EqualFold(name, "ID") && strings.EqualFold(kind, "INTEGER") && pk > 0 {
			idOK = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if !idOK || len(raw) > 256 {
		return 0, errFuzzyScope
	}
	names := []string{}
	for name := range raw {
		if name == "ID" || !columns[name] {
			return 0, errFuzzyScope
		}
		names = append(names, name)
	}
	sort.Strings(names)
	values, quoted := []any{}, []string{}
	for _, name := range names {
		value, err := fuzzySQLValue(raw[name])
		if err != nil {
			return 0, err
		}
		values = append(values, value)
		quoted = append(quoted, fuzzyQuoted(name))
	}
	query := "INSERT INTO " + fuzzyQuoted(table) + "(" + strings.Join(quoted, ",") + ")VALUES(" + strings.TrimSuffix(strings.Repeat("?,", len(values)), ",") + ")"
	result, err := tx.ExecContext(ctx, query, values...)
	if err := fuzzyOne(result, err); err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil || id < 1 {
		return 0, errFuzzyScope
	}
	return id, nil
}
func (service subscriptionService) fuzzyHistoryBatch(ctx context.Context, kind string, id int64) (string, error) {
	var batch string
	err := fuzzyReadTransaction(ctx, service, func(tx *sql.Tx) error {
		yes, err := fuzzyTable(ctx, tx, "GO_FUZZY_HISTORY_LINKS")
		if err != nil || !yes {
			return err
		}
		var fingerprint string
		err = tx.QueryRowContext(ctx, `SELECT BATCH_ID,HISTORY_FINGERPRINT FROM GO_FUZZY_HISTORY_LINKS WHERE HISTORY_ID=?`, id).Scan(&batch, &fingerprint)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		row, err := readFuzzyTableRow(ctx, tx, "RSS_HISTORY", id)
		if err != nil {
			return err
		}
		// A deleted/reused history ID must never select another rule's archive.
		if row == nil || text(row["TYPE"]) != kind || fuzzyDigest(row) != fingerprint {
			batch = ""
			return nil
		}
		j, err := readFuzzyBatch(ctx, tx, batch)
		if err != nil {
			return err
		}
		if j.Kind != kind || j.State != "completed" || j.SourceResult != "finished" {
			return errFuzzyScope
		}
		return nil
	})
	return batch, err
}
