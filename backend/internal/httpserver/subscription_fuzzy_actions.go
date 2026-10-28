package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

func fuzzyOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errFuzzyScope
	}
	return nil
}
func (api *fuzzySubscriptionAPI) write(ctx context.Context, action func(*sql.Tx) error) error {
	db, err := api.service().openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := action(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (api *fuzzySubscriptionAPI) intent(ctx context.Context, id string, index int, native subscriptionSubmissionProof, prepared downloaderconfig.Downloader, preparedOptions downloadAddOptions) error {
	return api.write(ctx, func(tx *sql.Tx) error {
		j, err := readFuzzyBatch(ctx, tx, id)
		if err != nil {
			return err
		}
		if j.State != "running" || index < 0 || index >= len(j.Items) || j.Items[index].State != "pending" {
			return errFuzzyScope
		}
		for i := 0; i < index; i++ {
			if j.Items[i].State != "accepted" {
				return errFuzzyScope
			}
		}
		if err := api.current(ctx, tx, j.authorization); err != nil {
			return err
		}
		d := j.definition
		c := d.Candidates[index]
		source := normalizeNativeSubscriptionRow(d.Raw)
		download := api.runner.planner.runner.download.service
		current, options, err := download.searchDownloadSettings(ctx, c.View.Site, addResourceRequest{Setting: text(source["download_setting"]), Directory: text(source["save_path"])})
		if err != nil {
			return err
		}
		options, err = download.preparePTSource(ctx, current, c.Resource, options)
		if err != nil {
			return err
		}
		if fuzzyDigest([]any{current, options}) != fuzzyDigest([]any{prepared, preparedOptions}) || native.DownloaderID != current.ID || native.DownloaderType != current.Type || native.ConfigHash != subscriptionResourceKey(current.Config) {
			return errFuzzyScope
		}
		plan := subscriptionSearchPlan{raw: d.Raw, mediaID: "fuzzy:" + d.Kind + ":" + strconv.FormatInt(d.SubscriptionID, 10), fuzzy: true}
		candidate := subscriptionCandidate{Title: c.View.Title, resource: c.Resource}
		owner, err := reserveSubscriptionSubmissionTx(ctx, tx, d.Kind, d.SubscriptionID, plan, candidate, native)
		if err != nil {
			return err
		}
		key := subscriptionResourceKey(c.Resource.DownloadURL)
		var original string
		if err := tx.QueryRowContext(ctx, `SELECT PAYLOAD FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS WHERE RESOURCE_KEY=? AND OWNER=?`, key, owner).Scan(&original); err != nil {
			return err
		}
		if decodeFuzzyDocument(original, &native, 2<<20) != nil {
			return errFuzzyScope
		}
		proof := fuzzyItemProof{Version: 2, BatchID: id, Index: index, Native: native}
		if _, err := decodeFuzzyItemProof(string(mustFuzzyJSON(proof)), j, index); err != nil {
			return err
		}
		payload := string(mustFuzzyJSON(proof))
		// Version 2 is deliberately not adopted by exact-subscription recovery:
		// its missing-episode/first-movie completion rules do not apply here.
		if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_SUBSCRIPTION_SUBMISSION_PROOFS SET PAYLOAD=? WHERE RESOURCE_KEY=? AND OWNER=? AND PAYLOAD=?`, payload, key, owner, original)); err != nil {
			return err
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_FUZZY_ITEM_ATTEMPTS(BATCH_ID,ORDINAL,OWNER,PAYLOAD,STATE) VALUES(?,?,?,?,'intent')`, id, index, owner, payload)); err != nil {
			return err
		}
		return fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_ITEMS SET STATE='intent',OWNER=?,PROOF=? WHERE BATCH_ID=? AND ORDINAL=? AND STATE='pending' AND OWNER='' AND PROOF=''`, owner, payload, id, index))
	})
}
func mustFuzzyJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
func verifyFuzzyClaim(ctx context.Context, tx *sql.Tx, j fuzzyBatch, index int) error {
	v := j.Items[index]
	c := j.definition.Candidates[index]
	var state, payload string
	err := tx.QueryRowContext(ctx, `SELECT r.STATE,p.PAYLOAD FROM GO_RSS_DOWNLOAD_CLAIMS r JOIN GO_SUBSCRIPTION_DOWNLOAD_CLAIMS s USING(RESOURCE_KEY) JOIN GO_SUBSCRIPTION_SUBMISSION_PROOFS p ON p.RESOURCE_KEY=s.RESOURCE_KEY AND p.OWNER=s.OWNER WHERE r.RESOURCE_KEY=? AND r.TASK_ID=0 AND s.OWNER=? AND s.KIND=? AND s.SUB_ID=?`, subscriptionResourceKey(c.Resource.DownloadURL), v.owner, j.Kind, j.SubscriptionID).Scan(&state, &payload)
	if err != nil {
		return err
	}
	if state != "pending" || payload != v.proof {
		return errFuzzyScope
	}
	return nil
}
func (api *fuzzySubscriptionAPI) result(ctx context.Context, id string, index int, accepted bool, verifyDownloader bool) error {
	return api.write(ctx, func(tx *sql.Tx) error {
		j, err := readFuzzyBatch(ctx, tx, id)
		if err != nil {
			return err
		}
		if index < 0 || index >= len(j.Items) {
			return errFuzzyScope
		}
		v := j.Items[index]
		if v.State == "accepted" && accepted {
			return nil
		}
		if v.State != "intent" && v.State != "uncertain" {
			return errFuzzyScope
		}
		proof, err := decodeFuzzyItemProof(v.proof, j, index)
		if err != nil {
			return err
		}
		if err := verifyFuzzyClaim(ctx, tx, j, index); err != nil {
			return err
		}
		state, reason := "uncertain", "downloader acceptance could not be established; no automatic resend"
		if accepted {
			if verifyDownloader {
				var configuration, kind string
				var enabled int
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(CONFIG,'{}'),COALESCE(TYPE,''),COALESCE(ENABLED,0) FROM DOWNLOADER WHERE ID=?`, proof.Native.DownloaderID).Scan(&configuration, &kind, &enabled); err != nil {
					return err
				}
				if enabled == 0 || kind != proof.Native.DownloaderType || subscriptionResourceKey(configuration) != proof.Native.ConfigHash {
					return errFuzzyScope
				}
			}
			if err := downloaderconfig.VerifyDownloadProfile(ctx, tx, proof.Native.ProfileDigest); err != nil {
				return err
			}
			c := j.definition.Candidates[index]
			key := subscriptionResourceKey(c.Resource.DownloadURL)
			if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_RSS_DOWNLOAD_CLAIMS SET STATE='submitted',UPDATED_AT=? WHERE RESOURCE_KEY=? AND TASK_ID=0 AND STATE='pending'`, time.Now().UTC().Format(time.RFC3339Nano), key)); err != nil {
				return err
			}
			if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO RSS_TORRENTS(TORRENT_NAME,ENCLOSURE,TYPE,TITLE,YEAR,SEASON,EPISODE) VALUES(?,?,?,?,?,?,?)`, c.View.Title, c.Resource.DownloadURL, c.View.Kind, c.View.Name, c.View.Year, c.View.Season, c.View.Episodes)); err != nil {
				return err
			}
			reference := "subscription:" + j.Kind + ":" + strconv.FormatInt(j.SubscriptionID, 10)
			if err := bus.EmitData(ctx, tx, "download_start", key+":"+v.owner, reference, c.View.Name+" 开始下载", "来自：模糊 RSS 订阅；类型："+j.Kind+"；年份："+c.View.Year+"；季："+c.View.Season+"；集："+c.View.Episodes+"。此为下载器接受，不代表下载或入库完成。", map[string]any{
				"media_info":   map[string]any{"title": c.View.Name, "year": c.View.Year, "type": j.Kind, "season": c.View.Season},
				"torrent_file": c.Resource.DownloadURL, "downloader_id": proof.Native.DownloaderID, "native_download": map[string]any{"resource_title": c.View.Title, "episodes": c.View.Episodes},
			}); err != nil {
				return err
			}
			state, reason = "accepted", "downloader accepted this exact submission; download completion is not proven"
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_ITEMS SET STATE=?,REASON=? WHERE BATCH_ID=? AND ORDINAL=? AND OWNER=? AND PROOF=? AND STATE IN('intent','uncertain')`, state, reason, id, index, v.owner, v.proof)); err != nil {
			return err
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_ITEM_ATTEMPTS SET STATE=? WHERE BATCH_ID=? AND ORDINAL=? AND OWNER=? AND PAYLOAD=? AND STATE IN('intent','uncertain')`, state, id, index, v.owner, v.proof)); err != nil {
			return err
		}
		if !accepted && j.State != "stopped" {
			return fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_BATCHES SET STATE='uncertain',REASON=? WHERE ID=? AND STATE IN('running','blocked','uncertain')`, reason, id))
		}
		return nil
	})
}

// Only a positively classified rejection before any Add request reaches this
// path. Unknown network receipts never release the shared resource claim.
func (api *fuzzySubscriptionAPI) rejected(ctx context.Context, id string, index int) error {
	return api.write(ctx, func(tx *sql.Tx) error {
		j, err := readFuzzyBatch(ctx, tx, id)
		if err != nil {
			return err
		}
		if index < 0 || index >= len(j.Items) || j.Items[index].State != "intent" {
			return errFuzzyScope
		}
		if err := verifyFuzzyClaim(ctx, tx, j, index); err != nil {
			return err
		}
		v := j.Items[index]
		key := subscriptionResourceKey(j.definition.Candidates[index].Resource.DownloadURL)
		for _, statement := range []string{
			`DELETE FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND TASK_ID=0 AND STATE='pending' AND EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS s WHERE s.RESOURCE_KEY=GO_RSS_DOWNLOAD_CLAIMS.RESOURCE_KEY AND s.OWNER=?)`,
			`DELETE FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS WHERE RESOURCE_KEY=? AND OWNER=?`,
			`DELETE FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND OWNER=?`,
		} {
			if err := fuzzyOne(tx.ExecContext(ctx, statement, key, v.owner)); err != nil {
				return err
			}
		}
		reason := "definitely rejected before submission; original attempt retained, fresh explicit approval required"
		if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_ITEMS SET STATE='rejected',REASON=? WHERE BATCH_ID=? AND ORDINAL=? AND OWNER=? AND STATE='intent'`, reason, id, index, v.owner)); err != nil {
			return err
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_ITEM_ATTEMPTS SET STATE='rejected' WHERE BATCH_ID=? AND ORDINAL=? AND OWNER=? AND PAYLOAD=? AND STATE='intent'`, id, index, v.owner, v.proof)); err != nil {
			return err
		}
		if j.State == "stopped" {
			return nil
		}
		return fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_BATCHES SET STATE='blocked',REASON=? WHERE ID=? AND STATE='running'`, reason, id))
	})
}
func archiveFuzzyGrant(ctx context.Context, tx *sql.Tx, kind string, id int64, raw string) error {
	if raw == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_FUZZY_GRANT_HISTORY(ID INTEGER PRIMARY KEY AUTOINCREMENT,KIND TEXT NOT NULL,SUB_ID INTEGER NOT NULL,DOCUMENT TEXT NOT NULL)`); err != nil {
		return err
	}
	return fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_FUZZY_GRANT_HISTORY(KIND,SUB_ID,DOCUMENT) VALUES(?,?,?)`, kind, id, raw))
}
func revokeFuzzyGrantTx(ctx context.Context, tx *sql.Tx, kind string, id int64) error {
	yes, err := fuzzyTable(ctx, tx, "GO_FUZZY_SUBSCRIPTION_GRANTS")
	if err != nil || !yes {
		return err
	}
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_FUZZY_SUBSCRIPTION_GRANTS WHERE KIND=? AND SUB_ID=?`, kind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := archiveFuzzyGrant(ctx, tx, kind, id, raw); err != nil {
		return err
	}
	return fuzzyOne(tx.ExecContext(ctx, `DELETE FROM GO_FUZZY_SUBSCRIPTION_GRANTS WHERE KIND=? AND SUB_ID=? AND DOCUMENT=?`, kind, id, raw))
}
func revokeFuzzySelectedTx(ctx context.Context, tx *sql.Tx, kind, predicate string, args ...any) error {
	yes, err := fuzzyTable(ctx, tx, "GO_FUZZY_SUBSCRIPTION_GRANTS")
	if err != nil || !yes {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT ID FROM "+subscriptionTable(kind)+" WHERE "+predicate, args...)
	if err != nil {
		return err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if len(ids) >= 10000 {
			rows.Close()
			return errFuzzyScope
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := revokeFuzzyGrantTx(ctx, tx, kind, id); err != nil {
			return err
		}
	}
	return nil
}
func archiveFuzzyRows(ctx context.Context, tx *sql.Tx, batch, table, predicate string, args ...any) (int, error) {
	yes, err := fuzzyTable(ctx, tx, table)
	if err != nil || !yes {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT * FROM "`+table+`" WHERE `+predicate, args...)
	if err != nil {
		return 0, err
	}
	columns, err := rows.Columns()
	if err != nil {
		rows.Close()
		return 0, err
	}
	records := []map[string]any{}
	bytes := 0
	for rows.Next() {
		v, p := make([]any, len(columns)), make([]any, len(columns))
		for i := range p {
			p[i] = &v[i]
		}
		if err := rows.Scan(p...); err != nil {
			rows.Close()
			return 0, err
		}
		record := map[string]any{}
		for i, c := range columns {
			record[c] = fuzzySQLCell(v[i])
		}
		b, err := json.Marshal(record)
		if err != nil {
			rows.Close()
			return 0, err
		}
		bytes += len(b)
		if len(records) >= 10000 || bytes > 8<<20 {
			rows.Close()
			return 0, errFuzzyScope
		}
		records = append(records, record)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(records)
	if err != nil {
		return 0, err
	}
	return len(records), fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_FUZZY_SOURCE_ARCHIVES(BATCH_ID,TABLE_NAME,DOCUMENT) VALUES(?,?,?)`, batch, table, string(payload)))
}
func (api *fuzzySubscriptionAPI) finalize(ctx context.Context, id string) error {
	return api.write(ctx, func(tx *sql.Tx) error {
		j, err := readFuzzyBatch(ctx, tx, id)
		if err != nil {
			return err
		}
		if j.State == "completed" {
			return nil
		}
		if j.State != "running" {
			return errFuzzyScope
		}
		for _, item := range j.Items {
			if item.State != "accepted" {
				return errFuzzyScope
			}
		}
		if err := api.current(ctx, tx, j.authorization); err != nil {
			return err
		}
		d := j.definition
		if err := verifyFuzzySource(ctx, tx, j.Kind, j.SubscriptionID, d.Raw); err != nil {
			return err
		}
		complete, advance, order := len(j.Items) > 0, false, 0
		if truthy(normalizeNativeSubscriptionRow(d.Raw)["over_edition"]) {
			complete = false
			for _, item := range j.Items {
				v := item.Candidate
				if v.Episodes == "" && v.GroupID > 0 && v.Priority != 0 {
					advance, order, complete = true, v.Priority, v.Priority >= v.TopOrder
					break
				}
			}
		}
		result, reason := "retained", "batch completed without closing the subscription; no download or intake completion implied"
		if complete {
			if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS RSS_HISTORY(ID INTEGER PRIMARY KEY,TYPE TEXT,RSSID TEXT,NAME TEXT,YEAR TEXT,TMDBID TEXT,SEASON TEXT,IMAGE TEXT,DESC TEXT,TOTAL INTEGER,START INTEGER,FINISH_TIME TEXT)`); err != nil {
				return err
			}
			values := []any{d.Raw["NAME"], d.Raw["YEAR"], d.Raw["TMDBID"], d.Raw["SEASON"], d.Raw["IMAGE"], d.Raw["DESC"], d.Raw["TOTAL_EP"], d.Raw["CURRENT_EP"]}
			for i, v := range values {
				value, err := fuzzySQLValue(v)
				if err != nil {
					return err
				}
				values[i] = value
			}
			args := append([]any{j.Kind, strconv.FormatInt(j.SubscriptionID, 10)}, values...)
			args = append(args, time.Now().Format("2006-01-02 15:04:05"))
			historyInsert, err := tx.ExecContext(ctx, `INSERT INTO RSS_HISTORY(TYPE,RSSID,NAME,YEAR,TMDBID,SEASON,IMAGE,DESC,TOTAL,START,FINISH_TIME) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, args...)
			if err := fuzzyOne(historyInsert, err); err != nil {
				return err
			}
			historyID, err := historyInsert.LastInsertId()
			if err != nil || historyID < 1 {
				return errFuzzyScope
			}
			history, err := readFuzzyTableRow(ctx, tx, "RSS_HISTORY", historyID)
			if err != nil || history == nil {
				return errFuzzyScope
			}
			if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_FUZZY_HISTORY_LINKS(HISTORY_ID,BATCH_ID,HISTORY_FINGERPRINT) VALUES(?,?,?) ON CONFLICT(HISTORY_ID) DO UPDATE SET BATCH_ID=excluded.BATCH_ID,HISTORY_FINGERPRINT=excluded.HISTORY_FINGERPRINT`, historyID, id, fuzzyDigest(history))); err != nil {
				return err
			}
			table := subscriptionTable(j.Kind)
			count, err := archiveFuzzyRows(ctx, tx, id, table, "ID=?", j.SubscriptionID)
			if err != nil || count != 1 {
				if err != nil {
					return err
				}
				return errFuzzyScope
			}
			if j.Kind == "TV" {
				count, err := archiveFuzzyRows(ctx, tx, id, "RSS_TV_EPISODES", "RSSID=?", j.SubscriptionID)
				if err != nil {
					return err
				}
				yes, err := fuzzyTable(ctx, tx, "RSS_TV_EPISODES")
				if err != nil {
					return err
				}
				if yes {
					r, err := tx.ExecContext(ctx, `DELETE FROM RSS_TV_EPISODES WHERE RSSID=?`, j.SubscriptionID)
					if err != nil {
						return err
					}
					if n, e := r.RowsAffected(); e != nil || n != int64(count) {
						return errFuzzyScope
					}
				}
			}
			if err := fuzzyOne(tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE ID=?", j.SubscriptionID)); err != nil {
				return err
			}
			if err := revokeFuzzyGrantTx(ctx, tx, j.Kind, j.SubscriptionID); err != nil {
				return err
			}
			reference := "subscription:" + j.Kind + ":" + strconv.FormatInt(j.SubscriptionID, 10)
			if err := bus.EmitData(ctx, tx, "rss_finished", "fuzzy:"+id, reference, text(d.Raw["NAME"])+" 订阅完成", "本轮模糊 RSS 批次已提交全部 "+strconv.Itoa(len(j.Items))+" 个目标；原行完整留档。此为订阅计划完成，不代表媒体下载或入库完成。", subscriptionEventData(d.Raw, j.Kind, j.SubscriptionID, false)); err != nil {
				return err
			}
			result, reason = "finished", "all selected submissions accepted; original subscription archived and finished, not media completion"
		} else if advance {
			oldGrant, raw, err := readFuzzyGrant(ctx, tx, j.Kind, j.SubscriptionID)
			if err != nil {
				return err
			}
			if oldGrant != j.authorization {
				return errFuzzyScope
			}
			if err := fuzzyOne(tx.ExecContext(ctx, "UPDATE "+subscriptionTable(j.Kind)+" SET FILTER_ORDER=?,STATE='R' WHERE ID=?", order, j.SubscriptionID)); err != nil {
				return err
			}
			current, err := readFuzzySourceRow(ctx, tx, j.Kind, j.SubscriptionID)
			if err != nil {
				return err
			}
			updated := oldGrant
			updated.Source = fuzzyDigest(current)
			if err := archiveFuzzyGrant(ctx, tx, j.Kind, j.SubscriptionID, raw); err != nil {
				return err
			}
			body := string(mustFuzzyJSON(updated))
			if body == "" {
				return errFuzzyScope
			}
			if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_GRANTS SET DOCUMENT=? WHERE KIND=? AND SUB_ID=? AND DOCUMENT=?`, body, j.Kind, j.SubscriptionID, raw)); err != nil {
				return err
			}
		}
		return fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_BATCHES SET STATE='completed',REASON=?,SOURCE_RESULT=? WHERE ID=? AND STATE='running'`, reason, result, id))
	})
}
func (api *fuzzySubscriptionAPI) resume(ctx context.Context, id, revision string, g fuzzyGrant) error {
	return api.write(ctx, func(tx *sql.Tx) error {
		j, err := readFuzzyBatch(ctx, tx, id)
		if err != nil {
			return err
		}
		if j.Revision != revision || j.State != "running" && j.State != "blocked" && j.State != "uncertain" {
			return errFuzzyScope
		}
		if !validFuzzyBatchAuthorization(j.definition, g) {
			return errFuzzyScope
		}
		if err := api.current(ctx, tx, g); err != nil {
			return err
		}
		for _, v := range j.Items {
			if v.State != "accepted" && v.State != "pending" && v.State != "rejected" {
				return errFuzzyScope
			}
			if v.State == "rejected" {
				var active bool
				key := subscriptionResourceKey(j.definition.Candidates[v.Index].Resource.DownloadURL)
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=?)`, key).Scan(&active); err != nil {
					return err
				}
				if active {
					return errFuzzyScope
				}
			}
		}
		body := string(mustFuzzyJSON(g))
		if body == "" {
			return errFuzzyScope
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_FUZZY_AUTH_HISTORY(BATCH_ID,EPOCH,DOCUMENT) VALUES(?,?,?) ON CONFLICT(BATCH_ID,EPOCH) DO NOTHING`, id, g.Epoch, body); err != nil {
			return err
		}
		var recorded string
		if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_FUZZY_AUTH_HISTORY WHERE BATCH_ID=? AND EPOCH=?`, id, g.Epoch).Scan(&recorded); err != nil || recorded != body {
			return errFuzzyScope
		}
		for _, v := range j.Items {
			if v.State == "rejected" {
				if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_ITEMS SET STATE='pending',REASON='',OWNER='',PROOF='' WHERE BATCH_ID=? AND ORDINAL=? AND STATE='rejected' AND OWNER=? AND PROOF=?`, id, v.Index, v.owner, v.proof)); err != nil {
					return err
				}
			}
		}
		return fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_BATCHES SET STATE='pending',REASON='fresh explicit approval for remaining unsent targets only',AUTH_DOCUMENT=? WHERE ID=? AND STATE=?`, body, id, j.State))
	})
}

// Keep stable row comparison SQL internal; no browser-supplied identifiers or
// conditions enter either this module's snapshots or the shared reservation.
func fuzzyQuoted(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
