package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/cloudflare"
	"github.com/0xforee/nas-tools/backend/internal/customhosts"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type cloudflareAPI struct {
	store     *cloudflare.Store
	organizer *organizationAPI
	hosts     *nativeHostsPlugin
	root      string
	transport http.RoundTripper
	mu        sync.Mutex
	cancel    context.CancelFunc
}

const cloudflareGuard = "native-cloudflare-speedtest"

func cfHash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func (api *cloudflareAPI) authority(actor string) (string, error) {
	base, err := (&eventWebhookAPI{organizer: api.organizer}).authority(actor)
	if err != nil {
		return "", err
	}
	cfg, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		return "", err
	}
	return cfHash([]byte(base + "/" + cloudflareProxyHash(objectValue(cfg["app"])["proxies"]))), nil
}
func cloudflareProxyHash(v any) string { raw, _ := json.Marshal(v); return cfHash(raw) }
func (api *cloudflareAPI) validate(ctx context.Context, c cloudflare.Control) error {
	if api.hosts == nil || c.Root != api.root || c.HostsPath != api.hosts.path {
		return cloudflare.ErrState
	}
	authority, err := api.authority(c.Actor)
	if err != nil || authority != c.Authority {
		return cloudflare.ErrState
	}
	return api.store.Valid(ctx, c)
}
func cfReadFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, cloudflare.ErrState
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 1<<20 {
		return nil, cloudflare.ErrState
	}
	return data, nil
}
func (api *cloudflareAPI) cancelRun() {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.cancel != nil {
		api.cancel()
	}
}
func (api *cloudflareAPI) reconcile(ctx context.Context, id string) error {
	j, err := api.store.Get(ctx, id)
	if err != nil || j.Plan == nil {
		return cloudflare.ErrState
	}
	c, err := api.store.Control(ctx)
	if err != nil || c.Epoch != j.Epoch || api.hosts == nil || c.HostsPath != api.hosts.path {
		return cloudflare.ErrState
	}
	data, err := cfReadFile(c.HostsPath)
	if err != nil {
		return err
	}
	return api.store.Finalize(ctx, id, cfHash(data))
}
func (api *cloudflareAPI) execute(parent context.Context, c cloudflare.Control, j cloudflare.Job) {
	ctx, cancel := context.WithCancel(parent)
	api.mu.Lock()
	api.cancel = cancel
	api.mu.Unlock()
	defer func() { cancel(); api.mu.Lock(); api.cancel = nil; api.mu.Unlock() }()
	fail := func(reason string) {
		book, done := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer done()
		row, err := api.store.Get(book, j.ID)
		if err != nil {
			return
		}
		state := "failed"
		for _, op := range row.Operations {
			if op.Kind == "execute" || op.Kind == "hosts" {
				state = "unknown"
			}
		}
		_ = api.store.Terminal(book, j.ID, state, reason)
	}
	settings, err := cloudflare.DecodeSettings(j.Source.Config)
	if err != nil {
		fail("原配置不可用")
		return
	}
	before := func(ctx context.Context, op cloudflare.Operation) error {
		if err := api.validate(ctx, c); err != nil {
			return err
		}
		return api.store.Before(ctx, j.ID, op)
	}
	cfg, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		fail("原代理配置不可用")
		return
	}
	transport := api.transport
	proxies := objectValue(objectValue(cfg["app"])["proxies"])
	if strings.TrimSpace(text(proxies["https"])) != "" {
		transport, err = siteProxyTransport(transport, proxies, "https")
		if err != nil {
			fail("原代理配置无效")
			return
		}
	}
	installation, err := (cloudflare.Installer{Root: c.Root, Client: &http.Client{Transport: transport}}).Ensure(ctx, settings, before)
	if err != nil {
		fail("原生测速程序安装或核对未完成；未修改 Hosts")
		return
	}
	result, err := cloudflare.Run(ctx, installation, settings, before)
	if err != nil {
		fail("本次测速未确认；不自动重发")
		return
	}
	plan, err := cloudflare.PlanHosts(j.Source.Hosts, settings, result.BestIP)
	if err != nil {
		fail("原 Hosts 目标不能核对；未写入新地址")
		return
	}
	api.hosts.mu.Lock()
	defer api.hosts.mu.Unlock()
	if err = api.validate(ctx, c); err != nil {
		fail("原执行范围已变化；未写入 Hosts")
		return
	}
	original, err := cfReadFile(c.HostsPath)
	if err != nil {
		fail("原 Hosts 文件不可用")
		return
	}
	mappings, _, err := customhosts.Parse(ctx, plan.Input)
	if err != nil || len(mappings) == 0 {
		fail("原 Hosts 映射无效")
		return
	}
	after, _, err := customhosts.Rewrite(string(original), mappings)
	if err != nil {
		fail("无法建立完整 Hosts 变更")
		return
	}
	if err = api.store.PrepareApply(ctx, j.ID, cloudflare.ApplyPlan{Hosts: plan, FileBefore: string(original), FileBeforeHash: cfHash(original), FileAfter: after}, result); err != nil {
		fail("Hosts 写入意图未提交")
		return
	}
	if err = api.validate(ctx, c); err != nil {
		fail("原执行范围已停止；Hosts 意图保留待核对")
		return
	}
	if _, err = customhosts.ApplyExact(ctx, c.HostsPath, plan.Input, string(original)); err != nil {
		fail("Hosts 写入未确认，保留原回执只读核对")
		return
	}
	book, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	if err = api.reconcile(book, j.ID); err != nil {
		fail("文件结果待核对，未重复写入")
		return
	}
	api.hosts.active = true
}
func (api *cloudflareAPI) tick(ctx context.Context) error {
	if api == nil || api.store == nil || api.organizer == nil || !api.organizer.pureGo || api.hosts == nil {
		return nil
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, cloudflareGuard)
	if err != nil {
		return err
	}
	defer guard.Close()
	if err = api.store.Recover(ctx); err != nil {
		return err
	}
	c, err := api.store.Control(ctx)
	if err != nil {
		return err
	}
	jobs, err := api.store.Jobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.State == "unknown" && j.Plan != nil {
			_ = api.reconcile(ctx, j.ID)
		}
	}
	c, err = api.store.Control(ctx)
	if err != nil {
		return err
	}
	// Existing work owns the slot before a newly due timer. Revoked queued
	// work must release its slot even while the old control is disabled.
	for _, j := range jobs {
		if j.State != "queued" {
			continue
		}
		if api.validate(ctx, c) != nil || j.Epoch != c.Epoch || j.Source != c.Source {
			if err = api.store.Terminal(ctx, j.ID, "blocked", "原接管已失效"); err != nil {
				return err
			}
			continue
		}
		api.execute(ctx, c, j)
		return nil
	}
	if api.validate(ctx, c) != nil {
		return nil
	}
	now := time.Now()
	next, nextOnce := c.NextRun, c.NextOnce
	for _, raw := range []string{c.NextOnce, c.NextRun} {
		if raw == "" {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, raw)
		if e != nil {
			return e
		}
		if at.After(now) {
			continue
		}
		if _, e = api.store.Queue(ctx, "timer:"+c.Epoch+":"+raw, false); e != nil {
			return e
		}
		if raw == c.NextOnce {
			nextOnce = ""
		}
		if raw == c.NextRun {
			s, e := cloudflare.DecodeSettings(c.Source.Config)
			if e != nil {
				return e
			}
			loc, e := time.LoadLocation(c.Location)
			if e != nil {
				return e
			}
			schedule, e := parseRSSSchedule(s.Cron, loc)
			if e != nil {
				return e
			}
			next = schedule.next(now).UTC().Format(time.RFC3339Nano)
		}
		// Consume one occurrence; the next poll can enqueue an independently due one.
		if e = api.store.Advance(ctx, c.Epoch, next, nextOnce); e != nil {
			return e
		}
		break
	}
	jobs, err = api.store.Jobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.State != "queued" {
			continue
		}
		if j.Epoch != c.Epoch || j.Source != c.Source {
			_ = api.store.Terminal(ctx, j.ID, "blocked", "原接管已失效")
			continue
		}
		api.execute(ctx, c, j)
		break
	}
	return nil
}
func startCloudflareWorker(ctx context.Context, api *cloudflareAPI) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if api == nil {
			return
		}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		_ = api.tick(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = api.tick(ctx)
			}
		}
	}()
	return func() { <-done }
}
func cfRoot(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "plugins", "CloudflareSpeedTest")
}

// Startup must preserve an in-flight/unknown after-image before the generic
// Hosts restore has a chance to reapply the older database mapping.
func (api *cloudflareAPI) restoreHosts(ctx context.Context, store hostsSettingsReader) (customhosts.Result, error) {
	guard, err := api.organizer.jobs.AcquireJob(ctx, cloudflareGuard)
	if errors.Is(err, organization.ErrBusy) {
		return customhosts.Result{}, nil
	}
	if err != nil {
		return customhosts.Result{}, err
	}
	defer guard.Close()
	api.hosts.mu.Lock()
	defer api.hosts.mu.Unlock()
	jobs, err := api.store.Jobs(ctx)
	if err != nil {
		return customhosts.Result{}, err
	}
	for _, j := range jobs {
		if j.Plan != nil && j.HostsPath == api.hosts.path && (j.State == "applying" || j.State == "unknown") {
			if err := api.reconcile(ctx, j.ID); err != nil {
				return customhosts.Result{}, nil
			}
		}
	}
	return restoreNativeHosts(ctx, store, api.hosts.path)
}
