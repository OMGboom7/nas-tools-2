package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

type classicSubscriptionRequest struct {
	Kind, State, Token string
	ID                 int64
	Episodes           []int
	Rank, Highest      int
	Notice             downloaderconfig.SubscriptionNotice
	Poster, Overview   *string
	Markers            []classicRSSMarker
}

type classicRSSMarker = downloaderconfig.SubscriptionMarker

func recordClassicRSS(ctx context.Context, tx *sql.Tx, markers []classicRSSMarker) error {
	for _, m := range markers {
		if m.Enclosure == "" {
			continue
		}
		seen, err := subscriptionTorrentRecorded(ctx, tx, m.Enclosure)
		if err != nil {
			return err
		}
		if !seen {
			if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO RSS_TORRENTS(TORRENT_NAME,ENCLOSURE,TYPE,TITLE,YEAR,SEASON,EPISODE) VALUES(?,?,?,?,?,?,?)`, m.Name, m.Enclosure, m.Type, m.Title, m.Year, m.Season, m.Episode)); err != nil {
				return err
			}
		}
	}
	return nil
}

type classicSubscriptionSource struct {
	Row      map[string]any
	Episodes []map[string]any
	Token    string
}

// Capture the source, including its episode ledger, before calling a site,
// media server or downloader. A delete and reinsert can reuse every SQL value.
func captureClassicSubscription(ctx context.Context, tx *sql.Tx, kind string, id int64) (classicSubscriptionSource, error) {
	s, err := readClassicSource(ctx, tx, kind, id)
	if err != nil || s.Row == nil {
		return s, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO GO_CLASSIC_SUBSCRIPTION_VERSIONS(KIND,SUB_ID,STAMP) VALUES(?,?,randomblob(16)) ON CONFLICT DO NOTHING`, kind, id); err != nil {
		return s, err
	}
	return s.withStoredToken(ctx, tx, kind, id)
}

func readClassicSource(ctx context.Context, r subscriptionRowReader, kind string, id int64) (classicSubscriptionSource, error) {
	var s classicSubscriptionSource
	var err error
	s.Row, err = readFuzzySourceRow(ctx, r, kind, id)
	if err == nil && s.Row != nil && kind == "TV" {
		s.Episodes, err = editionEpisodeRows(ctx, r, id)
	}
	return s, err
}

func (s classicSubscriptionSource) withStoredToken(ctx context.Context, r subscriptionRowReader, kind string, id int64) (classicSubscriptionSource, error) {
	var stamp string
	err := r.QueryRowContext(ctx, `SELECT hex(STAMP) FROM GO_CLASSIC_SUBSCRIPTION_VERSIONS WHERE KIND=? AND SUB_ID=?`, kind, id).Scan(&stamp)
	if err == nil {
		s.Token = fuzzyDigest([]any{kind, id, stamp, s.Row, s.Episodes})
	}
	return s, err
}

func listClassicSubscriptions(ctx context.Context, tx *sql.Tx, q classicSubscriptionRequest) ([]classicSubscriptionSource, error) {
	table := subscriptionTable(q.Kind)
	// INSERT also handles SQLite REPLACE with recursive triggers disabled.
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_CLASSIC_SUBSCRIPTION_VERSIONS(KIND TEXT,SUB_ID INTEGER,STAMP BLOB NOT NULL,PRIMARY KEY(KIND,SUB_ID))`); err != nil {
		return nil, err
	}
	for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
		row := "NEW"
		if op == "DELETE" {
			row = "OLD"
		}
		if _, err := tx.ExecContext(ctx, "CREATE TRIGGER IF NOT EXISTS GO_CLASSIC_"+table+"_"+op+" AFTER "+op+" ON "+table+" BEGIN DELETE FROM GO_CLASSIC_SUBSCRIPTION_VERSIONS WHERE KIND='"+q.Kind+"' AND SUB_ID="+row+".ID; END"); err != nil {
			return nil, err
		}
	}
	query := "SELECT ID FROM " + table
	var args []any
	if q.ID > 0 {
		query += " WHERE ID=?"
		args = append(args, q.ID)
	} else if q.State != "" {
		query += " WHERE STATE=?"
		args = append(args, q.State)
	}
	rows, err := tx.QueryContext(ctx, query+" ORDER BY ID", args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := make([]classicSubscriptionSource, 0, len(ids))
	for _, id := range ids {
		s, err := captureClassicSubscription(ctx, tx, q.Kind, id)
		if err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, nil
}

type classicProgressResult struct{ Status, Token string }

func ClassicSubscriptionCommand(ctx context.Context, path, op string, in io.Reader, out io.Writer) error {
	var q classicSubscriptionRequest
	input := &io.LimitedReader{R: in, N: 2 << 20}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 || (q.Kind != "TV" && q.Kind != "MOV") || q.ID < 0 {
		return errFuzzyScope
	}
	service := subscriptionService{databasePath: (config.Application{Path: path}).UserDatabasePath()}
	db, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var result any
	switch op {
	case "list":
		result, err = listClassicSubscriptions(ctx, tx, q)
	case "record":
		err = recordClassicRSS(ctx, tx, q.Markers)
		result = classicProgressResult{Status: "recorded"}
	default:
		if !fuzzyHex(q.Token, 64) || q.ID < 1 {
			return errFuzzyScope
		}
		result, err = applyClassicSubscription(ctx, tx, op, q)
		if err == nil {
			err = recordClassicRSS(ctx, tx, q.Markers)
		}
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

func applyClassicSubscription(ctx context.Context, tx *sql.Tx, op string, q classicSubscriptionRequest) (classicProgressResult, error) {
	stale := classicProgressResult{Status: "stale"}
	s, err := captureClassicSubscription(ctx, tx, q.Kind, q.ID)
	if err != nil || s.Token != q.Token {
		return stale, err
	}
	pending, err := readNativePendingSubscription(ctx, tx, q.Kind, q.ID)
	if err != nil || pending {
		return stale, err
	}
	cycle, err := readEditionCycle(ctx, tx, q.Kind, q.ID)
	if err != nil || cycle != nil {
		return stale, err
	}
	row := normalizeNativeSubscriptionRow(s.Row)
	if truthy(text(row["fuzzy_match"])) {
		return stale, nil
	}
	table := subscriptionTable(q.Kind)
	edition := truthy(text(row["over_edition"]))
	switch op {
	case "state":
		if q.State != "R" && q.State != "S" {
			return stale, errFuzzyScope
		}
		err = fuzzyOne(tx.ExecContext(ctx, "UPDATE "+table+" SET STATE=? WHERE ID=?", q.State, q.ID))
	case "remaining":
		if q.Kind != "TV" || edition || len(q.Episodes) == 0 {
			return stale, errFuzzyScope
		}
		slices.Sort(q.Episodes)
		q.Episodes = slices.Compact(q.Episodes)
		if q.Episodes[0] < 1 {
			return stale, errFuzzyScope
		}
		if err := fuzzyOne(tx.ExecContext(ctx, "UPDATE RSS_TVS SET STATE='R',LACK=? WHERE ID=?", len(q.Episodes), q.ID)); err != nil {
			return stale, err
		}
		if err := removeNativeTVEpisodes(ctx, tx, q.ID); err != nil {
			return stale, err
		}
		err = fuzzyOne(tx.ExecContext(ctx, "INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES(?,?)", q.ID, subscriptionEpisodeString(q.Episodes)))
	case "edition":
		if !edition || q.Rank == 0 {
			return stale, errFuzzyScope
		}
		previous := 0
		if raw := text(s.Row["FILTER_ORDER"]); raw != "" {
			previous, err = strconv.Atoi(raw)
			if err != nil {
				return stale, err
			}
		}
		rank := max(previous, q.Rank)
		if err := fuzzyOne(tx.ExecContext(ctx, "UPDATE "+table+" SET STATE='R',FILTER_ORDER=? WHERE ID=?", rank, q.ID)); err != nil {
			return stale, err
		}
		if q.Rank >= q.Highest && q.Rank >= previous {
			s.Row, err = readFuzzySourceRow(ctx, tx, q.Kind, q.ID)
			if err != nil || s.Row == nil {
				return stale, errSubscriptionSubmissionConflict
			}
			return finishClassicSubscription(ctx, tx, q, s, edition)
		}
	case "finish":
		if edition {
			return stale, errFuzzyScope
		}
		return finishClassicSubscription(ctx, tx, q, s, false)
	default:
		return stale, errFuzzyScope
	}
	if err != nil {
		return stale, err
	}
	next, err := captureClassicSubscription(ctx, tx, q.Kind, q.ID)
	return classicProgressResult{Status: "updated", Token: next.Token}, err
}

func finishClassicSubscription(ctx context.Context, tx *sql.Tx, q classicSubscriptionRequest, s classicSubscriptionSource, edition bool) (classicProgressResult, error) {
	var result classicProgressResult
	raw := s.Row
	var poster, overview any
	if q.Poster != nil {
		poster = *q.Poster
	}
	if q.Overview != nil {
		overview = *q.Overview
	}
	history, err := appendSubscriptionHistory(ctx, tx, []any{q.Kind, strconv.FormatInt(q.ID, 10), raw["NAME"], raw["YEAR"], raw["TMDBID"], raw["SEASON"], poster, overview, raw["TOTAL_EP"], raw["CURRENT_EP"], time.Now().Format("2006-01-02 15:04:05")})
	if err != nil {
		return result, err
	}
	if edition {
		if err := archiveEditionSource(ctx, tx, q.Kind, history, raw); err != nil {
			return result, err
		}
	} else if q.Kind == "TV" {
		season := 1
		if value := text(raw["SEASON"]); value != "" {
			season, err = strconv.Atoi(strings.TrimPrefix(value, "S"))
			if err != nil {
				return result, err
			}
		}
		has, err := hasSeasonProvenance(ctx, tx, q.ID, text(raw["TMDBID"]), season)
		if err != nil {
			return result, err
		}
		if has {
			selection, err := latestSourceSelection(ctx, tx, q.ID, text(raw["TMDBID"]), season)
			if err != nil {
				return result, err
			}
			if err := archiveEditionSource(ctx, tx, q.Kind, history, raw, selection); err != nil {
				return result, err
			}
		}
	}
	if q.Kind == "TV" {
		if err := removeNativeTVEpisodes(ctx, tx, q.ID); err != nil {
			return result, err
		}
	}
	if err := revokeFuzzyGrantTx(ctx, tx, q.Kind, q.ID); err != nil {
		return result, err
	}
	if err := fuzzyOne(tx.ExecContext(ctx, "DELETE FROM "+subscriptionTable(q.Kind)+" WHERE ID=?", q.ID)); err != nil {
		return result, err
	}
	ref := "classic-subscription:" + q.Kind + ":" + strconv.FormatInt(q.ID, 10)
	title := strings.TrimSpace(q.Notice.Title + " " + q.Notice.Season)
	if edition {
		title += " 已完成洗版"
	} else {
		title += " 已完成订阅"
	}
	body := "类型：" + q.Notice.Type
	if q.Notice.Vote != "" {
		body += "，" + q.Notice.Vote
	}
	data := subscriptionEventData(raw, q.Kind, q.ID, false)
	data["classic_notice"] = map[string]string{"image": q.Notice.Image, "link": q.Notice.Link}
	if err := bus.EmitData(ctx, tx, "rss_finished", s.Token, ref, title, body, data); err != nil {
		return result, err
	}
	return classicProgressResult{Status: "finished"}, nil
}
