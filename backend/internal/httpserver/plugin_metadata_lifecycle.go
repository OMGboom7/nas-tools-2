package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

var errPluginNotInstalled = errors.New("plugin is not installed")
var errInstalledPluginState = errors.New("installed plugin state is invalid")

func decodeInstalledPlugins(raw string) ([]string, error) {
	installed := []string{}
	if raw == "" {
		return installed, nil
	}
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &installed) != nil || len(installed) > 1024 {
		return nil, errInstalledPluginState
	}
	for _, id := range installed {
		if !pluginIDPattern.MatchString(id) {
			return nil, errInstalledPluginState
		}
	}
	return installed, nil
}

// These metadata plugins have no background workers: the Go matcher reads the
// installed list on each recognition. Persisting this state starts/stops their
// actual behavior, without loading Python modules or resetting saved settings.
func (service pluginService) mutateNativeMetadataPlugin(response http.ResponseWriter, request *http.Request, install bool) {
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
		writeAPIError(response, 403, 403, "only administrators may change installed plugins")
		return
	}
	id, ctx := strings.TrimSpace(request.PathValue("id")), request.Context()
	if install {
		raw, err := service.system.Get(ctx, "plugin."+id)
		values, decodeErr := decodeMetadataConfig(raw)
		if err != nil || decodeErr != nil {
			writeAPIError(response, 502, 502, "stored plugin configuration is unavailable")
			return
		}
		if err := validateMetadataConfig(ctx, id, values); err != nil {
			writeAPIError(response, 400, 400, "stored plugin configuration cannot be activated")
			return
		}
	}
	alreadyInstalled := false
	err = service.system.Update(ctx, "UserInstalledPlugins", func(raw string) (string, error) {
		installed, err := decodeInstalledPlugins(raw)
		if err != nil {
			return "", err
		}
		next := make([]string, 0, len(installed)+1)
		for _, current := range installed {
			if current == id {
				alreadyInstalled = true
				if !install {
					continue
				}
			}
			next = append(next, current)
		}
		if !install && !alreadyInstalled {
			return "", errPluginNotInstalled
		}
		if install && !alreadyInstalled {
			next = append(next, id)
		}
		if len(next) > 1024 {
			return "", errInstalledPluginState
		}
		encoded, err := json.Marshal(next)
		if err != nil || len(encoded) > 64<<10 {
			return "", errInstalledPluginState
		}
		return string(encoded), nil
	})
	if err != nil {
		if errors.Is(err, errPluginNotInstalled) && request.URL.Path == "/api/v1/plugin/uninstall" {
			writeJSON(response, 200, map[string]any{"code": 0, "success": true, "msg": "插件已卸载"})
			return
		}
		status, message := 502, "installed plugin state could not be updated"
		if errors.Is(err, errPluginNotInstalled) {
			status, message = 409, "plugin is not installed"
		}
		writeAPIError(response, status, status, message)
		return
	}
	message := "plugin uninstalled"
	if install {
		message = "plugin installed"
		if alreadyInstalled {
			message = "plugin is already installed"
		}
	}
	writeJSON(response, 200, map[string]any{"code": 0, "success": true, "message": message, "msg": message})
}
