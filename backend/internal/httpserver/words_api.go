package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/wordconfig"
)

type customWordsAPI struct {
	store     *wordconfig.Store
	auth      *nativeAuthentication
	config    *config.Store
	transport http.RoundTripper
}

func (api customWordsAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	claims, err := api.auth.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, 401, 401, "登录状态无效或已过期")
		return
	}
	action := strings.TrimPrefix(request.URL.Path, "/api/v1/words/")
	if action != "list" && action != "item/info" && action != "item/export" && action != "item/analyse" && !api.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(response, 403, 403, "仅管理员可以修改识别词")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 2<<20)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, 400, 400, "识别词请求格式错误")
		return
	}
	invalid := func() { writeAPIError(response, 400, 400, "识别词参数无效") }
	ctx := request.Context()
	switch action {
	case "item/export", "item/analyse", "item/import":
		api.share(response, request, action)
		return
	case "group/add":
		api.addGroup(response, request)
		return
	case "list":
		groups, words, readErr := api.store.List(ctx)
		if readErr != nil {
			err = readErr
			break
		}
		byGroup := map[int64][]wordconfig.Word{}
		for _, word := range words {
			byGroup[word.GroupID] = append(byGroup[word.GroupID], word)
		}
		groupWords := func(id int64) []wordconfig.Word {
			if items := byGroup[id]; items != nil {
				return items
			}
			return []wordconfig.Word{}
		}
		result := []map[string]any{{"id": "-1", "name": "通用", "link": "", "type": "1", "seasons": "0", "words": groupWords(-1)}}
		for _, group := range groups {
			kind := "tv"
			if group.Type == 1 {
				kind = "movie"
			}
			result = append(result, map[string]any{"id": group.ID, "name": group.Title + " (" + group.Year + ")", "link": "https://www.themoviedb.org/" + kind + "/" + strconv.FormatInt(group.TMDBID, 10), "type": group.Type, "seasons": group.Seasons, "words": groupWords(group.ID)})
		}
		writeJSON(response, 200, map[string]any{"code": 0, "result": result})
		return
	case "item/info":
		id, parseErr := strconv.ParseInt(request.Form.Get("wid"), 10, 64)
		if parseErr != nil || id <= 0 {
			invalid()
			return
		}
		word, readErr := api.store.Get(ctx, id)
		if errors.Is(readErr, wordconfig.ErrNotFound) {
			writeJSON(response, 200, map[string]any{"code": 0, "data": map[string]any{}})
			return
		}
		if readErr != nil {
			err = readErr
			break
		}
		writeJSON(response, 200, map[string]any{"code": 0, "data": word})
		return
	case "item/update":
		word, valid := parseCustomWord(request)
		if !valid {
			invalid()
			return
		}
		_, err = api.store.Save(ctx, word)
	case "item/delete", "group/delete":
		key := "id"
		if action == "group/delete" {
			key = "gid"
		}
		id, parseErr := strconv.ParseInt(request.Form.Get(key), 10, 64)
		if parseErr != nil || id <= 0 {
			invalid()
			return
		}
		if action == "group/delete" {
			err = api.store.DeleteGroup(ctx, id)
		} else {
			err = api.store.Delete(ctx, id)
		}
	case "item/status":
		flag := request.Form.Get("flag")
		enabled := 0
		switch flag {
		case "1", "enable":
			enabled = 1
		case "0", "disable":
		default:
			invalid()
			return
		}
		ids, valid := customWordIDs(request.Form["ids_info"])
		if !valid {
			invalid()
			return
		}
		err = api.store.SetStatus(ctx, ids, enabled)
	default:
		writeAPIError(response, 404, 404, "未知识别词操作")
		return
	}
	if err != nil {
		if errors.Is(err, wordconfig.ErrDuplicate) {
			writeJSON(response, 200, map[string]any{"code": 1, "msg": "识别词已存在"})
		} else if errors.Is(err, wordconfig.ErrNotFound) {
			writeAPIError(response, 404, 404, "识别词或识别词组不存在")
		} else {
			writeAPIError(response, 502, 502, "识别词存储操作失败")
		}
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "msg": ""})
}

