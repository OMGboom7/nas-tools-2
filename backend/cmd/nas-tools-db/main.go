// Command nas-tools-db is the database initializer bundled with legacy clients.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/0xforee/nas-tools/backend/internal/batchdownload"
	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/database"
	"github.com/0xforee/nas-tools/backend/internal/dbinit"
	"github.com/0xforee/nas-tools/backend/internal/downloadhistory"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/httpserver"
	"github.com/0xforee/nas-tools/backend/internal/notificationtemplate"
	"github.com/0xforee/nas-tools/backend/internal/pluginpage"
	"github.com/0xforee/nas-tools/backend/internal/recognitioncache"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
	"github.com/0xforee/nas-tools/backend/internal/searchresults"
	"github.com/0xforee/nas-tools/backend/internal/sitestats"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "batch-intersection" {
		if batchdownload.Intersect(os.Stdin, os.Stdout) != nil {
			slog.Error("invalid missing episodes")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "batch-download" {
		if batchdownload.Command(os.Stdin, os.Stdout) != nil {
			slog.Error("invalid batch download")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "recognition-cache" {
		if err := recognitioncache.Command(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("recognition cache unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "rss-scan" {
		if err := httpserver.ClassicRSSScanCommand(context.Background(), config.ApplicationPath(), os.Stdin, os.Stdout); err != nil {
			slog.Error("RSS scan unavailable")
			os.Exit(1)
		}
		return
	}

	if len(os.Args) == 2 && os.Args[1] == "site-feed" {
		if err := httpserver.SiteFeedCommand(context.Background(), config.ApplicationPath(), os.Stdin, os.Stdout); err != nil {
			slog.Error("site RSS unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "rss-history" {
		if err := rsstaskconfig.Command(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("RSS history unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "subscription-progress" {
		if err := httpserver.ClassicSubscriptionCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("subscription progress unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "classic-download" {
		if err := httpserver.ClassicDownloadCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("classic download unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "download-history" {
		if err := downloadhistory.Command(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("download history unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "torrent-metadata" {
		if err := torrenttransfer.MetadataCommand(context.Background(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("torrent metadata unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "search-results" {
		if err := searchresults.Command(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("search results unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "filter-resources" {
		if err := filterconfig.ServeResources(context.Background(), config.ApplicationPath(), os.Stdin, os.Stdout); err != nil {
			slog.Error("resource filter unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "library-inventory" {
		if err := httpserver.LibraryInventoryCommand(context.Background(), config.ApplicationPath(), os.Stdin, os.Stdout); err != nil {
			slog.Error("library inventory unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "tmdb-detail" {
		if err := httpserver.TMDBDetailCommand(context.Background(), config.ApplicationPath(), os.Stdin, os.Stdout); err != nil {
			slog.Error("media details unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "save-subscription" {
		if err := httpserver.ResolvedSubscriptionCommand(context.Background(), config.ApplicationPath(), os.Stdin, os.Stdout); err != nil {
			slog.Error("subscription write unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "search-titles" {
		if err := httpserver.SearchTitlesCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("media title search unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "render-notification" {
		if err := notificationtemplate.Command(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "telegram-receiver" {
		if err := httpserver.TelegramReceiverCommand(context.Background(), config.ApplicationPath(), os.Stdin, os.Stdout); err != nil {
			slog.Error("Telegram receiver unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "telegram-incoming" {
		if err := httpserver.TelegramIncomingCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Args[3], os.Stdin, os.Stdout); err != nil {
			slog.Error("Telegram callback unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "send-telegram" {
		if err := httpserver.SendTelegramCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin); err != nil {
			slog.Error("Telegram notification failed")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "send-webhook" {
		if err := httpserver.SendWebhookCommand(context.Background(), os.Args[2], os.Stdin); err != nil {
			slog.Error("webhook notification failed")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "message-center" {
		if err := httpserver.MessageCenterCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("system message center unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "site-admission" {
		if err := httpserver.SiteAdmissionCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdout); err != nil {
			slog.Error("site admission unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "site-statistics-scheduler" {
		if err := httpserver.StatisticsSchedulerCommand(context.Background(), config.ApplicationPath(), os.Stdin, os.Stdout); err != nil {
			slog.Error("site statistics scheduler unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "site-statistics-run" {
		if err := httpserver.StatisticsRunCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("site statistics refresh or notification unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "site-statistics-read" {
		if err := httpserver.StatisticsReadCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Args[3], os.Stdin, os.Stdout); err != nil {
			slog.Error("site statistics query unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "site-statistics" {
		if err := httpserver.ScheduledStatisticsCommand(context.Background(), config.ApplicationPath(), os.Stdin, os.Stdout); err != nil {
			slog.Error("scheduled site statistics unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "render-catalog" {
		if err := httpserver.RenderCatalogCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("browser catalog search unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "catalog-resources" {
		if err := builtinindexer.CatalogPageCommand(context.Background(), os.Stdin, os.Stdout); err != nil {
			slog.Error("catalog resource page unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "mteam-rss-server" {
		if err := httpserver.RunMTeamRSS(config.ApplicationPath(), os.Stdin, os.Stdout); err != nil {
			slog.Error("M-Team RSS service could not run", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "mteam-statistics" {
		if err := sitestats.MTeamStatisticsCommand(context.Background(), os.Stdin, os.Stdout); err != nil {
			slog.Error("M-Team account statistics unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && (os.Args[1] == "indexer-worker" || os.Args[1] == "indexer-view" || os.Args[1] == "indexer-check") {
		if err := httpserver.IndexerHostCommand(config.ApplicationPath(), os.Args[2], os.Args[1], os.Stdin, os.Stdout); err != nil {
			slog.Error("indexer plugin runtime unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "installed-indexer" {
		if err := database.InstalledIndexerCommand(context.Background(), config.ApplicationPath(), os.Args[2], os.Args[3], os.Stdin, os.Stdout); err != nil {
			slog.Error("installed indexer unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "external-indexer" {
		if err := externalindexer.PluginCommand(context.Background(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("external indexer request failed")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "plugin-editors" {
		if err := database.PluginEditors(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			if errors.Is(err, database.ErrPluginConfigChanged) {
				os.Exit(3)
			}
			slog.Error("plugin editor data unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "plugin-market" {
		if err := database.PluginMarket(context.Background(), config.ApplicationPath(), os.Args[2], os.Stdin, os.Stdout); err != nil {
			slog.Error("plugin market unavailable")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && (os.Args[1] == "mteam-profile" || os.Args[1] == "mteam-detail") {
		if err := builtinindexer.MTeamInspectCommand(context.Background(), os.Args[1], os.Stdin, os.Stdout); err != nil {
			slog.Error("M-Team site request failed")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "mteam-download" {
		if err := builtinindexer.MTeamDownloadCommand(context.Background(), os.Stdin, os.Stdout); err != nil {
			slog.Error("M-Team torrent download failed")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && (os.Args[1] == "torrentleech-resources" || os.Args[1] == "tnode-resources" || os.Args[1] == "mteam-resources") {
		if err := builtinindexer.ResourcePageCommand(context.Background(), os.Args[1], os.Stdin, os.Stdout); err != nil {
			slog.Error("site resource request failed")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "plugin-config" {
		if err := database.PluginConfig(context.Background(), config.ApplicationPath(), os.Args[2:], os.Stdin, os.Stdout); err != nil {
			if errors.Is(err, database.ErrPluginConfigChanged) {
				os.Exit(3)
			}
			slog.Error("plugin-config", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "plugin-installation" {
		if err := database.PluginInstallation(context.Background(), config.ApplicationPath(), os.Args[2:], os.Stdout); err != nil {
			slog.Error("plugin-installation", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "plugin-page" {
		if err := pluginpage.Convert(os.Stdin, os.Stdout); err != nil {
			slog.Error("plugin-page", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 2 || os.Args[1] != "init-db" {
		slog.Error("usage: nas-tools-db init-db")
		os.Exit(2)
	}
	if err := dbinit.Run(config.ApplicationPath(), "init-db"); err != nil {
		slog.Error("init-db", "error", err)
		os.Exit(1)
	}
}
