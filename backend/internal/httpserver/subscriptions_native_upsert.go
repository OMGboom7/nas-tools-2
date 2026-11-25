package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

var errSubscriptionAlreadyExists = errors.New("subscription already exists")

func (service subscriptionService) serveNativeSubscriptionUpsert(response http.ResponseWriter, request *http.Request, input subscriptionUpsertRequest) {
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	var metadata *nativeSubscriptionMetadata
	if !input.FuzzyMatch {
		var err error
		if input.MediaID == "" || strings.HasPrefix(input.MediaID, "BG:") || strings.HasPrefix(input.MediaID, "DB:") {
			input, err = service.resolveNativeSubscriptionInput(ctx, input)
			if err != nil {
				if errors.Is(err, errInvalidSubscriptionSelector) {
					writeAPIError(response, http.StatusBadRequest, 400, "invalid media ID")
					return
				}
				writeAPIError(response, http.StatusBadGateway, 502, "TMDB subscription search is unavailable")
				return
			}
			if input.MediaID == "" {
				writeAPIError(response, http.StatusNotFound, 404, "matching media was not found")
				return
			}
		}
		metadata, err = service.fetchNativeSubscriptionMetadata(ctx, input)
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "TMDB subscription metadata is unavailable")
			return
		}
	}
	id, err := service.upsertNativeSubscription(ctx, input, metadata)
	if err != nil {
		switch {
		case errors.Is(err, errSubscriptionAlreadyExists):
			writeJSON(response, http.StatusConflict, map[string]any{"code": 9, "success": false, "message": "订阅已存在"})
		case errors.Is(err, sql.ErrNoRows):
			writeAPIError(response, http.StatusNotFound, 404, "subscription not found")
		case errors.Is(err, errInvalidSubscriptionSelector):
			writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription ID")
		default:
			writeAPIError(response, http.StatusBadGateway, 502, "subscription storage is unavailable")
		}
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "添加订阅成功", "data": map[string]any{"id": strconv.FormatInt(id, 10)}})
}

