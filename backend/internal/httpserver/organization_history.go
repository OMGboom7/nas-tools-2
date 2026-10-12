package httpserver

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type organizationHistorySelector struct {
	ID          int64  `json:"id"`
	Fingerprint string `json:"fingerprint"`
}
type organizationHistoryItem struct {
	organization.HistoryRecord
	Fingerprint string `json:"fingerprint"`
	SourceID    string `json:"sourceId,omitempty"`
	TargetID    string `json:"targetId,omitempty"`
	Relative    string `json:"relative,omitempty"`
	NativeMode  string `json:"nativeMode,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func projectHistory(record organization.HistoryRecord, roots organizationRoots) organizationHistoryItem {
	item := organizationHistoryItem{HistoryRecord: record, Fingerprint: organization.Digest(record)}
	if record.SourceFilename == "" || filepath.Base(record.SourceFilename) != record.SourceFilename || record.SourceFilename == "." || record.SourceFilename == ".." || strings.ContainsAny(record.SourceFilename, "\\\x00\r\n") {
		item.Reason = "旧记录没有可核验的源文件名，保留原历史"
		return item
	}
	if !filepath.IsAbs(record.SourcePath) || filepath.Clean(record.SourcePath) != record.SourcePath {
		item.Reason = "旧来源路径不规范，保留原历史"
		return item
	}
	// Go history stores canonical physical paths. Resolve only configured roots,
	// never an arbitrary historical path, so a configured alias remains usable.
	expanded := organizationRoots{Sources: append([]organizationRoot{}, roots.Sources...), Targets: append([]organizationRoot{}, roots.Targets...)}
	for _, source := range roots.Sources {
		if canonical, err := filepath.EvalSymlinks(source.Path); err == nil && canonical != source.Path {
			source.Path = canonical
			expanded.Sources = append(expanded.Sources, source)
		}
	}
	for _, target := range roots.Targets {
		if canonical, err := filepath.EvalSymlinks(target.Path); err == nil && canonical != target.Path {
			target.Path = canonical
			expanded.Targets = append(expanded.Targets, target)
		}
	}
	projection := projectUnknown(organization.UnknownRecord{ID: record.ID, Path: filepath.Join(record.SourcePath, record.SourceFilename), Dest: record.Dest, Mode: record.Mode, State: "N"}, expanded)
	item.SourceID, item.TargetID, item.Relative, item.NativeMode, item.Reason = projection.SourceID, projection.TargetID, projection.Relative, projection.NativeMode, projection.Reason
	return item
}
func historyPathMatches(root, relative, path string) bool {
	if filepath.Join(root, relative) == path {
		return true
	}
	canonical, err := filepath.EvalSymlinks(root)
	return err == nil && filepath.Join(canonical, relative) == path
}
func (api organizationAPI) serveHistoryRecords(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	page := 1
	if value := r.URL.Query().Get("page"); value != "" {
		number, err := strconv.Atoi(value)
		if err != nil || number < 1 || number > 1000000 {
			writeAPIError(w, 400, 400, "invalid history page")
			return
		}
		page = number
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	if len(keyword) > 256 {
		writeAPIError(w, 400, 400, "invalid history search")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: api.service.databasePath}).String()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer db.Close()
	var exists bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='TRANSFER_HISTORY')`).Scan(&exists); err != nil {
		writeOrganizationError(w, err)
		return
	}
	items := []organizationHistoryItem{}
	total := 0
	if exists {
		roots, err := api.roots(ctx)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		pattern := "%" + keyword + "%"
		where := ` WHERE COALESCE(TITLE,'') LIKE ? OR COALESCE(SOURCE_FILENAME,'') LIKE ?`
		if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM TRANSFER_HISTORY`+where, pattern, pattern).Scan(&total); err != nil {
			writeOrganizationError(w, err)
			return
		}
		rows, err := db.QueryContext(ctx, `SELECT `+organization.HistoryColumns+` FROM TRANSFER_HISTORY`+where+` ORDER BY DATE DESC,ID DESC LIMIT 30 OFFSET ?`, pattern, pattern, (page-1)*30)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		for rows.Next() {
			record, err := organization.ScanHistory(rows)
			if err != nil {
				rows.Close()
				writeOrganizationError(w, err)
				return
			}
			items = append(items, projectHistory(record, roots))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
	}
	pages := (total + 29) / 30
	if pages < 1 {
		pages = 1
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"items": items, "total": total, "page": page, "totalPages": pages}})
}
func (api organizationAPI) bindHistory(ctx context.Context, input organizationPlanInput, roots organizationRoots) (*organization.HistoryRecord, *recognitionFailure) {
	if input.HistoryRecord == nil {
		return nil, nil
	}
	if input.UnknownRecord != nil {
		return nil, &recognitionFailure{422, "select either historical or unknown provenance, not both"}
	}
	if !api.pureGo || api.jobs == nil {
		return nil, &recognitionFailure{501, "history replanning requires disabled legacy backend"}
	}
	selector := input.HistoryRecord
	if selector.ID < 1 || len(selector.Fingerprint) != 64 {
		return nil, &recognitionFailure{422, "invalid history selector"}
	}
	record, err := api.jobs.GetHistory(ctx, selector.ID)
	if err != nil || organization.Digest(record) != selector.Fingerprint {
		return nil, &recognitionFailure{409, "history changed; review a fresh record before planning"}
	}
	projection := projectHistory(record, roots)
	if projection.Reason != "" {
		return nil, &recognitionFailure{422, "historical source, destination or mode is not supported"}
	}
	matched := false
	for _, source := range roots.Sources {
		if source.ID == input.SourceID && historyPathMatches(source.Path, input.Path, filepath.Join(record.SourcePath, record.SourceFilename)) {
			matched = true
		}
	}
	if !matched {
		return nil, &recognitionFailure{422, "history replan must select its exact configured original source"}
	}
	matched = false
	for _, target := range roots.Targets {
		if target.ID == input.TargetID && (record.Dest == "" || historyPathMatches(target.Path, ".", record.Dest)) {
			matched = true
		}
	}
	if !matched || record.Mode != "" && projection.NativeMode != input.Mode {
		return nil, &recognitionFailure{422, "historical destination or literal mode does not match"}
	}
	return &record, nil
}
