package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type subscriptionRefreshAPI struct {
	service subscriptionService
	auth    *nativeAuthentication
	pureGo  bool
	mu      sync.Mutex
	running bool
}

type subscriptionRefreshResult struct {
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

func (api *subscriptionRefreshAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if api.auth == nil {
		writeAPIError(w, 503, 503, "native subscriptions are unavailable")
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization token is invalid or expired")
		return
	}
	if !api.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission is required")
		return
	}
	result, failure := api.execute(r.Context())
	if failure != nil {
		writeJSON(w, failure.status, map[string]any{"code": failure.status, "success": false, "message": failure.message, "data": result})
		return
	}
	code := 0
	if result.Failed > 0 {
		code = 1
	}
	writeJSON(w, 200, map[string]any{"code": code, "success": code == 0, "data": result})
}

func (api *subscriptionRefreshAPI) execute(parent context.Context) (subscriptionRefreshResult, *recognitionFailure) {
	result := subscriptionRefreshResult{}
	if !api.pureGo {
		return result, &recognitionFailure{501, "native subscription refresh requires disabled legacy backend"}
	}
	api.mu.Lock()
	if api.running {
		api.mu.Unlock()
		return result, &recognitionFailure{409, "subscription refresh is already running"}
	}
	api.running = true
	api.mu.Unlock()
	defer func() { api.mu.Lock(); api.running = false; api.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	db, err := api.service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return result, &recognitionFailure{503, "subscription storage is unavailable"}
	}
	defer db.Close()
	for _, kind := range []string{"MOV", "TV"} {
		table := "RSS_MOVIES"
		if kind == "TV" {
			table = "RSS_TVS"
		}
		rows, err := readSubscriptionRows(ctx, db, table)
		if err != nil || len(rows) > 10000 {
			return result, &recognitionFailure{503, "subscription configuration is unavailable or exceeded limit"}
		}
		for _, raw := range rows {
			if ctx.Err() != nil {
				return result, &recognitionFailure{504, "subscription refresh was cancelled or timed out"}
			}
			item := normalizeNativeSubscriptionRow(raw)
			if text(raw["STATE"]) != "R" || text(item["fuzzy_match"]) == "true" || text(item["fuzzy_match"]) == "1" {
				result.Skipped++
				continue
			}
			input := subscriptionUpsertRequest{Type: kind, Name: text(raw["NAME"]), Year: text(raw["YEAR"]), MediaID: text(raw["TMDBID"]), Season: text(raw["SEASON"])}
			if kind == "TV" {
				if input.Season == "" {
					input.Season = "1"
				}
				if value := text(item["total_ep"]); value != "" && value != "0" {
					n, err := strconv.Atoi(value)
					if err != nil || n < 0 || n > 10000 {
						result.Failed++
						continue
					}
					input.TotalEpisodes = &n
				}
			}
			if validateSubscription(&input) != "" {
				result.Failed++
				continue
			}
			if !numericTMDBID(input.MediaID) {
				input, err = api.service.resolveNativeSubscriptionInput(ctx, input)
				if err != nil || input.MediaID == "" {
					result.Failed++
					continue
				}
			}
			metadata, err := api.service.fetchNativeSubscriptionMetadata(ctx, input)
			if err != nil {
				result.Failed++
				continue
			}
			updated, err := api.service.applySubscriptionRefresh(ctx, kind, raw, metadata)
			if err != nil {
				result.Failed++
			} else if updated {
				result.Updated++
			} else {
				result.Skipped++
			}
		}
	}
	return result, nil
}

