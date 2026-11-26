package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/recognitioncache"
)

func (api configurationAPI) recognitionCache(w http.ResponseWriter, r *http.Request) {
	if !api.requireAdministrator(w, r) {
		return
	}
	var input struct {
		Key, Title, Search string
		Page               int
		Expected           *recognitioncache.ExpectedEntry
		Confirm            bool
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
		writeAPIError(w, 400, 400, "缓存请求格式无效")
		return
	}
	op := path.Base(r.URL.Path)
	if (op == "delete" || op == "rename") && input.Expected == nil || op == "clear" && !input.Confirm {
		writeAPIError(w, 400, 400, "缺少所选缓存条目或清空确认")
		return
	}
	expire := false
	if op == "list" {
		snapshot, err := api.store.Snapshot()
		if err != nil {
			writeAPIError(w, 500, 500, "无法读取缓存设置")
			return
		}
		expire = recognitioncache.ExpireEnabled(snapshot)
	}
	q := recognitioncache.Request{Key: input.Key, Title: input.Title, Search: input.Search, Page: input.Page, Size: 25, Expected: input.Expected}
	data, err := recognitioncache.Run(r.Context(), (config.Application{Path: api.path}).UserDatabasePath(), op, q, expire)
	switch {
	case errors.Is(err, recognitioncache.ErrChanged):
		writeAPIError(w, 409, 409, "缓存条目已变化，请刷新后重试")
	case errors.Is(err, recognitioncache.ErrRequest):
		writeAPIError(w, 400, 400, "缓存条目参数无效")
	case err != nil:
		writeAPIError(w, 500, 500, "无法读取或更新识别缓存")
	default:
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
	}
}
