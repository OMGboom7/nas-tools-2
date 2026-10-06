package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

type systemPathAPI struct {
	config      *config.Store
	downloaders *downloaderconfig.Store
	database    string
}

type pathEntry struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Type string `json:"type"`
	Rel  string `json:"rel"`
	Ext  string `json:"ext,omitempty"`
	Size string `json:"size,omitempty"`
}

func (api systemPathAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	if api.config == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native configuration is unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid directory request")
		return
	}
	raw := strings.TrimSpace(request.Form.Get("dir"))
	filter := strings.ToUpper(strings.TrimSpace(request.Form.Get("filter")))
	if filter == "" {
		filter = "ALL"
	}
	if len(raw) > 4096 || len(filter) > 100 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid directory request")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	paths, err := api.paths(ctx, raw)
	if err != nil {
		writeJSON(response, http.StatusOK, map[string]any{"code": -1, "message": "加载路径失败"})
		return
	}
	result := make([]pathEntry, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		entry := pathEntry{Path: filepath.ToSlash(path), Name: filepath.Base(path), Rel: filepath.ToSlash(filepath.Dir(path))}
		if info.IsDir() {
			if !strings.Contains(filter, "ONLYDIR") && !strings.Contains(filter, "ALL") {
				continue
			}
			entry.Type = "dir"
		} else {
			ext := strings.TrimPrefix(filepath.Ext(path), ".")
			if !pathFilterAllowsFile(filter, ext) {
				continue
			}
			entry.Type, entry.Ext, entry.Size = "file", ext, legacyPathFileSize(info.Size())
		}
		result = append(result, entry)
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "count": len(result), "data": result})
}

func (api systemPathAPI) paths(ctx context.Context, raw string) ([]string, error) {
	switch raw {
	case "*SYNC-FOLDERS*":
		return api.syncFolders(ctx)
	case "*DOWNLOAD-FOLDERS*":
		return api.downloadFolders(ctx)
	case "*MEDIA-FOLDERS*":
		return api.mediaFolders()
	}
	if raw == "" {
		raw = "/"
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return nil, err
	}
	path := filepath.Clean(decoded)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		path = filepath.Dir(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, fmt.Errorf("directory has too many entries")
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, filepath.Join(path, entry.Name()))
	}
	return result, nil
}

func (api systemPathAPI) mediaFolders() ([]string, error) {
	snapshot, err := api.config.Snapshot()
	if err != nil {
		return nil, err
	}
	media := objectValue(snapshot["media"])
	paths := []string{}
	for _, key := range []string{"movie_path", "tv_path", "anime_path", "unknown_path"} {
		if values, ok := media[key].([]any); ok {
			for _, value := range values {
				paths = append(paths, text(value))
			}
		} else {
			paths = append(paths, text(media[key]))
		}
	}
	return cleanUniquePaths(paths), nil
}

func (api systemPathAPI) downloadFolders(ctx context.Context) ([]string, error) {
	if api.downloaders == nil {
		return nil, fmt.Errorf("downloaders unavailable")
	}
	items, err := api.downloaders.List(ctx)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, item := range items {
		var rules []struct {
			SavePath      string `json:"save_path"`
			ContainerPath string `json:"container_path"`
		}
		if item.DownloadDir == "" {
			continue
		}
		if json.Unmarshal([]byte(item.DownloadDir), &rules) != nil {
			return nil, fmt.Errorf("invalid downloader directories")
		}
		for _, rule := range rules {
			if rule.SavePath == "" {
				continue
			}
			if rule.ContainerPath != "" {
				paths = append(paths, rule.ContainerPath)
			} else {
				paths = append(paths, rule.SavePath)
			}
		}
	}
	return cleanUniquePaths(paths), nil
}

func (api systemPathAPI) syncFolders(ctx context.Context) ([]string, error) {
	if api.database == "" {
		return nil, fmt.Errorf("database unavailable")
	}
	dsn := (&url.URL{Scheme: "file", Path: api.database, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='CONFIG_SYNC_PATHS')`).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return []string{}, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(SOURCE,''),COALESCE(DEST,'') FROM CONFIG_SYNC_PATHS`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := []string{}
	for rows.Next() {
		var source, dest string
		if err := rows.Scan(&source, &dest); err != nil {
			return nil, err
		}
		paths = append(paths, source, dest)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cleanUniquePaths(paths), nil
}

func cleanUniquePaths(paths []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, raw := range paths {
		path := strings.TrimSpace(raw)
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}

func pathFilterAllowsFile(filter, ext string) bool {
	if strings.Contains(filter, "ONLYFILE") || strings.Contains(filter, "ALL") {
		return true
	}
	ext = strings.ToLower(ext)
	contains := func(list string) bool { return strings.Contains("|"+list+"|", "|"+ext+"|") }
	return strings.Contains(filter, "MEDIAFILE") && contains("mp4|mkv|ts|iso|rmvb|avi|mov|mpeg|mpg|wmv|3gp|asf|m4v|flv|m2ts|strm|tp|f4v") ||
		strings.Contains(filter, "SUBFILE") && contains("srt|ass|ssa") ||
		strings.Contains(filter, "AUDIOTRACKFILE") && contains("mka|flac|ape|wav")
}

func legacyPathFileSize(size int64) string {
	value, unit := float64(size), "B"
	for _, next := range []string{"K", "M", "G", "T"} {
		if value < 1024 {
			break
		}
		value /= 1024
		unit = next
	}
	value = math.Round(value*100) / 100
	text := strconv.FormatFloat(value, 'f', -1, 64)
	if !strings.Contains(text, ".") {
		text += ".0"
	}
	return text + unit
}
