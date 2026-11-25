package httpserver

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

func indexerLegacyFields(detail pluginConfigDetail) []any {
	rows := []any{}
	for _, f := range detail.Fields {
		column := map[string]any{"title": f.Title, "tooltip": f.Tooltip, "type": f.Type, "configured": f.Configured}
		if f.Type == "switch" {
			column["id"] = f.Key
		} else if f.Type == "multiselect" {
			choices := map[string]any{}
			for _, choice := range f.Options {
				choices[choice.Value] = map[string]string{"name": choice.Label}
			}
			column["type"], column["id"], column["content"] = "form-selectgroup", f.Key, choices
		} else {
			field := map[string]any{"id": f.Key, "placeholder": f.Placeholder, "configured": f.Configured}
			if f.Type == "select" {
				choices := map[string]string{}
				for _, choice := range f.Options {
					choices[choice.Value] = choice.Label
				}
				field["options"] = choices
			}
			column["content"] = []any{field}
		}
		rows = append(rows, []any{column})
	}
	return []any{map[string]any{"type": "div", "content": rows}}
}

func (s pluginService) nativePluginPageRoute(fallback http.Handler, action bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
		if err != nil {
			writeAPIError(w, 400, 400, "invalid plugin page request")
			return
		}
		probe := r.Clone(r.Context())
		probe.Body = io.NopCloser(bytes.NewReader(body))
		if probe.ParseForm() != nil {
			writeAPIError(w, 400, 400, "invalid plugin page request")
			return
		}
		id := strings.TrimSpace(probe.PostForm.Get("id"))
		if s.autoSub != nil && id == autoSubID {
			if action {
				writeAPIError(w, 400, 400, "字幕结果只支持查看")
			} else {
				s.autoSub.page(w, probe, s, true)
			}
			return
		}
		if s.disk != nil && id == diskID {
			if action {
				writeAPIError(w, 400, 400, "查重结果只支持查看")
			} else {
				s.disk.page(w, probe, s, true)
			}
			return
		}
		if s.helper != nil && id == helperID {
			if action {
				writeAPIError(w, 400, 400, "下载器助手扩展页只支持查看")
			} else {
				s.helperPage(w, probe, true)
			}
			return
		}
		if s.iyuu != nil && id == "IYUUAutoSeed" {
			if action {
				writeAPIError(w, 400, 400, "不支持的 IYUU 扩展页操作")
			} else {
				s.iyuu.partnerPage(w, probe, true)
			}
			return
		}
		if s.nativeArchivePage(id) {
			if !action {
				s.serveArchivePage(w, probe, true)
			} else if probe.PostForm.Get("action") == "delete" {
				s.removeArchiveRecord(w, probe, probe.PostForm.Get("record_id"), probe.PostForm.Get("confirmation"), probe.PostForm.Get("proof"))
			} else {
				writeAPIError(w, 400, 400, "不支持的插件扩展页操作")
			}
			return
		}
		if s.nativeSubscriptionHistory(id) {
			if !action {
				s.subscriptionHistoryPage(w, probe, id, true)
			} else if probe.PostForm.Get("action") == "delete" {
				s.removeSubscriptionHistory(w, probe, id, probe.PostForm.Get("record_id"))
			} else {
				writeAPIError(w, 400, 400, "不支持的插件扩展页操作")
			}
			return
		}
		if action {
			r.Body = io.NopCloser(bytes.NewReader(body))
			fallback.ServeHTTP(w, r)
			return
		}
		if s.nativeTorrentRemover(id) {
			s.serveTorrentRemoverPlugin(w, probe, "page")
			return
		}
		if s.nativeTorrentMark(id) {
			s.serveTorrentMarkPlugin(w, probe, "page")
			return
		}
		if s.nativeSyncTimer(id) {
			s.serveSyncTimerPlugin(w, probe, "page")
			return
		}
		if s.nativeAutoBackup(id) {
			s.serveAutoBackupPlugin(w, probe, "page")
			return
		}
		if s.nativeCookieCloud(id) {
			probe.SetPathValue("id", id)
			s.serveCookieCloudPlugin(w, probe, "legacy-page")
			return
		}
		if s.nativeSignin(id) {
			probe.SetPathValue("id", id)
			s.serveSigninPlugin(w, probe, "page")
			return
		}
		if s.nativeSpeedLimiter(id) {
			probe.SetPathValue("id", id)
			s.serveSpeedLimiterPlugin(w, probe, "page")
			return
		}
		if s.nativeMovieLike(id) {
			probe.SetPathValue("id", id)
			s.serveMovieLikePlugin(w, probe, "page")
			return
		}
		if s.nativeMediaSyncDelete(id) {
			probe.SetPathValue("id", id)
			s.serveMediaSyncDeletePlugin(w, probe, "page")
			return
		}
		if s.nativeEventWebhook(id) {
			probe.SetPathValue("id", id)
			s.serveEventWebhookPlugin(w, probe, "page")
			return
		}
		if s.nativeMediaRefresh(id) {
			probe.SetPathValue("id", id)
			s.serveMediaRefreshPlugin(w, probe, "page")
			return
		}
		if s.nativeLibrary(id) {
			probe.SetPathValue("id", id)
			s.serveLibraryPlugin(w, probe, "page")
			return
		}
		if s.nativeSubtitle(id) {
			probe.SetPathValue("id", id)
			s.serveSubtitlePlugin(w, probe, "page")
			return
		}
		if s.nativeOpenSubtitles(id) {
			probe.SetPathValue("id", id)
			s.serveOpenSubtitlePlugin(w, probe, "page")
			return
		}
		if s.nativeIndexer(id) {
			probe.SetPathValue("id", id)
			s.serveIndexerPlugin(w, probe, "page")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fallback.ServeHTTP(w, r)
	})
}
