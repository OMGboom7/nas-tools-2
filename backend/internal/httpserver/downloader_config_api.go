package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type downloaderConfigurationAPI struct {
	store          *downloaderconfig.Store
	system         *systemconfig.Store
	authentication *nativeAuthentication
	client         *http.Client
	legacyURL      string
}

func (api downloaderConfigurationAPI) list(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器查询格式错误")
		return
	}
	defaultID, err := api.system.Get(request.Context(), "DefaultDownloader")
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "默认下载器读取失败")
		return
	}
	if rawID := strings.TrimSpace(request.Form.Get("did")); rawID != "" {
		id, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil || id <= 0 {
			writeAPIError(response, http.StatusBadRequest, 400, "下载器 ID 格式错误")
			return
		}
		item, err := api.store.Get(request.Context(), id)
		if errors.Is(err, downloaderconfig.ErrNotFound) {
			writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"detail": nil}})
			return
		}
		if err != nil {
			writeAPIError(response, http.StatusInternalServerError, 1, "下载器读取失败")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"detail": downloaderRecord(item, defaultID)}})
		return
	}
	items, err := api.store.List(request.Context())
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "下载器列表读取失败")
		return
	}
	detail := make(map[string]any, len(items))
	for _, item := range items {
		detail[strconv.FormatInt(item.ID, 10)] = downloaderRecord(item, defaultID)
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"detail": detail}})
}

func (api downloaderConfigurationAPI) upsert(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 2<<20)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器配置格式错误")
		return
	}
	var item downloaderconfig.Downloader
	if rawID := strings.TrimSpace(request.Form.Get("did")); rawID != "" {
		item.ID, _ = strconv.ParseInt(rawID, 10, 64)
		if item.ID <= 0 {
			writeAPIError(response, http.StatusBadRequest, 400, "下载器 ID 格式错误")
			return
		}
	}
	item.Name = strings.TrimSpace(request.Form.Get("name"))
	item.Type = strings.ToLower(strings.TrimSpace(request.Form.Get("type")))
	item.RmtMode = strings.TrimSpace(request.Form.Get("rmt_mode"))
	item.Config = request.Form.Get("config")
	item.DownloadDir = request.Form.Get("download_dir")
	if item.DownloadDir == "" {
		item.DownloadDir = "[]"
	}
	var check any
	if item.Name == "" || len(item.Name) > 128 || !supportedDownloaderType(item.Type) ||
		json.Unmarshal([]byte(item.Config), &check) != nil || json.Unmarshal([]byte(item.DownloadDir), &check) != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器配置不完整")
		return
	}
	var ok bool
	if item.Enabled, ok = zeroOneInt(request.Form.Get("enabled")); !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器启用状态错误")
		return
	}
	if item.Transfer, ok = zeroOneInt(request.Form.Get("transfer")); !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器监控状态错误")
		return
	}
	if item.OnlyNastool, ok = zeroOneInt(request.Form.Get("only_nastool")); !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器隔离状态错误")
		return
	}
	if item.MatchPath, ok = zeroOneInt(defaultString(request.Form.Get("match_path"), "0")); !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器目录隔离状态错误")
		return
	}
	if _, err := api.store.Upsert(request.Context(), item); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, downloaderconfig.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeAPIError(response, status, 1, "下载器保存失败")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
}

func (api downloaderConfigurationAPI) delete(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器删除格式错误")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(request.Form.Get("did")), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器 ID 格式错误")
		return
	}
	if err := api.store.Delete(request.Context(), id); err != nil {
		writeAPIError(response, http.StatusNotFound, 404, "下载器不存在")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
}

func (api downloaderConfigurationAPI) setFlag(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器状态格式错误")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(request.Form.Get("did")), 10, 64)
	value, ok := zeroOneInt(request.Form.Get("checked"))
	if err != nil || id <= 0 || !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器状态参数错误")
		return
	}
	if err := api.store.SetFlag(request.Context(), id, request.Form.Get("flag"), value); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "下载器状态更新失败")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
}