func (service subscriptionService) upsertNativeSubscription(ctx context.Context, input subscriptionUpsertRequest, metadata *nativeSubscriptionMetadata) (int64, error) {
	database, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		return 0, err
	}
	defer database.Close()
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()
	id, err := upsertNativeSubscriptionTx(ctx, transaction, input, metadata)
	if err != nil {
		return 0, err
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// RSS subscriptions share the same transaction with their processed marker
// and successful task counter. No local completion is recorded before a write.
func upsertNativeSubscriptionTx(ctx context.Context, transaction *sql.Tx, input subscriptionUpsertRequest, metadata *nativeSubscriptionMetadata) (int64, error) {
	season := ""
	if input.Type == "TV" && input.Season != "" {
		number, _ := strconv.Atoi(input.Season) // validateSubscription normalized this value.
		season = fmt.Sprintf("S%02d", number)
	}
	name, year := input.Name, input.Year
	var tmdbID, image, description, note any
	var totalEpisodes, currentEpisode any
	total, lack, fuzzy, state := 0, 0, 1, "R"
	if metadata != nil {
		name, year = metadata.Title, metadata.Year
		tmdbID, image, description, note = metadata.ID, metadata.Image, metadata.Overview, metadata.Note
		season = metadata.Season
		total, lack, fuzzy, state = metadata.Total, metadata.Lack, 0, "D"
		if input.TotalEpisodes != nil {
			totalEpisodes = *input.TotalEpisodes
		}
		if input.CurrentEpisode != nil {
			currentEpisode = *input.CurrentEpisode
		}
	}
	if input.RSSSites == nil {
		input.RSSSites = []string{}
	}
	if input.SearchSites == nil {
		input.SearchSites = []string{}
	}
	rssSites, err := json.Marshal(input.RSSSites)
	if err != nil {
		return 0, err
	}
	searchSites, err := json.Marshal(input.SearchSites)
	if err != nil {
		return 0, err
	}
	var filterRule any
	if input.FilterRule != "" {
		if parsed, err := strconv.ParseInt(input.FilterRule, 10, 64); err == nil {
			filterRule = parsed
		}
	}
	var downloadSetting any
	if input.DownloadSetting != "" {
		if parsed, err := strconv.ParseInt(input.DownloadSetting, 10, 64); err == nil {
			downloadSetting = parsed
		}
	}
	columns := subscriptionColumns
	values := []any{name, year, tmdbID, image, string(rssSites), string(searchSites), boolInt(input.OverEdition), input.Quality, input.Resolution, filterRule, input.ReleaseGroup, input.Include, input.Exclude, input.SavePath, downloadSetting, fuzzy, state, description, note, input.Keyword}
	if input.Type == "TV" {
		columns += tvSubscriptionColumns
		values = append(values, season, totalEpisodes, currentEpisode, total, lack)
	}
	id, err := writeSubscriptionRow(ctx, transaction, subscriptionRow{
		ID: input.ID, Kind: input.Type, Name: name, Year: year, Season: season,
		Columns: columns, Values: values, ClearFilterOrder: input.OverEdition && !input.FuzzyMatch,
	})
	if err != nil {
		return 0, err
	}
	if input.ID == "" {
		reference := "subscription:" + input.Type + ":" + strconv.FormatInt(id, 10)
		if err := bus.EmitData(ctx, transaction, "rss_added", reference+":"+time.Now().UTC().Format(time.RFC3339Nano), reference, name+" 已添加订阅", "类型："+input.Type+"；年份："+year+"；季："+season, subscriptionEventData(map[string]any{
			"NAME": name, "YEAR": year, "TMDBID": tmdbID, "SEASON": season, "IMAGE": image, "RSS_SITES": input.RSSSites, "SEARCH_SITES": input.SearchSites, "OVER_EDITION": input.OverEdition,
			"FILTER_RESTYPE": input.Quality, "FILTER_PIX": input.Resolution, "FILTER_TEAM": input.ReleaseGroup, "FILTER_RULE": filterRule, "SAVE_PATH": input.SavePath, "DOWNLOAD_SETTING": downloadSetting,
		}, input.Type, id, true)); err != nil {
			return 0, err
		}
	}
	return id, nil
}

type subscriptionRow struct {
	ID, Kind, Columns            string
	Name, Year, Season           any
	Values                       []any
	AllSeasons, ClearFilterOrder bool
}

// Both the Go API and installed legacy clients reserve the writer before the
// duplicate check. An edit keeps its original row and episode data on failure.
func writeSubscriptionRow(ctx context.Context, transaction *sql.Tx, row subscriptionRow) (int64, error) {
	var id int64
	if row.ID != "" {
		parsed, err := strconv.ParseInt(row.ID, 10, 64)
		if err != nil || parsed <= 0 {
			return 0, errInvalidSubscriptionSelector
		}
		id = parsed
	}
	table := "RSS_MOVIES"
	if row.Kind == "TV" {
		table = "RSS_TVS"
	}
	if _, err := transaction.ExecContext(ctx, nativeSubscriptionSchema(row.Kind)); err != nil {
		return 0, err
	}
	if id != 0 {
		var exists int
		if err := transaction.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE ID=?)", id).Scan(&exists); err != nil {
			return 0, err
		}
		if exists == 0 {
			return 0, sql.ErrNoRows
		}
	}
	duplicateQuery := "SELECT EXISTS(SELECT 1 FROM " + table + " WHERE NAME=? AND YEAR IS ?"
	duplicateArgs := []any{row.Name, row.Year}
	if row.Kind == "TV" && !row.AllSeasons {
		duplicateQuery += " AND SEASON=?"
		duplicateArgs = append(duplicateArgs, row.Season)
	}
	duplicateQuery += " AND ID<>?)"
	duplicateArgs = append(duplicateArgs, id)
	var duplicate int
	if err := transaction.QueryRowContext(ctx, duplicateQuery, duplicateArgs...).Scan(&duplicate); err != nil {
		return 0, err
	}
	if duplicate != 0 {
		return 0, errSubscriptionAlreadyExists
	}
	var statement string
	if id != 0 {
		assignments := make([]string, 0, len(row.Values))
		for _, column := range strings.Split(row.Columns, ",") {
			assignments = append(assignments, column+"=?")
		}
		if row.ClearFilterOrder {
			assignments = append(assignments, "FILTER_ORDER=NULL")
		}
		row.Values = append(row.Values, id)
		if row.Kind == "TV" {
			if err := removeNativeTVEpisodes(ctx, transaction, id); err != nil {
				return 0, err
			}
		}
		statement = "UPDATE " + table + " SET " + strings.Join(assignments, ",") + " WHERE ID=? RETURNING ID"
	} else {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(row.Values)), ",")
		statement = "INSERT INTO " + table + " (" + row.Columns + ") VALUES (" + placeholders + ") RETURNING ID"
	}
	if err := transaction.QueryRowContext(ctx, statement, row.Values...).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errors.New("subscription write did not affect a row")
		}
		return 0, err
	}
	if err := revokeFuzzyGrantTx(ctx, transaction, row.Kind, id); err != nil {
		return 0, err
	}
	if err := closeEditionCycleTx(ctx, transaction, row.Kind, id, "rule-upsert"); err != nil {
		return 0, err
	}
	return id, nil
}

func nativeSubscriptionSchema(kind string) string {
	common := `ID INTEGER PRIMARY KEY, NAME TEXT, YEAR TEXT, KEYWORD TEXT, TMDBID TEXT, IMAGE TEXT, RSS_SITES TEXT, SEARCH_SITES TEXT, OVER_EDITION INTEGER, FILTER_ORDER INTEGER, FILTER_RESTYPE TEXT, FILTER_PIX TEXT, FILTER_RULE INTEGER, FILTER_TEAM TEXT, FILTER_INCLUDE TEXT, FILTER_EXCLUDE TEXT, SAVE_PATH TEXT, DOWNLOAD_SETTING INTEGER, FUZZY_MATCH INTEGER, STATE TEXT, DESC TEXT, NOTE TEXT`
	if kind == "MOV" {
		return "CREATE TABLE IF NOT EXISTS RSS_MOVIES (" + common + ")"
	}
	return "CREATE TABLE IF NOT EXISTS RSS_TVS (" + common + ", SEASON TEXT, TOTAL_EP INTEGER, CURRENT_EP INTEGER, TOTAL INTEGER, LACK INTEGER)"
}
