package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (api downloaderConfigurationAPI) testConnection(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 2<<20)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, 400, 400, "下载器测试参数格式错误")
		return
	}
	downloaderType := strings.ToLower(strings.TrimSpace(request.Form.Get("type")))
	var configuration map[string]any
	decoder := json.NewDecoder(io.LimitReader(strings.NewReader(request.Form.Get("config")), 1<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&configuration); err != nil || configuration == nil {
		writeAPIError(response, 400, 400, "下载器配置格式错误")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeAPIError(response, 400, 400, "下载器配置格式错误")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	handled, checkErr, _ := nativeDownloaderCheck(ctx, downloaderType, configuration, api.client.Transport)
	if handled {
		code := 0
		if checkErr != nil {
			code = 1
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": code, "success": checkErr == nil})
		return
	}
	// Cloud downloaders keep their compatibility path until their account APIs
	// are migrated. This branch is deleted together with the legacy proxy.
	result, err := postLegacy(ctx, api.client, api.legacyURL, "/api/v1/download/client/test", request.Header.Get("Authorization"), url.Values{"type": {downloaderType}, "config": {request.Form.Get("config")}})
	if err != nil {
		writeAPIError(response, 502, 502, "旧下载器测试服务不可用")
		return
	}
	writeJSON(response, http.StatusOK, result)
}
