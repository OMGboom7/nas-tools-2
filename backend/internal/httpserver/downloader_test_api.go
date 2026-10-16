package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	writeAPIError(response, 400, 400, "不支持的下载器类型")
}
