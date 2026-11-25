package systemconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"unicode"
)

var PluginIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,79}$`)

var ErrInstalledPluginState = errors.New("installed plugin state is invalid")

const pluginLifecyclePrefix = "GoPluginLifecycle."

func PluginLifecycleKey(id string) string { return pluginLifecyclePrefix + id }

func DecodeInstalledPlugins(raw string) ([]string, error) {
	installed := []string{}
	if raw == "" {
		return installed, nil
	}
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &installed) != nil || len(installed) > 1024 {
		return nil, ErrInstalledPluginState
	}
	for _, id := range installed {
		if !ValidStoredPluginID(id) {
			return nil, ErrInstalledPluginState
		}
	}
	return installed, nil
}

// SetPluginInstalledTx changes just one membership. Acquire SQLite's write
// reservation before reading, including for processes with deferred transactions.
func SetPluginInstalledTx(ctx context.Context, tx *sql.Tx, id string, install bool) (bool, error) {
	if !ValidStoredPluginID(id) {
		return false, ErrInstalledPluginState
	}
	if _, err := tx.ExecContext(ctx, "UPDATE SYSTEM_DICT SET VALUE=VALUE WHERE 0"); err != nil {
		return false, err
	}
	const key = "UserInstalledPlugins"
	already := false
	err := UpdateManyTx(ctx, tx, []string{key}, func(raw map[string]string) (map[string]string, error) {
		installed, err := DecodeInstalledPlugins(raw[key])
		if err != nil {
			return nil, err
		}
		already = slices.Contains(installed, id)
		if already == install {
			return nil, nil
		}
		if install {
			installed = append(installed, id)
		} else {
			installed = slices.DeleteFunc(installed, func(current string) bool { return current == id })
		}
		value, err := json.Marshal(installed)
		if err != nil || len(installed) > 1024 || len(value) > 64<<10 {
			return nil, ErrInstalledPluginState
		}
		return map[string]string{key: string(value)}, nil
	})
	return already, err
}

// ValidStoredPluginID also accepts identifiers from external Python classes.
// HTTP routes for executable plugins retain their own, narrower ID validation.
func ValidStoredPluginID(id string) bool {
	if id == "" || len(id) > 64<<10 {
		return false
	}
	for i, r := range id {
		if r == '_' || unicode.IsLetter(r) || unicode.IsOneOf([]*unicode.RangeTable{unicode.Nl, unicode.Other_ID_Start}, r) {
			continue
		}
		if i > 0 && (unicode.IsDigit(r) || unicode.IsOneOf([]*unicode.RangeTable{unicode.Mn, unicode.Mc, unicode.Pc, unicode.Other_ID_Continue}, r)) {
			continue
		}
		return false
	}
	return true
}

// InstalledPluginConfig reads the current membership and nullable old setting
// in the same SQLite snapshot. It never creates a default configuration row.
func (s *Store) InstalledPluginConfig(ctx context.Context, id string) (bool, string, error) {
	if !ValidStoredPluginID(id) {
		return false, "", ErrInvalidSetting
	}
	tx, err := s.database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()
	settings, err := readMany(ctx, tx, []string{"UserInstalledPlugins"})
	if err != nil {
		return false, "", err
	}
	ids, err := DecodeInstalledPlugins(settings["UserInstalledPlugins"])
	if err != nil || !slices.Contains(ids, id) {
		return false, "", err
	}
	var raw sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1", "plugin."+id).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", err
	}
	return true, raw.String, tx.Commit()
}

// PluginDeclaration contains display metadata from an actual loaded class.
// Runtime state and installation membership are read independently.
type PluginDeclaration struct {
	Source string         `json:"source,omitempty"`
	Digest string         `json:"digest,omitempty"`
	Level  *int           `json:"level"`
	Fields map[string]any `json:"fields"`
}

func (e PluginDeclaration) Visible(level int) bool {
	return e.Level != nil && *e.Level >= 0 && *e.Level <= level
}
