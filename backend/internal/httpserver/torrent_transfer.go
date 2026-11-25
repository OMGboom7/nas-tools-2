package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

type torrentTransferAPI struct {
	downloaders *downloaderconfig.Store
	database    string
	login       *siteLoginAPI
}

func (api torrentTransferAPI) record(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.login.actor(w, r, false); !ok {
		return
	}
	id := r.PathValue("id")
	if !torrenttransfer.Hex(id, 64) {
		writeAPIError(w, 400, 400, "转种回执编号无效")
		return
	}
	if api.database == "" {
		writeAPIError(w, 503, 503, "转种记录库不可用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: api.database}).String()+"?mode=ro")
	if err != nil {
		writeAPIError(w, 503, 503, "转种记录库不可用")
		return
	}
	defer db.Close()
	record, err := torrenttransfer.ReadStatus(ctx, db, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeAPIError(w, 404, 404, "未找到转种回执")
		return
	}
	if err != nil {
		writeAPIError(w, 503, 503, "读取转种记录失败")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "result": record})
}
