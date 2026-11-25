package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (service pluginService) serveNativeLegacyPluginApps(response http.ResponseWriter, request *http.Request) {
	administrator, ok := service.catalogReader(response, request)
	if !ok {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 128<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, 400, 400, "invalid plugin market request")
		return
	}
	withRuntime := request.PostForm.Get("with_runtime") == "1"
	data, err := service.nativePluginCatalogData(request.Context(), administrator, withRuntime)
	if err != nil {
		writeAPIError(response, 502, 502, "plugin catalog is unavailable")
		return
	}
	metadata, err := bundledPluginCatalog()
	if err != nil {
		writeAPIError(response, 500, 500, "bundled plugin catalog is invalid")
		return
	}
	result := map[string]any{}
	for _, item := range data.Items {
		entry, bundled := metadata[item.ID]
		if !bundled {
			entry.Icon, entry.Color = item.MarketIcon, item.MarketColor
		}
		row := map[string]any{"id": item.ID, "name": item.Name, "desc": item.Description, "version": item.Version, "author": item.Author, "author_url": item.AuthorURL, "installed": item.Installed, "icon": entry.Icon, "color": entry.Color, "native": item.Native}
		if withRuntime && item.Installed {
			row["runtime"] = map[string]any{"running": item.Running, "known": item.StateKnown,
				"configurable": item.Native && item.Configurable, "hasPage": item.CanViewPage}
		}
		result[item.ID] = row
	}
	// The old helper's statistics backend was disabled and returned {}.
	// Preserve that contract without introducing a fictitious external count.
	reply := map[string]any{"code": 0, "result": result, "statistic": map[string]any{}}
	if withRuntime {
		// A paired external extension still supplies its live form and state.
		complete := true
		for _, item := range data.Items {
			if item.Installed && !item.Native && service.legacyURL != "" {
				complete = false
				break
			}
		}
		if complete {
			reply["runtime_included"] = true
		}
	}
	writeJSON(response, 200, reply)
}

func (s pluginService) legacyCommandRoutes(reads map[string]http.HandlerFunc, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
		var envelope struct {
			Cmd  string          `json:"cmd"`
			Data json.RawMessage `json:"data"`
		}
		if err != nil || json.Unmarshal(raw, &envelope) != nil {
			writeAPIError(w, 400, 400, "原操作格式无效")
			return
		}
		if strings.EqualFold(strings.TrimSpace(envelope.Cmd), "restory_backup") {
			writeAPIError(w, 410, 410, "旧文件名恢复入口已迁移，请在服务页上传 ZIP 并按提示离线恢复")
			return
		}
		if envelope.Cmd == "get_plugin_page" {
			s.commandPage(w, r, envelope.Data)
			return
		}
		if envelope.Cmd == "run_plugin_method" {
			s.commandPageMethod(w, r, envelope.Data)
			return
		}
		if s.pluginCommand(w, r, envelope.Cmd, envelope.Data) {
			return
		}
		if read := reads[envelope.Cmd]; read != nil {
			if envelope.Cmd == "get_plugins_conf" && len(envelope.Data) > 0 {
				var input struct {
					ID *string `json:"id"`
				}
				if json.Unmarshal(envelope.Data, &input) != nil {
					writeAPIError(w, 400, 400, "插件配置查询无效")
					return
				}
				if input.ID != nil {
					r.PostForm = url.Values{"id": {*input.ID}}
					r.Form = r.PostForm
				}
			}
			r.Body = http.NoBody
			r.ContentLength = 0
			read(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next(w, r)
	}
}

// The old settings page posts JSON commands. Keep the original complete-config
// semantics and use the same local handlers as old form clients.
func (s pluginService) pluginCommand(w http.ResponseWriter, r *http.Request, cmd string, raw json.RawMessage) bool {
	var action string
	switch cmd {
	case "install_plugin":
		action = "install"
	case "uninstall_plugin":
		action = "uninstall"
	case "get_plugin_state":
		action = "status"
	case "update_plugin_config":
		action = "config"
	default:
		return false
	}
	if s.system == nil {
		writeAPIError(w, 503, 503, "插件管理需要挂载应用配置和数据库")
		return true
	}
	var in struct {
		ID, Plugin, Fingerprint string
		Config                  json.RawMessage
	}
	if json.Unmarshal(raw, &in) != nil {
		writeAPIError(w, 400, 400, "插件操作格式无效")
		return true
	}
	id := in.ID
	form := url.Values{}
	if action == "config" {
		id = in.Plugin
		if len(in.Config) == 0 || in.Config[0] != '{' {
			writeAPIError(w, 400, 400, "插件配置应为对象")
			return true
		}
		form.Set("config", string(in.Config))
		form.Set("fingerprint", in.Fingerprint)
	}
	id = strings.TrimSpace(id)
	if !pluginIDPattern.MatchString(id) {
		writeAPIError(w, 400, 400, "插件编号无效")
		return true
	}
	form.Set("id", id)
	req := r.Clone(r.Context())
	req.URL.Path = "/api/v1/plugin/" + action
	req.URL.RawQuery = ""
	req.Form, req.PostForm = form, form
	req.Body = http.NoBody
	req.ContentLength = 0
	if !s.dispatchPlugin(w, req, id, action) && !s.unavailablePlugin(w, req, id) {
		if _, ok := s.catalogReader(w, req); ok {
			writeAPIError(w, 501, 501, "当前部署尚未接管此插件命令")
		}
	}
	return true
}
