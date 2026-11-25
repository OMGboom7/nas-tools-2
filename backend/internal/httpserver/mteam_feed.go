package httpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

// This listener belongs to the plugin. Starting the main application alone
// does not reserve its legacy RSS port.
type mteamFeedService struct {
	system    *systemconfig.Store
	sites     *siteconfig.Store
	config    *config.Store
	limits    *siteRequestLimiter
	transport http.RoundTripper
	address   string
	requests  chan struct{}

	mu        sync.Mutex
	lifecycle context.Context
	server    *http.Server
	cancel    context.CancelFunc
	served    chan struct{}
}

func newMTeamFeed(system *systemconfig.Store, sites *siteconfig.Store, cfg *config.Store, limits *siteRequestLimiter, transport http.RoundTripper) *mteamFeedService {
	return &mteamFeedService{system: system, sites: sites, config: cfg, limits: limits, transport: transport, address: "0.0.0.0:8001", requests: make(chan struct{}, 1)}
}

// RunMTeamRSS serves the existing subscription address for a standalone legacy
// plugin manager. Its stdin pipe closes on plugin unload or parent process exit.
func RunMTeamRSS(path string, parent io.Reader, ready io.Writer) error {
	db := (config.Application{Path: path}).UserDatabasePath()
	system, err := systemconfig.Open(db)
	if err != nil {
		return err
	}
	defer system.Close()
	sites, err := siteconfig.Open(db)
	if err != nil {
		return err
	}
	defer sites.Close()
	feed := newMTeamFeed(system, sites, config.NewStore(path), &siteRequestLimiter{}, http.DefaultTransport)
	return feed.serveParent(parent, ready)
}

func (feed *mteamFeedService) serveParent(parent io.Reader, ready io.Writer) error {
	feed.lifecycle = context.Background()
	if err := feed.reload(); err != nil {
		return err
	}
	if feed.server == nil {
		return errMTeamFeedStopped
	}
	defer feed.stopLocked()
	if _, err := io.WriteString(ready, "ready\n"); err != nil {
		return err
	}
	_, err := io.Copy(io.Discard, parent)
	return err
}

func startMTeamFeed(ctx context.Context, feed *mteamFeedService) func() {
	if feed == nil {
		return func() {}
	}
	feed.mu.Lock()
	feed.lifecycle = ctx
	feed.mu.Unlock()
	_ = feed.reload()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				feed.mu.Lock()
				feed.lifecycle = nil
				feed.stopLocked()
				feed.mu.Unlock()
				return
			case <-ticker.C:
				_ = feed.reload()
			}
		}
	}()
	return func() { <-done }
}

func (feed *mteamFeedService) stopLocked() {
	if feed.server == nil {
		return
	}
	feed.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := feed.server.Shutdown(ctx); err != nil {
		_ = feed.server.Close()
	}
	<-feed.served
	feed.server = nil
}

func (feed *mteamFeedService) settings(ctx context.Context) (mteamFeedSettings, error) {
	raw, err := feed.system.ReadMany(ctx, mteamFeedKeys)
	if err != nil {
		return mteamFeedSettings{}, err
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], mteamFeedID)
	if err != nil || !installed {
		return mteamFeedSettings{}, err
	}
	return readMTeamFeed(raw[mteamFeedKey])
}
func (feed *mteamFeedService) reload() error {
	feed.mu.Lock()
	defer feed.mu.Unlock()
	ctx := feed.lifecycle
	if ctx == nil || ctx.Err() != nil {
		return nil
	}
	cfg, err := feed.settings(ctx)
	if err != nil || !cfg.enabled {
		feed.stopLocked()
		return err
	}
	if feed.server != nil {
		select {
		case <-feed.served:
			feed.stopLocked()
		default:
			return nil
		}
	}
	listener, err := net.Listen("tcp4", feed.address)
	if err != nil {
		return err
	}
	run, cancel := context.WithCancel(ctx)
	s := &http.Server{Handler: feed, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second, WriteTimeout: 95 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return run }}
	done := make(chan struct{})
	feed.server, feed.cancel, feed.served = s, cancel, done
	go func() { defer close(done); _ = s.Serve(listener) }()
	return nil
}

type mteamFeedAccount struct {
	id                     int64
	origin, key, ua, proxy string
}

