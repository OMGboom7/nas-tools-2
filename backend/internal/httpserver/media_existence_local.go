package httpserver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

var localMediaExtensions = map[string]bool{}
var localMissingValue = regexp.MustCompile(`[-_\s.]*\t`)
var localNameWhitespace = regexp.MustCompile(`\s+`)

func init() {
	for _, extension := range strings.Fields(".mp4 .mkv .ts .iso .rmvb .avi .mov .mpeg .mpg .wmv .3gp .asf .m4v .flv .m2ts .strm .tp .f4v") {
		localMediaExtensions[extension] = true
	}
}

func localNamingTemplate(media map[string]any, tv bool) string {
	key, format := "movie_name_format", "{title} ({year})/{title} ({year})-{part} - {videoFormat}"
	if tv {
		key, format = "tv_name_format", "{title} ({year})/Season {season}/{title} - {season_episode}-{part} - 第 {episode} 集"
	}
	if custom := text(media[key]); custom != "" {
		format = custom
	}
	return format
}

func localInventorySettings(media map[string]any, detail tmdbMediaDetails, tv bool) ([]string, string, error) {
	pathKey, format := "movie_path", localNamingTemplate(media, tv)
	if tv {
		pathKey = "tv_path"
		if text(media["anime_path"]) != "" || len(categoryArray(media["anime_path"])) > 0 {
			for _, genre := range categoryArray(detail.Attributes["genres"]) {
				if fmt.Sprint(objectValue(genre)["id"]) == "16" {
					pathKey = "anime_path"
				}
			}
		}
	}
	var roots []string
	if root, ok := media[pathKey].(string); ok && root != "" {
		roots = []string{root}
	} else {
		for _, value := range categoryArray(media[pathKey]) {
			root, ok := value.(string)
			if !ok || root == "" {
				return nil, "", mediaserver.ErrConfiguration
			}
			roots = append(roots, root)
		}
	}
	if len(roots) == 0 {
		return nil, "", errLocalMediaUnavailable
	}
	if len(roots) > 32 {
		return nil, "", mediaserver.ErrConfiguration
	}
	return roots, format, nil
}

func (service subscriptionService) nativeLocalMediaInventory(ctx context.Context, media map[string]any, meta mediameta.Metadata, detail tmdbMediaDetails, identity mediaserver.Identity, category []string) (mediaserver.Inventory, error) {
	_, format, err := localInventorySettings(media, detail, identity.TV)
	if err != nil {
		return mediaserver.Inventory{}, err
	}
	parts := strings.Split(format, "/")
	if len(parts) < 2 || identity.TV && len(parts) < 3 {
		return mediaserver.Inventory{}, errLibraryFormat
	}
	directoryFormat := strings.Join(parts[:len(parts)-1], "/")
	if libraryTemplateUsesField(directoryFormat, "en_title") && detail.EnglishTitle == nil {
		kind := "movie"
		if identity.TV {
			kind = "tv"
		}
		english, err := fetchNativeTMDBDetailsLanguage(ctx, service.configStore, service.client.Transport, kind, identity.TMDBID, "en")
		if err != nil {
			return mediaserver.Inventory{}, err
		}
		title := english.Title
		if identity.TV {
			title = english.Name
		}
		if strings.TrimSpace(title) == "" {
			return mediaserver.Inventory{}, errors.New("TMDB English title is unavailable")
		}
		detail.EnglishTitle = &title
	}
	return localMediaInventory(ctx, media, meta, detail, identity, category)
}

// Do not request metadata for escaped placeholders or filename-only fields.
func libraryTemplateUsesField(format, field string) bool {
	for index := 0; index < len(format); index++ {
		if format[index] != '{' {
			continue
		}
		if index+1 < len(format) && format[index+1] == '{' {
			index++
			continue
		}
		rest := format[index+1:]
		if strings.HasPrefix(rest, field) && len(rest) > len(field) && strings.ContainsRune("!:}", rune(rest[len(field)])) {
			return true
		}
	}
	return false
}

