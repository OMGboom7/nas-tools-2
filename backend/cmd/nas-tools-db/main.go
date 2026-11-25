// Command nas-tools-db is the database initializer bundled with legacy clients.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/database"
	"github.com/0xforee/nas-tools/backend/internal/dbinit"
	"github.com/0xforee/nas-tools/backend/internal/pluginpage"
)

func main() {
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
