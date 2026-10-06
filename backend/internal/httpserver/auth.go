package httpserver

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/auth"
)

type nativeAuthentication struct {
	service *auth.Service
}

func (authentication nativeAuthentication) login(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 1, "登录请求格式不正确")
		return
	}
	username := strings.TrimSpace(request.Form.Get("username"))
	password := request.Form.Get("password")
	if username == "" || password == "" || len(username) > 128 || len(password) > 1024 {
		writeAPIError(response, http.StatusUnauthorized, 1, "用户名或密码错误")
		return
	}
	user, err := authentication.service.Authenticate(request.Context(), username, password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeAPIError(response, http.StatusUnauthorized, 1, "用户名或密码错误")
			return
		}
		writeAPIError(response, http.StatusInternalServerError, 1, "用户数据读取失败")
		return
	}
	token, err := authentication.service.IssueToken(user.Name, 2*time.Hour)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "登录令牌生成失败")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"code": 0, "success": true,
		"data": map[string]any{
			"token": token,
			"userinfo": map[string]any{
				"userid": user.ID, "username": user.Name, "userpris": user.Permissions,
			},
		},
	})
}

func (authentication nativeAuthentication) logout(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if _, err := authentication.service.VerifyToken(token); err != nil {
		writeAPIError(response, http.StatusUnauthorized, 401, "登录状态无效或已过期")
		return
	}
	authentication.service.RevokeToken(token)
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
}

func (authentication nativeAuthentication) userInfo(response http.ResponseWriter, request *http.Request) {
	claims, err := authentication.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, http.StatusUnauthorized, 401, "登录状态无效或已过期")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 1, "请求格式不正确")
		return
	}
	username := strings.TrimSpace(request.Form.Get("username"))
	if username == "" {
		username = claims.Username
	}
	if username != claims.Username && !authentication.service.IsAdministrator(claims.Username) {
		writeAPIError(response, http.StatusForbidden, 403, "无权查看其他用户")
		return
	}
	user, err := authentication.service.FindUser(request.Context(), username)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeAPIError(response, http.StatusNotFound, 1, "用户名不正确")
			return
		}
		writeAPIError(response, http.StatusInternalServerError, 1, "用户数据读取失败")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"code": 0, "success": true,
		"data": userInfoData(user),
	})
}

func (authentication nativeAuthentication) userList(response http.ResponseWriter, request *http.Request) {
	claims, err := authentication.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil || !authentication.service.IsAdministrator(claims.Username) {
		writeAPIError(response, http.StatusForbidden, 403, "仅管理员可以管理用户")
		return
	}
	users, err := authentication.service.ListUsers(request.Context())
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 1, "用户列表读取失败")
		return
	}
	items := make([]map[string]any, 0, len(users))
	for _, user := range users {
		items = append(items, map[string]any{"id": user.ID, "name": user.Name, "pris": user.Permissions})
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"code": 0, "success": true, "message": "",
		"data": map[string]any{"result": items},
	})
}

func (authentication nativeAuthentication) manageUser(response http.ResponseWriter, request *http.Request) {
	claims, err := authentication.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil || !authentication.service.IsAdministrator(claims.Username) {
		writeAPIError(response, http.StatusForbidden, 403, "仅管理员可以管理用户")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 1, "请求格式不正确")
		return
	}
	operation := strings.TrimSpace(request.Form.Get("oper"))
	username := strings.TrimSpace(request.Form.Get("name"))
	switch operation {
	case "add":
		permissions := splitFormList(request.Form["pris"])
		user, err := authentication.service.CreateUser(request.Context(), username, request.Form.Get("password"), permissions)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrUserExists):
				writeAPIError(response, http.StatusConflict, 1, "用户名已存在")
			case errors.Is(err, auth.ErrInvalidUser):
				writeAPIError(response, http.StatusBadRequest, 1, "用户名、密码或权限不符合要求")
			default:
				writeAPIError(response, http.StatusInternalServerError, 1, "用户创建失败")
			}
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": userInfoData(user)})
	case "del", "delete":
		if err := authentication.service.DeleteUser(request.Context(), username); err != nil {
			switch {
			case errors.Is(err, auth.ErrUserNotFound):
				writeAPIError(response, http.StatusNotFound, 1, "用户不存在")
			case errors.Is(err, auth.ErrAdministratorImmutable):
				writeAPIError(response, http.StatusBadRequest, 1, "配置文件管理员不能在此删除")
			default:
				writeAPIError(response, http.StatusBadRequest, 1, "用户删除失败")
			}
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{}})
	default:
		writeAPIError(response, http.StatusBadRequest, 1, "不支持的用户操作")
	}
}

