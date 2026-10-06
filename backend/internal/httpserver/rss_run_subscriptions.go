package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
	"github.com/dlclark/regexp2"
)

func (api *rssRunAPI) runSubscriptions(ctx context.Context, task rsstaskconfig.Task) (rssRunResult, *recognitionFailure) {
	result := rssRunResult{}
	fail := func(status int, message string) (rssRunResult, *recognitionFailure) {
		return result, &recognitionFailure{status, message}
	}
	service := api.recognition.service
	if service.databasePath == "" {
		return fail(503, "native subscription storage is unavailable")
	}
	var note map[string]any
	if task.Note != "" && (len(task.Note) > 64<<10 || json.Unmarshal([]byte(task.Note), &note) != nil) {
		return fail(422, "invalid RSS task settings")
	}
	if task.SavePath == "" {
		task.SavePath = text(note["save_path"])
	}
	base := subscriptionUpsertRequest{OverEdition: task.OverEdition != 0, SavePath: task.SavePath, FilterRule: task.Filter}
	if task.DownloadSetting != 0 {
		base.DownloadSetting = strconv.FormatInt(task.DownloadSetting, 10)
	}
	if base.FilterRule != "" {
		id, err := strconv.ParseInt(base.FilterRule, 10, 64)
		if err != nil || id < -1 {
			return fail(422, "invalid RSS subscription filter group")
		}
	}
	if len(base.SavePath) > 4096 {
		return fail(422, "RSS subscription directory exceeded limit")
	}
	if task.Sites != "" {
		var sites map[string]json.RawMessage
		if len(task.Sites) > 64<<10 || json.Unmarshal([]byte(task.Sites), &sites) != nil || sites == nil {
			return fail(422, "invalid RSS subscription sites")
		}
		var err error
		base.RSSSites, err = rssSubscriptionSiteList(sites["rss_sites"])
		if err != nil {
			return fail(422, "invalid RSS subscription sites")
		}
		base.SearchSites, err = rssSubscriptionSiteList(sites["search_sites"])
		if err != nil {
			return fail(422, "invalid RSS subscription sites")
		}
	}
	if task.FilterArgs != "" {
		var args map[string]string
		if len(task.FilterArgs) > 16<<10 || json.Unmarshal([]byte(task.FilterArgs), &args) != nil || args == nil {
			return fail(422, "invalid RSS subscription filters")
		}
		base.Quality, base.Resolution, base.ReleaseGroup = args["restype"], args["pix"], args["team"]
	}
	conditions := []*regexp2.Regexp{nil, nil}
	for i, raw := range []string{task.Include, task.Exclude} {
		if raw == "" {
			continue
		}
		if len(raw) > 16<<10 {
			return fail(422, "RSS conditions exceeded limit")
		}
		condition, err := regexcompat.Compile(raw, regexp2.IgnoreCase)
		if err != nil {
			return fail(422, "invalid RSS condition")
		}
		conditions[i] = condition
	}
	configuration, err := api.preview.config.Snapshot()
	if err != nil {
		return fail(503, "RSS configuration is unavailable")
	}
	articles, failure := api.fetchRunArticles(ctx, task, configuration)
	if failure != nil {
		return result, failure
	}
	result.Total = len(articles)
	for _, article := range articles {
		if ctx.Err() != nil {
			return fail(504, "RSS execution was cancelled or timed out")
		}
		if strings.TrimSpace(article.Title) == "" {
			result.Skipped++
			continue
		}
		if len(article.Title) > 512 || (article.Type != "" && article.Type != "movie" && article.Type != "tv") {
			return fail(422, "invalid RSS subscription resource")
		}
		processed, err := api.preview.tasks.IsProcessed(ctx, task.Uses, article.Title, article.Year, "")
		if err != nil {
			return fail(503, "RSS history is unavailable")
		}
		if processed {
			result.Skipped++
			continue
		}
		key := article.Title
		if article.Year != "" {
			key += " " + article.Year
		}
		meta, revised, err := api.offlineMetadata(ctx, key, article.Description)
		if err != nil {
			return fail(422, "RSS name parsing failed")
		}
		matched := true
		for i, condition := range conditions {
			if condition == nil {
				continue
			}
			found, err := condition.MatchString(revised)
			if err != nil {
				return fail(422, "RSS condition matching failed")
			}
			matched = matched && (found != (i == 1))
		}
		if !matched || meta.Title == "" {
			result.Skipped++
			continue
		}
		input := base
		input.Name, input.Year, input.Type = meta.Title, meta.Year, "MOV"
		if meta.Episodes.TV || article.Type == "tv" {
			input.Type = "TV"
			if meta.Episodes.Season != nil {
				input.Season = strconv.Itoa(*meta.Episodes.Season)
			}
		}
		if article.Type == "movie" {
			input.Type, input.Season = "MOV", ""
		}
		if message := validateSubscription(&input); message != "" {
			return fail(422, "invalid RSS subscription identity")
		}
		// Already completed subscriptions must not be re-created from this RSS.
		history, err := service.rssSubscriptionHistory(ctx, input, nil)
		if err != nil {
			return fail(503, "subscription history is unavailable")
		}
		if history {
			result.Skipped++
			continue
		}
		input.MediaID, err = service.resolveNativeMediaID(ctx, input, true)
		if err != nil {
			return fail(502, "RSS subscription identification is unavailable")
		}
		if input.MediaID == "" {
			result.Skipped++
			continue
		}
		metadata, err := service.fetchNativeSubscriptionMetadata(ctx, input)
		if err != nil {
			return fail(502, "RSS subscription metadata is unavailable")
		}
		created, err := service.completeRSSSubscription(ctx, task.ID, key, input, metadata)
		if err != nil {
			return fail(503, "RSS subscription could not be persisted")
		}
		if created {
			result.Subscribed++
		} else {
			result.Skipped++
		}
	}
	return result, nil
}

