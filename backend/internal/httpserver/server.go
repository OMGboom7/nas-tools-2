package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/auth"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/database"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
	"github.com/0xforee/nas-tools/backend/internal/rssparserconfig"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
	"github.com/0xforee/nas-tools/backend/internal/searchcache"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/wordconfig"
)

const version = "0.1.0"

func New(cfg config.Config) (http.Handler, error) {
	return newHandler(cfg, http.DefaultTransport)
}

func newHandler(cfg config.Config, transport http.RoundTripper) (http.Handler, error) {
	handler, _, err := newRuntimeHandler(nil, cfg, transport)
	return handler, err
}

// NewWithContext starts production workers only in pure-Go mode. The returned
// wait function joins them after ctx cancellation; handler-only New is useful
// for embedders that do not own background execution.
func NewWithContext(ctx context.Context, cfg config.Config) (http.Handler, func(), error) {
	return newRuntimeHandler(ctx, cfg, http.DefaultTransport)
}

func newRuntimeHandler(workerContext context.Context, cfg config.Config, transport http.RoundTripper) (http.Handler, func(), error) {
	var runner *rssRunAPI
	handler, err := buildHandler(cfg, transport, &runner)
	if err != nil {
		return nil, nil, err
	}
	wait := func() {}
	if workerContext != nil && cfg.DisableLegacy && runner.preview.tasks != nil {
		location, err := rssWorkerLocation()
		if err != nil {
			return nil, nil, fmt.Errorf("invalid native RSS worker timezone")
		}
		waitRSS := startRSSWorker(workerContext, runner, location)
		waitRefresh := startSubscriptionRefreshWorker(workerContext, runner.refresh)
		waitSearch := startSubscriptionSearchWorker(workerContext, runner.search)
		wait = func() { waitRSS(); waitRefresh(); waitSearch() }
	}
	return handler, wait, nil
}

