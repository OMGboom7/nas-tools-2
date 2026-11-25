package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func waitDouban(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func startDoubanSyncWorker(ctx context.Context, api *doubanSyncAPI) func() {
	if !api.available() {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := api.tick(ctx, time.Now()); err != nil && ctx.Err() == nil {
				slog.Warn("豆瓣同步状态更新失败")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { <-done }
}
func sameDoubanRun(raw, actual map[string]string) bool {
	installed, err := indexerInstalled(actual["UserInstalledPlugins"], douban.SyncPlugin)
	return err == nil && installed && actual[doubanSettingsKey] == raw[doubanSettingsKey] && actual[doubanControlKey] == raw[doubanControlKey]
}

func (api *doubanSyncAPI) tick(ctx context.Context, now time.Time) error {
	store := api.plugins.system
	raw, err := store.ReadMany(ctx, doubanKeys)
	if err != nil {
		return err
	}
	c, err := doubanState(raw[doubanControlKey])
	if err != nil || c.Actor == "" {
		return err
	}
	due := func(t time.Time) bool { return !t.IsZero() && !t.After(now) }
	var command string
	if !c.Running && !due(c.Once) && !due(c.Next) {
		if c.RemoteSince.IsZero() {
			return nil
		}
		command, err = api.pendingCommand(ctx, c)
		if err != nil || command == "" {
			return err
		}
		receiver, _, e := api.remoteScope(ctx)
		if e != nil || receiver.Scope.ID != c.remoteID() {
			return nil
		}
	}
	if installed, err := indexerInstalled(raw["UserInstalledPlugins"], douban.SyncPlugin); err != nil || !installed {
		return err
	}
	guard, err := (*api.login.guards).AcquireJob(ctx, "douban-sync")
	if errors.Is(err, organization.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer guard.Close()
	save := func(c doubanControl) error {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		err = store.Transaction(ctx, func(tx *sql.Tx) error {
			if command != "" && c.Running {
				result, e := tx.ExecContext(ctx, "UPDATE GO_DOUBAN_COMMANDS SET PENDING=0 WHERE ID=? AND SCOPE=? AND PENDING=1", command, c.remoteID())
				if e != nil {
					return e
				}
				n, e := result.RowsAffected()
				if e != nil || n != 1 {
					return douban.ErrSettings
				}
			}
			return systemconfig.UpdateManyTx(ctx, tx, doubanKeys, func(actual map[string]string) (map[string]string, error) {
				if !sameDoubanRun(raw, actual) {
					return nil, douban.ErrSettings
				}
				return map[string]string{doubanControlKey: string(b)}, nil
			})
		})
		if err == nil {
			raw[doubanControlKey] = string(b)
		}
		return err
	}
	if c.Running {
		return save(doubanControl{Last: "上次同步中断。已提交的历史和订阅可在页面核对，重新保存设置后可继续。"})
	}
	scope, err := api.scope(ctx, c.Actor, raw)
	if err != nil || scope != c.Scope {
		return save(doubanControl{Last: "账号或同步设置已变化，请重新保存设置。"})
	}
	cfg, err := douban.ReadSettings(raw[doubanSettingsKey])
	if err != nil || !cfg.Valid() {
		return save(doubanControl{Last: "同步设置无效。"})
	}
	if due(c.Once) {
		c.Once = time.Time{}
	} else if due(c.Next) {
		c.Next = now.Add(cfg.Interval()).UTC()
	}
	c.Running = true
	c.Last = "正在同步豆瓣标记。"
	if err := save(c); err != nil {
		return err
	}
	current := func() error {
		actual, err := store.ReadMany(ctx, doubanKeys)
		if err != nil {
			return err
		}
		scope, err := api.scope(ctx, c.Actor, actual)
		if err != nil || scope != c.Scope || !sameDoubanRun(raw, actual) {
			return douban.ErrSettings
		}
		if command != "" {
			receiver, _, err := api.remoteScope(ctx)
			if err != nil || receiver.Scope.ID != c.remoteID() {
				return douban.ErrSettings
			}
		}
		return ctx.Err()
	}
	wait := func(delay time.Duration) error {
		if err := api.pause(ctx, delay); err != nil {
			return err
		}
		return current()
	}
	client := douban.Client{Transport: api.subscriptions.client.Transport}
	loc, err := rssWorkerLocation()
	if err != nil {
		return err
	}
	if err := current(); err != nil {
		return err
	}
	ids, err := client.Interests(ctx, cfg.InterestList, now.In(loc), func(context.Context) error { return wait(time.Duration(1+rand.IntN(5)) * time.Second) })
	c.Running = false
	if err != nil {
		c.Last = "读取豆瓣片单或动态失败，请核对账号和访问状态。"
		return save(c)
	}
	var changed, skipped, failed, incomplete, uncertain int
	for _, id := range ids {
		if err := current(); err != nil {
			return err
		}
		state, err := api.historyState(ctx, id)
		if err != nil {
			failed++
			continue
		}
		if state != "" && (state != "NEW" || !cfg.Subscribe && !cfg.Search) {
			skipped++
			continue
		}
		if err := wait(time.Duration(10+rand.IntN(51)) * time.Second); err != nil {
			return err
		}
		subject, err := client.InterestSubject(ctx, id, cfg.Cookie)
		if err != nil {
			failed++
			continue
		}
		ok, err := api.process(ctx, raw, cfg, subject, now.In(loc), current)
		if errors.Is(err, errDoubanIncomplete) {
			incomplete++
		} else if errors.Is(err, downloaderconfig.ErrManualPending) {
			uncertain++
		} else if err != nil {
			failed++
		} else if ok {
			changed++
		} else {
			skipped++
		}
	}
	c.Last = fmt.Sprintf("同步完成，写入 %d 条，已处理 %d 条，失败 %d 条。", changed, skipped, failed)
	if incomplete > 0 || uncertain > 0 {
		c.Last += fmt.Sprintf(" 未下齐 %d 条，下载回执待核对 %d 条。", incomplete, uncertain)
	}
	return save(c)
}

func readDoubanHistory(ctx context.Context, tx *sql.Tx, id string) (string, int64, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='PLUGIN_HISTORY' COLLATE NOCASE)").Scan(&exists); err != nil || !exists {
		return "", 0, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT CASE WHEN length(CAST(VALUE AS BLOB))<=1048576 THEN VALUE END FROM PLUGIN_HISTORY WHERE PLUGIN_ID=? AND KEY=?", douban.SyncPlugin, id)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	var state string
	var count int64
	for rows.Next() {
		var v []byte
		var entry struct {
			ID    json.RawMessage
			State string
		}
		if err := rows.Scan(&v); err != nil {
			return "", 0, err
		}
		if json.Unmarshal(v, &entry) != nil || douban.ID(entry.ID) != id || entry.State == "" || count > 0 && entry.State != state {
			return "", 0, systemconfig.ErrHistoryConflict
		}
		state = entry.State
		count++
	}
	return state, count, rows.Err()
}
func (api *doubanSyncAPI) historyState(ctx context.Context, id string) (string, error) {
	var state string
	err := api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error { var err error; state, _, err = readDoubanHistory(ctx, tx, id); return err })
	return state, err
}

func (api *doubanSyncAPI) process(ctx context.Context, raw map[string]string, cfg douban.Settings, s douban.Subject, now time.Time, current func() error) (bool, error) {
	target := "NEW"
	input := subscriptionUpsertRequest{Type: "MOV", Name: s.Title, Year: s.Year}
	if s.Kind == "tv" {
		input.Type = "TV"
		for _, title := range []string{s.Title, s.Original} {
			parsed, err := mediameta.Parse(title, "")
			if err != nil {
				return false, err
			}
			if season := parsed.Episodes.Season; season != nil {
				n := strconv.Itoa(*season)
				if input.Season != "" && input.Season != n || parsed.Episodes.EndSeason != nil && *parsed.Episodes.EndSeason != *season && !(cfg.Search && !cfg.Subscribe) {
					return false, douban.ErrResponse
				}
				input.Season = n
			}
		}
	}
	var metadata *nativeSubscriptionMetadata
	var err error
	if cfg.Search && !cfg.Subscribe {
		metadata, err = api.directDownload(ctx, raw, s, input, current)
		if err != nil {
			return false, err
		}
		target = "DOWNLOADED"
	} else if cfg.Subscribe {
		if err = applyStoredSubscriptionDefaults(raw["DefaultRssSetting"+input.Type], &input); err != nil {
			return false, err
		}
		if validateSubscription(&input) != "" {
			return false, douban.ErrResponse
		}
		input, err = api.subscriptions.resolveDoubanTitles(ctx, input, s.Original, s.Title, s.Year)
		if err != nil || input.MediaID == "" {
			return false, douban.ErrResponse
		}
		metadata, err = api.subscriptions.fetchNativeSubscriptionMetadata(ctx, input)
		if err != nil {
			return false, err
		}
		target = "RSS"
		if cfg.Search {
			detail, err := fetchNativeTMDBDetails(ctx, api.subscriptions.configStore, api.subscriptions.client.Transport, s.Kind, input.MediaID)
			if err != nil {
				return false, err
			}
			meta := mediameta.Metadata{Title: metadata.Title, Year: metadata.Year}
			if s.Kind == "tv" {
				season, err := strconv.Atoi(strings.TrimPrefix(metadata.Season, "S"))
				if err != nil {
					return false, err
				}
				meta.Episodes.Season = &season
				meta.Episodes.TV = true
			}
			coverage, err := api.subscriptions.nativeMediaExistence(ctx, meta, detail, s.Kind)
			if err != nil {
				return false, err
			}
			if coverage.Complete {
				target = "DOWNLOADED"
			}
		}
	}
	if err := current(); err != nil {
		return false, err
	}
	changed := false
	err = api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error {
		if err := systemconfig.UpdateManyTx(ctx, tx, doubanKeys, func(actual map[string]string) (map[string]string, error) {
			key := "DefaultRssSetting" + input.Type
			if !sameDoubanRun(raw, actual) || target == "RSS" && actual[key] != raw[key] {
				return nil, douban.ErrSettings
			}
			return nil, nil
		}); err != nil {
			return err
		}
		state, n, err := readDoubanHistory(ctx, tx, s.ID)
		if err != nil || state != "" && (state != "NEW" || target == "NEW") {
			return err
		}
		if target == "RSS" {
			if err := insertDoubanSubscription(ctx, tx, input, metadata, cfg.Search); err != nil {
				return err
			}
		}
		title, year := s.Title, s.Year
		if cfg.Search && metadata != nil {
			title, year = metadata.Title, metadata.Year
		}
		typ := "电影"
		if s.Kind == "tv" {
			typ = "电视剧"
		}
		date := now.Format("2006-01-02 15:04:05")
		b, err := json.Marshal(map[string]any{"id": s.ID, "name": title, "year": year, "type": typ, "rating": s.Rating, "image": s.Image, "state": target, "add_time": date})
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS PLUGIN_HISTORY (ID INTEGER PRIMARY KEY,PLUGIN_ID TEXT,KEY TEXT,VALUE TEXT,DATE TEXT)"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS GO_PLUGIN_HISTORY_PLUGIN_KEY ON PLUGIN_HISTORY(PLUGIN_ID,KEY)"); err != nil {
			return err
		}
		var result sql.Result
		if n == 0 {
			result, err = tx.ExecContext(ctx, "INSERT INTO PLUGIN_HISTORY(PLUGIN_ID,KEY,VALUE,DATE) VALUES(?,?,?,?)", douban.SyncPlugin, s.ID, string(b), date)
			n = 1
		} else {
			result, err = tx.ExecContext(ctx, "UPDATE PLUGIN_HISTORY SET VALUE=json_patch(VALUE,?) WHERE PLUGIN_ID=? AND KEY=?", string(b), douban.SyncPlugin, s.ID)
		}
		if err != nil {
			return err
		}
		written, err := result.RowsAffected()
		if err != nil || written != n {
			return systemconfig.ErrHistoryConflict
		}
		stored, count, err := readDoubanHistory(ctx, tx, s.ID)
		if err != nil || stored != target || count != n {
			return systemconfig.ErrHistoryConflict
		}
		changed = true
		return nil
	})
	return changed && err == nil, err
}
func insertDoubanSubscription(ctx context.Context, tx *sql.Tx, input subscriptionUpsertRequest, m *nativeSubscriptionMetadata, search bool) error {
	if _, err := tx.ExecContext(ctx, nativeSubscriptionSchema(input.Type)); err != nil {
		return err
	}
	table := "RSS_MOVIES"
	where := "TMDBID=?"
	args := []any{m.ID}
	if input.Type == "TV" {
		table = "RSS_TVS"
		where += " AND SEASON=?"
		args = append(args, m.Season)
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE "+where+")", args...).Scan(&exists); err != nil || exists {
		return err
	}
	id, err := upsertNativeSubscriptionTx(ctx, tx, input, m)
	if errors.Is(err, errSubscriptionAlreadyExists) {
		// A matching title/year can belong to a different TMDB subject.
		// The verified same-ID/season case was handled above.
		return systemconfig.ErrHistoryConflict
	}
	if err != nil {
		return err
	}
	want := "D"
	if !search {
		want = "R"
		if _, err = tx.ExecContext(ctx, "UPDATE "+table+" SET STATE='R' WHERE ID=?", id); err != nil {
			return err
		}
	}
	var state, tmdb string
	if err = tx.QueryRowContext(ctx, "SELECT STATE,TMDBID FROM "+table+" WHERE ID=?", id).Scan(&state, &tmdb); err != nil || state != want || tmdb != m.ID {
		return systemconfig.ErrHistoryConflict
	}
	return nil
}
