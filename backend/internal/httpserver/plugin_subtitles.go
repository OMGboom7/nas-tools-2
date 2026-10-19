package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

type subtitleAPI struct {
	organizer *organizationAPI
	queue     *subtitles.Store
	transport http.RoundTripper
}

func (s pluginService) nativeSubtitle(id string) bool {
	return s.subtitles != nil && strings.TrimSpace(id) == subtitles.Plugin
}
func (s pluginService) subtitleSummary(ctx context.Context) (bool, bool) {
	if s.subtitles == nil || !s.subtitles.organizer.pureGo {
		return false, false
	}
	raw, err := s.system.Get(ctx, subtitles.ConfigKey)
	if err != nil {
		return false, false
	}
	values, err := decodeMetadataConfig(raw)
	if err != nil {
		return false, false
	}
	_, err = subtitles.Parse(values)
	return err == nil, true
}
func subtitleDetail(values map[string]any) pluginConfigDetail {
	fields := []pluginConfigField{{Key: "host", Title: "ChineseSubFinder 地址", Type: "text", Required: true, WriteOnly: true, Options: []pluginConfigChoice{}, Placeholder: "http://127.0.0.1:19035"}, {Key: "api_key", Title: "API Key", Type: "text", Required: true, WriteOnly: true, Options: []pluginConfigChoice{}}}
	for i, suffix := range []string{"", "2", "3"} {
		fields = append(fields, pluginConfigField{Key: "local_path" + suffix, Title: "本地路径 " + strconv.Itoa(i+1), Type: "text", WriteOnly: true, Options: []pluginConfigChoice{}, Section: "路径映射", Tooltip: "两端成对填写，按完整目录前缀依次映射；留空保留原配置。"}, pluginConfigField{Key: "remote_path" + suffix, Title: "字幕服务路径 " + strconv.Itoa(i+1), Type: "text", WriteOnly: true, Options: []pluginConfigChoice{}, Section: "路径映射"})
	}
	for i := range fields {
		fields[i].Configured = configuredPluginValue(values[fields[i].Key])
	}
	return pluginConfigDetail{ID: subtitles.Plugin, Name: subtitles.Plugin, Fields: fields, Values: map[string]any{}, Meta: pluginConfigDetailMeta{HasPage: true}}
}

func (s pluginService) serveSubtitlePlugin(w http.ResponseWriter, r *http.Request, action string) {
	if s.subtitles == nil || s.auth == nil || s.system == nil {
		writeAPIError(w, 503, 503, "native subtitle service unavailable")
		return
	}
	claims, err := s.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	if _, err := s.auth.service.FindUser(r.Context(), claims.Username); err != nil {
		writeAPIError(w, 401, 401, "account unavailable")
		return
	}
	write := action == "save" || action == "legacy-save" || action == "install" || action == "uninstall"
	if (write || action == "page") && !s.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission required")
		return
	}
	if write && !s.subtitles.organizer.pureGo {
		writeAPIError(w, 501, 501, "native subtitle changes require disabled legacy backend")
		return
	}
	k := []string{"UserInstalledPlugins", subtitles.ConfigKey, subtitles.EpochKey}
	if !write {
		raw, err := s.system.ReadMany(r.Context(), k)
		if err != nil {
			writeAPIError(w, 502, 502, "subtitle configuration unavailable")
			return
		}
		installed, err := indexerInstalled(raw[k[0]], subtitles.Plugin)
		if err != nil {
			writeAPIError(w, 502, 502, "installed plugin list unavailable")
			return
		}
		values, err := decodeMetadataConfig(raw[k[1]])
		if err != nil {
			writeAPIError(w, 502, 502, "subtitle configuration unavailable")
			return
		}
		if action == "status" {
			var state any
			if installed && s.subtitles.organizer.pureGo {
				_, err := subtitles.Parse(values)
				state = err == nil
			}
			writeJSON(w, 200, map[string]any{"code": 0, "state": state})
			return
		}
		if !installed {
			writeAPIError(w, 404, 404, "plugin is not installed")
			return
		}
		if action == "page" {
			s.subtitlePage(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": subtitleDetail(values)})
		return
	}
	var input pluginConfigRequest
	var complete map[string]any
	if action == "save" && !decodeServiceRequest(w, r, &input, "invalid subtitle configuration") {
		return
	}
	if action == "legacy-save" {
		raw := r.PostForm.Get("config")
		complete, err = decodeMetadataConfig(raw)
		if err != nil || raw == "" {
			writeAPIError(w, 400, 400, "invalid subtitle configuration")
			return
		}
	}
	err = s.system.UpdateMany(r.Context(), k, func(raw map[string]string) (map[string]string, error) {
		ids, err := decodeInstalledPlugins(raw[k[0]])
		if err != nil {
			return nil, err
		}
		found := false
		next := []string{}
		for _, id := range ids {
			if id == subtitles.Plugin {
				found = true
				if action == "uninstall" {
					continue
				}
			}
			next = append(next, id)
		}
		if action == "install" {
			if found {
				return nil, nil
			}
			if len(next) >= 1024 {
				return nil, subtitles.ErrConfig
			}
			next = append(next, subtitles.Plugin)
		} else if !found {
			return nil, errPluginNotInstalled
		}
		values, err := decodeMetadataConfig(raw[k[1]])
		if err != nil && action != "uninstall" && complete == nil {
			return nil, err
		}
		before := organization.Digest(values)
		if complete != nil {
			values = complete
		}
		if action == "save" {
			var message string
			values, message = mergePluginConfig(values, subtitleDetail(values).Fields, input)
			if message != "" {
				return nil, subtitles.ErrConfig
			}
		}
		result := map[string]string{}
		if action != "uninstall" {
			if action != "install" || configuredPluginValue(values["host"]) || configuredPluginValue(values["api_key"]) {
				if _, err := subtitles.Parse(values); err != nil {
					return nil, err
				}
			}
			if action == "save" || action == "legacy-save" {
				data, err := json.Marshal(values)
				if err != nil || len(data) > 64<<10 {
					return nil, subtitles.ErrConfig
				}
				result[k[1]] = string(data)
			}
		}
		encoded, _ := json.Marshal(next)
		result[k[0]] = string(encoded)
		if action == "install" || action == "uninstall" || organization.Digest(values) != before {
			epoch, err := newIndexerClaim()
			if err != nil {
				return nil, err
			}
			result[k[2]] = epoch
		}
		return result, nil
	})
	if err != nil {
		if action == "uninstall" && r.URL.Path == "/api/v1/plugin/uninstall" && errors.Is(err, errPluginNotInstalled) {
			writeJSON(w, 200, map[string]any{"code": 0, "success": true})
			return
		}
		status := 502
		if errors.Is(err, subtitles.ErrConfig) {
			status = 400
		}
		if errors.Is(err, errPluginNotInstalled) {
			status = 409
		}
		writeAPIError(w, status, status, "subtitle plugin operation rejected; stored configuration not changed")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "msg": "操作成功", "message": "配置已保存；只为后续完成入库的媒体登记字幕任务，不重发旧回执"})
}

