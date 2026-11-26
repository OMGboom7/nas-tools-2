package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

func validOrdinaryRSS(h *downloaderconfig.OrdinaryRSS) bool {
	if (h.Kind != "MOV" && h.Kind != "TV") || h.ID < 1 || !fuzzyHex(h.Token, 64) || h.Marker.Name == "" || h.Marker.Enclosure == "" || h.Season < 0 || h.Season > 1000 {
		return false
	}
	switch h.Mode {
	case "record":
		return true
	case "finish":
		return h.Kind == "MOV" && h.Notice.Title != ""
	case "edition":
		return h.Rank > 0 && h.Notice.Title != ""
	case "state":
		return true
	case "episodes":
		if h.Kind != "TV" || h.Notice.Title == "" || len(h.Missing) == 0 || len(h.Missing) > 10000 || len(h.Covered) > 10000 {
			return false
		}
		for _, units := range [][]int{h.Missing, h.Covered} {
			for _, n := range units {
				if n < 1 || n > 10000 {
					return false
				}
			}
		}
		return len(h.Covered) > 0
	}
	return false
}

func checkOrdinaryRSSSource(ctx context.Context, db *sql.DB, h *downloaderconfig.OrdinaryRSS) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	source, err := captureClassicSubscription(ctx, tx, h.Kind, h.ID)
	if err != nil {
		return err
	}
	if source.Token != h.Token || text(source.Row["STATE"]) != "R" {
		return errSubscriptionSubmissionConflict
	}
	pending, err := readNativePendingSubscription(ctx, tx, h.Kind, h.ID)
	if err != nil {
		return err
	}
	cycle, err := readEditionCycle(ctx, tx, h.Kind, h.ID)
	if err != nil {
		return err
	}
	if pending || cycle != nil {
		return errSubscriptionSubmissionConflict
	}
	row := normalizeNativeSubscriptionRow(source.Row)
	switch {
	case truthy(row["fuzzy_match"]):
		if h.Mode != "record" {
			return errSubscriptionSubmissionConflict
		}
	case truthy(row["over_edition"]):
		if h.Mode != "edition" && h.Mode != "state" && h.Mode != "record" {
			return errSubscriptionSubmissionConflict
		}
	case h.Kind == "MOV":
		if h.Mode != "finish" {
			return errSubscriptionSubmissionConflict
		}
	default:
		season := 1
		if value := text(row["season"]); value != "" {
			season, err = strconv.Atoi(strings.TrimPrefix(value, "S"))
			if err != nil {
				return err
			}
		}
		if len(source.Episodes) > 1 {
			return errSubscriptionSubmissionConflict
		}
		if len(source.Episodes) == 1 {
			if saved := text(source.Episodes[0]["EPISODES"]); saved != "" {
				allowed := map[int]bool{}
				for _, value := range strings.Split(saved, ",") {
					n, err := strconv.Atoi(strings.TrimSpace(value))
					if err != nil {
						return err
					}
					allowed[n] = true
				}
				for _, n := range h.Missing {
					if !allowed[n] {
						return errSubscriptionSubmissionConflict
					}
				}
			}
		}
		if h.Mode != "episodes" || season != h.Season {
			return errSubscriptionSubmissionConflict
		}
	}
	return tx.Commit()
}

func ordinaryRSSBusy(ctx context.Context, tx subscriptionRowReader, kind string, id int64, token string) (bool, error) {
	exists, err := fuzzyTable(ctx, tx, "GO_ORDINARY_RSS_RECEIPTS")
	if !exists || err != nil {
		return false, err
	}
	var busy bool
	if token == "" {
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_ORDINARY_RSS_RECEIPTS WHERE KIND=? AND SUB_ID=?)`, kind, id).Scan(&busy)
		if err != nil || !busy {
			return false, err
		}
		s, err := readClassicSource(ctx, tx, kind, id)
		if err != nil || s.Row == nil {
			return false, err
		}
		s, err = s.withStoredToken(ctx, tx, kind, id)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		token = s.Token
	}
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_ORDINARY_RSS_RECEIPTS r LEFT JOIN GO_ORDINARY_RSS_CONTINUATIONS c USING(TOKEN) WHERE KIND=? AND SUB_ID=? AND (r.TOKEN=? OR c.CURRENT_TOKEN=?))`, kind, id, token, token).Scan(&busy)
	return busy, err
}