func localMediaInventory(ctx context.Context, media map[string]any, meta mediameta.Metadata, detail tmdbMediaDetails, identity mediaserver.Identity, category []string) (mediaserver.Inventory, error) {
	result := mediaserver.Inventory{Episodes: map[int]map[int]bool{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if strings.TrimSpace(identity.Title) == "" || len(identity.Title) > 4096 {
		return result, mediaserver.ErrConfiguration
	}
	roots, format, err := localInventorySettings(media, detail, identity.TV)
	if err != nil {
		return result, err
	}
	parts := strings.Split(format, "/")
	if len(parts) < 2 || identity.TV && len(parts) < 3 {
		return result, errors.New("unsupported library naming template")
	}
	directoryFormat := strings.Join(parts[:len(parts)-1], "/")
	if identity.TV {
		directoryFormat = strings.Join(parts[:len(parts)-2], "/")
	}
	values := map[string]string{
		"title": localLibraryName(identity.Title, media), "name": localLibraryName(meta.Title, media),
		"year": identity.Year, "tmdbid": identity.TMDBID,
		"original_title": localLibraryName(text(detail.Attributes["original_title"]), media),
		"videoFormat":    meta.Resolution, "releaseGroup": meta.Team, "customization": meta.Customization,
		"effect": meta.Effect, "videoCodec": meta.VideoCodec, "audioCodec": meta.AudioCodec,
	}
	// TV details do not include IMDb identity without an external-IDs request.
	// Missing metadata must not silently change a configured IMDb directory.
	if imdbID, available := detail.Attributes["imdb_id"]; available {
		values["imdbid"] = text(imdbID)
	}
	if detail.EnglishTitle != nil {
		values["en_title"] = localLibraryName(*detail.EnglishTitle, media)
	}
	year, _ := strconv.Atoi(identity.Year)
	decade := year / 10 * 10
	values["decade_short"] = strconv.Itoa(decade) + "s"
	values["decade_long"] = strconv.Itoa(decade) + "-" + strconv.Itoa(decade+9)
	if identity.TV {
		values["original_title"] = localLibraryName(text(detail.Attributes["original_name"]), media)
	}
	directory, err := renderLocalDirectory(directoryFormat, values)
	if err != nil {
		return result, err
	}
	group := ""
	if len(category) > 0 {
		group = category[0]
	}
	if group != "" && (filepath.Base(group) != group || group == "." || group == ".." || strings.ContainsAny(group, "\\\x00")) {
		return result, mediaserver.ErrConfiguration
	}
	seasons := []int{1}
	if identity.TV {
		seasons = nil
		if meta.Episodes.Season != nil {
			end := *meta.Episodes.Season
			if meta.Episodes.EndSeason != nil {
				end = *meta.Episodes.EndSeason
			}
			if *meta.Episodes.Season < 0 || end < *meta.Episodes.Season || end-*meta.Episodes.Season > 1000 {
				return result, mediaserver.ErrConfiguration
			}
			for number := *meta.Episodes.Season; number <= end; number++ {
				seasons = append(seasons, number)
			}
		} else if meta.Episodes.Episode != nil {
			seasons = []int{1}
		} else {
			for _, season := range detail.Seasons {
				if season.Number > 0 {
					seasons = append(seasons, season.Number)
				}
			}
		}
		if len(seasons) > 1000 {
			return result, mediaserver.ErrConfiguration
		}
	}
	visited := 0
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return result, mediaserver.ErrConfiguration
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return result, err
		}
		info, err := os.Stat(root)
		if err != nil {
			return result, err
		}
		if !info.IsDir() {
			return result, mediaserver.ErrConfiguration
		}
		for _, season := range seasons {
			paths := []string{filepath.Join(group, directory)}
			if identity.TV {
				values["season"] = strconv.Itoa(season)
				seasonDirectory, err := renderLocalDirectory(parts[len(parts)-2], values)
				if err != nil {
					return result, err
				}
				paths[0] = filepath.Join(paths[0], seasonDirectory)
			} else {
				paths = append(paths, filepath.Join("精选", directory))
			}
			for _, relative := range paths {
				files, err := scanLocalMedia(ctx, root, relative, &visited)
				if err != nil {
					return result, err
				}
				for _, file := range files {
					if !identity.TV {
						result.ItemIDs = []string{"local"}
						continue
					}
					fileMeta, err := mediameta.Parse(filepath.Base(file), "")
					if err != nil {
						return result, err
					}
					if localLibraryName(fileMeta.Title, media) != localLibraryName(identity.Title, media) || fileMeta.Episodes.Episode == nil {
						continue
					}
					fileSeason := 1
					if fileMeta.Episodes.Season != nil {
						fileSeason = *fileMeta.Episodes.Season
					}
					if fileSeason != season {
						continue
					}
					end := *fileMeta.Episodes.Episode
					if fileMeta.Episodes.EndEpisode != nil {
						end = *fileMeta.Episodes.EndEpisode
					}
					if end > 10000 || end < *fileMeta.Episodes.Episode {
						return result, mediaserver.ErrResponse
					}
					if result.Episodes[season] == nil {
						result.Episodes[season] = map[int]bool{}
					}
					for episode := *fileMeta.Episodes.Episode; episode <= end; episode++ {
						result.Episodes[season][episode] = true
					}
					result.ItemIDs = []string{"local"}
				}
			}
		}
	}
	return result, nil
}

func localLibraryName(value string, media map[string]any) string {
	value = strings.Map(func(char rune) rune {
		if strings.ContainsRune("*?\\/\"<>|,", char) || media["filename_keep_punctuation"] != true && strings.ContainsRune("~，？", char) {
			return -1
		}
		return char
	}, value)
	value = strings.TrimSpace(localNameWhitespace.ReplaceAllString(value, " "))
	if media["filename_prefer_barre"] == true {
		return strings.NewReplacer(":", " - ", "：", " - ").Replace(value)
	}
	return strings.ReplaceAll(value, ":", "：")
}

func renderLocalDirectory(template string, values map[string]string) (string, error) {
	rendered, err := renderLocalTemplate(template, values, 0)
	if err != nil {
		return "", err
	}
	value := localMissingValue.ReplaceAllString(rendered, "")
	if len(value) > 4096 || value == "" || filepath.IsAbs(value) || strings.ContainsAny(value, "\\\x00") {
		return "", mediaserver.ErrConfiguration
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return "", mediaserver.ErrConfiguration
		}
	}
	return value, nil
}

func scanLocalMedia(ctx context.Context, root, relative string, visited *int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() || filepath.IsAbs(relative) {
		return nil, mediaserver.ErrConfiguration
	}
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		if part == "" || part == "." || part == ".." {
			return nil, mediaserver.ErrConfiguration
		}
	}
	path := root
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("library directory is not a regular directory")
		}
	}
	var files []string
	err = filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		(*visited)++
		if *visited > 10000 {
			return errors.New("local inventory scan limit exceeded")
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "@Recycle" || name == "#recycle" || strings.HasPrefix(name, "@eaDir") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return errors.New("symlink library directories are unsupported")
			}
			if !info.Mode().IsRegular() {
				return nil
			}
		} else if !entry.Type().IsRegular() {
			return nil
		}
		if !entry.IsDir() && localMediaExtensions[strings.ToLower(filepath.Ext(name))] {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}
