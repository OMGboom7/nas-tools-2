package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func (api organizationAPI) serveCompatUnknownList(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	if _, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 16<<10)); err != nil {
		writeAPIError(w, 400, 400, "invalid unknown list request")
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
	if !exists {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "", "data": map[string]any{"items": []any{}}})
		return
	}
	rows, err := db.QueryContext(ctx, `SELECT ID,COALESCE(PATH,''),COALESCE(DEST,''),COALESCE(MODE,'') FROM TRANSFER_UNKNOWN WHERE STATE='N' AND COALESCE(PATH,'')!='' ORDER BY ID`)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer rows.Close()
	// Stream the old all-records contract without accumulating the whole table.
	// A read/write failure leaves incomplete JSON, never a completed partial list.
	w.Header().Set("Content-Type", "application/json")
	if _, err = w.Write([]byte(`{"code":0,"success":true,"message":"","data":{"items":[`)); err != nil {
		return
	}
	first := true
	for rows.Next() {
		var id int64
		var path, dest, mode string
		if rows.Scan(&id, &path, &dest, &mode) != nil {
			return
		}
		path, dest = strings.ReplaceAll(path, "\\", "/"), strings.ReplaceAll(dest, "\\", "/")
		var rmtMode any = ""
		if mode != "" {
			if value, known := transferModes[mode]; known {
				rmtMode = value
			} else {
				rmtMode = nil
			}
		}
		item := map[string]any{"id": id, "path": path, "to": dest, "name": path, "sync_mode": mode, "rmt_mode": rmtMode}
		raw, err := json.Marshal(item)
		if err != nil {
			return
		}
		if !first {
			if _, err = w.Write([]byte(",")); err != nil {
				return
			}
		}
		first = false
		if _, err = w.Write(raw); err != nil {
			return
		}
	}
	if rows.Err() != nil || ctx.Err() != nil {
		return
	}
	_, _ = w.Write([]byte("]}}"))
}

func organizationRecordID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != raw {
		writeAPIError(w, 400, 400, "invalid unknown record ID")
		return 0, false
	}
	return id, true
}
func (api organizationAPI) recordsAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if !api.authorize(w, r) {
		return false
	}
	if !api.pureGo || api.records == nil {
		writeAPIError(w, 501, 501, "record mutations require the Python backend to be disabled")
		return false
	}
	return true
}
func (api organizationAPI) unknownRecordActor(w http.ResponseWriter, r *http.Request) string {
	if !api.recordsAuthorized(w, r) {
		return ""
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization token is invalid or expired")
		return ""
	}
	return claims.Username
}
func (api organizationAPI) serveUnknownDeleteReview(w http.ResponseWriter, r *http.Request) {
	if !api.recordsAuthorized(w, r) {
		return
	}
	id, ok := organizationRecordID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	review, err := api.records.ReviewUnknownDelete(ctx, id)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
}
func (api organizationAPI) serveUnknownDelete(w http.ResponseWriter, r *http.Request) {
	actor := api.unknownRecordActor(w, r)
	if actor == "" {
		return
	}
	id, ok := organizationRecordID(w, r)
	if !ok {
		return
	}
	var input struct {
		Confirm     bool   `json:"confirm"`
		RecordOnly  bool   `json:"confirmRecordDeletion"`
		Fingerprint string `json:"reviewFingerprint"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid unknown record deletion confirmation") {
		return
	}
	if !input.Confirm || !input.RecordOnly || len(input.Fingerprint) != 64 {
		writeAPIError(w, 400, 400, "explicit reviewed record-only deletion confirmation is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	receipts, err := api.records.DeleteUnknown(ctx, []organization.UnknownDeleteSelector{{ID: id, Fingerprint: input.Fingerprint}}, actor, true)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": receipts[0]})
}
func (api organizationAPI) serveUnknownDeletes(w http.ResponseWriter, r *http.Request) {
	if !api.recordsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	receipts, err := api.records.ListUnknownDeletes(ctx)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": receipts})
}
func (api organizationAPI) serveUnknownRestore(w http.ResponseWriter, r *http.Request) {
	actor := api.unknownRecordActor(w, r)
	if actor == "" {
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
		Restore bool `json:"confirmRecordRestore"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid unknown record restore confirmation") {
		return
	}
	if !input.Confirm || !input.Restore {
		writeAPIError(w, 400, 400, "explicit record-only restore confirmation is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	receipt, err := api.records.RestoreUnknownDelete(ctx, r.PathValue("receipt"), actor)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": receipt})
}

func legacyUnknownIDs(raw json.RawMessage) ([]organization.UnknownDeleteSelector, error) {
	var values []json.RawMessage
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
	} else {
		values = []json.RawMessage{raw}
	}
	if len(values) < 1 || len(values) > 100 {
		return nil, organization.ErrState
	}
	result := []organization.UnknownDeleteSelector{}
	for _, value := range values {
		text := string(value)
		if len(value) > 0 && value[0] == '"' {
			if err := json.Unmarshal(value, &text); err != nil {
				return nil, err
			}
		}
		id, err := strconv.ParseInt(text, 10, 64)
		if err != nil || id < 1 || strconv.FormatInt(id, 10) != text {
			return nil, organization.ErrState
		}
		result = append(result, organization.UnknownDeleteSelector{ID: id})
	}
	return result, nil
}
func (api organizationAPI) serveCompatUnknownDelete(w http.ResponseWriter, r *http.Request) {
	actor := api.unknownRecordActor(w, r)
	if actor == "" {
		return
	}
	var raw json.RawMessage
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media == "application/json" {
		var input struct {
			ID json.RawMessage `json:"id"`
		}
		if !decodeOrganizationRequest(w, r, &input, "invalid legacy unknown deletion") {
			return
		}
		raw = input.ID
	} else if media == "application/x-www-form-urlencoded" {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if r.ParseForm() != nil || len(r.PostForm) != 1 || len(r.PostForm["id"]) != 1 {
			writeAPIError(w, 400, 400, "invalid legacy unknown deletion")
			return
		}
		raw, _ = json.Marshal(r.PostForm.Get("id"))
	} else {
		writeAPIError(w, 400, 400, "unsupported unknown deletion content type")
		return
	}
	selectors, err := legacyUnknownIDs(raw)
	if err != nil {
		writeAPIError(w, 400, 400, "invalid legacy unknown record IDs")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	receipts, err := api.records.DeleteUnknown(ctx, selectors, actor, false)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "", "data": map[string]any{"receipts": receipts}})
}
