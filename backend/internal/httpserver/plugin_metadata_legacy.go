package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Scope legacy-route takeover to plugins with real Go implementations. Restore
// the original request body for unmigrated plugins; parsing must not consume a
// request that still belongs to the existing legacy handler.
func (service pluginService) metadataLegacyRoute(action string, fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, 128<<10))
		if err != nil {
			writeAPIError(response, 400, 400, "plugin request is invalid or too large")
			return
		}
		probe := request.Clone(request.Context())
		probe.Body = io.NopCloser(bytes.NewReader(body))
		if err := probe.ParseForm(); err != nil {
			writeAPIError(response, 400, 400, "invalid plugin request")
			return
		}
		id := strings.TrimSpace(probe.PostForm.Get("id"))
		if !pluginIDPattern.MatchString(id) {
			writeAPIError(response, 400, 400, "invalid plugin id")
			return
		}
		if service.nativeHosts(id) {
			probe.SetPathValue("id", id)
			service.serveHostsPlugin(response, probe, action)
			return
		}
		if !nativeMetadataPlugin(id) {
			request.Body = io.NopCloser(bytes.NewReader(body))
			fallback.ServeHTTP(response, request)
			return
		}
		probe.SetPathValue("id", id)
		if action == "status" {
			service.serveNativeMetadataStatus(response, probe, id)
			return
		}
		if action == "install" || action == "uninstall" {
			service.mutateNativeMetadataPlugin(response, probe, action == "install")
			return
		}
		service.saveLegacyMetadataConfig(response, probe, id)
	})
}

// The old form endpoint sends a complete configuration, not the React editor's
// write-only merge request. An empty string or omitted field therefore clears
// the old value, and unknown keys in the submitted object remain preserved.
func (service pluginService) saveLegacyMetadataConfig(response http.ResponseWriter, request *http.Request, id string) {
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
		writeAPIError(response, 403, 403, "only administrators may change plugin configuration")
		return
	}
	raw := request.PostForm.Get("config")
	values, err := decodeMetadataConfig(raw)
	if raw == "" || err != nil {
		writeAPIError(response, 400, 400, "invalid metadata plugin configuration")
		return
	}
	if err := validateMetadataConfig(request.Context(), id, values); err != nil {
		writeAPIError(response, 400, 400, "invalid metadata plugin configuration")
		return
	}
	encoded, err := json.Marshal(values)
	if err != nil || len(encoded) > 64<<10 {
		writeAPIError(response, 400, 400, "invalid metadata plugin configuration")
		return
	}
	if err := service.system.Set(request.Context(), "plugin."+id, string(encoded)); err != nil {
		writeAPIError(response, 502, 502, "plugin configuration could not be saved")
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "msg": "保存成功"})
}
