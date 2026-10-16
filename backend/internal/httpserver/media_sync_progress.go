package httpserver

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

func (api mediaSyncAPI) progressReporter(id string, ctx context.Context) func(mediaserver.SyncProgress) error {
	var last time.Time
	var previous mediaserver.SyncProgress
	return func(p mediaserver.SyncProgress) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		force := p.Stage != previous.Stage || p.Library != previous.Library || p.LibrariesDone != previous.LibrariesDone
		if !force && time.Since(last) < time.Second {
			return nil
		}
		persist, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := api.store.ReportProgress(persist, id, p, time.Now()); err != nil {
			return err
		}
		last = time.Now()
		previous = p
		return nil
	}
}
func (api mediaSyncAPI) progress(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	activities, err := api.store.Activities(r.Context())
	if err != nil {
		writeAPIError(w, 503, 503, "media synchronization progress unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"activities": activities, "indeterminate": true}})
}
func (api mediaSyncAPI) compatProgress(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		writeAPIError(w, 400, 400, "invalid progress request")
		return
	}
	if r.Form.Get("type") != "mediasync" {
		writeAPIError(w, 501, 501, "this progress type has not been migrated; no legacy fallback")
		return
	}
	activities, err := api.store.Activities(r.Context())
	if err != nil {
		writeAPIError(w, 503, 503, "media synchronization progress unavailable")
		return
	}
	text := "暂无 Go 媒体库同步记录"
	value := 0
	enabled := false
	state := "idle"
	generation := ""
	if len(activities) > 0 {
		a := activities[0]
		p := a.Progress
		state = a.State
		enabled = state == "collecting"
		generation = a.SnapshotGeneration
		text = "媒体库同步 " + state + "：库 " + strconv.Itoa(p.LibrariesDone) + "/" + strconv.Itoa(p.LibrariesTotal) + "，已处理影视 " + strconv.Itoa(p.ItemsDone) + "，集数 " + strconv.Itoa(p.EpisodesDone) + "，内容页 " + strconv.Itoa(p.PagesRead) + "；" + a.Reason
		if state == "completed" && generation != "" {
			value = 100
		} else if enabled {
			text += "；已登记采集，尚未提交成功"
		}
	}
	writeJSON(w, 200, map[string]any{"code": 0, "value": value, "text": text, "enable": enabled, "state": state, "generation": generation, "indeterminate": value != 100})
}
