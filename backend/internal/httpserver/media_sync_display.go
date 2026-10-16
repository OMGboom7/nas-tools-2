package httpserver

import (
	"context"
	"net/http"
	"strconv"

	"github.com/0xforee/nas-tools/backend/internal/mediasync"
)

func (api mediaSyncAPI) cachedPresence(ctx context.Context, queries []mediasync.CacheQuery) (mediasync.CacheBatch, error) {
	result := mediasync.CacheBatch{Cache: mediasync.CacheSummary{State: "unavailable", DisplayOnly: true, Libraries: []string{}}, Results: make([]mediasync.CachePresence, len(queries))}
	for i := range result.Results {
		result.Results[i].State = "unavailable"
	}
	if api.config == nil || api.store == nil {
		return result, nil
	}
	_, kind, _, digest, err := api.connection()
	if err != nil {
		return result, nil
	}
	return api.store.LookupBatch(ctx, kind, digest, queries)
}
func (api mediaSyncAPI) lookup(w http.ResponseWriter, r *http.Request) {
	// Display badges use the same authenticated audience as discovery. No
	// downloader permission or live server request is obtained by this lookup.
	if api.auth == nil {
		writeAPIError(w, 503, 503, "native authentication unavailable")
		return
	}
	if _, err := api.auth.service.VerifyToken(r.Header.Get("Authorization")); err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	var input struct {
		Items []mediasync.CacheQuery `json:"items"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid cache lookup") {
		return
	}
	if len(input.Items) == 0 || len(input.Items) > 200 {
		writeAPIError(w, 400, 400, "select 1 to 200 exact cache identities")
		return
	}
	if err := mediasync.ValidateCacheQueries(input.Items); err != nil {
		writeAPIError(w, 400, 400, "invalid display cache identities")
		return
	}
	result, err := api.cachedPresence(r.Context(), input.Items)
	if err != nil {
		writeAPIError(w, 400, 400, "cache identities invalid or display snapshot unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": result})
}
func (api mediaSyncAPI) compatSyncStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	_, kind, _, digest, err := api.connection()
	if err != nil {
		writeAPIError(w, 503, 503, "media configuration unavailable")
		return
	}
	view, err := api.store.View(r.Context(), kind, 1)
	if err != nil {
		writeAPIError(w, 503, 503, "media snapshot unavailable")
		return
	}
	message := "未同步"
	if view.Generation != "" {
		message = "电影：" + strconv.Itoa(view.Movies) + "，电视剧：" + strconv.Itoa(view.Series) + "，同步时间：" + view.Updated + "（展示快照，非实时）"
		if view.ConfigDigest != digest {
			message += "；配置已变化，此快照不代表当前媒体库"
		}
	}
	writeJSON(w, 200, map[string]any{"code": 0, "text": message, "displayOnly": true, "current": view.ConfigDigest == digest, "generation": view.Generation})
}