func ordinaryRSSKeys(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Run under the volume scan lock. First replay actual task acknowledgements
// even if the original RSS link, title lookup or torrent has disappeared.
func recoverOrdinaryRSS(ctx context.Context, db *sql.DB, path string) (map[string]string, error) {
	finished := map[string]string{}
	exists, err := fuzzyTable(ctx, db, "GO_ORDINARY_RSS_RECEIPTS")
	if !exists || err != nil {
		return finished, err
	}
	ids, err := ordinaryRSSKeys(ctx, db, `SELECT r.ID FROM GO_ORDINARY_RSS_RECEIPTS r JOIN GO_MANUAL_DOWNLOAD_SUBMISSIONS m ON m.ID=r.ID WHERE m.STATE='pending' AND r.ACK!='' ORDER BY r.rowid`)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		store, err := downloaderconfig.Open(path)
		if err != nil {
			return nil, err
		}
		defer store.Close()
		service := downloadService{client: http.DefaultClient, downloaders: store}
		for _, id := range ids {
			r, err := store.RestoreOrdinaryRSS(ctx, id, "")
			if err != nil {
				slog.Warn("ordinary RSS acknowledgement awaits recovery", "receipt", id)
				continue
			}
			if r.Definition.DownloaderType == "qbittorrent" {
				item, err := store.Get(ctx, r.Definition.DownloaderID)
				if err == nil && bus.Hash(item) == r.Definition.ConfigDigest {
					service.removeClassicMarker(ctx, item, r.TaskID, id)
				}
			}
		}
	}
	tokens, err := ordinaryRSSKeys(ctx, db, `SELECT DISTINCT r.TOKEN FROM GO_ORDINARY_RSS_RECEIPTS r JOIN GO_MANUAL_DOWNLOAD_SUBMISSIONS m ON m.ID=r.ID WHERE m.STATE='submitted' ORDER BY r.TOKEN`)
	if err != nil {
		return nil, err
	}
	for _, token := range tokens {
		enclosure, err := commitOrdinaryRSS(ctx, db, token)
		if err != nil {
			return nil, err
		}
		if enclosure != "" {
			finished[token] = enclosure
		}
	}
	return finished, nil
}

type ordinaryResult struct {
	id, key string
	h       downloaderconfig.OrdinaryRSS
}

