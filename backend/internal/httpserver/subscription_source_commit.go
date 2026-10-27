package httpserver

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

type subscriptionSourceOutcome struct {
	complete  bool
	edition   *subscriptionEditionSnapshot
	historyID int64
}

// Source/history/quality semantics are shared by single and joint receipts.
// The caller owns the enclosing all-or-nothing download receipt transaction.
func (service subscriptionService) applySubscriptionSourceTx(ctx context.Context, tx *sql.Tx, kind string, id int64, plan *subscriptionSearchPlan, candidate *subscriptionCandidate, remaining []int) (subscriptionSourceOutcome, error) {
	archive := plan.edition != nil || plan.crossMember || plan.fileSelectionID != ""
	if !archive && kind == "TV" {
		var err error
		archive, err = hasSeasonProvenance(ctx, tx, id, plan.mediaID, plan.season)
		if err != nil {
			return subscriptionSourceOutcome{}, err
		}
	}
	archiveRaw := plan.raw
	if archive && !plan.typedSource && plan.edition == nil {
		var err error
		archiveRaw, err = readFuzzySourceRow(ctx, tx, kind, id)
		if err != nil || archiveRaw == nil {
			return subscriptionSourceOutcome{}, errSubscriptionSubmissionConflict
		}
	}
	var historyID int64
	complete := plan.LibraryComplete || kind == "TV" && len(remaining) == 0 || kind == "MOV" && candidate != nil
	var edition *subscriptionEditionSnapshot
	if plan.edition != nil {
		var err error
		complete, edition, err = persistEditionTx(ctx, tx, kind, id, *plan, candidate, remaining)
		if err != nil {
			return subscriptionSourceOutcome{}, err
		}
	}
	if complete {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS RSS_HISTORY (ID INTEGER PRIMARY KEY,TYPE TEXT,RSSID TEXT,NAME TEXT,YEAR TEXT,TMDBID TEXT,SEASON TEXT,IMAGE TEXT,DESC TEXT,TOTAL INTEGER,START INTEGER,FINISH_TIME TEXT)`); err != nil {
			return subscriptionSourceOutcome{}, err
		}
		values := []any{kind, strconv.FormatInt(id, 10), plan.raw["NAME"], plan.raw["YEAR"], plan.mediaID, plan.raw["SEASON"], plan.raw["IMAGE"], plan.raw["DESC"], plan.raw["TOTAL_EP"], plan.raw["CURRENT_EP"], time.Now().Format("2006-01-02 15:04:05")}
		if plan.edition != nil || plan.typedSource {
			for i, v := range values {
				value, err := fuzzySQLValue(v)
				if err != nil {
					return subscriptionSourceOutcome{}, err
				}
				values[i] = value
			}
		}
		insert, err := tx.ExecContext(ctx, `INSERT INTO RSS_HISTORY(TYPE,RSSID,NAME,YEAR,TMDBID,SEASON,IMAGE,DESC,TOTAL,START,FINISH_TIME) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, values...)
		if err := fuzzyOne(insert, err); err != nil {
			return subscriptionSourceOutcome{}, err
		}
		historyID, err = insert.LastInsertId()
		if err != nil || historyID < 1 {
			return subscriptionSourceOutcome{}, errSubscriptionSubmissionConflict
		}
		where := "ID=?"
		expected := []any{historyID}
		for i, column := range []string{"TYPE", "RSSID", "NAME", "YEAR", "TMDBID", "SEASON", "IMAGE", "DESC", "TOTAL", "START", "FINISH_TIME"} {
			where += " AND " + column + " IS ?"
			expected = append(expected, values[i])
		}
		var matches bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM RSS_HISTORY WHERE "+where+")", expected...).Scan(&matches); err != nil || !matches {
			return subscriptionSourceOutcome{}, errSubscriptionSubmissionConflict
		}
		if archive {
			selectionID := plan.fileSelectionID
			if plan.edition == nil && selectionID == "" {
				var err error
				selectionID, err = latestSourceSelection(ctx, tx, id, plan.mediaID, plan.season)
				if err != nil {
					return subscriptionSourceOutcome{}, err
				}
			}
			if err := archiveEditionSource(ctx, tx, kind, historyID, archiveRaw, selectionID); err != nil {
				return subscriptionSourceOutcome{}, err
			}
		}
		if kind == "TV" {
			if err := removeNativeTVEpisodes(ctx, tx, id); err != nil {
				return subscriptionSourceOutcome{}, err
			}
		}
		if err := fuzzyOne(tx.ExecContext(ctx, "DELETE FROM "+subscriptionTable(kind)+" WHERE ID=?", id)); err != nil {
			return subscriptionSourceOutcome{}, err
		}
		reference := "subscription:" + kind + ":" + strconv.FormatInt(id, 10)
		if err := bus.Emit(ctx, tx, "rss_finished", reference+":"+time.Now().UTC().Format(time.RFC3339Nano), reference, text(plan.raw["NAME"])+" 订阅完成", "类型："+kind+"；年份："+text(plan.raw["YEAR"])+"；季："+text(plan.raw["SEASON"])+"。此为订阅计划完成，不代表文件已入库。"); err != nil {
			return subscriptionSourceOutcome{}, err
		}
	} else if plan.edition != nil {
		// persistEditionTx preserved the ordinary episode ledger.
	} else if kind == "TV" {
		if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE RSS_TVS SET STATE='R',TOTAL=?,LACK=? WHERE ID=?`, plan.total, len(remaining), id)); err != nil {
			return subscriptionSourceOutcome{}, err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS RSS_TV_EPISODES (ID INTEGER PRIMARY KEY,RSSID TEXT,EPISODES TEXT)`); err != nil {
			return subscriptionSourceOutcome{}, err
		}
		if err := removeNativeTVEpisodes(ctx, tx, id); err != nil {
			return subscriptionSourceOutcome{}, err
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES (?,?)`, strconv.FormatInt(id, 10), subscriptionEpisodeString(remaining))); err != nil {
			return subscriptionSourceOutcome{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE RSS_MOVIES SET STATE='R' WHERE ID=?`, id); err != nil {
			return subscriptionSourceOutcome{}, err
		}
	}

	return subscriptionSourceOutcome{complete, edition, historyID}, nil
}
