package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
)

var errMetadataConfig = errors.New("invalid metadata plugin configuration")
var errStoredMetadataConfig = errors.New("invalid stored metadata plugin configuration")

func nativeMetadataPlugin(id string) bool {
	return id == "CustomReleaseGroups" || id == "Customization"
}

func metadataPluginFields(id string, values map[string]any) pluginConfigDetail {
	name, key, title := "自定义制作组/字幕组", "release_groups", "制作组/字幕组正则"
	if id == "Customization" {
		name, key, title = "自定义占位符", "customization", "占位符正则"
	}
	fields := []pluginConfigField{
		{Key: key, Title: title, Type: "textarea", WriteOnly: true, Options: []pluginConfigChoice{}, Placeholder: "多个表达式请用分号或换行分隔"},
		{Key: "separator", Title: "自定义分隔符", Type: "text", WriteOnly: true, Options: []pluginConfigChoice{}, Placeholder: "留空使用 @"},
	}
	for index := range fields {
		fields[index].Configured = configuredPluginValue(values[fields[index].Key])
	}
	return pluginConfigDetail{ID: id, Name: name, Fields: fields, Values: map[string]any{}}
}

func decodeMetadataConfig(raw string) (map[string]any, error) {
	values := map[string]any{}
	if raw == "" {
		return values, nil
	}
	if len(raw) > 64<<10 || !utf8.ValidString(raw) {
		return nil, errStoredMetadataConfig
	}
	if err := json.Unmarshal([]byte(raw), &values); err != nil || values == nil {
		return nil, errStoredMetadataConfig
	}
	return values, nil
}

func validateMetadataConfig(ctx context.Context, id string, values map[string]any) error {
	options := mediameta.LabelOptions{}
	field := "release_groups"
	if id == "Customization" {
		field = "customization"
	}
	for _, key := range []string{field, "separator"} {
		if value, found := values[key]; found {
			text, ok := value.(string)
			if !ok || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
				return errMetadataConfig
			}
		}
	}
	if id == "Customization" {
		options.Customization, options.CustomSeparator = text(values[field]), text(values["separator"])
	} else {
		options.ReleaseGroups, options.ReleaseSeparator = text(values[field]), text(values["separator"])
	}
	if _, _, err := mediameta.MatchLabels(ctx, "", options); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errMetadataConfig
	}
	return nil
}

func (service pluginService) serveNativeMetadataConfig(response http.ResponseWriter, request *http.Request) {
	if service.auth == nil {
		writeAPIError(response, 503, 503, "native authentication is unavailable")
		return
	}
	claims, err := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, 401, 401, "authorization token is invalid or expired")
		return
	}
	if request.Method == http.MethodPut && !service.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(response, 403, 403, "only administrators may change plugin configuration")
		return
	}
	id, ctx := strings.TrimSpace(request.PathValue("id")), request.Context()
	rawInstalled, err := service.system.Get(ctx, "UserInstalledPlugins")
	if err != nil || len(rawInstalled) > 64<<10 {
		writeAPIError(response, 502, 502, "installed plugin configuration is unavailable")
		return
	}
	installed, decodeErr := decodeInstalledPlugins(rawInstalled)
	if decodeErr != nil {
		writeAPIError(response, 502, 502, "installed plugin configuration is invalid")
		return
	}
	found := false
	for _, plugin := range installed {
		if plugin == id {
			found = true
		}
	}
	if !found {
		writeAPIError(response, 404, 404, "installed plugin not found")
		return
	}
	if request.Method == http.MethodGet {
		raw, err := service.system.Get(ctx, "plugin."+id)
		values, decodeErr := decodeMetadataConfig(raw)
		if err != nil || decodeErr != nil {
			writeAPIError(response, 502, 502, "plugin configuration is unavailable")
			return
		}
		writeJSON(response, 200, map[string]any{"code": 0, "success": true, "data": metadataPluginFields(id, values)})
		return
	}
	var input pluginConfigRequest
	if !decodeServiceRequest(response, request, &input, "invalid plugin configuration request") {
		return
	}
	err = service.system.Update(ctx, "plugin."+id, func(raw string) (string, error) {
		current, err := decodeMetadataConfig(raw)
		if err != nil {
			return "", err
		}
		values, message := mergePluginConfig(current, metadataPluginFields(id, current).Fields, input)
		if message != "" {
			return "", errMetadataConfig
		}
		if err := validateMetadataConfig(ctx, id, values); err != nil {
			return "", err
		}
		encoded, err := json.Marshal(values)
		if err != nil || len(encoded) > 64<<10 {
			return "", errMetadataConfig
		}
		return string(encoded), nil
	})
	if err != nil {
		status, message := 502, "plugin configuration could not be saved"
		if errors.Is(err, errMetadataConfig) {
			status, message = 400, "invalid metadata plugin configuration"
		}
		writeAPIError(response, status, status, message)
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "success": true, "message": "plugin configuration saved"})
}
