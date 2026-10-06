package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
	"github.com/dlclark/regexp2"
)

type rssNameAPI struct {
	tasks       *rsstaskconfig.Store
	filters     *filterconfig.Store
	recognition mediaNameAPI
}

func (api rssNameAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if api.tasks == nil || api.filters == nil {
		writeAPIError(response, 503, 503, "native RSS recognition is unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 256<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, 400, 400, "invalid RSS name test request")
		return
	}
	id, err := strconv.ParseInt(request.Form.Get("taskid"), 10, 64)
	title := request.Form.Get("title")
	if err != nil || id <= 0 || strings.TrimSpace(title) == "" || len(title) > 64<<10 {
		writeAPIError(response, 400, 400, "invalid RSS name test parameters")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
	defer cancel()
	task, err := api.tasks.Get(ctx, id)
	if err != nil {
		status := 500
		if errors.Is(err, rsstaskconfig.ErrNotFound) {
			status = 404
		}
		writeAPIError(response, status, status, "RSS task is unavailable")
		return
	}
	result, failure := api.recognition.recognize(ctx, title, "")
	if failure != nil {
		writeAPIError(response, failure.status, failure.status, failure.message)
		return
	}
	if result == nil {
		writeUnrecognizedMedia(response)
		return
	}
	matched := true
	for _, condition := range []struct {
		pattern string
		exclude bool
	}{{task.Include, false}, {task.Exclude, true}} {
		if condition.pattern == "" {
			continue
		}
		if len(condition.pattern) > 16<<10 {
			writeAPIError(response, 422, 422, "RSS condition too large")
			return
		}
		compiled, err := regexcompat.Compile(condition.pattern, regexp2.IgnoreCase)
		if err != nil {
			writeAPIError(response, 422, 422, "invalid RSS condition")
			return
		}
		match, err := compiled.MatchString(text(result.data["rev_string"]))
		if err != nil {
			writeAPIError(response, 422, 422, "RSS condition matching failed")
			return
		}
		matched = matched && (match != condition.exclude)
	}
	group := int64(0)
	if task.Uses == "D" && task.Filter != "" {
		group, err = strconv.ParseInt(task.Filter, 10, 64)
		if err != nil || group < -1 {
			writeAPIError(response, 422, 422, "invalid RSS rule group")
			return
		}
	}
	filter, err := api.filters.Match(ctx, group, filterconfig.TorrentMetadata{Title: text(result.data["rev_string"]), Movie: result.kind == "movie", Episodes: result.meta.Episodes.Count})
	if err != nil {
		writeAPIError(response, 422, 422, "RSS rule matching failed")
		return
	}
	matched = matched && filter.Matched
	coverage, err := api.recognition.service.nativeMediaExistence(ctx, result.meta, result.detail, result.kind, text(result.data["category"]))
	if err != nil {
		status, message := 502, "media inventory lookup failed"
		if errors.Is(err, errLocalMediaUnavailable) {
			status, message = 503, "media server or local media directories must be configured"
		}
		if errors.Is(err, mediaserver.ErrUnknownCoverage) {
			status, message = 422, "season episode totals are unavailable"
		}
		writeAPIError(response, status, status, message)
		return
	}
	result.data["match_flag"], result.data["exist_flag"] = matched, coverage.Complete
	writeJSON(response, 200, map[string]any{"code": 0, "data": result.data})
}
