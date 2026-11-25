package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func rankRunCurrent(raw, actual map[string]string) bool {
	ok, err := indexerInstalled(actual["UserInstalledPlugins"], douban.RankPlugin)
	return err == nil && ok && raw[rankControlKey] == actual[rankControlKey] && raw[rankSettingsKey] == actual[rankSettingsKey]
}

func (api *doubanSyncAPI) rankTick(ctx context.Context, now time.Time) error {
	store := api.plugins.system
	raw, err := store.ReadMany(ctx, rankKeys)
	if err != nil {
		return err
	}
	c, err := doubanState(raw[rankControlKey])
	if err != nil || c.Actor == "" {
		return err
	}
	due := func(at time.Time) bool { return !at.IsZero() && !at.After(now) }
	if !c.Running && !due(c.Once) && !due(c.Next) {
		return nil
	}
	if !rankRunCurrent(raw, raw) {
		return nil
	}
	guard, err := (*api.login.guards).AcquireJob(ctx, "douban-rank")
	if errors.Is(err, organization.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer guard.Close()
	save := func(next doubanControl) error {
		b, err := json.Marshal(next)
		if err != nil {
			return err
		}
		err = store.UpdateMany(ctx, rankKeys, func(actual map[string]string) (map[string]string, error) {
			if !rankRunCurrent(raw, actual) {
				return nil, douban.ErrSettings
			}
			return map[string]string{rankControlKey: string(b)}, nil
		})
		if err == nil {
			raw[rankControlKey] = string(b)
		}
		return err
	}
	scope, err := api.rankScope(c.Actor, raw)
	if err != nil || scope != c.Scope {
		return save(doubanControl{Last: "账号或媒体配置已变化，请重新保存榜单设置。"})
	}
	cfg, err := douban.ReadRankSettings(raw[rankSettingsKey])
	if err != nil {
		return err
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return err
	}
	if due(c.Once) {
		c.Once = time.Time{}
	}
	if due(c.Next) {
		c.Next, err = cookieCloudSchedule(cfg.Cron, loc, now)
		if err != nil {
			return err
		}
	}
	c.Running = true
	c.Last = "正在刷新豆瓣榜单。"
	if err = save(c); err != nil {
		return err
	}

	current := func() error {
		actual, err := store.ReadMany(ctx, rankKeys)
		if err != nil {
			return err
		}
		scope, err := api.rankScope(c.Actor, actual)
		if err != nil || scope != c.Scope || !rankRunCurrent(raw, actual) {
			return douban.ErrSettings
		}
		return ctx.Err()
	}
	client := douban.Client{Transport: api.subscriptions.client.Transport}
	seen := map[string]bool{}
	added, owned, skipped, failed := 0, 0, 0, 0
	for _, address := range cfg.URLs() {
		if err := current(); err != nil {
			return err
		}
		items, err := client.RankFeed(ctx, address)
		if err != nil {
			failed++
			continue
		}
		for _, entry := range items {
			if err := current(); err != nil {
				return err
			}
			key := entry.Marker()
			if seen[key] {
				skipped++
				continue
			}
			result, err := api.rankEntry(ctx, raw, cfg, entry, now.In(loc), current)
			if err != nil {
				failed++
				continue
			}
			seen[key] = true
			switch result {
			case "added":
				added++
			case "owned":
				owned++
			default:
				skipped++
			}
		}
	}
	c.Running = false
	c.Last = fmt.Sprintf("榜单刷新完成，新增订阅 %d 条，库中已有 %d 条，跳过 %d 条，失败 %d 项。", added, owned, skipped, failed)
	return save(c)
}

func rankMarked(ctx context.Context, tx *sql.Tx, key string) (bool, error) {
	var seen bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE TORRENT_NAME=?)", key).Scan(&seen)
	return seen, err
}

func (api *doubanSyncAPI) rankMedia(ctx context.Context, entry douban.RankEntry) (*nativeRecognition, error) {
	if entry.ID == "" {
		result, problem := api.runner.recognition.recognize(ctx, entry.Title, "")
		if problem != nil {
			return nil, errors.New(problem.message)
		}
		return result, nil
	}
	client := douban.Client{Transport: api.subscriptions.client.Transport}
	subject, err := client.InterestSubject(ctx, entry.ID, "")
	if err != nil {
		return nil, err
	}
	input := subscriptionUpsertRequest{Type: "MOV"}
	if subject.Kind == "tv" {
		input.Type = "TV"
	}
	input, err = api.subscriptions.resolveDoubanTitles(ctx, input, subject.Original, subject.Title, subject.Year)
	if err != nil || input.MediaID == "" {
		return nil, err
	}
	media, problem := api.runner.recognition.recognizeSelected(ctx, subject.Title, subject.Original, subject.Kind, input.MediaID, nil, nil)
	if problem != nil {
		return nil, errors.New(problem.message)
	}
	return media, nil
}

