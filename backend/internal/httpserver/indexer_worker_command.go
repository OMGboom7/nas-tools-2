package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

// IndexerHostCommand serves one installed extension in the standalone host.
// Closing the parent pipe cancels the running probe as well as the timer.
func IndexerHostCommand(path, id, command string, parent io.Reader, out io.Writer) error {
	if !nativeIndexerPlugin(id) || (command != "indexer-view" && command != "indexer-worker" && command != "indexer-check") {
		return errIndexerPlugin
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	db := (config.Application{Path: path}).UserDatabasePath()
	if _, err := os.Stat(db); err != nil {
		return err
	}
	store, err := systemconfig.Open(db)
	if err != nil {
		return err
	}
	defer store.Close()
	m := nativeIndexerPlugins{system: store, transport: http.DefaultTransport, ownsRuntime: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if command != "indexer-worker" {
		if command == "indexer-check" {
			if err := m.refresh(ctx, id, time.Now(), true); err != nil {
				return err
			}
		}
		values, state, installed, err := m.read(ctx, id)
		if err != nil {
			return err
		}
		if !installed || state.Status != "ready" || state.Items == nil {
			state.Items = []indexerPluginRow{}
		}
		if len(state.Items) > 0 {
			cfg, _, err := indexerConfiguration(id, values)
			if err != nil {
				return err
			}
			for i, item := range state.Items {
				// Older snapshots have only a display host. Keep that value until the next refresh.
				if item.RemoteID != "" {
					endpoint, err := externalindexer.DiscoveryURL(cfg, item.RemoteID)
					if err != nil {
						return err
					}
					state.Items[i].Domain = endpoint
				}
			}
		}
		return json.NewEncoder(out).Encode(state.Items)
	}
	go func() { _, _ = io.Copy(io.Discard, parent); cancel() }()
	if err := m.refresh(ctx, id, time.Now(), true); err != nil && !errors.Is(err, errIndexerScope) {
		return err
	}
	if _, err := io.WriteString(out, "ready\n"); err != nil {
		return err
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := m.refresh(ctx, id, now, false); err != nil && !errors.Is(err, errIndexerScope) && ctx.Err() == nil {
				slog.Warn("indexer list refresh unavailable", "plugin", id)
			}
		}
	}
}
