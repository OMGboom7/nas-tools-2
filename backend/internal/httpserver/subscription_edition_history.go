package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

type editionSourceArchive struct {
	Version         int
	Kind            string
	Raw             map[string]any
	Episodes        []map[string]any
	FileSelectionID string `json:"fileSelectionId,omitempty"`
}

func editionEpisodeRows(ctx context.Context, r fuzzyReader, id int64) ([]map[string]any, error) {
	out := []map[string]any{}
	yes, err := fuzzyTable(ctx, r, "RSS_TV_EPISODES")
	if err != nil || !yes {
		return out, err
	}
	rows, err := r.QueryContext(ctx, `SELECT * FROM RSS_TV_EPISODES WHERE RSSID=? ORDER BY ID`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil || len(cols) > 256 {
		return nil, errFuzzyScope
	}
	size := 0
	for rows.Next() {
		values, pointers := make([]any, len(cols)), make([]any, len(cols))
		for i := range pointers {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, k := range cols {
			row[strings.ToUpper(k)] = fuzzySQLCell(values[i])
		}
		size += len(mustFuzzyJSON(row))
		if len(out) >= 10000 || size > 8<<20 {
			return nil, errFuzzyScope
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
func archiveEditionSource(ctx context.Context, tx *sql.Tx, kind string, historyID int64, raw map[string]any, selectionIDs ...string) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_SUBSCRIPTION_EDITION_ARCHIVES(HISTORY_ID INTEGER PRIMARY KEY,DOCUMENT TEXT NOT NULL,HISTORY_FP TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_SUBSCRIPTION_EDITION_RESTORES(REQUEST_ID TEXT PRIMARY KEY,HISTORY_ID INTEGER NOT NULL,ARCHIVE_FP TEXT NOT NULL,NEW_ID INTEGER NOT NULL,CREATED TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	d := editionSourceArchive{Version: 1, Kind: kind, Raw: raw, Episodes: []map[string]any{}}
	if len(selectionIDs) > 1 {
		return errFuzzyScope
	}
	if !truthy(text(normalizeNativeSubscriptionRow(raw)["over_edition"])) {
		if len(selectionIDs) != 1 || !fuzzyHex(selectionIDs[0], 32) || truthy(text(normalizeNativeSubscriptionRow(raw)["fuzzy_match"])) {
			return errFuzzyScope
		}
		d.Version, d.FileSelectionID = 2, selectionIDs[0]
	}
	if kind == "TV" {
		var err error
		id, _ := strconv.ParseInt(text(raw["ID"]), 10, 64)
		d.Episodes, err = editionEpisodeRows(ctx, tx, id)
		if err != nil {
			return err
		}
	}
	row, err := readFuzzyTableRow(ctx, tx, "RSS_HISTORY", historyID)
	if err != nil || row == nil {
		return errFuzzyScope
	}
	return fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_SUBSCRIPTION_EDITION_ARCHIVES(HISTORY_ID,DOCUMENT,HISTORY_FP) VALUES(?,?,?) ON CONFLICT(HISTORY_ID) DO UPDATE SET DOCUMENT=excluded.DOCUMENT,HISTORY_FP=excluded.HISTORY_FP`, historyID, string(mustFuzzyJSON(d)), fuzzyDigest(row)))
}
func readEditionArchive(ctx context.Context, r fuzzyReader, kind string, id int64) (*editionSourceArchive, error) {
	yes, err := fuzzyTable(ctx, r, "GO_SUBSCRIPTION_EDITION_ARCHIVES")
	if err != nil || !yes {
		return nil, err
	}
	var raw, fp string
	err = r.QueryRowContext(ctx, `SELECT DOCUMENT,HISTORY_FP FROM GO_SUBSCRIPTION_EDITION_ARCHIVES WHERE HISTORY_ID=?`, id).Scan(&raw, &fp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row, err := readFuzzyTableRow(ctx, r, "RSS_HISTORY", id)
	if err != nil {
		return nil, err
	}
	if row == nil || text(row["TYPE"]) != kind || fuzzyDigest(row) != fp {
		return nil, nil
	}
	var d editionSourceArchive
	if decodeFuzzyDocument(raw, &d, 10<<20) != nil || d.Version != 1 && d.Version != 2 || d.Kind != kind || d.Raw == nil || len(d.Raw) > 256 || len(d.Episodes) > 10000 {
		return nil, errFuzzyScope
	}
	source := normalizeNativeSubscriptionRow(d.Raw)
	if d.Version == 1 && (!truthy(text(source["over_edition"])) || d.FileSelectionID != "") || d.Version == 2 && (truthy(text(source["over_edition"])) || truthy(text(source["fuzzy_match"])) || !fuzzyHex(d.FileSelectionID, 32)) {
		return nil, errFuzzyScope
	}
	for _, v := range d.Raw {
		if _, err := fuzzySQLValue(v); err != nil {
			return nil, err
		}
	}
	return &d, nil
}
func (service subscriptionService) restoreEditionHistory(ctx context.Context, kind string, id int64, requestID string) (int64, bool, error) {
	var archive *editionSourceArchive
	err := fuzzyReadTransaction(ctx, service, func(tx *sql.Tx) error {
		var err error
		archive, err = readEditionArchive(ctx, tx, kind, id)
		return err
	})
	if err != nil || archive == nil {
		return 0, false, err
	}
	if requestID == "" {
		requestID = fuzzyDigest([]any{"legacy-edition-restore", id, archive})[:32]
	}
	if !fuzzyHex(requestID, 32) {
		return 0, true, errFuzzyScope
	}
	db, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		return 0, true, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, true, err
	}
	defer tx.Rollback()
	current, err := readEditionArchive(ctx, tx, kind, id)
	if err != nil || current == nil || fuzzyDigest(*current) != fuzzyDigest(*archive) {
		return 0, true, errFuzzyScope
	}
	var previous, created int64
	var fp string
	err = tx.QueryRowContext(ctx, `SELECT HISTORY_ID,ARCHIVE_FP,NEW_ID FROM GO_SUBSCRIPTION_EDITION_RESTORES WHERE REQUEST_ID=?`, requestID).Scan(&previous, &fp, &created)
	if err == nil {
		if previous != id || fp != fuzzyDigest(*archive) {
			return 0, true, errFuzzyScope
		}
		return created, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, true, err
	}
	predicate, args := "NAME IS ? AND COALESCE(YEAR,'')=?", []any{current.Raw["NAME"], text(current.Raw["YEAR"])}
	if kind == "TV" {
		predicate += " AND SEASON IS ?"
		args = append(args, current.Raw["SEASON"])
	}
	var duplicate bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+subscriptionTable(kind)+" WHERE "+predicate+")", args...).Scan(&duplicate); err != nil {
		return 0, true, err
	}
	if duplicate {
		return 0, true, errSubscriptionAlreadyExists
	}
	copy := map[string]any{}
	for k, v := range current.Raw {
		if k != "ID" {
			copy[k] = v
		}
	}
	copy["STATE"] = "D"
	created, err = insertFuzzyClone(ctx, tx, subscriptionTable(kind), copy)
	if err != nil {
		return 0, true, err
	}
	if err := closeEditionCycleTx(ctx, tx, kind, created, "history-restore"); err != nil {
		return 0, true, err
	}
	if err := revokeFuzzyGrantTx(ctx, tx, kind, created); err != nil {
		return 0, true, err
	}
	if kind == "TV" {
		for _, row := range current.Episodes {
			delete(row, "ID")
			value, err := fuzzySQLValue(row["RSSID"])
			if err != nil {
				return 0, true, err
			}
			switch value.(type) {
			case string:
				row["RSSID"] = strconv.FormatInt(created, 10)
			case float64:
				row["RSSID"] = float64(created)
			default:
				row["RSSID"] = created
			}
			if _, err := insertFuzzyClone(ctx, tx, "RSS_TV_EPISODES", row); err != nil {
				return 0, true, err
			}
		}
	}
	if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_SUBSCRIPTION_EDITION_RESTORES(REQUEST_ID,HISTORY_ID,ARCHIVE_FP,NEW_ID,CREATED) VALUES(?,?,?,?,?)`, requestID, id, fuzzyDigest(*archive), created, time.Now().UTC().Format(time.RFC3339Nano))); err != nil {
		return 0, true, err
	}
	if err := bus.Emit(ctx, tx, "rss_added", "edition-restore:"+requestID, "subscription:"+kind+":"+strconv.FormatInt(created, 10), text(copy["NAME"])+" 已恢复洗版订阅", "保留原洗版、过滤、目录及优先级配置并重新排队；旧历史与接收事实保留，此请求不直接添加下载。"); err != nil {
		return 0, true, err
	}
	return created, true, tx.Commit()
}