func (api *doubanSyncAPI) rankEntry(ctx context.Context, raw map[string]string, cfg douban.RankSettings, entry douban.RankEntry, now time.Time, current func() error) (string, error) {
	key := entry.Marker()
	var marked bool
	err := api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error { var err error; marked, err = rankMarked(ctx, tx, key); return err })
	if err != nil || marked {
		return "", err
	}
	media, err := api.rankMedia(ctx, entry)
	if err != nil || media == nil {
		return "", err
	}
	input := subscriptionUpsertRequest{Type: "MOV", MediaID: strconv.FormatInt(media.detail.ID, 10), Name: text(media.data["title"]), Year: text(media.data["year"])}
	year, _ := strconv.Atoi(input.Year)
	vote, _ := media.detail.Attributes["vote_average"].(float64)
	if year > 0 && year < cfg.Year || vote > 0 && vote < cfg.Vote {
		return "", nil
	}
	lookupSeason := 1
	if media.kind == "tv" {
		input.Type = "TV"
		if season := media.meta.Episodes.Season; season != nil {
			lookupSeason = *season
			input.Season = strconv.Itoa(*season)
		}
	}
	if err = applyStoredSubscriptionDefaults(raw["DefaultRssSetting"+input.Type], &input); err != nil {
		return "", err
	}
	if validateSubscription(&input) != "" {
		return "", douban.ErrResponse
	}
	snapshot, err := api.subscriptions.configStore.Snapshot()
	if err != nil {
		return "", err
	}
	if err := current(); err != nil {
		return "", err
	}
	inventory, err := api.subscriptions.nativeMediaInventory(ctx, media.meta, media.detail, media.kind, media.libraryCategory)
	if err != nil {
		return "", err
	}
	owned := len(inventory.ItemIDs) > 0
	if input.Type == "TV" {
		owned = len(inventory.Episodes[lookupSeason]) > 0
	}
	// For an unqualified series the old lookup used season one, while
	// construction of a new subscription selected the latest season.
	historyInput := input
	if input.Type == "TV" {
		historyInput.Season = strconv.Itoa(lookupSeason)
	}
	if err := current(); err != nil {
		return "", err
	}
	result := ""
	err = api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error {
		if err := systemconfig.UpdateManyTx(ctx, tx, rankKeys, func(actual map[string]string) (map[string]string, error) {
			if !rankRunCurrent(raw, actual) || raw["DefaultRssSetting"+input.Type] != actual["DefaultRssSetting"+input.Type] {
				return nil, douban.ErrSettings
			}
			return nil, nil
		}); err != nil {
			return err
		}
		if found, err := rankMarked(ctx, tx, key); err != nil || found {
			return err
		}
		_, count, err := readDoubanPluginHistory(ctx, tx, douban.RankPlugin, input.MediaID)
		if err != nil {
			return err
		}
		state := "RSS"
		if owned {
			state = "DOWNLOADED"
			result = "owned"
		} else {
			historical, err := rssSubscriptionHistoryTx(ctx, tx, historyInput, nil)
			if err != nil {
				return err
			}
			if !historical {
				// The generic attributes hold JSON numbers as floats. Use the exact decoded TMDB identity.
				attrs := maps.Clone(media.detail.Attributes)
				attrs["id"] = media.detail.ID
				details, err := json.Marshal(attrs)
				if err != nil {
					return err
				}
				m, err := subscriptionMetadataFromJSON(input, details, objectValue(snapshot["app"]))
				if err != nil {
					return err
				}
				created, err := ensureDoubanSubscription(ctx, tx, input, m, true)
				if err != nil {
					return err
				}
				if created {
					result = "added"
				}
				res, err := tx.ExecContext(ctx, "INSERT INTO RSS_TORRENTS(TORRENT_NAME,ENCLOSURE) VALUES(?,NULL)", key)
				if err != nil {
					return err
				}
				if n, err := res.RowsAffected(); err != nil || n != 1 {
					return systemconfig.ErrHistoryConflict
				}
				if found, err := rankMarked(ctx, tx, key); err != nil || !found {
					return systemconfig.ErrHistoryConflict
				}
			}
		}

		date := now.Format("2006-01-02 15:04:05")
		imageBase := strings.TrimRight(strings.TrimSpace(text(objectValue(snapshot["app"])["tmdb_image_url"])), "/")
		if imageBase == "" {
			imageBase = "https://image.tmdb.org"
		}
		data, err := json.Marshal(map[string]any{"id": input.MediaID, "name": input.Name, "type": text(media.data["type"]), "year": input.Year, "rating": vote, "image": tmdbImageURL(imageBase, text(media.detail.Attributes["poster_path"])), "state": state, "add_time": date})
		if err != nil {
			return err
		}
		return saveDoubanPluginHistory(ctx, tx, douban.RankPlugin, input.MediaID, state, date, data, count)
	})
	return result, err
}
