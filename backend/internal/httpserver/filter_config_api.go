package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
)

type filterConfigurationAPI struct {
	store          *filterconfig.Store
	authentication *nativeAuthentication
}

func (api filterConfigurationAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	claims, err := api.authentication.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, 401, 401, "登录状态无效或已过期")
		return
	}
	if !api.authentication.service.IsAdministrator(claims.Username) {
		writeAPIError(response, 403, 403, "仅管理员可以管理过滤规则")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 2<<20)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, 400, 400, "过滤规则参数格式错误")
		return
	}
	parseID := func(key string, zero bool) (int64, bool) {
		id, err := strconv.ParseInt(strings.TrimSpace(request.Form.Get(key)), 10, 64)
		return id, err == nil && (id > 0 || zero && id == 0)
	}
	invalid := func() { writeAPIError(response, 400, 400, "过滤规则参数不完整或格式错误") }
	ctx := request.Context()
	data := map[string]any{}
	switch strings.TrimPrefix(request.URL.Path, "/api/v1/filterrule/") {
	case "list":
		var groups, templates []filterconfig.GroupInfo
		groups, err = api.store.List(ctx)
		if err == nil {
			templates, err = filterconfig.Builtins(ctx)
		}
		data["ruleGroups"] = filterGroupRecords(groups, false)
		data["initRules"] = filterGroupRecords(templates, true)
	case "group/restore":
		values := request.Form["groupids"]
		if len(values) == 1 && strings.HasPrefix(strings.TrimSpace(values[0]), "[") {
			var decoded []any
			if json.Unmarshal([]byte(values[0]), &decoded) != nil {
				invalid()
				return
			}
			values = make([]string, 0, len(decoded))
			for _, value := range decoded {
				values = append(values, text(value))
			}
		}
		if len(values) == 0 || len(values) > 3 {
			invalid()
			return
		}
		ids := make([]int64, 0, len(values))
		for _, value := range values {
			id, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil || id <= 0 {
				invalid()
				return
			}
			ids = append(ids, id)
		}
		// init_rulegroups was legacy client-supplied SQL. Only server templates are used.
		err = api.store.Restore(ctx, ids)
	case "rule/share":
		id, ok := parseID("id", false)
		if !ok {
			invalid()
			return
		}
		data["string"], err = api.store.Export(ctx, id)
	case "rule/import":
		data["id"], err = api.store.Import(ctx, request.Form.Get("content"))
	case "group/add":
		name, flag := strings.TrimSpace(request.Form.Get("name")), request.Form.Get("default")
		if name == "" || len(name) > 256 || flag != "Y" && flag != "N" {
			invalid()
			return
		}
		var id int64
		id, err = api.store.AddGroup(ctx, name, flag == "Y")
		data["id"] = id
	case "group/default":
		id, ok := parseID("id", true)
		if !ok {
			invalid()
			return
		}
		err = api.store.SetDefault(ctx, id)
	case "group/delete":
		id, ok := parseID("id", false)
		if !ok {
			invalid()
			return
		}
		err = api.store.DeleteGroup(ctx, id)
	case "rule/update":
		groupID, ok := parseID("group_id", false)
		if !ok {
			invalid()
			return
		}
		id := int64(0)
		if request.Form.Get("rule_id") != "" {
			id, ok = parseID("rule_id", true)
			if !ok {
				invalid()
				return
			}
		}
		name, priority := strings.TrimSpace(request.Form.Get("rule_name")), strings.TrimSpace(request.Form.Get("rule_pri"))
		if _, parseErr := strconv.Atoi(priority); name == "" || len(name) > 256 || parseErr != nil {
			invalid()
			return
		}
		id, err = api.store.SaveRule(ctx, filterconfig.Rule{ID: id, GroupID: groupID, Name: name, Priority: priority, Include: request.Form.Get("rule_include"), Exclude: request.Form.Get("rule_exclude"), Size: request.Form.Get("rule_sizelimit"), Free: request.Form.Get("rule_free")})
		data["id"] = id
	case "rule/delete":
		id, ok := parseID("id", false)
		if !ok {
			invalid()
			return
		}
		err = api.store.DeleteRule(ctx, id)
	case "rule/info":
		id, ok := parseID("ruleid", false)
		groupID, groupOK := parseID("groupid", false)
		if !ok || !groupOK {
			invalid()
			return
		}
		var rule filterconfig.Rule
		rule, err = api.store.Rule(ctx, groupID, id)
		info := map[string]any{}
		if errors.Is(err, filterconfig.ErrNotFound) {
			err = nil
		} else if err == nil {
			freeText := ""
			if rule.Free != "" {
				freeText = map[string]string{"1.0 1.0": "普通", "1.0 0.0": "免费", "2.0 0.0": "2X免费"}[rule.Free]
				if freeText == "" {
					freeText = "全部"
				}
			}
			info = map[string]any{"id": rule.ID, "group": strconv.FormatInt(rule.GroupID, 10), "name": rule.Name, "pri": rule.Priority, "include": rule.Include, "exclude": rule.Exclude, "size": rule.Size, "free": rule.Free, "free_text": freeText}
		}
		data["info"] = info
	default:
		writeAPIError(response, 404, 404, "接口不存在")
		return
	}
	if errors.Is(err, filterconfig.ErrInvalidTemplate) {
		invalid()
		return
	}
	if errors.Is(err, filterconfig.ErrTemplateConflict) {
		writeAPIError(response, 409, 409, "模板规则 ID 已被其他规则组占用，未修改任何规则")
		return
	}
	if errors.Is(err, filterconfig.ErrInvalidShare) || errors.Is(err, filterconfig.ErrEmptyGroup) {
		writeAPIError(response, 400, 400, "分享内容格式错误或规则组为空")
		return
	}
	if errors.Is(err, filterconfig.ErrNotFound) {
		writeAPIError(response, 404, 404, "过滤规则或规则组不存在")
		return
	}
	if err != nil {
		writeAPIError(response, 500, 500, "过滤规则数据库操作失败")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func filterGroupRecords(groups []filterconfig.GroupInfo, template bool) []map[string]any {
	result := make([]map[string]any, 0, len(groups))
	lines := func(value string) []string {
		if value == "" {
			return []string{}
		}
		return strings.Split(value, "\n")
	}
	for _, group := range groups {
		rules := make([]map[string]any, 0, len(group.Rules))
		for _, rule := range group.Rules {
			freeText := ""
			if rule.Free != "" {
				freeText = map[string]string{"1.0 1.0": "普通", "1.0 0.0": "免费", "2.0 0.0": "2X免费"}[rule.Free]
				if freeText == "" {
					freeText = "全部"
				}
			}
			item := map[string]any{"id": rule.ID, "group": strconv.FormatInt(rule.GroupID, 10), "name": rule.Name, "pri": rule.Priority, "include": lines(rule.Include), "exclude": lines(rule.Exclude), "size": rule.Size, "free": rule.Free, "free_text": freeText}
			if template {
				item["include"], item["exclude"] = rule.Include, rule.Exclude
			}
			rules = append(rules, item)
		}
		flag := "N"
		if group.Group.Default {
			flag = "Y"
		}
		item := map[string]any{"id": group.Group.ID, "name": group.Group.Name, "default": flag, "note": group.Group.Note, "rules": rules}
		if template {
			item["sql"] = []string{}
		}
		result = append(result, item)
	}
	return result
}