func buildHandler(cfg config.Config, transport http.RoundTripper, runnerOut **rssRunAPI) (http.Handler, error) {
	if cfg.HostsPath != "" && (!filepath.IsAbs(cfg.HostsPath) || !cfg.DisableLegacy) {
		return nil, fmt.Errorf("native hosts restoration requires an absolute path and disabled legacy backend")
	}
	if cfg.DisableLegacy {
		cfg.LegacyBackendURL = ""
	}
	var legacyFallback http.Handler = http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		writeAPIError(response, http.StatusNotImplemented, 501, "endpoint has not been migrated to Go")
	})
	if cfg.LegacyBackendURL != "" {
		legacyURL, err := url.Parse(cfg.LegacyBackendURL)
		if err != nil {
			return nil, fmt.Errorf("parse legacy backend URL: %w", err)
		}
		legacyProxy := httputil.NewSingleHostReverseProxy(legacyURL)
		legacyProxy.Transport = transport
		legacyProxy.ErrorHandler = func(response http.ResponseWriter, request *http.Request, proxyErr error) {
			slog.Error("legacy backend request failed", "path", request.URL.Path, "error", proxyErr)
			writeJSON(response, http.StatusBadGateway, map[string]string{"error": "legacy backend is unavailable"})
		}
		legacyFallback = legacyProxy
	}
	images, err := newMediaImageProxy(transport)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", health)
	var nativeAuth *nativeAuthentication
	var nativeConfig *config.Store
	var nativeSystemConfig *systemconfig.Store
	var nativeDownloaders *downloaderconfig.Store
	var nativeSites *siteconfig.Store
	var nativeFilters *filterconfig.Store
	var nativeNotifications *notificationconfig.Store
	var nativeRSSParsers *rssparserconfig.Store
	var nativeRSSTasks *rsstaskconfig.Store
	var nativeWords *wordconfig.Store
	var nativeUserDatabasePath string
	if cfg.ApplicationConfigPath != "" {
		application, err := config.LoadApplication(cfg.ApplicationConfigPath)
		if err != nil {
			return nil, err
		}
		if _, err := database.EnsureMigrationBackup(application.UserDatabasePath()); err != nil {
			return nil, fmt.Errorf("create pre-Go database backup: %w", err)
		}
		nativeUserDatabasePath = application.UserDatabasePath()
		authService, err := auth.NewService(application)
		if err != nil {
			return nil, fmt.Errorf("create native authentication: %w", err)
		}
		authentication := nativeAuthentication{service: authService}
		nativeAuth = &authentication
		nativeSystemConfig, err = systemconfig.Open(application.UserDatabasePath())
		if err != nil {
			return nil, fmt.Errorf("open native system configuration: %w", err)
		}
		nativeConfig = config.NewStore(cfg.ApplicationConfigPath)
		images.config = nativeConfig
		nativeDownloaders, err = downloaderconfig.Open(application.UserDatabasePath())
		if err != nil {
			return nil, fmt.Errorf("open native downloader configuration: %w", err)
		}
		nativeSites, err = siteconfig.Open(application.UserDatabasePath())
		if err != nil {
			return nil, fmt.Errorf("open native site configuration: %w", err)
		}
		nativeFilters, err = filterconfig.OpenWritable(application.UserDatabasePath())
		if err != nil {
			return nil, fmt.Errorf("open native filter configuration: %w", err)
		}
		nativeNotifications, err = notificationconfig.Open(application.UserDatabasePath())
		if err != nil {
			return nil, fmt.Errorf("open native notification configuration: %w", err)
		}
		nativeRSSParsers, err = rssparserconfig.Open(application.UserDatabasePath())
		if err != nil {
			return nil, fmt.Errorf("open native RSS parser configuration: %w", err)
		}
		parserAPI := rssParserAPI{store: nativeRSSParsers}
		for _, path := range []string{"list", "info", "update", "delete"} {
			mux.HandleFunc("POST /api/v1/rss/parser/"+path, parserAPI.serveHTTP)
		}
		nativeRSSTasks, err = rsstaskconfig.Open(application.UserDatabasePath())
		if err != nil {
			return nil, fmt.Errorf("open native RSS task configuration: %w", err)
		}
		taskAPI := rssTaskAPI{tasks: nativeRSSTasks, parsers: nativeRSSParsers, filters: nativeFilters}
		for _, path := range []string{"list", "info", "update", "delete"} {
			mux.HandleFunc("POST /api/v1/rss/"+path, taskAPI.serveHTTP)
		}
		mux.HandleFunc("POST /api/v1/rss/item/history", taskAPI.serveHTTP)
		mux.HandleFunc("POST /api/v1/rss/item/set", taskAPI.serveHTTP)
		configuration := configurationAPI{
			store: nativeConfig, authentication: nativeAuth,
			path: cfg.ApplicationConfigPath, system: nativeSystemConfig,
		}
		downloaderConfiguration := downloaderConfigurationAPI{
			store: nativeDownloaders, system: nativeSystemConfig, authentication: nativeAuth,
			client: &http.Client{Transport: transport}, legacyURL: cfg.LegacyBackendURL,
		}
		filterConfiguration := filterConfigurationAPI{store: nativeFilters, authentication: nativeAuth}
		nativeWords, err = wordconfig.Open(application.UserDatabasePath())
		if err != nil {
			return nil, fmt.Errorf("open native custom words: %w", err)
		}
		wordsAPI := customWordsAPI{store: nativeWords, auth: nativeAuth, config: nativeConfig, transport: transport}
		for _, path := range []string{"list", "item/info", "item/update", "item/delete", "item/status", "group/delete", "group/add", "item/export", "item/analyse", "item/import"} {
			mux.HandleFunc("POST /api/v1/words/"+path, wordsAPI.serveHTTP)
		}
		for _, path := range []string{"list", "group/add", "group/default", "group/delete", "group/restore", "rule/update", "rule/delete", "rule/info", "rule/share", "rule/import"} {
			mux.HandleFunc("POST /api/v1/filterrule/"+path, filterConfiguration.serveHTTP)
		}
		mux.HandleFunc("POST /api/v1/user/login", authentication.login)
		mux.HandleFunc("POST /api/v1/user/info", authentication.userInfo)
		mux.HandleFunc("POST /api/v1/user/list", authentication.userList)
		mux.HandleFunc("POST /api/v1/user/manage", authentication.manageUser)
		mux.HandleFunc("POST /api/v1/user/auth", authentication.authorizeUser)
		mux.HandleFunc("POST /api/v1/system/logout", authentication.logout)
		mux.HandleFunc("POST /api/v1/config/info", configuration.info)
		mux.HandleFunc("POST /api/v1/config/update", configuration.update)
		mux.HandleFunc("POST /api/v1/config/directory", configuration.updateDirectory)
		mux.HandleFunc("POST /api/v1/config/set", configuration.setSystem)
		mux.HandleFunc("POST /api/v1/download/client/list", downloaderConfiguration.list)
		mux.HandleFunc("POST /api/v1/download/client/add", downloaderConfiguration.upsert)
		mux.HandleFunc("POST /api/v1/download/client/delete", downloaderConfiguration.delete)
		mux.HandleFunc("POST /api/v1/download/client/check", downloaderConfiguration.setFlag)
		mux.HandleFunc("POST /api/v1/download/client/test", downloaderConfiguration.testConnection)
		mux.HandleFunc("POST /api/v1/download/config/info", downloaderConfiguration.settings)
		mux.HandleFunc("POST /api/v1/download/config/list", downloaderConfiguration.settings)
		mux.HandleFunc("POST /api/v1/download/config/update", downloaderConfiguration.upsertSetting)
		mux.HandleFunc("POST /api/v1/download/config/delete", downloaderConfiguration.deleteSetting)
		mux.HandleFunc("POST /api/v1/download/config/directory", downloaderConfiguration.directories)
	}
	dashboard := dashboardService{
		client: &http.Client{Transport: transport},
		images: images,
		config: nativeConfig,
		auth:   nativeAuth,
	}
	mux.HandleFunc("GET /api/v1/dashboard", dashboard.serveHTTP)
	if nativeConfig != nil {
		mux.HandleFunc("POST /api/v1/library/space", dashboard.serveCompatSpace)
		mux.HandleFunc("POST /api/v1/library/mediaserver/statistics", dashboard.serveCompatStatistics)
		mux.HandleFunc("POST /api/v1/library/mediaserver/latest", dashboard.serveCompatLatest)
		mux.HandleFunc("POST /api/v1/library/mediaserver/resume", dashboard.serveCompatResume)
	}
	mux.HandleFunc("GET /api/v1/dashboard/image", images.serveHTTP)
	paths := systemPathAPI{config: nativeConfig, downloaders: nativeDownloaders, database: nativeUserDatabasePath}
	mux.HandleFunc("POST /api/v1/system/path", paths.serveHTTP)
	systemTools := systemToolsAPI{config: nativeConfig, transport: transport}
	mux.HandleFunc("POST /api/v1/system/version", systemTools.version)
	mux.HandleFunc("POST /api/v1/service/network/test", systemTools.networkTest)
	mux.HandleFunc("POST /api/v1/service/rule/test", (ruleTestAPI{filters: nativeFilters, words: nativeWords}).serveHTTP)
	mux.HandleFunc("POST /api/v1/media/category/list", (mediaCategoryAPI{config: nativeConfig, configPath: cfg.ApplicationConfigPath, defaultPath: cfg.DefaultCategoryPath}).list)
	mux.HandleFunc("POST /api/v1/rss/preview", (rssPreviewAPI{tasks: nativeRSSTasks, parsers: nativeRSSParsers, config: nativeConfig, transport: transport}).serveHTTP)
	transferHistory := transferHistoryAPI{database: nativeUserDatabasePath}
	mux.HandleFunc("POST /api/v1/organization/history/list", transferHistory.serveHTTP)
	mux.HandleFunc("POST /api/v1/organization/history/statistics", transferHistory.serveHTTP)
	var nativeSearchResources *searchcache.Store
	if nativeConfig != nil {
		nativeSearchResources = searchcache.New()
	}
	search := searchService{
		legacyURL: cfg.LegacyBackendURL,
		client:    &http.Client{Transport: transport},
		images:    images,
	}
	siteLimits := &siteRequestLimiter{}
	if nativeSearchResources != nil {
		search.native = &nativeExternalResourceSearch{system: nativeSystemConfig, auth: nativeAuth, resources: nativeSearchResources, transport: transport, images: images,
			siteLimits: siteLimits,
			sites:      nativeSites, config: nativeConfig, catalogPath: cfg.SiteCatalogPath,
			recognition: &mediaNameAPI{service: subscriptionService{client: &http.Client{Transport: transport}, configStore: nativeConfig, systemConfig: nativeSystemConfig, words: nativeWords}, categories: mediaCategoryAPI{config: nativeConfig, configPath: cfg.ApplicationConfigPath, defaultPath: cfg.DefaultCategoryPath}},
		}
	}
	mux.HandleFunc("POST /api/v1/search/resources", search.serveHTTP)
	downloads := downloadService{
		legacyURL:    cfg.LegacyBackendURL,
		client:       &http.Client{Transport: transport},
		images:       images,
		downloaders:  nativeDownloaders,
		sites:        nativeSites,
		configStore:  nativeConfig,
		systemConfig: nativeSystemConfig,
		resources:    nativeSearchResources,
		auth:         nativeAuth,
		siteLimits:   siteLimits,
	}
	mux.HandleFunc("GET /api/v1/downloads", downloads.serveList)
	mux.HandleFunc("POST /api/v1/downloads/resource", downloads.addResource)
	mux.HandleFunc("POST /api/v1/downloads/magnet", downloads.addMagnet)
	mux.HandleFunc("POST /api/v1/downloads/torrent", downloads.addTorrent)
	mux.HandleFunc("POST /api/v1/downloads/link", downloads.addSiteLink)
	mux.HandleFunc("POST /api/v1/rss/item/download", (rssItemDownloadAPI{tasks: nativeRSSTasks, downloaders: nativeDownloaders, system: nativeSystemConfig, sites: nativeSites, config: nativeConfig, service: downloads}).serveHTTP)
	mux.HandleFunc("POST /api/v1/downloads/{id}/{action}", downloads.control)
	mux.HandleFunc("POST /api/v1/download/now", downloads.serveCompatNow)
	mux.HandleFunc("POST /api/v1/download/history", downloads.serveCompatHistory)
	mux.HandleFunc("POST /api/v1/download/start", func(response http.ResponseWriter, request *http.Request) {
		downloads.serveCompatControl(response, request, "start")
	})
	mux.HandleFunc("POST /api/v1/download/stop", func(response http.ResponseWriter, request *http.Request) {
		downloads.serveCompatControl(response, request, "stop")
	})
	mux.HandleFunc("POST /api/v1/download/remove", func(response http.ResponseWriter, request *http.Request) {
		downloads.serveCompatControl(response, request, "remove")
	})
	subscriptions := subscriptionService{
		client:       &http.Client{Transport: transport},
		images:       images,
		downloaders:  nativeDownloaders,
		sites:        nativeSites,
		filters:      nativeFilters,
		configStore:  nativeConfig,
		catalogPath:  cfg.SiteCatalogPath,
		systemConfig: nativeSystemConfig,
		databasePath: nativeUserDatabasePath,
		words:        nativeWords,
	}
	mux.HandleFunc("GET /api/v1/subscriptions", subscriptions.serveList)
	rssRunner := &rssRunAPI{pureGo: cfg.DisableLegacy, filters: nativeFilters,
		preview:     rssPreviewAPI{tasks: nativeRSSTasks, parsers: nativeRSSParsers, config: nativeConfig, transport: transport},
		download:    rssItemDownloadAPI{tasks: nativeRSSTasks, downloaders: nativeDownloaders, system: nativeSystemConfig, sites: nativeSites, config: nativeConfig, service: downloads},
		recognition: mediaNameAPI{service: subscriptions, categories: mediaCategoryAPI{config: nativeConfig, configPath: cfg.ApplicationConfigPath, defaultPath: cfg.DefaultCategoryPath}},
	}
	mux.HandleFunc("POST /api/v1/rss/run", rssRunner.serveHTTP)
	mux.HandleFunc("POST /api/v1/subscriptions/search/plan", (subscriptionSearchPlanner{runner: rssRunner, search: search.native}).serveHTTP)
	subscriptionRunner := &subscriptionSearchRunner{planner: subscriptionSearchPlanner{runner: rssRunner, search: search.native}}
	rssRunner.search = subscriptionRunner
	subscriptionRunner.feeds = &subscriptionRSSAPI{runner: subscriptionRunner}
	mux.HandleFunc("POST /api/v1/subscriptions/rss/run", subscriptionRunner.feeds.serveHTTP)
	mux.HandleFunc("POST /api/v1/subscriptions/search/run", subscriptionRunner.serveHTTP)
	mux.HandleFunc("POST /api/v1/subscriptions/{type}/{id}/reconcile", subscriptionRunner.serveReconcile)
	rssRunner.refresh = &subscriptionRefreshAPI{service: subscriptions, auth: nativeAuth, pureGo: cfg.DisableLegacy}
	mux.HandleFunc("POST /api/v1/subscriptions/refresh", rssRunner.refresh.serveHTTP)
	mux.HandleFunc("POST /api/v1/rss/name/test", (rssNameAPI{tasks: nativeRSSTasks, filters: nativeFilters, recognition: mediaNameAPI{service: subscriptions, categories: mediaCategoryAPI{config: nativeConfig, configPath: cfg.ApplicationConfigPath, defaultPath: cfg.DefaultCategoryPath}}}).serveHTTP)
	mux.HandleFunc("POST /api/v1/service/name/test", (mediaNameAPI{service: subscriptions, categories: mediaCategoryAPI{config: nativeConfig, configPath: cfg.ApplicationConfigPath, defaultPath: cfg.DefaultCategoryPath}}).serveHTTP)
	mux.HandleFunc("GET /api/v1/service/mediainfo", (mediaNameAPI{service: subscriptions, categories: mediaCategoryAPI{config: nativeConfig, configPath: cfg.ApplicationConfigPath, defaultPath: cfg.DefaultCategoryPath}}).serveAPIKey)
	mux.HandleFunc("/api/v1/service/mediainfo", func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Allow", "GET, HEAD")
		writeAPIError(response, http.StatusMethodNotAllowed, 405, "method not allowed")
	})
	mux.HandleFunc("POST /api/v1/subscribe/movie/list", subscriptions.withNativeStorage(subscriptions.serveCompatMovieList))
	mux.HandleFunc("POST /api/v1/subscribe/tv/list", subscriptions.withNativeStorage(subscriptions.serveCompatTVList))
	mux.HandleFunc("POST /api/v1/subscribe/history", subscriptions.withNativeStorage(subscriptions.serveCompatHistoryList))
	mux.HandleFunc("POST /api/v1/subscribe/redo", subscriptions.withNativeStorage(subscriptions.serveCompatSubscriptionHistoryRedo))
	mux.HandleFunc("POST /api/v1/subscribe/add", subscriptions.withNativeStorage(subscriptions.serveCompatSubscriptionUpsert))
	mux.HandleFunc("POST /api/v1/subscribe/delete", subscriptions.withNativeStorage(subscriptions.serveCompatSubscriptionRemove))
	mux.HandleFunc("POST /api/v1/subscribe/history/delete", subscriptions.withNativeStorage(subscriptions.serveCompatHistoryRemove))
	mux.HandleFunc("POST /api/v1/subscribe/search", subscriptionRunner.serveCompatSearch)
	mux.HandleFunc("POST /api/v1/subscriptions", subscriptions.upsert)
	mux.HandleFunc("POST /api/v1/media/tv/seasons", subscriptions.serveCompatTVSeasons)
	mux.HandleFunc("GET /api/v1/subscriptions/options", subscriptions.serveOptions)
	mux.HandleFunc("POST /api/v1/site/indexers", subscriptions.serveCompatIndexers)
	mux.HandleFunc("POST /api/v1/subscriptions/{type}/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("action") == "refresh" {
			subscriptionRunner.serveHTTP(w, r)
			return
		}
		subscriptions.control(w, r)
	})
	mux.HandleFunc("POST /api/v1/subscriptions/history/{type}/{id}/{action}", subscriptions.controlHistory)
	discovery := discoveryService{client: &http.Client{Transport: transport}, images: images, config: nativeConfig, databasePath: nativeUserDatabasePath}
	mux.HandleFunc("POST /api/v1/discovery", discovery.serveHTTP)
	sites := siteService{client: &http.Client{Transport: transport}, downloaders: nativeDownloaders, store: nativeSites, filters: nativeFilters, configStore: nativeConfig, catalogPath: cfg.SiteCatalogPath, siteLimits: siteLimits}
	mux.HandleFunc("GET /api/v1/sites", sites.serveList)
	mux.HandleFunc("POST /api/v1/sites", sites.create)
	mux.HandleFunc("GET /api/v1/sites/options", sites.serveOptions)
	mux.HandleFunc("GET /api/v1/sites/{id}", sites.serveDetail)
	mux.HandleFunc("PUT /api/v1/sites/{id}", sites.update)
	mux.HandleFunc("DELETE /api/v1/sites/{id}", sites.delete)
	mux.HandleFunc("POST /api/v1/sites/{id}/test", sites.testConnection)
	mux.HandleFunc("POST /api/v1/site/list", sites.serveCompatList)
	mux.HandleFunc("POST /api/v1/site/info", sites.serveCompatInfo)
	mux.HandleFunc("POST /api/v1/site/update", sites.serveCompatUpdate)
	mux.HandleFunc("POST /api/v1/site/delete", sites.serveCompatDelete)
	mux.HandleFunc("POST /api/v1/site/test", sites.serveCompatTest)
	mux.HandleFunc("POST /api/v1/site/cookie/update", sites.serveCompatCookieUpdate)
	mux.HandleFunc("GET /api/v1/site/sites", sites.serveCompatAPISites)
	services := serviceOverviewService{
		client:      &http.Client{Transport: transport},
		configStore: nativeConfig, systemConfig: nativeSystemConfig, downloaders: nativeDownloaders,
		sites: nativeSites, catalogPath: cfg.SiteCatalogPath,
	}
	mux.HandleFunc("GET /api/v1/services", services.serveList)
	mux.HandleFunc("POST /api/v1/services/{kind}/{id}/test", services.test)
	mux.HandleFunc("GET /api/v1/services/downloader/{id}", services.serveDownloaderConfig)
	mux.HandleFunc("GET /api/v1/services/downloader-options", services.serveDownloaderOptions)
	mux.HandleFunc("POST /api/v1/services/downloader", services.createDownloader)
	mux.HandleFunc("PUT /api/v1/services/downloader/{id}", services.updateDownloader)
	mux.HandleFunc("DELETE /api/v1/services/downloader/{id}", services.deleteDownloader)
	mux.HandleFunc("POST /api/v1/services/downloader/{id}/default", services.setDefaultDownloader)
	mux.HandleFunc("GET /api/v1/services/media/{id}", services.serveMediaConfig)
	mux.HandleFunc("PUT /api/v1/services/media/{id}", services.updateMediaConfig)
	notifications := notificationService{client: &http.Client{Transport: transport}, store: nativeNotifications, config: nativeConfig}
	if nativeNotifications != nil {
		mux.HandleFunc("POST /api/v1/message/client/info", notifications.serveLegacyInfo)
		mux.HandleFunc("POST /api/v1/message/client/options", notifications.serveLegacyOptions)
		mux.HandleFunc("POST /api/v1/message/client/update", notifications.serveLegacyUpdate)
		mux.HandleFunc("POST /api/v1/message/client/status", notifications.serveLegacyStatus)
		mux.HandleFunc("POST /api/v1/message/client/delete", notifications.serveLegacyDelete)
		mux.HandleFunc("POST /api/v1/message/client/test", notifications.serveLegacyTest)
		mux.HandleFunc("POST /api/v1/message/custom/send", notifications.serveLegacyCustomMessage)
	}
	mux.HandleFunc("GET /api/v1/notifications", notifications.serveList)
	mux.HandleFunc("GET /api/v1/notifications/options", notifications.serveOptions)
	mux.HandleFunc("POST /api/v1/notifications", notifications.create)
	mux.HandleFunc("GET /api/v1/notifications/{id}", notifications.serveDetail)
	mux.HandleFunc("PUT /api/v1/notifications/{id}", notifications.update)
	mux.HandleFunc("PUT /api/v1/notifications/{id}/status", notifications.updateStatus)
	mux.HandleFunc("POST /api/v1/notifications/{id}/test", notifications.test)
	mux.HandleFunc("POST /api/v1/notifications/custom-message", notifications.sendCustomMessage)
	mux.HandleFunc("DELETE /api/v1/notifications/{id}", notifications.delete)
	plugins := pluginService{legacyURL: cfg.LegacyBackendURL, client: &http.Client{Transport: transport}, system: nativeSystemConfig, auth: nativeAuth}
	if cfg.HostsPath != "" {
		plugins.hosts = &nativeHostsPlugin{path: cfg.HostsPath}
	}
	if nativeSystemConfig != nil {
		mux.HandleFunc("POST /api/v1/plugin/apps", plugins.serveNativeLegacyPluginApps)
		for _, action := range []string{"install", "uninstall", "config", "status"} {
			mux.Handle("POST /api/v1/plugin/"+action, plugins.metadataLegacyRoute(action, legacyFallback))
		}
		mux.Handle("POST /api/v1/plugin/list", plugins.metadataLegacyListRoute(legacyFallback))
	}
	mux.HandleFunc("GET /api/v1/plugins", plugins.serveList)
	mux.HandleFunc("POST /api/v1/plugins/{id}/install", plugins.install)
	mux.HandleFunc("GET /api/v1/plugins/{id}/page", plugins.servePage)
	mux.HandleFunc("DELETE /api/v1/plugins/{id}/page/records", plugins.deletePageRecord)
	mux.HandleFunc("GET /api/v1/plugins/{id}", plugins.serveConfig)
	mux.HandleFunc("PUT /api/v1/plugins/{id}", plugins.updateConfig)
	mux.HandleFunc("DELETE /api/v1/plugins/{id}", plugins.uninstall)
	mux.Handle("/api/", legacyFallback)

	if cfg.FrontendDist != "" {
		staticHandler, err := spaHandler(cfg.FrontendDist)
		if err != nil {
			return nil, err
		}
		mux.Handle("/", staticHandler)
	} else {
		mux.HandleFunc("/", func(response http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodGet {
				response.Header().Set("Allow", http.MethodGet)
				writeJSON(response, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
				return
			}
			writeJSON(response, http.StatusOK, map[string]string{
				"service": "nas-tools-go",
				"version": version,
			})
		})
	}

	var handler http.Handler = mux
	if nativeAuth != nil {
		handler = nativeAuth.protectAPI(handler)
	}
	if cfg.HostsPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result, err := restoreNativeHosts(ctx, nativeSystemConfig, cfg.HostsPath)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("restore native CustomHosts: %w", err)
		}
		if result.Applied {
			plugins.hosts.active = true
			slog.Info("native CustomHosts restored", "invalid_lines", len(result.Invalid), "legacy_block", result.LegacyBlock)
		}
	}
	*runnerOut = rssRunner
	return logging(handler), nil
}

func health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "nas-tools-go",
		"version": version,
	})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		slog.Error("cannot encode response", "error", err)
	}
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(response, request)
		slog.Info("HTTP request", "method", request.Method, "path", request.URL.Path, "duration", time.Since(started))
	})
}

func spaHandler(root string) (http.Handler, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("frontend distribution: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("frontend distribution is not a directory: %s", root)
	}

	fileServer := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requested := filepath.Join(root, filepath.Clean(request.URL.Path))
		if _, err := os.Stat(requested); err == nil {
			fileServer.ServeHTTP(response, request)
			return
		} else if !os.IsNotExist(err) {
			http.Error(response, "cannot read frontend asset", http.StatusInternalServerError)
			return
		}

		index, err := fs.ReadFile(os.DirFS(root), "index.html")
		if err != nil {
			http.Error(response, "frontend is unavailable", http.StatusNotFound)
			return
		}
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = response.Write(index)
	}), nil
}
