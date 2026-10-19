// Package scraper builds intake sidecars. It never owns media transfer or
// overwrites existing library files; publication belongs to the intake journal.
package scraper

import (
	"encoding/json"
	"errors"
)

const ConfigKey = "UserScraperConf"

var ErrConfig = errors.New("invalid scraper configuration")
var ErrMetadata = errors.New("scraper metadata unavailable or inconsistent")

type Flags map[string]bool
type Options struct {
	NFO      map[string]Flags `json:"scraper_nfo"`
	Pictures map[string]Flags `json:"scraper_pic"`
}

var Fields = Options{
	NFO:      map[string]Flags{"movie": {"basic": false, "credits": false, "credits_chinese": false}, "tv": {"basic": false, "credits": false, "credits_chinese": false, "season_basic": false, "episode_basic": false, "episode_credits": false}},
	Pictures: map[string]Flags{"movie": {"poster": false, "backdrop": false, "background": false, "logo": false, "disc": false, "banner": false, "thumb": false}, "tv": {"poster": false, "backdrop": false, "background": false, "logo": false, "clearart": false, "banner": false, "thumb": false, "season_poster": false, "season_banner": false, "season_thumb": false, "episode_thumb": false, "episode_thumb_ffmpeg": false}},
}

// Parse retains known independent switches, including explicitly false values.
// Unknown configuration is never discarded by the caller's merge operation.
func Parse(raw string) (Options, []string, error) {
	o := Options{NFO: map[string]Flags{}, Pictures: map[string]Flags{}}
	if raw == "" {
		return o, nil, nil
	}
	var groups map[string]json.RawMessage
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &groups) != nil || groups == nil {
		return o, nil, ErrConfig
	}
	warnings := []string{}
	for family, contents := range groups {
		var dst, known map[string]Flags
		switch family {
		case "scraper_nfo":
			dst, known = o.NFO, Fields.NFO
		case "scraper_pic":
			dst, known = o.Pictures, Fields.Pictures
		default:
			warnings = append(warnings, "unrecognized stored scraper settings were preserved but not executed")
			continue
		}
		if string(contents) == "null" {
			continue
		}
		var kinds map[string]Flags
		if json.Unmarshal(contents, &kinds) != nil {
			return o, nil, ErrConfig
		}
		for kind, flags := range kinds {
			dst[kind] = Flags{}
			for name, enabled := range flags {
				if _, ok := known[kind][name]; !ok {
					if enabled {
						warnings = append(warnings, "unrecognized enabled scraper option requires review")
					}
					continue
				}
				dst[kind][name] = enabled
			}
		}
	}
	return o, warnings, nil
}

func (o Options) ValidatePatch() error {
	for family, kinds := range map[string]map[string]Flags{"nfo": o.NFO, "pic": o.Pictures} {
		known := Fields.NFO
		if family == "pic" {
			known = Fields.Pictures
		}
		for kind, flags := range kinds {
			if known[kind] == nil {
				return ErrConfig
			}
			for name := range flags {
				if _, ok := known[kind][name]; !ok {
					return ErrConfig
				}
			}
		}
	}
	return nil
}