func (api downloaderConfigurationAPI) settings(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "下载设置查询格式错误")
		return
	}
	rawID := strings.TrimSpace(request.Form.Get("sid"))
	if rawID != "" {
		item, err := api.downloadSetting(request.Context(), rawID)
		if errors.Is(err, downloaderconfig.ErrSettingNotFound) {
			item = map[string]any{}
		} else if err != nil {
			writeAPIError(response, http.StatusBadRequest, 400, "下载设置读取失败")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"data": item}})
		return
	}
	items, err := api.store.ListSettings(request.Context())
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "下载设置列表读取失败")
		return
	}
	result := make([]map[string]any, 0, len(items)+1)
	preset, err := api.downloadSetting(request.Context(), "-1")
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "预设下载设置读取失败")
		return
	}
	result = append(result, preset)
	for _, item := range items {
		result = append(result, api.downloadSettingRecord(request.Context(), item))
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"data": result}})
}

func (api downloaderConfigurationAPI) upsertSetting(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "下载设置格式错误")
		return
	}
	item := downloaderconfig.DownloadSetting{
		Name: strings.TrimSpace(request.Form.Get("name")), Category: strings.TrimSpace(request.Form.Get("category")),
		Tags: strings.TrimSpace(request.Form.Get("tags")), DownloaderID: strings.TrimSpace(request.Form.Get("downloader")),
	}
	if item.Name == "" || len(item.Name) > 128 {
		writeAPIError(response, http.StatusBadRequest, 400, "下载设置名称不能为空")
		return
	}
	if rawID := strings.TrimSpace(request.Form.Get("sid")); rawID != "" {
		item.ID, _ = strconv.ParseInt(rawID, 10, 64)
		if item.ID <= 0 {
			writeAPIError(response, http.StatusBadRequest, 400, "下载设置 ID 格式错误")
			return
		}
	}
	var ok bool
	if item.Paused, ok = optionalNonNegativeInt(request.Form.Get("is_paused")); !ok || item.Paused > 1 {
		writeAPIError(response, http.StatusBadRequest, 400, "暂停状态格式错误")
		return
	}
	if item.UploadLimit, ok = optionalNonNegativeInt(request.Form.Get("upload_limit")); !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "上传限速格式错误")
		return
	}
	if item.DownloadLimit, ok = optionalNonNegativeInt(request.Form.Get("download_limit")); !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "下载限速格式错误")
		return
	}
	if item.SeedingTimeLimit, ok = optionalNonNegativeInt(request.Form.Get("seeding_time_limit")); !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "做种时间格式错误")
		return
	}
	ratio, err := optionalNonNegativeFloat(request.Form.Get("ratio_limit"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "分享率格式错误")
		return
	}
	item.RatioLimit = int(math.Round(ratio * 100))
	if item.DownloaderID != "" {
		downloaderID, err := strconv.ParseInt(item.DownloaderID, 10, 64)
		if err != nil || downloaderID <= 0 {
			writeAPIError(response, http.StatusBadRequest, 400, "下载器 ID 格式错误")
			return
		}
		if _, err := api.store.Get(request.Context(), downloaderID); err != nil {
			writeAPIError(response, http.StatusBadRequest, 400, "下载器不存在")
			return
		}
	}
	if _, err := api.store.UpsertSetting(request.Context(), item); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, downloaderconfig.ErrSettingNotFound) {
			status = http.StatusNotFound
		}
		writeAPIError(response, status, 1, "下载设置保存失败")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
}

func (api downloaderConfigurationAPI) deleteSetting(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "下载设置删除格式错误")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(request.Form.Get("sid")), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(response, http.StatusBadRequest, 400, "下载设置 ID 格式错误")
		return
	}
	if err := api.store.DeleteSetting(request.Context(), id); err != nil {
		writeAPIError(response, http.StatusNotFound, 404, "下载设置不存在")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
}

func (api downloaderConfigurationAPI) directories(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "保存目录查询格式错误")
		return
	}
	settingID := strings.TrimSpace(request.Form.Get("sid"))
	paths, err := nativeDownloadSavePaths(request.Context(), api.store, api.system, settingID)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "下载设置不存在")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"paths": paths}})
}