func parseCustomWord(request *http.Request) (wordconfig.Word, bool) {
	form := request.Form
	word := wordconfig.Word{Replaced: form.Get("new_replaced"), Replace: form.Get("new_replace"), Front: form.Get("new_front"), Back: form.Get("new_back"), Offset: form.Get("new_offset"), Help: form.Get("new_help"), Season: -1}
	var err error
	word.ID, err = strconv.ParseInt(form.Get("id"), 10, 64)
	if err != nil || word.ID < 0 {
		return word, false
	}
	word.GroupID, err = strconv.ParseInt(form.Get("gid"), 10, 64)
	if err != nil || word.GroupID != -1 && word.GroupID <= 0 {
		return word, false
	}
	word.Type, err = strconv.Atoi(form.Get("type"))
	if err != nil || word.Type < 1 || word.Type > 4 {
		return word, false
	}
	word.Enabled, err = strconv.Atoi(form.Get("enabled"))
	if err != nil || word.Enabled < 0 || word.Enabled > 1 {
		return word, false
	}
	if form.Get("regex") != "" {
		word.Regex, err = strconv.Atoi(form.Get("regex"))
		if err != nil || word.Regex < 0 || word.Regex > 1 {
			return word, false
		}
	}
	if form.Get("group_type") == "1" {
		word.Season = -2
	} else if form.Get("group_type") != "2" {
		return word, false
	} else if form.Get("season") != "" {
		word.Season, err = strconv.Atoi(form.Get("season"))
		if err != nil || word.Season < -1 || word.Season > 10000 {
			return word, false
		}
	}
	for _, value := range []string{word.Replaced, word.Replace, word.Front, word.Back, word.Offset, word.Help} {
		if len(value) > 16<<10 || strings.ContainsRune(value, 0) {
			return word, false
		}
	}
	if word.Type != 4 && word.Replaced == "" {
		return word, false
	}
	if word.Type == 3 || word.Type == 4 {
		if !strings.Contains(word.Offset, "EP") || strings.Trim(strings.ReplaceAll(word.Offset, "EP", ""), "+-*/0123456789") != "" {
			return word, false
		}
	} else {
		word.Front, word.Back, word.Offset = "", "", ""
	}
	if word.Type == 1 || word.Type == 4 {
		word.Replace = ""
	}
	if word.Type == 4 {
		word.Replaced = ""
	}
	return word, true
}

func customWordIDs(values []string) ([]int64, bool) {
	if len(values) == 1 && strings.HasPrefix(strings.TrimSpace(values[0]), "[") {
		var decoded []any
		if json.Unmarshal([]byte(values[0]), &decoded) != nil {
			return nil, false
		}
		values = make([]string, 0, len(decoded))
		for _, value := range decoded {
			values = append(values, text(value))
		}
	}
	if len(values) > 1000 {
		return nil, false
	}
	ids := []int64{}
	for _, value := range values {
		if index := strings.LastIndexByte(value, '_'); index >= 0 {
			value = value[index+1:]
		}
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || id <= 0 {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

func (api customWordsAPI) addGroup(response http.ResponseWriter, request *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(request.Form.Get("tmdb_id")), 10, 64)
	kind := request.Form.Get("tmdb_type")
	if err != nil || id <= 0 || kind != "movie" && kind != "tv" {
		writeAPIError(response, 400, 400, "媒体类型或 TMDB ID 无效")
		return
	}
	groupType := 1
	if kind == "tv" {
		groupType = 2
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	exists, err := api.store.HasGroup(ctx, id, groupType)
	if err != nil {
		writeAPIError(response, 502, 502, "识别词组存储不可用")
		return
	}
	if exists {
		writeJSON(response, 200, map[string]any{"code": 1, "msg": "识别词组（TMDB ID）已存在"})
		return
	}
	detail, err := fetchNativeTMDBDetails(ctx, api.config, api.transport, kind, strconv.FormatInt(id, 10))
	if err != nil {
		writeJSON(response, 200, map[string]any{"code": 1, "msg": "添加失败，无法查询到TMDB信息"})
		return
	}
	title, date, seasons := strings.TrimSpace(detail.Title), detail.ReleaseDate, 0
	if kind == "tv" {
		title, date, seasons = strings.TrimSpace(detail.Name), detail.FirstAirDate, detail.SeasonCount
	}
	if title == "" || len(title) > 1000 || seasons < 0 || seasons > 10000 {
		writeJSON(response, 200, map[string]any{"code": 1, "msg": "添加失败，TMDB信息无效"})
		return
	}
	year := ""
	if len(date) >= 4 {
		year = date[:4]
	}
	err = api.store.AddGroup(ctx, wordconfig.Group{Title: title, Year: year, Type: groupType, TMDBID: id, Seasons: seasons})
	if errors.Is(err, wordconfig.ErrDuplicate) {
		writeJSON(response, 200, map[string]any{"code": 1, "msg": "识别词组（TMDB ID）已存在"})
		return
	}
	if err != nil {
		writeAPIError(response, 502, 502, "识别词组保存失败")
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "msg": ""})
}
