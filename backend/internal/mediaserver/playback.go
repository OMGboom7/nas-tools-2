package mediaserver

import (
	"context"
	"net/url"
	"strings"
)

// PlaybackURL constructs a UI link, never a stream with an API key. Callers
// obtain itemID from a current identity lookup, not an unbound display cache.
func (client *Client) PlaybackURL(ctx context.Context, itemID, playHost string) (string, error) {
	if !validInventoryID(itemID) {
		return "", ErrConfiguration
	}
	base := *client.endpoint
	if client.kind == "emby" {
		base.Path = strings.TrimSuffix(base.Path, "/emby/System/Info")
	} else if client.kind == "jellyfin" {
		base.Path = strings.TrimSuffix(base.Path, "/System/Info")
	}
	if playHost == "" {
		playHost = base.Scheme + "://" + base.Host + base.Path
	}
	if !strings.Contains(playHost, "://") {
		playHost = "http://" + playHost
	}
	parsed, err := url.Parse(playHost)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", ErrConfiguration
	}
	root := strings.TrimRight(parsed.String(), "/")
	if client.kind == "plex" {
		contents, err := client.plexGET(ctx, "/", nil)
		if err != nil {
			return "", err
		}
		machine, err := parsePlexMachineID(contents)
		if err != nil {
			return "", err
		}
		entry := root + "/web/index.html"
		if strings.EqualFold(parsed.Hostname(), "app.plex.tv") {
			entry = root + "/desktop/"
		}
		return entry + "#!/server/" + url.PathEscape(machine) + "/details?key=" + url.QueryEscape("/library/metadata/"+itemID), nil
	}
	var info struct {
		ID string `json:"Id"`
	}
	if err := client.readJSON(ctx, client.endpoint.String(), &info); err != nil {
		return "", err
	}
	if info.ID == "" {
		return "", ErrResponse
	}
	entry := "details"
	suffix := ""
	if client.kind == "emby" {
		entry = "item"
		suffix = "&context=home"
	}
	return root + "/web/index.html#!/" + entry + "?id=" + url.QueryEscape(itemID) + suffix + "&serverId=" + url.QueryEscape(info.ID), nil
}