func (authentication nativeAuthentication) authorizeUser(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]any{
		"code": 0, "success": true, "message": "认证成功", "data": map[string]any{},
	})
}

func userInfoData(user auth.User) map[string]any {
	return map[string]any{"userid": user.ID, "username": user.Name, "userpris": user.Permissions}
}

func splitFormList(values []string) []string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, strings.Split(value, ",")...)
	}
	return items
}

func (authentication nativeAuthentication) protectAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		// This public integration route authenticates its own API key, including
		// keys containing dots. A JWT must never substitute for that API key.
		if request.URL.Path == "/api/v1/service/mediainfo" {
			next.ServeHTTP(response, request)
			return
		}
		token := request.Header.Get("Authorization")
		if isMigratedProtectedPath(request.URL.Path) {
			if _, err := authentication.service.VerifyToken(token); err != nil {
				writeAPIError(response, http.StatusUnauthorized, 401, "登录状态无效或已过期")
				return
			}
		} else if strings.Count(token, ".") == 2 {
			// Unknown routes can still be public webhooks or API-key endpoints, so
			// only validate them when the caller actually supplied a JWT. This also
			// prevents a revoked Go token from continuing through the Flask proxy.
			if _, err := authentication.service.VerifyToken(token); err != nil {
				writeAPIError(response, http.StatusUnauthorized, 401, "登录状态无效或已过期")
				return
			}
		}
		next.ServeHTTP(response, request)
	})
}

func isMigratedProtectedPath(path string) bool {
	if path == "/api/v1/service/name/test" {
		return true
	}
	if path == "/api/v1/dashboard/image" || path == "/api/v1/user/login" {
		return false
	}
	for _, exact := range []string{"/api/v1/site/list", "/api/v1/site/info", "/api/v1/site/update", "/api/v1/site/delete", "/api/v1/site/test", "/api/v1/site/indexers", "/api/v1/message/client/info", "/api/v1/message/client/options", "/api/v1/message/client/update", "/api/v1/message/client/status", "/api/v1/message/client/delete", "/api/v1/message/client/test", "/api/v1/message/custom/send", "/api/v1/subscribe/movie/list", "/api/v1/subscribe/tv/list", "/api/v1/subscribe/history", "/api/v1/library/space", "/api/v1/library/mediaserver/statistics", "/api/v1/library/mediaserver/latest", "/api/v1/library/mediaserver/resume"} {
		if path == exact {
			return true
		}
	}
	if path == "/api/v1/subscribe/add" || path == "/api/v1/subscribe/delete" || path == "/api/v1/subscribe/redo" || path == "/api/v1/subscribe/history/delete" {
		return true
	}
	for _, prefix := range []string{
		"/api/v1/dashboard", "/api/v1/search/resources", "/api/v1/downloads",
		"/api/v1/subscriptions", "/api/v1/discovery", "/api/v1/sites",
		"/api/v1/services", "/api/v1/notifications", "/api/v1/plugins", "/api/v1/plugin",
		"/api/v1/system/logout", "/api/v1/system/path", "/api/v1/system/version", "/api/v1/service/network/test", "/api/v1/service/rule/test", "/api/v1/media/category/list", "/api/v1/media/tv/seasons", "/api/v1/organization/history/list", "/api/v1/organization/history/statistics",
		"/api/v1/user",
		"/api/v1/words",
		"/api/v1/config",
		"/api/v1/download",
		"/api/v1/rss",
	} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
