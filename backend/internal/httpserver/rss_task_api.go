package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/rssparserconfig"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
)

type rssTaskAPI struct {
	tasks   *rsstaskconfig.Store
	parsers *rssparserconfig.Store
	filters *filterconfig.Store
}

func (api rssTaskAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	if request.URL.Path == "/api/v1/rss/list" {
		api.list(response, ctx)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 256<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS task request")
		return
	}
	id := int64(0)
	rawID := strings.TrimSpace(request.Form.Get("id"))
	if request.URL.Path == "/api/v1/rss/item/set" {
		rawID = strings.TrimSpace(request.Form.Get("taskid"))
	}
	if raw := rawID; raw != "" {
		var err error
		id, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS task id")
			return
		}
	}
	if request.URL.Path != "/api/v1/rss/update" && id == 0 {
		writeAPIError(response, http.StatusBadRequest, 400, "RSS task id is required")
		return
	}
	switch request.URL.Path {
	case "/api/v1/rss/info":
		task, err := api.tasks.Get(ctx, id)
		if errors.Is(err, rsstaskconfig.ErrNotFound) {
			writeJSON(response, http.StatusOK, map[string]any{"code": 0, "detail": map[string]any{}})
			return
		}
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "RSS task is unavailable")
			return
		}
		names, err := api.filterNames(ctx)
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "RSS task filters are unavailable")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "detail": rssTaskView(task, names)})
	case "/api/v1/rss/delete":
		if err := api.tasks.Delete(ctx, id); err != nil {
			if errors.Is(err, rsstaskconfig.ErrNotFound) {
				writeJSON(response, http.StatusOK, map[string]any{"code": 1})
				return
			}
			writeAPIError(response, http.StatusBadGateway, 502, "RSS task could not be deleted")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0})
	case "/api/v1/rss/item/history":
		items, err := api.tasks.History(ctx, id)
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "RSS task history is unavailable")
			return
		}
		if len(items) == 0 {
			writeJSON(response, http.StatusOK, map[string]any{"code": 1, "msg": "无下载记录"})
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "data": items, "count": len(items)})
	case "/api/v1/rss/item/set":
		var articles []rsstaskconfig.Article
		raw := request.Form.Get("articles")
		if raw == "" || raw == "null" || raw == "[]" {
			writeJSON(response, http.StatusOK, map[string]any{"code": 2})
			return
		}
		if json.Unmarshal([]byte(raw), &articles) != nil || len(articles) == 0 || len(articles) > 100 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS articles")
			return
		}
		for _, article := range articles {
			if strings.TrimSpace(article.Title) == "" || len(article.Title) > 512 || len(article.Enclosure) > 4096 || len(article.Year) > 20 {
				writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS article")
				return
			}
		}
		flag := request.Form.Get("flag")
		if flag != "set_finished" && flag != "set_unfinish" {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS article action")
			return
		}
		if err := api.tasks.SetArticles(ctx, id, flag, articles); err != nil {
			if errors.Is(err, rsstaskconfig.ErrNotFound) {
				writeJSON(response, http.StatusOK, map[string]any{"code": 1})
				return
			}
			writeAPIError(response, http.StatusBadGateway, 502, "RSS article state could not be saved")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0})
	case "/api/v1/rss/update":
		task, err := rssTaskFromForm(request.Form, id)
		if err != nil {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS task configuration")
			return
		}
		if id > 0 {
			old, err := api.tasks.Get(ctx, id)
			if err != nil && !errors.Is(err, rsstaskconfig.ErrNotFound) {
				writeAPIError(response, http.StatusBadGateway, 502, "RSS task is unavailable")
				return
			}
			if err == nil {
				task.Note = mergedRSSNote(old.Note, task.Note)
			}
		}
		if _, err := api.tasks.Upsert(ctx, task); err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "RSS task could not be saved")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0})
	default:
		writeAPIError(response, http.StatusNotFound, 404, "RSS task endpoint not found")
	}
}

