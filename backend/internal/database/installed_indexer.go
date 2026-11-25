package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"slices"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
)

// The paired scheduler may retain a discovery result across a settings change.
// Resolve membership and credentials together before each external request.
func InstalledIndexerCommand(ctx context.Context, path, id, action string, in io.Reader, out io.Writer) error {
	if id != "Jackett" && id != "Prowlarr" || action != "discover" && action != "search" {
		return externalindexer.ErrConfig
	}
	var query externalindexer.PluginQuery
	input := &io.LimitedReader{R: in, N: 32 << 10}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&query) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
		return externalindexer.ErrConfig
	}
	cfg, err := installedIndexerConfig(ctx, path, id)
	if err != nil {
		return err
	}
	return externalindexer.RunPluginQuery(ctx, action, cfg, query, out)
}

func installedIndexerConfig(ctx context.Context, path, id string) (externalindexer.Config, error) {
	cfg := externalindexer.Config{Kind: id}
	db, closeDB, err := openPluginDatabase(path, true)
	if err != nil {
		return cfg, err
	}
	defer closeDB()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return cfg, err
	}
	defer tx.Rollback()
	ids, err := readPluginInstallation(ctx, tx)
	if err != nil {
		return cfg, err
	}
	if !slices.Contains(ids, id) {
		return cfg, externalindexer.ErrConfig
	}
	_, raw, err := readPluginValue(ctx, tx, "plugin."+id)
	if err != nil {
		return cfg, err
	}
	var stored struct {
		Host, Password string
		APIKey         string `json:"api_key"`
	}
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &stored) != nil {
		return cfg, externalindexer.ErrConfig
	}
	cfg.Host, cfg.Password, cfg.APIKey = stored.Host, stored.Password, stored.APIKey
	return cfg, tx.Commit()
}
