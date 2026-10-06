package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/0xforee/nas-tools/backend/internal/customhosts"
)

type nativeHostsPlugin struct {
	mu     sync.Mutex
	path   string
	active bool
}

func (service pluginService) nativeHosts(id string) bool {
	return strings.TrimSpace(id) == "CustomHosts" && service.system != nil && service.hosts != nil
}

func hostsPluginDetail(values map[string]any) pluginConfigDetail {
	return pluginConfigDetail{ID: "CustomHosts", Name: "自定义Hosts", Fields: []pluginConfigField{
		{Key: "hosts", Title: "Hosts 映射", Type: "textarea", WriteOnly: true, Configured: configuredPluginValue(values["hosts"]), Options: []pluginConfigChoice{}},
		{Key: "err_hosts", Title: "无效映射", Type: "textarea", ReadOnly: true, Options: []pluginConfigChoice{}},
		{Key: "enable", Title: "开启 Hosts 同步", Type: "switch", Options: []pluginConfigChoice{}},
	}, Values: map[string]any{"enable": values["enable"] == true}}
}

// Save disabled intent first, then apply, then commit enabled state. There is
// no atomic transaction spanning SQLite and a bind-mounted hosts file. On any
// file/final-commit failure the persisted config remains disabled and callers
// receive an error; already-written mappings are intentionally not removed.
func (service pluginService) saveHosts(ctx context.Context, values map[string]any, installed bool) error {
	input, err := nativeHostsInput(values["hosts"])
	if err != nil {
		return errMetadataConfig
	}
	enabled := false
	if raw, ok := values["enable"]; ok {
		var valid bool
		enabled, valid = raw.(bool)
		if !valid {
			return errMetadataConfig
		}
	}
	valid, invalid, err := customhosts.Parse(ctx, input)
	if err != nil {
		return errMetadataConfig
	}
	if enabled && len(valid) == 0 {
		return errMetadataConfig
	}
	errorsList := []string{}
	for _, line := range invalid {
		errorsList = append(errorsList, line.Text)
	}
	values["err_hosts"] = strings.Join(errorsList, "\n")
	encode := func() (string, error) {
		data, err := json.Marshal(values)
		if err != nil || len(data) > 64<<10 {
			return "", errMetadataConfig
		}
		return string(data), nil
	}
	// Validate encoding before touching state.
	if _, err := encode(); err != nil {
		return err
	}
	values["enable"] = false
	raw, err := encode()
	if err != nil {
		return err
	}
	if err := service.system.Set(ctx, "plugin.CustomHosts", raw); err != nil {
		return err
	}
	service.hosts.active = false
	if enabled && installed {
		result, err := customhosts.Apply(ctx, service.hosts.path, input)
		if err != nil {
			return err
		}
		if !result.Applied {
			return errors.New("hosts mappings were not applied")
		}
	}
	values["enable"] = enabled
	raw, err = encode()
	if err != nil {
		return err
	}
	if err := service.system.Set(ctx, "plugin.CustomHosts", raw); err != nil {
		return err
	}
	service.hosts.active = enabled && installed
	return nil
}

func (service pluginService) serveHostsPlugin(response http.ResponseWriter, request *http.Request, action string) {
	if service.auth == nil {
		writeAPIError(response, 503, 503, "native authentication is unavailable")
		return
	}
	claims, err := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, 401, 401, "authorization token is invalid or expired")
		return
	}
	if !service.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(response, 403, 403, "only administrators may access Hosts configuration")
		return
	}
	service.hosts.mu.Lock()
	defer service.hosts.mu.Unlock()
	ctx := request.Context()
	installedIDs, err := service.nativeMetadataInstalled(ctx)
	if err != nil {
		writeAPIError(response, 502, 502, "installed plugin state is unavailable")
		return
	}
	installed := false
	for _, id := range installedIDs {
		installed = installed || id == "CustomHosts"
	}
	if action == "status" {
		var state any
		if installed {
			state = service.hosts.active
		}
		writeJSON(response, 200, map[string]any{"code": 0, "state": state})
		return
	}
	if action == "uninstall" {
		if !installed && request.URL.Path != "/api/v1/plugin/uninstall" {
			writeAPIError(response, 409, 409, "plugin is not installed")
			return
		}
		err = service.system.Update(ctx, "UserInstalledPlugins", func(raw string) (string, error) {
			ids, err := decodeInstalledPlugins(raw)
			if err != nil {
				return "", err
			}
			next := []string{}
			for _, id := range ids {
				if id != "CustomHosts" {
					next = append(next, id)
				}
			}
			data, err := json.Marshal(next)
			return string(data), err
		})
		if err != nil {
			writeAPIError(response, 502, 502, "installed plugins could not be saved")
			return
		}
		service.hosts.active = false
		writeJSON(response, 200, map[string]any{"code": 0, "success": true, "msg": "插件已卸载；已写入的 Hosts 映射保留"})
		return
	}
	if !installed && (action == "get" || action == "save") {
		writeAPIError(response, 404, 404, "installed plugin not found")
		return
	}
	raw, err := service.system.Get(ctx, "plugin.CustomHosts")
	values, decodeErr := decodeMetadataConfig(raw)
	if err != nil || decodeErr != nil {
		writeAPIError(response, 502, 502, "plugin configuration is unavailable")
		return
	}
	if action == "get" {
		writeJSON(response, 200, map[string]any{"code": 0, "success": true, "data": hostsPluginDetail(values)})
		return
	}
	if action == "save" {
		var input pluginConfigRequest
		if !decodeServiceRequest(response, request, &input, "invalid plugin configuration request") {
			return
		}
		var message string
		values, message = mergePluginConfig(values, hostsPluginDetail(values).Fields, input)
		if message != "" {
			writeAPIError(response, 400, 400, message)
			return
		}
	} else if action == "config" {
		raw = request.PostForm.Get("config")
		values, err = decodeMetadataConfig(raw)
		if err != nil || raw == "" {
			writeAPIError(response, 400, 400, "invalid hosts configuration")
			return
		}
	} else if action == "install" {
		// Preflight saved configuration before changing installation state.
		input, inputErr := nativeHostsInput(values["hosts"])
		valid, _, parseErr := customhosts.Parse(ctx, input)
		enabled, enableValid := values["enable"].(bool)
		_, hasEnable := values["enable"]
		if inputErr != nil || parseErr != nil || hasEnable && !enableValid || enabled && len(valid) == 0 {
			writeAPIError(response, 400, 400, "invalid saved hosts configuration")
			return
		}
		err = service.system.Update(ctx, "UserInstalledPlugins", func(raw string) (string, error) {
			ids, err := decodeInstalledPlugins(raw)
			if err != nil {
				return "", err
			}
			found := false
			for _, id := range ids {
				found = found || id == "CustomHosts"
			}
			if !found {
				ids = append(ids, "CustomHosts")
			}
			data, err := json.Marshal(ids)
			if len(ids) > 1024 || len(data) > 64<<10 {
				return "", errInstalledPluginState
			}
			return string(data), err
		})
		if err != nil {
			writeAPIError(response, 502, 502, "installed plugins could not be saved")
			return
		}
		installed = true
	} else {
		writeAPIError(response, 400, 400, "unknown hosts action")
		return
	}
	if err := service.saveHosts(ctx, values, installed); err != nil {
		status := 502
		if errors.Is(err, errMetadataConfig) {
			status = 400
		}
		writeAPIError(response, status, status, "Hosts configuration was not activated; check configuration and file permissions")
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "success": true, "message": "Hosts configuration saved", "msg": "保存成功"})
}
