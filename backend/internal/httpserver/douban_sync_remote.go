package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

// Bind the original interactive transports, their callback endpoints and their
// sender restrictions to the administrator's saved remote command settings.
func (api *doubanSyncAPI) commandSources(ctx context.Context) ([]siteSigninRemoteSource, string, error) {
	receiver := api.plugins.siteSignin
	if receiver == nil || receiver.store == nil {
		return nil, "", sitesignin.ErrScope
	}
	sources, channels, err := receiver.remoteSources(ctx)
	if err != nil || len(sources) == 0 {
		return nil, "", sitesignin.ErrScope
	}
	cfg, err := api.login.config.Snapshot()
	if err != nil {
		return nil, "", err
	}
	security := objectValue(cfg["security"])
	digest := sitelogin.Hash([]any{channels, objectValue(cfg["app"])["domain"], objectValue(cfg["laboratory"])["telegram_domain"], security["telegram_webhook_allow_ip"], security["synology_webhook_allow_ip"]})
	return sources, digest, nil
}

func (c doubanControl) remoteID() string {
	return sitelogin.Hash([]any{sitesignin.DoubanReceiver, c.Scope, c.RemoteChannels, c.RemoteSince})
}

func (api *doubanSyncAPI) remoteScope(ctx context.Context) (sitesignin.Control, []siteSigninRemoteSource, error) {
	var none sitesignin.Control
	raw, err := api.plugins.system.ReadMany(ctx, doubanKeys)
	if err != nil {
		return none, nil, err
	}
	c, err := doubanState(raw[doubanControlKey])
	installed, installErr := indexerInstalled(raw["UserInstalledPlugins"], douban.SyncPlugin)
	if err != nil || installErr != nil || !installed || c.RemoteSince.IsZero() {
		return none, nil, sitesignin.ErrScope
	}
	scope, err := api.scope(ctx, c.Actor, raw)
	if err != nil || c.Scope != scope {
		return none, nil, sitesignin.ErrScope
	}
	sources, channels, err := api.commandSources(ctx)
	if err != nil || channels != c.RemoteChannels {
		return none, nil, sitesignin.ErrScope
	}
	return sitesignin.Control{Enabled: true, Scope: sitesignin.Scope{ID: c.remoteID(), Actor: c.Actor, ConfigDigest: scope, CommandDigest: channels, PluginDigest: sitesignin.DoubanReceiver, Created: c.RemoteSince.Format(time.RFC3339Nano), Options: sitesignin.Options{Remote: true}}}, sources, nil
}

func (api *doubanSyncAPI) queueCommand(ctx context.Context, source siteSigninRemoteSource, id, user, username string, at time.Time) error {
	receiver, sources, err := api.remoteScope(ctx)
	if err != nil {
		return err
	}
	allowed := false
	for _, current := range sources {
		if current.digest == source.digest && current.allowed(user, "/db") {
			allowed = true
			break
		}
	}
	since, err := time.Parse(time.RFC3339Nano, receiver.Scope.Created)
	if !allowed || err != nil || at.Before(since.Truncate(time.Second)) {
		return sitesignin.ErrScope
	}
	// Provider delivery IDs belong to the original channel. Reauthorizing the
	// plugin or rotating its credentials cannot make an old delivery new.
	key := sitelogin.Hash([]any{source.channel.Type, source.channel.ID, id})
	return api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error {
		if err := systemconfig.UpdateManyTx(ctx, tx, doubanKeys, func(raw map[string]string) (map[string]string, error) {
			c, err := doubanState(raw[doubanControlKey])
			installed, installErr := indexerInstalled(raw["UserInstalledPlugins"], douban.SyncPlugin)
			if err != nil || installErr != nil || !installed || c.remoteID() != receiver.Scope.ID {
				return nil, sitesignin.ErrScope
			}
			return nil, nil
		}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_DOUBAN_COMMANDS(ID TEXT PRIMARY KEY, SCOPE TEXT NOT NULL, PENDING INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS GO_DOUBAN_COMMANDS_PENDING ON GO_DOUBAN_COMMANDS(SCOPE,PENDING)`); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "INSERT INTO GO_DOUBAN_COMMANDS(ID,SCOPE,PENDING) VALUES(?,?,1) ON CONFLICT(ID) DO NOTHING", key, receiver.Scope.ID)
		if err != nil {
			return err
		}
		var found bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM GO_DOUBAN_COMMANDS WHERE ID=?)", key).Scan(&found); err != nil {
			return err
		}
		if !found {
			return sitesignin.ErrState
		}
		n, err := result.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		if err = bus.PublishSignal(ctx, tx, "message.incoming", key, source.digest, remoteMessageData(source.channel.Type, user, username, "/db")); err != nil {
			return err
		}
		if err = bus.PublishSignal(ctx, tx, "douban.sync", key, source.digest, map[string]any{}); err != nil {
			return err
		}
		return bus.EmitInteractiveReply(ctx, tx, "douban_sync_ack", key, key, source.digest, user, source.channel.ID)
	})
}

func (api *doubanSyncAPI) pendingCommand(ctx context.Context, c doubanControl) (string, error) {
	var id string
	err := api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_DOUBAN_COMMANDS')").Scan(&exists); err != nil || !exists {
			return err
		}
		err := tx.QueryRowContext(ctx, "SELECT ID FROM GO_DOUBAN_COMMANDS WHERE SCOPE=? AND PENDING=1 ORDER BY rowid LIMIT 1", c.remoteID()).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return id, err
}
