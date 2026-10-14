package httpserver

import (
	"path/filepath"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

// Anime is a TV metadata classification, not a separate TMDB namespace.
// A manual ANIME hint must never turn a movie or non-animation TV detail into
// animation. Directory configuration does not change the verified media type.
func animationDetail(detail tmdbMediaDetails) bool {
	for _, genre := range categoryArray(detail.Attributes["genres"]) {
		if number(objectValue(genre)["id"]) == 16 {
			return true
		}
	}
	return false
}

func mediaLibraryPaths(media map[string]any, key string) ([]string, error) {
	value := media[key]
	if value == nil {
		return nil, nil
	}
	values := []any{value}
	switch paths := value.(type) {
	case []any:
		values = paths
	case []string:
		values = make([]any, len(paths))
		for i, path := range paths {
			values[i] = path
		}
	case string:
	default:
		return nil, organization.ErrPath
	}
	if len(values) > 128 {
		return nil, organization.ErrLimit
	}
	result := []string{}
	for _, value := range values {
		path, ok := value.(string)
		if !ok {
			return nil, organization.ErrPath
		}
		if path == "" {
			continue
		}
		if len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) || strings.ContainsAny(path, "\x00\r\n") {
			return nil, organization.ErrPath
		}
		result = append(result, filepath.Clean(path))
	}
	return result, nil
}

func mediaTargetType(media map[string]any, kind string, detail tmdbMediaDetails) (string, error) {
	if kind == "movie" {
		return "MOV", nil
	}
	if animationDetail(detail) {
		paths, err := mediaLibraryPaths(media, "anime_path")
		if err != nil {
			return "", err
		}
		if len(paths) > 0 {
			return "ANIME", nil
		}
	}
	return "TV", nil
}

func (api mediaNameAPI) recognitionLibraryCategory(kind string, detail tmdbMediaDetails, category string) (string, error) {
	if category == "" || kind != "tv" || !animationDetail(detail) {
		return category, nil
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return "", err
	}
	paths, err := mediaLibraryPaths(objectValue(snapshot["media"]), "anime_path")
	if err != nil {
		return "", err
	}
	if len(paths) > 0 {
		return category, nil
	}
	// The old organizer reuses TV's category-enabled flag when anime libraries
	// are absent, while the metadata category remains the verified anime one.
	root, err := api.categories.load()
	if err != nil {
		return "", err
	}
	enabled, err := categoryEnabled(root, "tv")
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", nil
	}
	return category, nil
}
