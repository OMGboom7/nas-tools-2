package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

// The due slot remains pending until the transaction creating a subscription
// also consumes it. A crash on either side of commit cannot add a second film.
type movieRandomRun struct {
	api      *doubanSyncAPI
	raw      map[string]string
	control  doubanControl
	snapshot map[string]any
	now      time.Time
}

func (api *doubanSyncAPI) movieRandomTick(ctx context.Context, now time.Time) error {
	raw, err := api.plugins.system.ReadMany(ctx, movieRandomKeys)
	if err != nil {
		return err
	}
	c, err := doubanState(raw[movieRandomControlKey])
	if err != nil || c.Actor == "" {
		return err
	}
	due := func(at time.Time) bool { return !at.IsZero() && !at.After(now) }
	if !due(c.Next) && !due(c.Once) {
		return nil
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], movieRandomID)
	if err != nil || !installed {
		return err
	}
	guard, err := (*api.login.guards).AcquireJob(ctx, "movie-random")
	if errors.Is(err, organization.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer guard.Close()
	run := movieRandomRun{api: api, raw: raw, control: c}
	scope, err := api.scheduleScope(c.Actor, raw[movieRandomSettingsKey])
	if err != nil || scope != c.Scope {
		run.control = doubanControl{}
		return run.finish(ctx, "账号或媒体配置已变化，请重新保存电影随机订阅设置。")
	}
	values, err := movieRandomValues(raw[movieRandomSettingsKey])
	if err != nil {
		return err
	}
	cfg, filters, err := decodeRandomSettings(values)
	if err != nil {
		return err
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return err
	}
	run.now = now.In(loc)
	if due(c.Once) {
		run.control.Once = time.Time{}
	}
	if due(c.Next) {
		run.control.Next, err = cookieCloudSchedule(cfg.Cron, loc, now)
		if err != nil {
			return err
		}
	}
	run.snapshot, err = api.subscriptions.configStore.Snapshot()
	if err != nil {
		return err
	}
	return run.draw(ctx, filters)
}

func (run *movieRandomRun) current(ctx context.Context) error {
	actual, err := run.api.plugins.system.ReadMany(ctx, movieRandomKeys)
	if err != nil {
		return err
	}
	if !maps.Equal(run.raw, actual) {
		return douban.ErrSettings
	}
	scope, err := run.api.scheduleScope(run.control.Actor, actual[movieRandomSettingsKey])
	if err != nil || scope != run.control.Scope {
		return douban.ErrSettings
	}
	return ctx.Err()
}

func (run *movieRandomRun) updateControl(ctx context.Context, tx *sql.Tx, last string) error {
	return systemconfig.UpdateManyTx(ctx, tx, movieRandomKeys, func(actual map[string]string) (map[string]string, error) {
		if !maps.Equal(actual, run.raw) {
			return nil, douban.ErrSettings
		}
		if last == "" {
			return nil, nil
		}
		c := run.control
		c.Last = last
		b, err := json.Marshal(c)
		return map[string]string{movieRandomControlKey: string(b)}, err
	})
}
func (run *movieRandomRun) finish(ctx context.Context, last string) error {
	return run.api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error { return run.updateControl(ctx, tx, last) })
}

// Verify the discover row before using it to create a persistent subscription.
func matchesRandomFilters(item tmdbRecommendation, filters map[string]string) bool {
	if item.ID <= 0 || strings.TrimSpace(item.Title) == "" || tmdbImageURL("", item.PosterPath) == "" {
		return false
	}
	if date := filters["primary_release_date.gte"]; date != "" && (len(item.ReleaseDate) != 10 || item.ReleaseDate < date) {
		return false
	}
	if language := filters["with_original_language"]; language != "" && item.OriginalLanguage != language {
		return false
	}
	if vote := filters["vote_average.gte"]; vote != "" {
		minimum, _ := strconv.ParseFloat(vote, 64)
		if item.VoteAverage < minimum {
			return false
		}
	}
	if genre := filters["with_genres"]; genre != "" {
		id, _ := strconv.Atoi(genre)
		return slices.Contains(item.GenreIDs, id)
	}
	return true
}