func (api rssTaskAPI) list(response http.ResponseWriter, ctx context.Context) {
	tasks, err := api.tasks.List(ctx)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "RSS task list is unavailable")
		return
	}
	parsers, err := api.parsers.List(ctx)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "RSS parser list is unavailable")
		return
	}
	names, err := api.filterNames(ctx)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "RSS task filters are unavailable")
		return
	}
	result := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, rssTaskView(task, names))
	}
	// Preserve the legacy list envelope, including its historical success=false.
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": false, "data": map[string]any{"tasks": result, "parsers": parsers}})
}

func (api rssTaskAPI) filterNames(ctx context.Context) (map[string]string, error) {
	groups, err := api.filters.Groups(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(groups))
	for _, group := range groups {
		names[strconv.FormatInt(group.ID, 10)] = group.Name
	}
	return names, nil
}

func rssTaskView(task rsstaskconfig.Task, names map[string]string) map[string]any {
	note := map[string]any{}
	_ = json.Unmarshal([]byte(task.Note), &note)
	uses := task.Uses
	if uses == "S" {
		uses = "R"
	}
	usesText := map[string]string{"D": "下载", "R": "订阅", "S": "搜索"}[task.Uses]
	proxy := note["proxy"] == true || note["proxy"] == "Y" || note["proxy"] == "1"
	savePath := task.SavePath
	if savePath == "" {
		savePath = text(note["save_path"])
	}
	recognization := task.Recognization
	if recognization == "" {
		recognization = text(note["recognization"])
	}
	if recognization == "" {
		recognization = "Y"
	}
	var downloadSetting any = task.DownloadSetting
	if task.DownloadSetting == 0 {
		downloadSetting = ""
	}
	return map[string]any{
		"id": task.ID, "name": task.Name, "address": rssStringList(task.Address),
		"parser": rssValueList(task.Parser), "proxy": proxy, "interval": task.Interval,
		"uses": uses, "uses_text": usesText, "include": task.Include, "exclude": task.Exclude,
		"filter": task.Filter, "filter_name": names[task.Filter], "update_time": task.UpdateTime,
		"counter": task.ProcessCount, "state": task.State == "Y" || task.State == "1" || strings.EqualFold(task.State, "true"),
		"save_path": savePath, "download_setting": downloadSetting, "recognization": recognization,
		"over_edition": task.OverEdition,
		"sites":        rssObject(task.Sites, map[string]any{"rss_sites": []string{}, "search_sites": []string{}}),
		"filter_args":  rssObject(task.FilterArgs, map[string]any{"restype": "", "pix": "", "team": ""}),
	}
}

func rssStringList(raw string) []string {
	var result []string
	if json.Unmarshal([]byte(raw), &result) == nil && result != nil {
		return result
	}
	if raw == "" {
		return []string{}
	}
	return []string{raw}
}

func rssValueList(raw string) []any {
	var result []any
	if json.Unmarshal([]byte(raw), &result) == nil && result != nil {
		return result
	}
	if raw == "" {
		return []any{}
	}
	return []any{raw}
}

func rssObject(raw string, fallback map[string]any) map[string]any {
	var result map[string]any
	if json.Unmarshal([]byte(raw), &result) == nil && result != nil {
		return result
	}
	return fallback
}

func mergedRSSNote(original, replacement string) string {
	note := rssObject(original, map[string]any{})
	for key, value := range rssObject(replacement, map[string]any{}) {
		note[key] = value
	}
	encoded, _ := json.Marshal(note)
	return string(encoded)
}