func (api downloaderConfigurationAPI) downloadSetting(ctx context.Context, rawID string) (map[string]any, error) {
	if rawID == "-1" {
		defaultDownloader, err := api.system.Get(ctx, "DefaultDownloader")
		if err != nil {
			return nil, err
		}
		preset := map[string]any{
			"id": -1, "name": "预设", "category": "", "tags": "NASTOOL", "is_paused": 0,
			"upload_limit": 0, "download_limit": 0, "ratio_limit": 0, "seeding_time_limit": 0,
			"downloader": defaultDownloader, "downloader_name": "", "downloader_type": "",
		}
		if id, err := strconv.ParseInt(defaultDownloader, 10, 64); err == nil {
			if downloader, err := api.store.Get(ctx, id); err == nil {
				preset["downloader_name"], preset["downloader_type"] = downloader.Name, downloader.Type
			}
		}
		return preset, nil
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		return nil, downloaderconfig.ErrSettingNotFound
	}
	item, err := api.store.GetSetting(ctx, id)
	if err != nil {
		return nil, err
	}
	return api.downloadSettingRecord(ctx, item), nil
}

func (api downloaderConfigurationAPI) downloadSettingRecord(ctx context.Context, item downloaderconfig.DownloadSetting) map[string]any {
	downloaderName, downloaderType := "", ""
	if id, err := strconv.ParseInt(item.DownloaderID, 10, 64); err == nil {
		if downloader, err := api.store.Get(ctx, id); err == nil {
			downloaderName, downloaderType = downloader.Name, downloader.Type
		}
	}
	return map[string]any{
		"id": item.ID, "name": item.Name, "category": item.Category, "tags": item.Tags,
		"is_paused": item.Paused, "upload_limit": item.UploadLimit, "download_limit": item.DownloadLimit,
		"ratio_limit": float64(item.RatioLimit) / 100, "seeding_time_limit": item.SeedingTimeLimit,
		"downloader": item.DownloaderID, "downloader_name": downloaderName, "downloader_type": downloaderType,
	}
}

func nativeDownloadSettingOptions(ctx context.Context, store *downloaderconfig.Store) ([]subscriptionOption, error) {
	items, err := store.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	options := make([]subscriptionOption, 0, len(items)+1)
	options = append(options, subscriptionOption{Value: "-1", Label: "预设"})
	for _, item := range items {
		options = append(options, subscriptionOption{Value: strconv.FormatInt(item.ID, 10), Label: item.Name})
	}
	sort.SliceStable(options, func(i, j int) bool { return options[i].Label < options[j].Label })
	return options, nil
}

func nativeDownloadSavePaths(ctx context.Context, store *downloaderconfig.Store, system *systemconfig.Store, settingID string) ([]string, error) {
	api := downloaderConfigurationAPI{store: store, system: system}
	if settingID == "" {
		settingID, _ = system.Get(ctx, "DefaultDownloadSetting")
		if settingID == "" {
			settingID = "-1"
		}
	}
	setting, err := api.downloadSetting(ctx, settingID)
	if err != nil {
		return nil, err
	}
	downloaderID := text(setting["downloader"])
	paths := make([]string, 0)
	if downloaderID != "" {
		id, _ := strconv.ParseInt(downloaderID, 10, 64)
		downloader, err := store.Get(ctx, id)
		if err == nil {
			var rules []map[string]any
			if json.Unmarshal([]byte(downloader.DownloadDir), &rules) == nil {
				seen := make(map[string]bool)
				for _, rule := range rules {
					path := strings.TrimSpace(text(rule["save_path"]))
					if path != "" && !seen[path] {
						seen[path] = true
						paths = append(paths, path)
					}
				}
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func (api downloaderConfigurationAPI) requireAdministrator(response http.ResponseWriter, request *http.Request) bool {
	claims, err := api.authentication.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, http.StatusUnauthorized, 401, "登录状态无效或已过期")
		return false
	}
	if !api.authentication.service.IsAdministrator(claims.Username) {
		writeAPIError(response, http.StatusForbidden, 403, "仅管理员可以修改下载器")
		return false
	}
	return true
}

func supportedDownloaderType(value string) bool {
	return value == "qbittorrent" || value == "transmission" || value == "aria2" || value == "pan115" || value == "pikpak"
}

func zeroOneInt(value string) (int, bool) {
	if value == "0" {
		return 0, true
	}
	if value == "1" {
		return 1, true
	}
	return 0, false
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func optionalNonNegativeInt(value string) (int, bool) {
	if strings.TrimSpace(value) == "" {
		return 0, true
	}
	number, err := strconv.Atoi(value)
	return number, err == nil && number >= 0
}

func optionalNonNegativeFloat(value string) (float64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number < 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, errors.New("invalid non-negative number")
	}
	return number, nil
}
