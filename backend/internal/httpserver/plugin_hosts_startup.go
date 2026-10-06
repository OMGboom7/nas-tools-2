package httpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/customhosts"
)

type hostsSettingsReader interface {
	Get(context.Context, string) (string, error)
}

// Restoration deliberately does not rewrite configuration: file application
// and a database commit are not an atomic operation. A failed restore prevents
// startup instead of advertising enabled but unapplied hosts.
func restoreNativeHosts(ctx context.Context, store hostsSettingsReader, path string) (customhosts.Result, error) {
	result := customhosts.Result{}
	if path == "" {
		return result, nil
	}
	if store == nil {
		return result, errors.New("hosts restoration requires native system configuration")
	}
	raw, err := store.Get(ctx, "UserInstalledPlugins")
	if err != nil {
		return result, err
	}
	installed, err := decodeInstalledPlugins(raw)
	if err != nil {
		return result, err
	}
	found := false
	for _, id := range installed {
		found = found || id == "CustomHosts"
	}
	if !found {
		return result, nil
	}
	raw, err = store.Get(ctx, "plugin.CustomHosts")
	if err != nil {
		return result, err
	}
	values, err := decodeMetadataConfig(raw)
	if err != nil {
		return result, err
	}
	enabled := false
	if value, exists := values["enable"]; exists {
		var ok bool
		enabled, ok = value.(bool)
		if !ok {
			return result, customhosts.ErrInput
		}
	}
	if !enabled {
		return result, nil
	}
	input, err := nativeHostsInput(values["hosts"])
	if err != nil {
		return result, err
	}
	result, err = customhosts.Apply(ctx, path, input)
	if err == nil && !result.Applied {
		err = errors.New("enabled hosts configuration contains no valid mappings")
	}
	return result, err
}

// Legacy plugin versions persist either textarea text or an array of lines.
func nativeHostsInput(value any) (string, error) {
	switch lines := value.(type) {
	case nil:
		return "", nil
	case string:
		return lines, nil
	case []any:
		if len(lines) > 4096 {
			return "", customhosts.ErrInput
		}
		var input strings.Builder
		for _, line := range lines {
			text, ok := line.(string)
			if !ok || input.Len()+len(text)+1 > 64<<10 {
				return "", customhosts.ErrInput
			}
			input.WriteString(strings.TrimRight(text, "\r\n"))
			input.WriteByte('\n')
		}
		return input.String(), nil
	default:
		return "", customhosts.ErrInput
	}
}