func rssTaskFromForm(form url.Values, id int64) (rsstaskconfig.Task, error) {
	var task rsstaskconfig.Task
	task.ID = id
	task.Name = strings.TrimSpace(form.Get("name"))
	task.Uses = strings.TrimSpace(form.Get("uses"))
	task.Interval = strings.TrimSpace(form.Get("interval"))
	task.State = strings.TrimSpace(form.Get("state"))
	if task.Name == "" || len(task.Name) > 200 || task.Uses != "D" && task.Uses != "R" || len(task.Interval) > 100 || task.Interval == "" || task.State != "Y" && task.State != "N" && task.State != "1" && task.State != "0" {
		return task, errors.New("invalid task fields")
	}
	addresses, parsers, err := rssAddressesAndParsers(form)
	if err != nil {
		return task, err
	}
	encoded, _ := json.Marshal(addresses)
	task.Address = string(encoded)
	encoded, _ = json.Marshal(parsers)
	task.Parser = string(encoded)
	task.Include = form.Get("include")
	task.Exclude = form.Get("exclude")
	task.Filter = form.Get("rule")
	if task.Filter == "" {
		task.Filter = form.Get("filterrule")
	}
	task.SavePath = form.Get("save_path")
	task.Recognization = form.Get("recognization")
	if len(task.Include) > 4096 || len(task.Exclude) > 4096 || len(task.Filter) > 100 || len(task.SavePath) > 4096 {
		return task, errors.New("task field too long")
	}
	if raw := strings.TrimSpace(form.Get("download_setting")); raw != "" {
		task.DownloadSetting, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || task.DownloadSetting < 0 {
			return task, errors.New("invalid download setting")
		}
	}
	if raw := strings.TrimSpace(form.Get("over_edition")); raw != "" {
		task.OverEdition, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || task.OverEdition < 0 {
			return task, errors.New("invalid over edition")
		}
	}
	if task.Uses == "R" {
		task.Sites = form.Get("sites")
		if task.Sites == "" {
			task.Sites = `{"rss_sites":[],"search_sites":[]}`
		}
		if !json.Valid([]byte(task.Sites)) || len(task.Sites) > 64<<10 {
			return task, errors.New("invalid sites")
		}
		filterArgs := map[string]string{"restype": form.Get("restype"), "pix": form.Get("pix"), "team": form.Get("team")}
		encoded, _ = json.Marshal(filterArgs)
		task.FilterArgs = string(encoded)
	} else {
		task.Sites = `null`
		task.FilterArgs = `null`
	}
	note := map[string]any{"proxy": form.Get("proxy")}
	encoded, _ = json.Marshal(note)
	task.Note = string(encoded)
	task.UpdateTime = time.Now().Format("2006-01-02 15:04:05")
	return task, nil
}

func rssAddressesAndParsers(form url.Values) ([]string, []string, error) {
	values := make(map[int]struct{ address, parser string })
	if raw := form.Get("address_parser"); raw != "" {
		var object map[string]string
		if json.Unmarshal([]byte(raw), &object) != nil {
			return nil, nil, errors.New("invalid address parser")
		}
		for key, value := range object {
			form.Set(key, value)
		}
	}
	for key, valuesForKey := range form {
		if key == "address_parser" {
			continue
		}
		parts := strings.SplitN(key, "_", 2)
		if len(parts) != 2 || parts[0] != "address" && parts[0] != "parser" {
			continue
		}
		index, err := strconv.Atoi(parts[1])
		if err != nil || index < 0 || index > 100 || len(valuesForKey) != 1 {
			return nil, nil, errors.New("invalid address parser index")
		}
		pair := values[index]
		if parts[0] == "address" {
			pair.address = strings.TrimSpace(valuesForKey[0])
		} else {
			pair.parser = strings.TrimSpace(valuesForKey[0])
		}
		values[index] = pair
	}
	if len(values) == 0 && form.Get("address") != "" && form.Get("parser") != "" {
		values[0] = struct{ address, parser string }{strings.TrimSpace(form.Get("address")), strings.TrimSpace(form.Get("parser"))}
	}
	if len(values) == 0 || len(values) > 20 {
		return nil, nil, errors.New("missing addresses")
	}
	indices := make([]int, 0, len(values))
	for index := range values {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	addresses, parsers := make([]string, 0, len(indices)), make([]string, 0, len(indices))
	for _, index := range indices {
		pair := values[index]
		if pair.address == "" || len(pair.address) > 4096 || pair.parser == "" || len(pair.parser) > 30 {
			return nil, nil, errors.New("invalid address or parser")
		}
		addresses = append(addresses, pair.address)
		parsers = append(parsers, pair.parser)
	}
	return addresses, parsers, nil
}
