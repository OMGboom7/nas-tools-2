package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/auth"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type configurationAPI struct {
	store          *config.Store
	authentication *nativeAuthentication
	path           string
	system         *systemconfig.Store
}

func (api configurationAPI) setSystem(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "系统配置格式错误")
		return
	}
	key := strings.TrimSpace(request.Form.Get("key"))
	value := request.Form.Get("value")
	if len(key) > 128 || len(value) > 1<<20 {
		writeAPIError(response, http.StatusBadRequest, 400, "系统配置过长")
		return
	}
	if err := api.system.Set(request.Context(), key, value); err != nil {
		if errors.Is(err, systemconfig.ErrInvalidSetting) {
			writeAPIError(response, http.StatusBadRequest, 400, "系统配置键和值不能为空")
		} else {
			writeAPIError(response, http.StatusInternalServerError, 1, "系统配置保存失败")
		}
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
}

func (api configurationAPI) info(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	snapshot, err := api.store.Snapshot()
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "配置读取失败")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": snapshot})
}

func (api configurationAPI) update(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	items, err := decodeConfigurationItems(response, request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, err.Error())
		return
	}
	if test, _ := items["test"].(bool); test {
		delete(items, "test")
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
		return
	}
	if err := normalizeSensitiveConfiguration(items); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, err.Error())
		return
	}
	if err := api.store.Update(items); err != nil {
		if errors.Is(err, config.ErrInvalidConfigPath) {
			writeAPIError(response, http.StatusBadRequest, 400, "配置键格式错误")
		} else {
			writeAPIError(response, http.StatusInternalServerError, 1, "配置保存失败")
		}
		return
	}
	if err := api.reloadAuthentication(); err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "配置已保存，但认证配置重载失败")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
}

func (api configurationAPI) updateDirectory(response http.ResponseWriter, request *http.Request) {
	if !api.requireAdministrator(response, request) {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "目录配置格式错误")
		return
	}
	err := api.store.UpdateDirectory(
		strings.TrimSpace(request.Form.Get("oper")),
		strings.TrimSpace(request.Form.Get("key")),
		request.Form.Get("value"),
		request.Form.Get("replace_value"),
	)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
}

func (api configurationAPI) requireAdministrator(response http.ResponseWriter, request *http.Request) bool {
	claims, err := api.authentication.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, http.StatusUnauthorized, 401, "登录状态无效或已过期")
		return false
	}
	if !api.authentication.service.IsAdministrator(claims.Username) {
		writeAPIError(response, http.StatusForbidden, 403, "仅管理员可以修改系统配置")
		return false
	}
	return true
}

func (api configurationAPI) reloadAuthentication() error {
	application, err := config.LoadApplication(api.path)
	if err != nil {
		return err
	}
	return api.authentication.service.ReloadApplication(application)
}

func decodeConfigurationItems(response http.ResponseWriter, request *http.Request) (map[string]any, error) {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	if strings.Contains(request.Header.Get("Content-Type"), "application/json") {
		var payload struct {
			Items map[string]any `json:"items"`
		}
		decoder := json.NewDecoder(request.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil || len(payload.Items) == 0 {
			return nil, errors.New("配置项格式错误")
		}
		return payload.Items, nil
	}
	if err := request.ParseForm(); err != nil {
		return nil, errors.New("配置项格式错误")
	}
	encoded := request.Form.Get("items")
	if encoded == "" {
		return nil, errors.New("缺少配置项")
	}
	items := make(map[string]any)
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&items); err != nil || len(items) == 0 {
		return nil, errors.New("配置项格式错误")
	}
	return items, nil
}

func normalizeSensitiveConfiguration(items map[string]any) error {
	if value, exists := items["app.login_password"]; exists {
		password, ok := value.(string)
		if !ok || password == "" {
			return errors.New("登录密码不能为空")
		}
		if !strings.HasPrefix(password, "[hash]") {
			hash, err := auth.HashPassword(password)
			if err != nil {
				return errors.New("登录密码加密失败")
			}
			items["app.login_password"] = "[hash]" + hash
		}
	}
	if value, exists := items["security.api_key"]; exists {
		secret, ok := value.(string)
		if !ok || strings.TrimSpace(secret) == "" {
			return errors.New("API 密钥不能为空")
		}
	}
	if value, exists := items["app.proxies"]; exists {
		proxy, ok := value.(string)
		if !ok {
			return errors.New("代理配置格式错误")
		}
		if proxy == "" {
			items["app.proxies"] = map[string]any{"http": nil, "https": nil}
		} else {
			if !strings.HasPrefix(proxy, "http") && !strings.HasPrefix(proxy, "sock") {
				proxy = "http://" + proxy
			}
			items["app.proxies"] = map[string]any{"http": proxy, "https": proxy}
		}
	}
	return nil
}
