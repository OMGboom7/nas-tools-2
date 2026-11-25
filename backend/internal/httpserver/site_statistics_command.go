package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

type scheduledStatsTarget struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type scheduledStatsResult struct {
	Name     string              `json:"name"`
	Data     map[string]any      `json:"data"`
	Notify   bool                `json:"notify,omitempty"`
	Messages []sitestats.Message `json:"messages,omitempty"`
}

// ScheduledStatisticsCommand serves the original page and scheduler. Callers
// select installed site records; all credentials and destinations come from the database.
func ScheduledStatisticsCommand(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	var targets []scheduledStatsTarget
	input := &io.LimitedReader{R: in, N: 4 << 20}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&targets) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 || len(targets) == 0 {
		return sitestats.ErrState
	}
	seen := map[int64]bool{}
	for _, target := range targets {
		if target.ID <= 0 || seen[target.ID] || target.Name == "" || target.URL == "" {
			return sitestats.ErrState
		}
		seen[target.ID] = true
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	db := (config.Application{Path: path}).UserDatabasePath()
	if info, err := os.Stat(db); err != nil || !info.Mode().IsRegular() {
		return sitestats.ErrState
	}
	if organization.CopySupported {
		locks, err := organization.OpenJobLocks(db)
		if err != nil {
			return err
		}
		guard, err := locks.AcquireJob(ctx, "native-site-statistics")
		if err != nil {
			return err
		}
		defer guard.Close()
	}
	app := config.NewStore(path)
	cfg, err := app.Snapshot()
	if err != nil {
		return err
	}
	sites, err := siteconfig.Open(db)
	if err != nil {
		return err
	}
	defer sites.Close()
	store, err := sitestats.Open(db)
	if err != nil {
		return err
	}
	defer store.Close()
	all, err := sites.List(ctx)
	if err != nil {
		return err
	}
	selected := map[int64]siteconfig.Site{}
	counts := map[string]int{}
	for _, site := range all {
		row, err := nativeSiteRecord(site)
		if err != nil || !truthy(row["statistic_enable"]) {
			continue
		}
		counts[text(row["strict_url"])]++
		if seen[site.ID] {
			selected[site.ID] = site
		}
	}
	engine := browserruntime.New(defaultString(os.Getenv("NASTOOL_BROWSER_BINARY"), "auto"))
	limits := &siteRequestLimiter{}
	fingerprint := statsNetworkConfig(cfg)
	browserAvailable := engine.Available()
	results := make([]scheduledStatsResult, len(targets))
	slots := make(chan struct{}, 10)
	var group sync.WaitGroup
	for i, target := range targets {
		results[i] = scheduledStatsResult{Name: target.Name, Data: map[string]any{"err_msg": "站点配置已变化或统计地址不唯一"}}
		site, ok := selected[target.ID]
		address := safeSiteOrigin(defaultString(site.SignURL, site.RSSURL))
		if !ok || site.Name != target.Name || address != target.URL || counts[address] != 1 {
			continue
		}
		slots <- struct{}{}
		group.Add(1)
		go func() {
			defer group.Done()
			defer func() { <-slots }()
			guard := func(ctx context.Context) error {
				current, err := sites.Get(ctx, site.ID)
				if err != nil || current != site {
					return sitestats.ErrState
				}
				latest, err := app.Snapshot()
				if err != nil || statsNetworkConfig(latest) != fingerprint {
					return sitestats.ErrState
				}
				return ctx.Err()
			}
			before := func(ctx context.Context, r sitestats.Request) error {
				if err := limits.Wait(ctx, sites, site.ID); err != nil {
					return err
				}
				return guard(ctx)
			}
			notify := siteMessageEnabled(site)
			client := sitestats.Client{Transport: http.DefaultTransport, Renderer: engine, Emulate: browserAvailable,
				ReadMessages: notify, Guard: guard, Before: before}
			v, err := collectSiteStatistics(ctx, site, cfg, client)
			if err == nil {
				err = guard(ctx)
			}
			if err == nil {
				err = store.SaveScheduled(ctx, site, v, time.Now())
			}
			if err != nil {
				message := "站点数据未能完整获取或保存"
				if errors.Is(err, sitestats.ErrLogin) {
					message = "未检测到已登录，请更新 Cookie"
				}
				results[i].Data = map[string]any{"err_msg": message}
				return
			}
			results[i] = scheduledStatsResult{Name: site.Name, Notify: notify, Messages: v.Messages,
				Data: map[string]any{"upload": v.Upload, "username": v.Username, "user_level": v.Level, "join_at": v.Joined,
					"last_seen": v.LastSeen, "download": v.Download, "ratio": v.Ratio, "seeding": v.Seeding,
					"seeding_size": v.SeedingSize, "leeching": v.Leeching, "bonus": v.Bonus, "url": v.URL,
					"err_msg": nil, "message_unread": v.Unread}}
		}()
	}
	group.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(results)
}

func statsNetworkConfig(cfg map[string]any) string {
	app := objectValue(cfg["app"])
	return sitestats.Hash([]any{app["proxies"], app["user_agent"]})
}
