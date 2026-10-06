package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

type dashboardService struct {
	client *http.Client
	images *mediaImageProxy
	config *config.Store
	auth   *nativeAuthentication
}

func (service dashboardService) serveHTTP(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeJSON(response, http.StatusUnauthorized, map[string]any{
			"code": 401, "success": false, "message": "missing authorization token",
		})
		return
	}
	if service.config == nil || service.auth == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native media configuration is unavailable")
		return
	}
	claims, err := service.auth.service.VerifyToken(token)
	if err != nil {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	snapshot, err := service.config.Snapshot()
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "media configuration is unavailable")
		return
	}
	mediaKind := strings.ToLower(strings.TrimSpace(text(objectValue(snapshot["media"])["media_server"])))
	if mediaKind == "" {
		mediaKind = "emby"
	}
	if mediaKind != "emby" && mediaKind != "jellyfin" && mediaKind != "plex" {
		writeAPIError(response, http.StatusNotImplemented, 501, "media server type is not migrated")
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()

	data := map[string]any{
		"statistics": map[string]string{},
		"storage":    map[string]any{},
		"resume":     []any{},
		"latest":     []any{},
	}
	warnings := make([]string, 0)
	successful := 0
	{
		storage, err := nativeLibrarySpace(service.config)
		if err != nil {
			warnings = append(warnings, "storage unavailable")
		} else {
			data["storage"] = map[string]any{
				"total": text(storage["TotalSpace"]), "used": text(storage["UsedSapce"]),
				"free": text(storage["FreeSpace"]), "usedPercent": number(storage["UsedPercent"]),
			}
			successful++
		}
	}
	{
		statistics, err := service.nativeMediaStatistics(ctx)
		if err != nil {
			warnings = append(warnings, "statistics unavailable")
		} else {
			data["statistics"] = statistics
			successful++
		}
	}
	{
		items, err := service.nativeLatest(ctx, mediaKind, claims.Username, 18)
		if err != nil {
			warnings = append(warnings, "latest unavailable")
		} else {
			data["latest"] = items
			successful++
		}
	}
	{
		items, err := service.nativeResume(ctx, mediaKind, claims.Username, 12)
		if err != nil {
			warnings = append(warnings, "resume unavailable")
		} else {
			data["resume"] = items
			successful++
		}
	}

	if successful == 0 {
		writeJSON(response, http.StatusBadGateway, map[string]any{
			"code": 502, "success": false, "message": "media dashboard is unavailable",
		})
		return
	}

	data["warnings"] = warnings
	writeJSON(response, http.StatusOK, map[string]any{
		"code": 0, "success": true, "data": data,
	})
}

func (service dashboardService) nativeResume(ctx context.Context, kind, username string, limit int) ([]any, error) {
	snapshot, err := service.config.Snapshot()
	if err != nil {
		return nil, err
	}
	configuration := objectValue(snapshot[kind])
	credential := text(configuration["api_key"])
	if kind == "plex" {
		credential = text(configuration["token"])
	}
	client, err := mediaserver.New(kind, text(configuration["host"]), credential, service.client.Transport)
	if err != nil {
		return nil, err
	}
	items, err := client.Resume(ctx, username, text(configuration["play_host"]), limit)
	if err != nil {
		return nil, err
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		image := ""
		if service.images != nil {
			image = service.images.rewriteProtected(kind, item.Image)
		}
		result = append(result, map[string]any{"id": item.ID, "name": item.Name, "type": item.Type, "image": image, "link": item.Link, "percent": item.Percent})
	}
	return result, nil
}

func (service dashboardService) nativeLatest(ctx context.Context, kind, username string, limit int) ([]any, error) {
	snapshot, err := service.config.Snapshot()
	if err != nil {
		return nil, err
	}
	configuration := objectValue(snapshot[kind])
	credential := text(configuration["api_key"])
	if kind == "plex" {
		credential = text(configuration["token"])
	}
	client, err := mediaserver.New(kind, text(configuration["host"]), credential, service.client.Transport)
	if err != nil {
		return nil, err
	}
	items, err := client.Latest(ctx, username, text(configuration["play_host"]), limit)
	if err != nil {
		return nil, err
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		image := ""
		if service.images != nil {
			image = service.images.rewriteProtected(kind, item.Image)
		}
		result = append(result, map[string]any{"id": item.ID, "name": item.Name, "type": item.Type, "image": image, "link": item.Link})
	}
	return result, nil
}

func (service dashboardService) serveCompatLatest(response http.ResponseWriter, request *http.Request) {
	service.serveCompatMediaList(response, request, false)
}

func (service dashboardService) serveCompatResume(response http.ResponseWriter, request *http.Request) {
	service.serveCompatMediaList(response, request, true)
}

func (service dashboardService) serveCompatMediaList(response http.ResponseWriter, request *http.Request, resume bool) {
	if service.config == nil || service.auth == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native media configuration is unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid media list request")
		return
	}
	limit := 20
	if resume {
		limit = 12
	}
	if raw := strings.TrimSpace(request.Form.Get("num")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid media list limit")
			return
		}
		limit = parsed
	}
	snapshot, err := service.config.Snapshot()
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "media configuration is unavailable")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(text(objectValue(snapshot["media"])["media_server"])))
	if kind == "" {
		kind = "emby"
	}
	claims, err := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	var items []any
	if resume {
		items, err = service.nativeResume(ctx, kind, claims.Username, limit)
	} else {
		items, err = service.nativeLatest(ctx, kind, claims.Username, limit)
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "media list is unavailable")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "list": items})
}