func rssSubscriptionSiteList(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []string{}, nil
	}
	var items []any
	if json.Unmarshal(raw, &items) != nil || len(items) > 500 {
		return nil, errors.New("invalid subscription sites")
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		value := ""
		switch v := item.(type) {
		case string:
			value = v
		case float64:
			if v <= 0 || v > 1<<53 || v != float64(int64(v)) {
				return nil, errors.New("invalid site ID")
			}
			value = strconv.FormatInt(int64(v), 10)
		default:
			return nil, errors.New("invalid site selector")
		}
		if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("invalid site selector")
		}
		result = append(result, value)
	}
	return result, nil
}

func rssSubscriptionHistoryTx(ctx context.Context, tx *sql.Tx, input subscriptionUpsertRequest, metadata *nativeSubscriptionMetadata) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='RSS_HISTORY')`).Scan(&exists); err != nil || !exists {
		return false, err
	}
	name, year, season, mediaID := input.Name, input.Year, "", input.MediaID
	if input.Type == "TV" && input.Season != "" {
		n, _ := strconv.Atoi(input.Season)
		season = fmt.Sprintf("S%02d", n)
	}
	if metadata != nil {
		name, year, season, mediaID = metadata.Title, metadata.Year, metadata.Season, metadata.ID
	}
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_HISTORY WHERE TYPE=? AND COALESCE(SEASON,'')=? AND ((NAME=? AND COALESCE(YEAR,'')=?) OR (?<>'' AND TMDBID=?)))`, input.Type, season, name, year, mediaID, mediaID).Scan(&exists)
	return exists, err
}

func (service subscriptionService) rssSubscriptionHistory(ctx context.Context, input subscriptionUpsertRequest, metadata *nativeSubscriptionMetadata) (bool, error) {
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
	return rssSubscriptionHistoryTx(ctx, tx, input, metadata)
}

func (service subscriptionService) completeRSSSubscription(ctx context.Context, taskID int64, key string, input subscriptionUpsertRequest, metadata *nativeSubscriptionMetadata) (bool, error) {
	if metadata == nil || !numericTMDBID(metadata.ID) || metadata.Title == "" || key == "" || len(key) > 1024 || (input.Type != "MOV" && input.Type != "TV") {
		return false, errors.New("invalid RSS subscription completion")
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
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM CONFIG_USER_RSS WHERE ID=? AND USES IN ('R','S'))`, taskID).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, rsstaskconfig.ErrNotFound
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?)`, key).Scan(&exists); err != nil || exists {
		return false, err
	}
	if exists, err := rssSubscriptionHistoryTx(ctx, tx, input, metadata); err != nil || exists {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, nativeSubscriptionSchema(input.Type)); err != nil {
		return false, err
	}
	table := "RSS_MOVIES"
	if input.Type == "TV" {
		table = "RSS_TVS"
	}
	query := "SELECT EXISTS(SELECT 1 FROM " + table + " WHERE TMDBID=?"
	args := []any{metadata.ID}
	if input.Type == "TV" {
		query += " AND SEASON=?"
		args = append(args, metadata.Season)
	}
	if err := tx.QueryRowContext(ctx, query+")", args...).Scan(&exists); err != nil {
		return false, err
	}
	created := false
	if !exists {
		_, err = upsertNativeSubscriptionTx(ctx, tx, input, metadata)
		created = err == nil
		if err != nil && !errors.Is(err, errSubscriptionAlreadyExists) {
			return false, err
		}
	}
	kind := "电影"
	if input.Type == "TV" {
		kind = "电视剧"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO RSS_TORRENTS (TORRENT_NAME,ENCLOSURE,TYPE,TITLE,YEAR,SEASON) VALUES (?,?,?,?,?,?)`, key, key, kind, metadata.Title, metadata.Year, metadata.Season); err != nil {
		return false, err
	}
	if created {
		if _, err := tx.ExecContext(ctx, `UPDATE CONFIG_USER_RSS SET PROCESS_COUNT=MAX(CAST(COALESCE(PROCESS_COUNT,'0') AS INTEGER),0)+1, UPDATE_TIME=? WHERE ID=?`, time.Now().Format("2006-01-02 15:04:05"), taskID); err != nil {
			return false, err
		}
	}
	return created, tx.Commit()
}