func commitOrdinaryRSS(ctx context.Context, db *sql.DB, token string) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT r.ID,r.RESOURCE_KEY,r.DOCUMENT FROM GO_ORDINARY_RSS_RECEIPTS r JOIN GO_MANUAL_DOWNLOAD_SUBMISSIONS m ON m.ID=r.ID WHERE r.TOKEN=? AND m.STATE='submitted' AND m.TASK_ID!='' AND (r.ACK='' OR r.ACK=m.TASK_ID) ORDER BY r.rowid`, token)
	if err != nil {
		return "", err
	}
	var items []ordinaryResult
	for rows.Next() {
		var it ordinaryResult
		var raw string
		if err := rows.Scan(&it.id, &it.key, &raw); err != nil {
			rows.Close()
			return "", err
		}
		if json.Unmarshal([]byte(raw), &it.h) != nil || !validOrdinaryRSS(&it.h) || it.h.Token != token || subscriptionResourceKey(it.h.Marker.Enclosure) != it.key {
			rows.Close()
			return "", errFuzzyScope
		}
		items = append(items, it)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(items) == 0 {
		return "", err
	}
	h := items[0].h
	current := token
	err = tx.QueryRowContext(ctx, `SELECT CURRENT_TOKEN FROM GO_ORDINARY_RSS_CONTINUATIONS WHERE TOKEN=?`, token).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	source, err := captureClassicSubscription(ctx, tx, h.Kind, h.ID)
	if err != nil {
		return "", err
	}
	var done string
	if source.Token == current {
		pending, err := readNativePendingSubscription(ctx, tx, h.Kind, h.ID)
		if err != nil || pending {
			return "", err
		}
		cycle, err := readEditionCycle(ctx, tx, h.Kind, h.ID)
		if err != nil || cycle != nil {
			return "", err
		}
		op, q, selected, err := ordinaryRSSChange(items, source, current != token)
		if err != nil {
			return "", err
		}
		if op != "" {
			result, err := applyClassicSubscription(ctx, tx, op, q)
			if err != nil {
				return "", err
			}
			if result.Status == "finished" {
				done = selected
			}
			if result.Status == "updated" {
				if _, err = tx.ExecContext(ctx, `INSERT INTO GO_ORDINARY_RSS_CONTINUATIONS VALUES(?,?) ON CONFLICT(TOKEN) DO UPDATE SET CURRENT_TOKEN=excluded.CURRENT_TOKEN`, token, result.Token); err != nil {
					return "", err
				}
			}
		}
	}
	for _, it := range items {
		if err := recordClassicRSS(ctx, tx, []classicRSSMarker{it.h.Marker}); err != nil {
			return "", err
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `DELETE FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND TASK_ID=0 AND STATE='pending'`, it.key)); err != nil {
			return "", err
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `DELETE FROM GO_ORDINARY_RSS_RECEIPTS WHERE ID=?`, it.id)); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM GO_ORDINARY_RSS_CONTINUATIONS WHERE TOKEN=? AND NOT EXISTS(SELECT 1 FROM GO_ORDINARY_RSS_RECEIPTS WHERE TOKEN=?)`, token, token); err != nil {
		return "", err
	}
	return done, tx.Commit()
}

func ordinaryRSSChange(items []ordinaryResult, source classicSubscriptionSource, continued bool) (string, classicSubscriptionRequest, string, error) {
	h := items[0].h
	mode := ""
	covered := map[int]bool{}
	for _, item := range items {
		v := item.h
		if v.ID != h.ID || v.Kind != h.Kind || v.Season != h.Season {
			return "", classicSubscriptionRequest{}, "", errFuzzyScope
		}
		if v.Mode == "record" {
			continue
		}
		quality := v.Mode == "edition" || v.Mode == "state"
		if mode != "" && mode != v.Mode && !(quality && (mode == "edition" || mode == "state")) {
			return "", classicSubscriptionRequest{}, "", errFuzzyScope
		}
		if quality && mode != "" && v.Rank <= h.Rank {
			continue
		}
		if mode == "episodes" && !slices.Equal(h.Missing, v.Missing) {
			return "", classicSubscriptionRequest{}, "", errFuzzyScope
		}
		if mode == "" || quality {
			h = v
		}
		mode = v.Mode
		for _, n := range v.Covered {
			covered[n] = true
		}
	}
	q := classicSubscriptionRequest{Kind: h.Kind, ID: h.ID, Token: source.Token, State: "R", Rank: h.Rank, Highest: h.Highest, Notice: h.Notice, Poster: h.Poster, Overview: h.Overview}
	if mode == "episodes" {
		missing := h.Missing
		if continued {
			if len(source.Episodes) != 1 {
				return "", q, "", errFuzzyScope
			}
			missing = nil
			for _, s := range strings.Split(text(source.Episodes[0]["EPISODES"]), ",") {
				n, err := strconv.Atoi(s)
				if err != nil {
					return "", q, "", err
				}
				missing = append(missing, n)
			}
		}
		for _, n := range missing {
			if !covered[n] {
				q.Episodes = append(q.Episodes, n)
			}
		}
		if len(q.Episodes) == len(missing) {
			mode = ""
		} else if len(q.Episodes) > 0 {
			mode = "remaining"
		} else {
			mode = "finish"
		}
	}
	return mode, q, h.Marker.Enclosure, nil
}