func (service dashboardService) nativeMediaStatistics(ctx context.Context) (map[string]string, error) {
	snapshot, err := service.config.Snapshot()
	if err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(text(objectValue(snapshot["media"])["media_server"])))
	if kind == "" {
		kind = "emby"
	}
	configuration := objectValue(snapshot[kind])
	credential := text(configuration["api_key"])
	if kind == "plex" {
		credential = text(configuration["token"])
	}
	client, err := mediaserver.New(kind, text(configuration["host"]), credential, service.client.Transport)
	if err != nil {
		return nil, err
	}
	counts, err := client.Counts(ctx)
	if err != nil {
		return nil, err
	}
	episodes := ""
	if counts.Episodes != 0 {
		episodes = formatMediaCount(counts.Episodes)
	}
	return map[string]string{
		"movies": formatMediaCount(counts.Movies), "series": formatMediaCount(counts.Series),
		"episodes": episodes, "music": formatMediaCount(counts.Songs), "users": strconv.FormatInt(counts.Users, 10),
	}, nil
}

func (service dashboardService) serveCompatStatistics(response http.ResponseWriter, request *http.Request) {
	if service.config == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native media configuration is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	statistics, err := service.nativeMediaStatistics(ctx)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "media server statistics are unavailable")
		return
	}
	users, _ := strconv.ParseInt(statistics["users"], 10, 64)
	writeJSON(response, http.StatusOK, map[string]any{
		"code": 0, "Movie": statistics["movies"], "Series": statistics["series"],
		"Episodes": statistics["episodes"], "Music": statistics["music"], "User": users,
	})
}

func formatMediaCount(value int64) string {
	digits := strconv.FormatInt(value, 10)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}

func nativeLibrarySpace(store *config.Store) (map[string]any, error) {
	snapshot, err := store.Snapshot()
	if err != nil {
		return nil, err
	}
	media := objectValue(snapshot["media"])
	seen := map[uint64]bool{}
	var total, free float64
	for _, key := range []string{"movie_path", "tv_path", "anime_path"} {
		paths := slice(media[key])
		if _, list := media[key].([]any); !list {
			paths = []any{media[key]}
		}
		for _, raw := range paths {
			path := strings.TrimSpace(text(raw))
			if path == "" {
				continue
			}
			info, err := os.Stat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return nil, fmt.Errorf("unsupported file system for %q", path)
			}
			device := uint64(stat.Dev)
			if seen[device] {
				continue
			}
			var volume syscall.Statfs_t
			if err := syscall.Statfs(path, &volume); err != nil {
				return nil, err
			}
			seen[device] = true
			total += float64(volume.Blocks) * float64(volume.Bsize) / (1 << 30)
			free += float64(volume.Bavail) * float64(volume.Bsize) / (1 << 30)
		}
	}
	if total == 0 {
		return map[string]any{"TotalSpace": 0, "FreeSpace": 0, "UsedSapce": 0, "UsedPercent": 0}, nil
	}
	used := total - free
	return map[string]any{
		"TotalSpace": formatLibrarySpace(total), "FreeSpace": formatLibrarySpace(free),
		"UsedSapce": formatLibrarySpace(used), "UsedPercent": fmt.Sprintf("%.1f", used/total*100),
	}, nil
}

func (service dashboardService) serveCompatSpace(response http.ResponseWriter, request *http.Request) {
	if service.config == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native media configuration is unavailable")
		return
	}
	storage, err := nativeLibrarySpace(service.config)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "media library space is unavailable")
		return
	}
	storage["code"] = 0
	writeJSON(response, http.StatusOK, storage)
}

func formatLibrarySpace(gigabytes float64) string {
	unit := "GB"
	if gigabytes > 1024 {
		gigabytes /= 1024
		unit = "TB"
	}
	return fmt.Sprintf("%.2f %s", gigabytes, unit)
}

func legacyPayload(value map[string]any) map[string]any {
	if payload, ok := value["data"].(map[string]any); ok {
		return payload
	}
	return value
}

func postLegacy(ctx context.Context, client *http.Client, legacyURL, path, token string, form url.Values) (map[string]any, error) {
	if legacyURL == "" {
		return nil, errors.New("legacy backend is disabled")
	}
	endpoint := strings.TrimRight(legacyURL, "/") + path
	body := ""
	if form != nil {
		body = form.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("legacy service returned HTTP %d", response.StatusCode)
	}

	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func text(value any) string {
	switch item := value.(type) {
	case nil:
		return ""
	case string:
		return item
	case json.Number:
		return item.String()
	default:
		return fmt.Sprint(item)
	}
}

func number(value any) float64 {
	switch item := value.(type) {
	case json.Number:
		parsed, _ := item.Float64()
		return parsed
	case float64:
		return item
	case int:
		return float64(item)
	case string:
		parsed, _ := strconv.ParseFloat(item, 64)
		return parsed
	default:
		return 0
	}
}
