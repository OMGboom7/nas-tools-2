package database

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

// PluginMarket retains discovery order for legacy plugins implementing indexers.
func PluginMarket(ctx context.Context, configPath, access string, in io.Reader, out io.Writer) error {
	level, err := strconv.Atoi(access)
	if err != nil || level < 0 {
		return errors.New("invalid market access level")
	}
	input := &io.LimitedReader{R: in, N: 4 << 20}
	decoder := json.NewDecoder(input)
	decoder.UseNumber()
	var entries []systemconfig.PluginDeclaration
	if decoder.Decode(&entries) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 || entries == nil {
		return errors.New("invalid plugin declarations")
	}
	db, closeDB, err := openPluginDatabase(configPath, true)
	if err != nil {
		return err
	}
	defer closeDB()
	ids, err := readPluginInstallation(ctx, db)
	if err != nil {
		return err
	}
	installed := map[string]bool{}
	for _, id := range ids {
		installed[id] = true
	}
	rows := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if !entry.Visible(level) {
			continue
		}
		id, ok := entry.Fields["id"].(string)
		if !ok || !systemconfig.ValidStoredPluginID(id) {
			return errors.New("invalid market plugin id")
		}
		row := map[string]any{"id": id, "installed": installed[id]}
		for _, key := range []string{"name", "desc", "version", "icon", "color", "author", "author_url"} {
			if v, exists := entry.Fields[key]; exists {
				row[key] = v
			}
		}
		rows = append(rows, row)
	}
	return json.NewEncoder(out).Encode(rows)
}
