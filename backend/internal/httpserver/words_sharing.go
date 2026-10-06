package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/wordconfig"
)

func (api customWordsAPI) share(response http.ResponseWriter, request *http.Request, action string) {
	if action == "item/export" {
		values := []string{}
		if raw := request.Form.Get("ids_info"); raw != "" {
			values = strings.Split(raw, "@")
		}
		selection, err := parseWordSelection(values)
		if err != nil {
			writeAPIError(response, 400, 400, "识别词选择无效")
			return
		}
		code, err := api.store.Export(request.Context(), selection, request.Form.Get("note"))
		if err != nil {
			writeAPIError(response, 400, 400, "识别词导出失败")
			return
		}
		writeJSON(response, 200, map[string]any{"code": 0, "string": code})
		return
	}
	groups, note, err := wordconfig.ParseShare(request.Form.Get("import_code"))
	if err != nil {
		writeAPIError(response, 400, 400, "识别词分享代码无效")
		return
	}
	if action == "item/analyse" {
		keys := make([]string, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := []map[string]any{}
		for _, key := range keys {
			group := groups[key]
			name, link := group.Title, ""
			if group.Year != "" {
				name += "（" + group.Year + "）"
			}
			if group.TMDBID > 0 {
				kind := "tv"
				if group.Type == 1 {
					kind = "movie"
				}
				link = "https://www.themoviedb.org/" + kind + "/" + strconv.FormatInt(group.TMDBID, 10)
			}
			var seasons any = group.Seasons
			if group.Seasons == 0 {
				seasons = ""
			}
			result = append(result, map[string]any{"id": group.ID, "name": name, "link": link, "type": group.Type, "seasons": seasons, "words": group.Words})
		}
		writeJSON(response, 200, map[string]any{"code": 0, "groups": result, "note_string": note})
		return
	}
	values := request.Form["ids_info"]
	if len(values) == 1 && strings.HasPrefix(strings.TrimSpace(values[0]), "[") {
		if err := json.Unmarshal([]byte(values[0]), &values); err != nil {
			writeAPIError(response, 400, 400, "识别词选择无效")
			return
		}
	}
	selection, err := parseWordSelection(values)
	if err == nil {
		err = api.store.Import(request.Context(), groups, selection)
	}
	if err != nil {
		if errors.Is(err, wordconfig.ErrDuplicate) {
			writeJSON(response, 200, map[string]any{"code": 1, "msg": "识别词已存在"})
		} else if errors.Is(err, wordconfig.ErrInvalidShare) {
			writeAPIError(response, 400, 400, "识别词选择或分享代码无效")
		} else {
			writeAPIError(response, 502, 502, "识别词导入失败")
		}
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "msg": ""})
}

func parseWordSelection(values []string) ([]wordconfig.Selection, error) {
	if len(values) > 1000 {
		return nil, wordconfig.ErrInvalidShare
	}
	result := []wordconfig.Selection{}
	for _, value := range values {
		parts := strings.Split(value, "_")
		if len(parts) != 2 {
			return nil, wordconfig.ErrInvalidShare
		}
		groupID, groupErr := strconv.ParseInt(parts[0], 10, 64)
		wordID, wordErr := strconv.ParseInt(parts[1], 10, 64)
		if groupErr != nil || wordErr != nil || groupID != -1 && groupID <= 0 || wordID <= 0 {
			return nil, wordconfig.ErrInvalidShare
		}
		result = append(result, wordconfig.Selection{GroupID: groupID, WordID: wordID})
	}
	return result, nil
}
