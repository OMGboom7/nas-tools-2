package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

var ErrPluginConfigChanged = errors.New("plugin configuration changed")

// PluginConfig reads or replaces one plugin's own setting on the data host.
// Conditional writes pair the editor's server-side merge with this SQLite transaction.
func PluginConfig(ctx context.Context, configPath string, args []string, in io.Reader, out io.Writer) error {
	if len(args) != 2 || (args[0] != "read" && args[0] != "save") || !systemconfig.ValidStoredPluginID(args[1]) {
		return errors.New("usage: plugin-config read|save ID")
	}
	id := args[1]
	key := "plugin." + id
	read := args[0] == "read"
	db, closeDB, err := openPluginDatabase(configPath, read)
	if err != nil {
		return err
	}
	defer closeDB()
	if read {
		value, raw, err := readPluginValue(ctx, db, key)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"value": value, "revision": pluginConfigRevision(raw)})
	}

	var input struct {
		Value    json.RawMessage
		Revision *string
	}
	decoder := json.NewDecoder(in)
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || len(input.Value) == 0 || input.Value[0] != '{' {
		return errors.New("invalid plugin configuration")
	}
	keys := []string{key}
	if input.Revision != nil {
		if raw, err := hex.DecodeString(*input.Revision); err != nil || len(raw) != sha256.Size {
			return errors.New("invalid expected configuration")
		}
		keys = append(keys, "UserInstalledPlugins")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Old extensions could clear this nullable field. Normalize it only in
	// this replacement transaction; a conflict or failed write rolls it back.
	if _, err := tx.ExecContext(ctx, "UPDATE SYSTEM_DICT SET VALUE='' WHERE TYPE='SystemConfig' AND KEY=? AND VALUE IS NULL", key); err != nil {
		return err
	}
	err = systemconfig.UpdateManyTx(ctx, tx, keys, func(current map[string]string) (map[string]string, error) {
		if input.Revision != nil {
			installed, err := systemconfig.DecodeInstalledPlugins(current["UserInstalledPlugins"])
			if err != nil {
				return nil, err
			}
			if !slices.Contains(installed, id) || pluginConfigRevision(current[key]) != *input.Revision {
				return nil, ErrPluginConfigChanged
			}
		}
		return map[string]string{key: string(input.Value)}, nil
	})
	if err != nil {
		return err
	}
	return tx.Commit()
}

func readPluginValue(ctx context.Context, db pluginQuery, key string) (value any, raw string, err error) {
	var stored sql.NullString
	err = db.QueryRowContext(ctx, "SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1", key).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil || !stored.Valid {
		return nil, "", err
	}
	raw = stored.String
	// The original reader recognized JSON arrays and objects only at the first byte.
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		if !json.Valid([]byte(raw)) {
			return nil, "", errors.New("invalid stored plugin JSON")
		}
		return json.RawMessage(raw), raw, nil
	}
	return raw, raw, nil
}

func pluginConfigRevision(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
