package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type transferHistoryAPI struct{ database string }

const transferHistoryColumns = "ID,MODE,TYPE,CATEGORY,TMDBID,TITLE,YEAR,SEASON_EPISODE,SOURCE,SOURCE_PATH,SOURCE_FILENAME,DEST,DEST_PATH,DEST_FILENAME,DATE"

var transferModes = map[string]string{
	"硬链接": "link", "软链接": "softlink", "复制": "copy", "移动": "move",
	"Rclone复制": "rclonecopy", "Rclone移动": "rclone", "Minio复制": "miniocopy", "Minio移动": "minio",
}

func (api transferHistoryAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	if api.database == "" {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native database is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: api.database}).String()+"?mode=ro")
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "transfer history is unavailable")
		return
	}
	defer db.Close()
	switch request.URL.Path {
	case "/api/v1/organization/history/list":
		api.list(response, request, ctx, db)
	case "/api/v1/organization/history/statistics":
		api.statistics(response, ctx, db)
	default:
		writeAPIError(response, http.StatusNotFound, 404, "transfer history endpoint not found")
	}
}

func (api transferHistoryAPI) list(response http.ResponseWriter, request *http.Request, ctx context.Context, db *sql.DB) {
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid history request")
		return
	}
	page, pageSize := 1, 30
	if raw := request.Form.Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid history page")
			return
		}
	}
	if raw := request.Form.Get("pagenum"); raw != "" {
		var err error
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 200 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid history page size")
			return
		}
	}
	keyword := strings.TrimSpace(request.Form.Get("keyword"))
	if len(keyword) > 256 || page > 1000000 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid history request")
		return
	}
	where, args := "", []any{}
	if keyword != "" {
		where = " WHERE SOURCE_FILENAME LIKE ? OR TITLE LIKE ?"
		pattern := "%" + keyword + "%"
		args = append(args, pattern, pattern)
	}
	var total int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM TRANSFER_HISTORY"+where, args...).Scan(&total)
	if isMissingTransferHistory(err) {
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "total": 0, "result": []any{}, "totalPage": 1, "pageNum": pageSize})
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "transfer history is unavailable")
		return
	}
	queryArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := db.QueryContext(ctx, "SELECT "+transferHistoryColumns+" FROM TRANSFER_HISTORY"+where+" ORDER BY DATE DESC, ID DESC LIMIT ? OFFSET ?", queryArgs...)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "transfer history is unavailable")
		return
	}
	defer rows.Close()
	columns := strings.Split(transferHistoryColumns, ",")
	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range dest {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "transfer history is unavailable")
			return
		}
		item := make(map[string]any, len(columns)+2)
		for i, name := range columns {
			if raw, ok := values[i].([]byte); ok {
				item[name] = string(raw)
			} else {
				item[name] = values[i]
			}
		}
		mode, _ := item["MODE"].(string)
		item["SYNC_MODE"], item["RMT_MODE"] = item["MODE"], transferModes[mode]
		if mode == "" || transferModes[mode] == "" {
			item["RMT_MODE"] = nil
		}
		result = append(result, item)
	}
	if rows.Err() != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "transfer history is unavailable")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "total": total, "result": result, "totalPage": total/pageSize + 1, "pageNum": pageSize})
}

func (api transferHistoryAPI) statistics(response http.ResponseWriter, ctx context.Context, db *sql.DB) {
	begin := time.Now().AddDate(0, 0, -90).Format("2006-01-02 15:04:05")
	rows, err := db.QueryContext(ctx, `SELECT TYPE, substr(DATE,1,10), COUNT(*) FROM TRANSFER_HISTORY WHERE DATE > ? GROUP BY TYPE, substr(DATE,1,10) ORDER BY DATE`, begin)
	labels := []string{}
	movies, tvs, anime := []int{}, []int{}, []int{}
	if isMissingTransferHistory(err) {
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "Labels": labels, "MovieNums": movies, "TvNums": tvs, "AnimeNums": anime})
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "transfer statistics are unavailable")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var kind, date sql.NullString
		var count int
		if err := rows.Scan(&kind, &date, &count); err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "transfer statistics are unavailable")
			return
		}
		if count == 0 {
			continue
		}
		found := false
		for _, label := range labels {
			if label == date.String {
				found = true
				break
			}
		}
		if !found {
			labels = append(labels, date.String)
		}
		switch kind.String {
		case "电影":
			movies, tvs, anime = append(movies, count), append(tvs, 0), append(anime, 0)
		case "电视剧":
			movies, tvs, anime = append(movies, 0), append(tvs, count), append(anime, 0)
		default:
			movies, tvs, anime = append(movies, 0), append(tvs, 0), append(anime, count)
		}
	}
	if rows.Err() != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "transfer statistics are unavailable")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "Labels": labels, "MovieNums": movies, "TvNums": tvs, "AnimeNums": anime})
}

func isMissingTransferHistory(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && strings.Contains(err.Error(), "no such table: TRANSFER_HISTORY")
}
