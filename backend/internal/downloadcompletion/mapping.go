package downloadcompletion

import (
	"encoding/json"
	"path"
	"strings"
)

type Mapping struct {
	SavePath      string `json:"save_path"`
	ContainerPath string `json:"container_path"`
}
type Location struct {
	Root      string `json:"root"`
	Directory string `json:"directory"`
	Mapped    bool   `json:"mapped"`
}

// Resolve maps directory components once, never arbitrary string replacement.
// The most specific configured prefix wins. Equally specific disagreements
// are errors, not an ordering-dependent choice. Unmapped fallback remains
// confined to this downloader's configured local roots.
func Resolve(raw, directory string, requireMapping bool) (Location, error) {
	directory = strings.TrimRight(directory, "/")
	if !Absolute(directory) || len(raw) > 1<<20 {
		return Location{}, ErrManifest
	}
	var mappings []Mapping
	if json.Unmarshal([]byte(raw), &mappings) != nil || len(mappings) == 0 || len(mappings) > 128 {
		return Location{}, ErrManifest
	}
	for i := range mappings {
		m := &mappings[i]
		m.SavePath = strings.TrimRight(m.SavePath, "/")
		if m.ContainerPath == "" {
			m.ContainerPath = m.SavePath
		} else {
			m.ContainerPath = strings.TrimRight(m.ContainerPath, "/")
		}
		if !Absolute(m.SavePath) || !Absolute(m.ContainerPath) {
			return Location{}, ErrManifest
		}
	}
	choose := func(remote bool) (Location, bool, error) {
		best := -1
		var result Location
		for _, m := range mappings {
			prefix := m.ContainerPath
			if remote {
				prefix = m.SavePath
			}
			if directory != prefix && !strings.HasPrefix(directory, prefix+"/") {
				continue
			}
			local := m.ContainerPath + strings.TrimPrefix(directory, prefix)
			next := Location{Root: m.ContainerPath, Directory: path.Clean(local), Mapped: remote}
			if len(prefix) > best {
				best = len(prefix)
				result = next
			} else if len(prefix) == best && next != result {
				return Location{}, false, ErrManifest
			}
		}
		return result, best >= 0, nil
	}
	if result, found, err := choose(true); err != nil || found {
		return result, err
	}
	if requireMapping {
		return Location{}, ErrManifest
	}
	if result, found, err := choose(false); err != nil || found {
		return result, err
	}
	return Location{}, ErrManifest
}
