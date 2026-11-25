// Command nas-tools-db is the database initializer bundled with legacy clients.
package main

import (
	"log/slog"
	"os"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/dbinit"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "init-db" {
		slog.Error("usage: nas-tools-db init-db")
		os.Exit(2)
	}
	if err := dbinit.Run(config.ApplicationPath(), "init-db"); err != nil {
		slog.Error("init-db", "error", err)
		os.Exit(1)
	}
}