func (service subscriptionService) applySubscriptionRefresh(ctx context.Context, kind string, raw map[string]any, metadata *nativeSubscriptionMetadata) (bool, error) {
	if metadata == nil || !numericTMDBID(metadata.ID) || metadata.Title == "" || (kind != "MOV" && kind != "TV") {
		return false, errors.New("invalid subscription refresh metadata")
	}
	db, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		return false, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	name := metadata.Title
	configuration, err := service.configStore.Snapshot()
	if err != nil {
		return false, err
	}
	if follow, ok := objectValue(configuration["media"])["name_follow_tmdb_changed"].(bool); ok && !follow {
		name = text(raw["NAME"])
	}
	description := metadata.Overview
	var legacy map[string]any
	if json.Unmarshal([]byte(text(raw["DESC"])), &legacy) == nil && legacy != nil {
		description = text(raw["DESC"])
	} // Keep legacy settings stored here.
	var note map[string]any
	oldNote := text(raw["NOTE"])
	if len(oldNote) > 1<<20 || (oldNote != "" && (json.Unmarshal([]byte(oldNote), &note) != nil || note == nil)) {
		return false, errors.New("invalid subscription note")
	}
	if note == nil {
		note = map[string]any{}
	}
	var newNote map[string]any
	if json.Unmarshal([]byte(metadata.Note), &newNote) != nil || newNote == nil {
		return false, errors.New("invalid metadata note")
	}
	for key, value := range newNote {
		note[key] = value
	}
	encoded, err := json.Marshal(note)
	if err != nil || len(encoded) > 1<<20 {
		return false, errors.New("subscription note exceeded limit")
	}
	table, assignments := "RSS_MOVIES", "NAME=?,YEAR=?,TMDBID=?,IMAGE=?,DESC=?,NOTE=?"
	args := []any{name, metadata.Year, metadata.ID, metadata.Image, description, string(encoded)}
	var missing []int
	changedTotal := false
	if kind == "TV" {
		table = "RSS_TVS"
		total, err := strconv.Atoi(text(raw["TOTAL"]))
		if err != nil {
			return false, err
		}
		lack, err := strconv.Atoi(text(raw["LACK"]))
		if err != nil {
			return false, err
		}
		if total < 0 || total > 10000 || lack < 0 || lack > total || metadata.Total <= 0 || metadata.Total > 10000 {
			return false, errors.New("invalid subscription progress")
		}
		changedTotal = total != metadata.Total
		nextLack := lack
		if changedTotal {
			missing, err = refreshMissingEpisodes(ctx, tx, text(raw["ID"]), total, lack, metadata.Total)
			if err != nil {
				return false, err
			}
			nextLack = len(missing)
		}
		assignments += ",TOTAL=?,LACK=?"
		args = append(args, metadata.Total, nextLack)
	}
	// Compare every source field involved in identification/progress. A user edit,
	// concurrent download or deletion during the TMDB request is not overwritten.
	where := "ID=?"
	args = append(args, raw["ID"])
	keys := []string{"NAME", "YEAR", "TMDBID", "STATE", "FUZZY_MATCH", "DESC", "NOTE"}
	if kind == "TV" {
		keys = append(keys, "SEASON", "TOTAL", "LACK", "TOTAL_EP", "CURRENT_EP")
	}
	for _, key := range keys {
		where += " AND " + key + " IS ?"
		args = append(args, raw[key])
	}
	result, err := tx.ExecContext(ctx, "UPDATE "+table+" SET "+assignments+" WHERE "+where, args...)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if changedTotal {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS RSS_TV_EPISODES (ID INTEGER PRIMARY KEY,RSSID TEXT,EPISODES TEXT)`); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM RSS_TV_EPISODES WHERE RSSID=?`, raw["ID"]); err != nil {
			return false, err
		}
		parts := make([]string, len(missing))
		for i, n := range missing {
			parts[i] = strconv.Itoa(n)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO RSS_TV_EPISODES (RSSID,EPISODES) VALUES (?,?)`, raw["ID"], strings.Join(parts, ",")); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func refreshMissingEpisodes(ctx context.Context, tx *sql.Tx, id string, total, lack, nextTotal int) ([]int, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='RSS_TV_EPISODES')`).Scan(&exists); err != nil {
		return nil, err
	}
	set := map[int]bool{}
	found := false
	if exists {
		rows, err := tx.QueryContext(ctx, `SELECT COALESCE(EPISODES,'') FROM RSS_TV_EPISODES WHERE RSSID=?`, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return nil, err
			}
			if found || len(raw) > 64<<10 {
				return nil, errors.New("invalid episode progress")
			}
			found = true
			if raw == "" {
				continue
			}
			for _, part := range strings.Split(raw, ",") {
				n, err := strconv.Atoi(part)
				if err != nil || n < 1 || n > total || set[n] {
					return nil, errors.New("invalid episode progress")
				}
				set[n] = true
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if found && len(set) != lack {
		return nil, errors.New("episode count conflicts with progress")
	}
	if !found {
		for n := total - lack + 1; n <= total; n++ {
			set[n] = true
		}
	}
	for n := total + 1; n <= nextTotal; n++ {
		set[n] = true
	}
	missing := []int{}
	for n := range set {
		if n <= nextTotal {
			missing = append(missing, n)
		}
	}
	sort.Ints(missing)
	return missing, nil
}

func startSubscriptionRefreshWorker(ctx context.Context, api *subscriptionRefreshAPI) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		runSubscriptionRefreshWorker(ctx, api, ticker.C)
	}()
	return func() { <-done }
}

func runSubscriptionRefreshWorker(ctx context.Context, api *subscriptionRefreshAPI, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok || ctx.Err() != nil {
				return
			}
			result, failure := api.execute(ctx)
			if failure != nil {
				if ctx.Err() == nil {
					slog.Warn("native subscription refresh failed", "status", failure.status)
				}
			} else {
				slog.Info("native subscription refresh finished", "updated", result.Updated, "skipped", result.Skipped, "failed", result.Failed)
			}
		}
	}
}
