package httpserver

import (
	"errors"
	"net/http"
	"os"
	"slices"

	"github.com/0xforee/nas-tools/backend/internal/mediaarchive"
)

func (s pluginService) nativeArchivePage(id string) bool {
	return id == mediaarchive.Plugin && mediaarchive.Supported && s.system != nil && s.archiveConfigPath != ""
}

func (s pluginService) archiveDirectory(w http.ResponseWriter, r *http.Request) (*os.File, bool) {
	if _, ok := s.catalogReader(w, r); !ok {
		return nil, false
	}
	w.Header().Set("Cache-Control", "private, no-store")
	const key = "plugin." + mediaarchive.Plugin
	raw, err := s.system.ReadMany(r.Context(), []string{"UserInstalledPlugins", key})
	if err != nil {
		archivePageError(w, err)
		return nil, false
	}
	installed, err := decodeInstalledPlugins(raw["UserInstalledPlugins"])
	if err != nil {
		archivePageError(w, err)
		return nil, false
	}
	if !slices.Contains(installed, mediaarchive.Plugin) {
		writeAPIError(w, 404, 404, "未安装媒体库归档插件")
		return nil, false
	}
	var custom string
	if !s.archiveContainer {
		config, err := decodeMetadataConfig(raw[key])
		if err != nil {
			archivePageError(w, err)
			return nil, false
		}
		if path := config["archive_path"]; path != nil {
			var ok bool
			custom, ok = path.(string)
			if !ok {
				writeAPIError(w, 409, 409, "归档目录配置无效")
				return nil, false
			}
		}
	}
	dir, err := mediaarchive.Open(s.archiveConfigPath, custom, s.archiveContainer)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		archivePageError(w, err)
		return nil, false
	}
	return dir, true
}

func archivePageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeAPIError(w, 404, 404, "归档文件不存在或已被删除")
	case errors.Is(err, mediaarchive.ErrChanged):
		writeAPIError(w, 409, 409, "归档文件或目录已变化，请重新查看")
	default:
		writeAPIError(w, 503, 503, "无法访问归档目录或配置")
	}
}

func (s pluginService) serveArchivePage(w http.ResponseWriter, r *http.Request, legacy bool) {
	dir, ok := s.archiveDirectory(w, r)
	if !ok {
		return
	}
	if dir != nil {
		defer dir.Close()
	}
	after := r.URL.Query().Get("after")
	if after != "" && !mediaarchive.ValidName(after) {
		writeAPIError(w, 400, 400, "归档分页文件名无效")
		return
	}
	table := pluginPageTable{Columns: []string{"归档名称", "归档大小", "归档时间"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
	data := pluginPageData{Title: "归档记录", Sections: []string{}, ReadOnly: true, CanDeleteRecords: true, DeleteConfirmation: "typeRecordId"}
	if dir != nil {
		files, next, err := mediaarchive.List(r.Context(), dir, after)
		if err != nil {
			archivePageError(w, err)
			return
		}
		data.NextCursor = next
		for _, f := range files {
			table.Rows = append(table.Rows, []string{f.Name, legacyPathFileSize(f.Size), f.Time})
			table.RowActions = append(table.RowActions, &pluginPageRowAction{Type: "delete", RecordID: f.Name, Confirmation: "typeRecordId", Proof: f.Proof})
		}
	}
	if s.nativeArchiveRunner(mediaarchive.Plugin) {
		if raw, err := s.system.Get(r.Context(), archiveControlKey); err == nil {
			if control, err := decodeArchiveControl(raw); err == nil && control.Last != "" {
				data.Sections = append(data.Sections, control.Last)
			}
		}
	}
	data.Tables = []pluginPageTable{table}
	writeNativeRecordPage(w, data, legacy)
}

func (s pluginService) removeArchiveRecord(w http.ResponseWriter, r *http.Request, name, confirm, proof string) {
	if !mediaarchive.ValidName(name) || name != confirm {
		writeAPIError(w, 400, 400, "请输入完整的归档文件名")
		return
	}
	dir, ok := s.archiveDirectory(w, r)
	if !ok {
		return
	}
	if dir == nil {
		archivePageError(w, os.ErrNotExist)
		return
	}
	defer dir.Close()
	if err := mediaarchive.Remove(dir, name, confirm, proof); err != nil {
		archivePageError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "归档文件已删除"})
}
