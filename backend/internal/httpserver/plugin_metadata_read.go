package httpserver

import (
	"context"
	"net/http"
	"strings"
)

func (service pluginService) nativeMetadataInstalled(ctx context.Context) ([]string, error) {
	raw, err := service.system.Get(ctx, "UserInstalledPlugins")
	if err != nil {
		return nil, err
	}
	return decodeInstalledPlugins(raw)
}

func (service pluginService) nativeMetadataReadConfig(ctx context.Context, id string) (map[string]any, bool, error) {
	raw, err := service.system.Get(ctx, "plugin."+id)
	if err != nil {
		return nil, false, err
	}
	values, err := decodeMetadataConfig(raw)
	if err != nil {
		return nil, false, err
	}
	if service.nativeHosts(id) {
		input, err := nativeHostsInput(values["hosts"])
		if err != nil {
			return nil, false, err
		}
		service.hosts.mu.Lock()
		active := service.hosts.active
		service.hosts.mu.Unlock()
		return map[string]any{"hosts": input, "enable": values["enable"] == true, "err_hosts": values["err_hosts"]}, active, nil
	}
	if err := validateMetadataConfig(ctx, id, values); err != nil {
		return nil, false, err
	}
	key := "release_groups"
	if id == "Customization" {
		key = "customization"
	}
	// Only declared metadata fields belong in the legacy editor response.
	// Unknown persisted state can contain private caches and must not leak.
	config := map[string]any{}
	for _, field := range []string{key, "separator"} {
		if value, found := values[field]; found {
			config[field] = value
		}
	}
	active := text(values[key]) != ""
	if id == "CustomReleaseGroups" {
		active = active || text(values["separator"]) != ""
	}
	return config, active, nil
}

func (service pluginService) serveNativeMetadataStatus(response http.ResponseWriter, request *http.Request, id string) {
	installed, err := service.nativeMetadataInstalled(request.Context())
	if err != nil {
		writeAPIError(response, 502, 502, "installed plugin state is unavailable")
		return
	}
	for _, current := range installed {
		if current != id {
			continue
		}
		_, active, err := service.nativeMetadataReadConfig(request.Context(), id)
		if err != nil {
			writeAPIError(response, 502, 502, "plugin state is unavailable")
			return
		}
		writeJSON(response, 200, map[string]any{"code": 0, "state": active})
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "state": nil})
}

func (service pluginService) metadataLegacyListRoute(fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		installed, err := service.nativeMetadataInstalled(request.Context())
		if err != nil {
			writeAPIError(response, 502, 502, "installed plugin state is unavailable")
			return
		}
		for _, id := range installed {
			if !nativeMetadataPlugin(id) && !service.nativeHosts(id) {
				fallback.ServeHTTP(response, request)
				return
			}
		}
		result := map[string]any{}
		for _, id := range installed {
			config, active, err := service.nativeMetadataReadConfig(request.Context(), id)
			if err != nil {
				writeAPIError(response, 502, 502, "plugin configuration is unavailable")
				return
			}
			if service.nativeHosts(id) {
				if service.auth == nil {
					writeAPIError(response, 503, 503, "native authentication is unavailable")
					return
				}
				claims, authErr := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
				if authErr != nil {
					writeAPIError(response, 401, 401, "authorization token is invalid or expired")
					return
				}
				if !service.auth.service.IsAdministrator(claims.Username) {
					continue
				}
				fields := []any{map[string]any{"type": "div", "content": []any{
					[]any{map[string]any{"title": "Hosts 映射", "type": "textarea", "content": map[string]any{"id": "hosts", "rows": 5}}},
					[]any{map[string]any{"title": "无效映射", "type": "textarea", "readonly": true, "content": map[string]any{"id": "err_hosts", "rows": 3}}},
					[]any{map[string]any{"title": "开启 Hosts 同步", "type": "switch", "id": "enable"}},
				}}}
				result[id] = map[string]any{"name": "自定义Hosts", "desc": "修改系统hosts文件，加速网络访问。", "version": "1.0", "icon": "hosts.png", "prefix": "customhosts_", "color": "#02C4E0", "fields": fields, "config": config, "state": active}
				continue
			}
			detail := metadataPluginFields(id, config)
			key, description, color, icon := "release_groups", "添加无法识别的制作组/字幕组，自定义多个组间分隔符", "#00ADEF", "teamwork.png"
			if id == "Customization" {
				key, description, color, icon = "customization", "添加自定义占位符识别正则，自定义多个结果间分隔符", "#E64D1C", "regex.png"
			}
			fields := []any{map[string]any{"type": "div", "content": []any{
				[]any{map[string]any{"title": detail.Fields[0].Title, "type": "textarea", "content": map[string]any{"id": key, "rows": 5, "placeholder": detail.Fields[0].Placeholder}}},
				[]any{map[string]any{"title": "自定义分隔符", "type": "text", "content": []any{map[string]any{"id": "separator", "placeholder": "留空使用 @"}}}},
			}}}
			result[id] = map[string]any{"name": detail.Name, "desc": description, "version": "1.0", "icon": icon, "prefix": strings.ToLower(id) + "_", "color": color, "fields": fields, "config": config, "state": active}
		}
		writeJSON(response, 200, map[string]any{"code": 0, "result": result})
	})
}
