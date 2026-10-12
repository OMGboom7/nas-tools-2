package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type organizationUnknownSelector struct {
	ID          int64  `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

func (api organizationAPI) serveUnknownReview(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	review, err := api.jobs.ReviewUnknown(ctx, r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
}

type organizationUnknownItem struct {
	organization.UnknownRecord
	Fingerprint string `json:"fingerprint"`
	SourceID    string `json:"sourceId,omitempty"`
	TargetID    string `json:"targetId,omitempty"`
	Relative    string `json:"relative,omitempty"`
	NativeMode  string `json:"nativeMode,omitempty"`
	JobID       string `json:"jobId,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func projectUnknown(record organization.UnknownRecord, roots organizationRoots) organizationUnknownItem {
	item := organizationUnknownItem{UnknownRecord: record, Fingerprint: organization.Digest(record), NativeMode: transferModes[record.Mode]}
	if record.Mode != "" && !organization.SupportedMode(item.NativeMode) {
		item.Reason = "旧转移方式尚未迁移，不能自动改为本地方式"
		return item
	}
	if !filepath.IsAbs(record.Path) || filepath.Clean(record.Path) != record.Path || strings.ContainsAny(record.Path, "\\\x00\r\n") {
		item.Reason = "旧路径不规范或不适用于本地文件系统，记录保留"
		return item
	}
	longest := -1
	for _, source := range roots.Sources {
		rel, err := filepath.Rel(source.Path, record.Path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && len(source.Path) > longest {
			item.SourceID, item.Relative, longest = source.ID, rel, len(source.Path)
		}
	}
	if item.SourceID == "" {
		item.Reason = "旧路径未落在当前启用来源目录内，记录保留"
		return item
	}
	for _, target := range roots.Targets {
		if record.Dest == "" || record.Dest == target.Path {
			item.TargetID = target.ID
			break
		}
	}
	if item.TargetID == "" {
		item.Reason = "旧目标未匹配当前配置目录，记录保留"
	}
	return item
}

func (api organizationAPI) serveUnknown(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	page, size := 1, 30
	var err error
	if value := r.URL.Query().Get("page"); value != "" {
		page, err = strconv.Atoi(value)
		if err != nil || page < 1 || page > 1000000 {
			writeAPIError(w, 400, 400, "invalid unknown record page")
			return
		}
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	if len(keyword) > 256 {
		writeAPIError(w, 400, 400, "invalid unknown record search")
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
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='TRANSFER_UNKNOWN')`).Scan(&exists); err != nil {
		writeOrganizationError(w, err)
		return
	}
	items := []organizationUnknownItem{}
	total := 0
	if exists {
		roots, err := api.roots(ctx)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		pattern := "%" + keyword + "%"
		if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM TRANSFER_UNKNOWN WHERE STATE='N' AND COALESCE(PATH,'') LIKE ?`, pattern).Scan(&total); err != nil {
			writeOrganizationError(w, err)
			return
		}
		rows, err := db.QueryContext(ctx, `SELECT ID,COALESCE(PATH,''),COALESCE(DEST,''),COALESCE(MODE,''),STATE FROM TRANSFER_UNKNOWN WHERE STATE='N' AND COALESCE(PATH,'') LIKE ? ORDER BY ID DESC LIMIT ? OFFSET ?`, pattern, size, (page-1)*size)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		for rows.Next() {
			var record organization.UnknownRecord
			if err = rows.Scan(&record.ID, &record.Path, &record.Dest, &record.Mode, &record.State); err != nil {
				rows.Close()
				writeOrganizationError(w, err)
				return
			}
			items = append(items, projectUnknown(record, roots))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_ORGANIZATION_UNKNOWN_LINKS')`).Scan(&exists); err != nil {
			writeOrganizationError(w, err)
			return
		}
		if exists {
			for i := range items {
				err = db.QueryRowContext(ctx, `SELECT JOB_ID FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE RECORD_ID=? AND STATE='pending'`, items[i].ID).Scan(&items[i].JobID)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					writeOrganizationError(w, err)
					return
				}
			}
		}
	}
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"items": items, "total": total, "page": page, "pageSize": size, "totalPages": pages}})
}

func (api organizationAPI) bindUnknown(ctx context.Context, input organizationPlanInput, roots organizationRoots) (*organization.UnknownRecord, *recognitionFailure) {
	if input.UnknownRecord == nil {
		return nil, nil
	}
	if !api.pureGo || api.jobs == nil {
		return nil, &recognitionFailure{501, "unknown record execution requires disabled legacy backend"}
	}
	selector := input.UnknownRecord
	if selector.ID < 1 || len(selector.Fingerprint) != 64 {
		return nil, &recognitionFailure{422, "invalid unknown record selector"}
	}
	record, err := api.jobs.GetUnknown(ctx, selector.ID)
	if err != nil {
		return nil, &recognitionFailure{409, "unknown record unavailable; refresh its saved state"}
	}
	if record.State != "N" || organization.Digest(record) != selector.Fingerprint {
		return nil, &recognitionFailure{409, "unknown record changed; refresh before planning"}
	}
	projection := projectUnknown(record, roots)
	if projection.Reason != "" {
		return nil, &recognitionFailure{422, "unknown record cannot be mapped to supported configured directories or mode"}
	}
	matched := false
	for _, source := range roots.Sources {
		if source.ID == input.SourceID && filepath.Join(source.Path, input.Path) == record.Path {
			matched = true
		}
	}
	if !matched {
		return nil, &recognitionFailure{422, "unknown record must use its exact configured source scope"}
	}
	matched = false
	for _, target := range roots.Targets {
		if target.ID == input.TargetID && (record.Dest == "" || record.Dest == target.Path) {
			matched = true
		}
	}
	if !matched || record.Mode != "" && projection.NativeMode != input.Mode {
		return nil, &recognitionFailure{422, "unknown record destination or literal transfer mode does not match"}
	}
	return &record, nil
}