func (run *movieRandomRun) draw(ctx context.Context, filters map[string]string) error {
	getPage := func(page int) (tmdbPage, error) {
		return fetchTMDBPage(ctx, run.api.subscriptions.client, run.snapshot, "/discover/movie", page, filters)
	}
	if err := run.current(ctx); err != nil {
		return err
	}
	first, err := getPage(1)
	if err != nil || first.TotalPages == nil || *first.TotalPages < 0 {
		return run.finish(ctx, "获取 TMDB 电影列表失败。")
	}
	pages := rand.Perm(min(*first.TotalPages, 500))
	seen := map[int64]bool{}
	failed := 0
	for _, offset := range pages[:min(5, len(pages))] {
		if err := run.current(ctx); err != nil {
			return err
		}
		page := first
		if offset != 0 {
			page, err = getPage(offset + 1)
			if err != nil {
				failed++
				continue
			}
		}
		for _, i := range rand.Perm(len(page.Results)) {
			item := page.Results[i]
			if seen[item.ID] || !matchesRandomFilters(item, filters) {
				continue
			}
			seen[item.ID] = true
			if err := run.current(ctx); err != nil {
				return err
			}
			added, err := run.candidate(ctx, item)
			if err != nil {
				if errors.Is(err, douban.ErrSettings) || ctx.Err() != nil {
					return err
				}
				failed++
				continue
			}
			if added {
				return nil
			}
		}
	}
	return run.finish(ctx, fmt.Sprintf("本次未新增订阅，已检查 %d 部电影，失败 %d 项。", len(seen), failed))
}

func (run *movieRandomRun) candidate(ctx context.Context, item tmdbRecommendation) (bool, error) {
	api := run.api
	id := strconv.FormatInt(item.ID, 10)
	marker := "movierandom: " + item.Title + " (DB:" + id + ")"
	var marked bool
	err := api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		marked, err = rankMarked(ctx, tx, marker)
		return err
	})
	if err != nil || marked {
		return false, err
	}
	media, problem := api.runner.recognition.recognizeSelected(ctx, item.Title, "", "movie", id, nil, nil)
	if problem != nil || media == nil || media.detail.ID != item.ID {
		return false, douban.ErrResponse
	}
	input := subscriptionUpsertRequest{Type: "MOV", MediaID: id, Name: text(media.data["title"]), Year: text(media.data["year"])}
	if err = applyStoredSubscriptionDefaults(run.raw["DefaultRssSettingMOV"], &input); err != nil {
		return false, err
	}
	if validateSubscription(&input) != "" {
		return false, douban.ErrResponse
	}
	meta, err := recognizedSubscriptionMetadata(input, media, run.snapshot)
	if err != nil {
		return false, err
	}
	if err := run.current(ctx); err != nil {
		return false, err
	}
	inventory, err := api.subscriptions.nativeMediaInventory(ctx, media.meta, media.detail, "movie", media.libraryCategory)
	if err != nil {
		return false, err
	}
	if err := run.current(ctx); err != nil {
		return false, err
	}
	added := false
	err = api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error {
		if err := run.updateControl(ctx, tx, ""); err != nil {
			return err
		}
		if marked, err := rankMarked(ctx, tx, marker); err != nil || marked {
			return err
		}
		state := "DOWNLOADED"
		if len(inventory.ItemIDs) == 0 {
			state = "RSS"
			historical, err := rssSubscriptionHistoryTx(ctx, tx, input, meta)
			if err != nil {
				return err
			}
			if !historical {
				added, err = ensureDoubanSubscription(ctx, tx, input, meta, true)
				if err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, "INSERT INTO RSS_TORRENTS(TORRENT_NAME,ENCLOSURE) VALUES(?,NULL)", marker); err != nil {
					return err
				}
				if found, err := rankMarked(ctx, tx, marker); err != nil || !found {
					return systemconfig.ErrHistoryConflict
				}
			}
		}
		_, count, err := readDoubanPluginHistory(ctx, tx, movieRandomID, id)
		if err != nil {
			return err
		}
		date := run.now.Format("2006-01-02 15:04:05")
		base := strings.TrimRight(text(objectValue(run.snapshot["app"])["tmdb_image_url"]), "/")
		if base == "" {
			base = "https://image.tmdb.org"
		}
		data, err := json.Marshal(map[string]any{"id": item.ID, "name": item.Title, "type": "电影", "year": meta.Year, "rating": math.RoundToEven(item.VoteAverage*10) / 10, "image": tmdbImageURL(base, item.PosterPath), "state": state, "add_time": date})
		if err != nil {
			return err
		}
		if err := saveDoubanPluginHistory(ctx, tx, movieRandomID, id, state, date, data, count); err != nil {
			return err
		}
		if added {
			return run.updateControl(ctx, tx, "新增订阅 1 条："+meta.Title)
		}
		return nil
	})
	return added && err == nil, err
}