func (feed *mteamFeedService) account(ctx context.Context) (mteamFeedAccount, error) {
	sites, err := feed.sites.List(ctx)
	if err != nil {
		return mteamFeedAccount{}, err
	}
	// The old plugin kept the last matching site in ascending priority order.
	var selected siteconfig.Site
	var origin string
	for _, s := range sites {
		if domain, ok := builtinindexer.MTeamFeedOrigin(s.SignURL); ok {
			selected, origin = s, domain
		}
	}
	if origin == "" || selected.APIKey == "" {
		return mteamFeedAccount{}, builtinindexer.ErrConfig
	}
	cfg, err := feed.config.Snapshot()
	if err != nil {
		return mteamFeedAccount{}, err
	}
	app := objectValue(cfg["app"])
	note, err := decodeMetadataConfig(selected.Note)
	if err != nil && selected.Note != "null" {
		return mteamFeedAccount{}, err
	}
	ua := defaultString(text(note["ua"]), text(app["user_agent"]))
	account := mteamFeedAccount{id: selected.ID, origin: origin, key: selected.APIKey, ua: defaultString(ua, defaultSiteUserAgent)}
	if truthy(note["proxy"]) || note["proxy"] == "Y" {
		account.proxy = strings.TrimSpace(text(objectValue(app["proxies"])["https"]))
		if account.proxy == "" {
			return mteamFeedAccount{}, builtinindexer.ErrConfig
		}
	}
	return account, nil
}

var errMTeamFeedStopped = errors.New("M-Team feed is disabled or settings changed")

func (feed *mteamFeedService) serveFeed(ctx context.Context) ([]byte, int, error) {
	cfg, err := feed.settings(ctx)
	if err != nil {
		return nil, 0, err
	}
	if !cfg.enabled {
		return nil, 0, errMTeamFeedStopped
	}
	account, err := feed.account(ctx)
	if err != nil {
		return nil, 0, err
	}
	transport := feed.transport
	if account.proxy != "" {
		transport, err = siteProxyTransport(transport, map[string]any{"https": account.proxy}, "https")
		if err != nil {
			return nil, 0, err
		}
		defer transport.(*http.Transport).CloseIdleConnections()
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return nil, 0, err
	}
	current := func() error {
		actual, err := feed.settings(ctx)
		if err != nil {
			return err
		}
		if !actual.enabled || actual.minimumGiB != cfg.minimumGiB || !slices.Equal(actual.modes, cfg.modes) {
			return errMTeamFeedStopped
		}
		source, err := feed.account(ctx)
		if err != nil {
			return err
		}
		if source != account {
			return errMTeamFeedStopped
		}
		return ctx.Err()
	}
	var items []builtinindexer.MTeamFeedItem
	failed := 0
	for _, mode := range cfg.modes {
		if err := feed.limits.Wait(ctx, feed.sites, account.id); err != nil {
			return nil, 0, err
		}
		if err := current(); err != nil {
			return nil, 0, err
		}
		rows, err := builtinindexer.FetchMTeamFeedCategory(ctx, account.origin, account.key, account.ua, mode, cfg.minimumGiB<<30, time.Now().In(loc), transport)
		if err != nil {
			failed++
			continue
		}
		items = append(items, rows...)
	}
	if err := current(); err != nil {
		return nil, 0, err
	}
	if failed == len(cfg.modes) {
		return nil, failed, builtinindexer.ErrResponse
	}
	data, err := builtinindexer.EncodeMTeamFeed(items, time.Now().In(loc))
	return data, failed, err
}
func (feed *mteamFeedService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.EscapedPath() != "/rss_pub.xml" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	select {
	case feed.requests <- struct{}{}:
		defer func() { <-feed.requests }()
	case <-ctx.Done():
		http.Error(w, "RSS request expired", http.StatusServiceUnavailable)
		return
	}
	data, failed, err := feed.serveFeed(ctx)
	if err != nil {
		if errors.Is(err, errMTeamFeedStopped) {
			http.Error(w, "RSS settings changed", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "M-Team RSS unavailable", http.StatusBadGateway)
		}
		return
	}
	if failed > 0 {
		w.Header().Set("X-Nastool-Failed-Categories", strconv.Itoa(failed))
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write(data)
}
