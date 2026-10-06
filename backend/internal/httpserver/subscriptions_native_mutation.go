package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var errInvalidSubscriptionSelector = errors.New("invalid subscription selector")

func (service subscriptionService) serveCompatSubscriptionRemove(response http.ResponseWriter, request *http.Request) {
	if _, ok := requireServiceToken(response, request); !ok {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription delete request")
		return
	}
	kind := strings.ToUpper(strings.TrimSpace(request.Form.Get("type")))
	if kind == "电影" {
		kind = "MOV"
	} else if kind != "MOV" && kind != "" {
		// Python treated every non-movie media type as a TV subscription.
		kind = "TV"
	}
	if kind == "" {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription type")
		return
	}
	if strings.TrimSpace(request.Form.Get("rssid")) == "" {
		ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
		defer cancel()
		if err := service.removeNativeSubscriptionBySelector(ctx, kind, request.Form); err != nil {
			if errors.Is(err, errInvalidSubscriptionSelector) {
				writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription season")
				return
			}
			writeAPIError(response, http.StatusBadGateway, 502, "subscription deletion is unavailable")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "page": request.Form.Get("page"), "name": request.Form.Get("name")})
		return
	}
	service.serveNativeSubscriptionRemove(response, request, kind, request.Form.Get("rssid"), false)
}

func (service subscriptionService) serveCompatHistoryRemove(response http.ResponseWriter, request *http.Request) {
	if _, ok := requireServiceToken(response, request); !ok {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid history delete request")
		return
	}
	service.serveNativeSubscriptionRemove(response, request, "", request.Form.Get("rssid"), true)
}

func (service subscriptionService) serveNativeSubscriptionRemove(response http.ResponseWriter, request *http.Request, kind, rawID string, history bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(response, http.StatusBadRequest, 400, "valid subscription ID is required")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	if err := service.removeNativeSubscription(ctx, kind, id, history); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeAPIError(response, http.StatusNotFound, 404, "subscription not found")
			return
		}
		writeAPIError(response, http.StatusBadGateway, 502, "subscription deletion is unavailable")
		return
	}
	result := map[string]any{"code": 0, "success": true}
	if !history && request.Form != nil {
		result["page"] = request.Form.Get("page")
		result["name"] = request.Form.Get("name")
	}
	writeJSON(response, http.StatusOK, result)
}

func (service subscriptionService) removeNativeSubscription(ctx context.Context, kind string, id int64, history bool) error {
	database, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer database.Close()
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	table := "RSS_HISTORY"
	if !history {
		if kind == "MOV" {
			table = "RSS_MOVIES"
		} else {
			table = "RSS_TVS"
		}
	}
	var exists int
	if err := transaction.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE ID=?)", id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return sql.ErrNoRows
	}
	if table == "RSS_TVS" {
		if err := removeNativeTVEpisodes(ctx, transaction, id); err != nil {
			return err
		}
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM "+table+" WHERE ID=?", id); err != nil {
		return err
	}
	return transaction.Commit()
}

func (service subscriptionService) openNativeSubscriptionWriteDatabase() (*sql.DB, error) {
	// Reserve the writer at BEGIN, before reading the source snapshot. Deferred
	// read-to-write upgrades can fail immediately when another writer is waiting.
	databaseURL := (&url.URL{Scheme: "file", Path: service.databasePath, RawQuery: "mode=rw&_pragma=busy_timeout(5000)&_txlock=immediate"}).String()
	database, err := sql.Open("sqlite", databaseURL)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	return database, nil
}

func removeNativeTVEpisodes(ctx context.Context, transaction *sql.Tx, id int64) error {
	var episodesTable int
	if err := transaction.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='RSS_TV_EPISODES')").Scan(&episodesTable); err != nil {
		return err
	}
	if episodesTable != 0 {
		_, err := transaction.ExecContext(ctx, "DELETE FROM RSS_TV_EPISODES WHERE RSSID=?", id)
		return err
	}
	return nil
}

func (service subscriptionService) removeNativeSubscriptionBySelector(ctx context.Context, kind string, form url.Values) error {
	name := strings.TrimSpace(form.Get("name"))
	if name == "" { // Matches the old no-op for a missing name and rssid.
		return nil
	}
	year := strings.TrimSpace(form.Get("year"))
	if year != "" {
		name = strings.TrimSuffix(name, " ("+year+")")
	}
	tmdbID := strings.TrimSpace(form.Get("tmdbid"))
	if _, err := strconv.ParseUint(tmdbID, 10, 64); err != nil {
		tmdbID = ""
	}
	database, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer database.Close()
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if kind == "MOV" {
		if tmdbID != "" {
			if _, err := transaction.ExecContext(ctx, "DELETE FROM RSS_MOVIES WHERE TMDBID=?", tmdbID); err != nil {
				return err
			}
		}
		if year == "" {
			year = "None" // Python's str(None) in the old movie selector.
		}
		if _, err := transaction.ExecContext(ctx, "DELETE FROM RSS_MOVIES WHERE NAME=? AND YEAR=?", name, year); err != nil {
			return err
		}
		return transaction.Commit()
	}
	season := strings.TrimSpace(form.Get("season"))
	if season != "" {
		number, err := strconv.Atoi(season)
		if err != nil {
			return errInvalidSubscriptionSelector
		}
		season = strconv.Itoa(number)
	}
	var id int64
	if tmdbID != "" {
		query := "SELECT ID FROM RSS_TVS WHERE TMDBID=?"
		args := []any{tmdbID}
		if season != "" {
			query += " AND SEASON=?"
			args = append(args, season)
		}
		query += " ORDER BY ID LIMIT 1"
		err = transaction.QueryRowContext(ctx, query, args...).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if id == 0 {
		query := "SELECT ID FROM RSS_TVS WHERE NAME=?"
		args := []any{name}
		if season != "" {
			query += " AND SEASON=?"
			args = append(args, season)
		}
		if tmdbID != "" {
			query += " AND (TMDBID IS NULL OR TMDBID='' OR TMDBID=?)"
			args = append(args, tmdbID)
		}
		query += " ORDER BY ID LIMIT 1"
		err = transaction.QueryRowContext(ctx, query, args...).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if id == 0 {
		return transaction.Commit()
	}
	if err := removeNativeTVEpisodes(ctx, transaction, id); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM RSS_TVS WHERE ID=?", id); err != nil {
		return err
	}
	return transaction.Commit()
}
