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