func (s pluginService) subtitlePage(w http.ResponseWriter, r *http.Request) {
	if s.subtitles.queue == nil {
		writeAPIError(w, 503, 503, "subtitle receipts require the native intake journal")
		return
	}
	rows, err := s.subtitles.queue.List(r.Context(), false)
	if err != nil {
		writeAPIError(w, 503, 503, "subtitle receipts unavailable")
		return
	}
	data := pluginPageData{Title: "ChineseSubFinder 字幕任务回执", ReadOnly: true, Sections: []string{"仅表示已提交字幕任务，不代表字幕已经下载。响应不确定或 sending 中断记录不自动重发，请到原服务核查。", "展示最多 100 条，未解决回执优先；不返回服务地址、密钥或完整媒体路径。"}, Tables: []pluginPageTable{}}
	t := pluginPageTable{Columns: []string{"媒体", "入库任务", "状态", "字幕任务 ID", "说明", "更新时间"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
	for _, row := range rows {
		t.Rows = append(t.Rows, []string{cleanPluginPageText(row.Name, 600), row.JobID, row.State, row.ExternalID, row.Reason, row.Updated})
		t.RowActions = append(t.RowActions, nil)
	}
	data.Tables = append(data.Tables, t)
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}

func (api *subtitleAPI) tick(ctx context.Context) error {
	if api == nil || api.organizer == nil || !api.organizer.pureGo || api.queue == nil {
		return nil
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, "intake-enrichment-outbox")
	if err != nil {
		return err
	}
	defer guard.Close()
	rows, err := api.queue.List(ctx, true)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		pending, err := api.organizer.jobs.ScrapePending(ctx, row.JobID, row.Ordinal)
		if err != nil {
			return err
		}
		if pending {
			continue
		}
		valid := false
		job, err := api.organizer.jobs.Get(ctx, row.JobID)
		if err == nil && job.State == "completed" && len(job.Items) == len(job.Definition.Entries) && row.Ordinal >= 0 && row.Ordinal < len(job.Items) {
			item := job.Items[row.Ordinal]
			valid = item.Index == row.Ordinal && item.Kind == "media" && item.MediaType == row.MediaType && filepath.Join(job.Definition.TargetRoot, item.Target) == row.LocalPath && organization.SubtitleOriginDigest(job, item) == row.OriginDigest
			if valid {
				valid = api.organizer.validateJobConfiguration(ctx, job) == nil && organization.VerifyCompletedTarget(ctx, job, item) == nil
			}
		}
		cfg, claimed, err := api.queue.Claim(ctx, row)
		if errors.Is(err, subtitles.ErrState) {
			continue
		}
		if err != nil {
			return err
		}
		state, remote := "blocked", ""
		if valid {
			call, cancel := context.WithTimeout(ctx, 20*time.Second)
			remote, err = subtitles.Submit(call, cfg, claimed.Payload, api.transport)
			cancel()
			state = "uncertain"
			if err == nil {
				state = "submitted"
			}
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = api.queue.Finish(persist, claimed.ID, state, remote)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func startSubtitleWorker(ctx context.Context, api *subtitleAPI) func() {
	if api == nil || api.organizer == nil || !api.organizer.pureGo {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			_ = api.tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { <-done }
}
