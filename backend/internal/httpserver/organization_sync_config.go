package httpserver

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type syncConfigPatch struct {
	Source        *string `json:"source,omitempty"`
	Dest          *string `json:"dest,omitempty"`
	Unknown       *string `json:"unknown,omitempty"`
	Mode          *string `json:"mode,omitempty"`
	Note          *string `json:"note,omitempty"`
	Enabled       *bool   `json:"enabled,omitempty"`
	Rename        *bool   `json:"rename,omitempty"`
	Compatibility *bool   `json:"compatibility,omitempty"`
	Locating      *bool   `json:"locating,omitempty"`
}
type syncConfigInput struct {
	Action                string          `json:"action"`
	ID                    string          `json:"id,omitempty"`
	Patch                 syncConfigPatch `json:"patch,omitempty"`
	RestoreID             string          `json:"restoreId,omitempty"`
	Fingerprint           string          `json:"fingerprint,omitempty"`
	Confirm               bool            `json:"confirm,omitempty"`
	ConfirmInvalidateJobs bool            `json:"confirmInvalidateJobs,omitempty"`
}

func syncConfigMode(value string) (string, bool) {
	if mapped := transferModes[value]; mapped != "" {
		value = mapped
	}
	switch value {
	case "copy", "link", "softlink", "move", "rclonecopy", "rclone", "miniocopy", "minio":
		return value, true
	}
	return "", false
}
func applySyncPatch(r organization.SyncRecord, p syncConfigPatch) (organization.SyncRecord, error) {
	changed := map[string]bool{}
	for _, field := range []struct {
		key   string
		value *string
		out   *string
	}{{"source", p.Source, &r.Source}, {"dest", p.Dest, &r.Dest}, {"unknown", p.Unknown, &r.Unknown}, {"mode", p.Mode, &r.Mode}, {"note", p.Note, &r.Note}} {
		if field.value == nil {
			continue
		}
		value := *field.value
		if len(value) > 4096 || strings.ContainsRune(value, '\x00') {
			return r, organization.ErrPath
		}
		if field.key == "source" || field.key == "dest" || field.key == "unknown" {
			if value != "" {
				if strings.ContainsAny(value, "\r\n") {
					return r, organization.ErrPath
				}
				value = filepath.Clean(value)
			}
		}
		if field.key == "mode" {
			var ok bool
			value, ok = syncConfigMode(value)
			if !ok {
				return r, organization.ErrMode
			}
		}
		*field.out = value
		changed[field.key] = true
	}
	for _, field := range []struct {
		key   string
		value *bool
		out   *int64
	}{{"enabled", p.Enabled, &r.Enabled}, {"rename", p.Rename, &r.Rename}, {"compatibility", p.Compatibility, &r.Compatibility}, {"locating", p.Locating, &r.Locating}} {
		if field.value != nil {
			*field.out = 0
			if *field.value {
				*field.out = 1
			}
			changed[field.key] = true
		}
	}
	nulls := []string{}
	for _, key := range r.NullFields {
		if !changed[key] {
			nulls = append(nulls, key)
		}
	}
	r.NullFields = nulls
	return r, nil
}
func syncConfigID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != value {
		return 0, organization.ErrState
	}
	return id, nil
}

func (api organizationAPI) reviewSyncConfig(ctx context.Context, input syncConfigInput, actor string) (organization.SyncConfigReview, *recognitionFailure) {
	fail := func(status int, message string) (organization.SyncConfigReview, *recognitionFailure) {
		return organization.SyncConfigReview{}, &recognitionFailure{status, message}
	}
	before, err := api.records.ListSync(ctx)
	if err != nil {
		return fail(503, "sync configuration is unavailable")
	}
	beforeCopy := append([]organization.SyncRecord{}, before...)
	after := append([]organization.SyncRecord{}, before...)
	review := organization.SyncConfigReview{Action: input.Action, Actor: actor, Before: beforeCopy}
	if input.Action == "restore" {
		if input.ID != "" || input.Patch != (syncConfigPatch{}) || len(input.RestoreID) != 32 {
			return fail(400, "select one saved configuration receipt for undo")
		}
		receipt, err := api.records.GetSyncConfigReceipt(ctx, input.RestoreID)
		if err != nil {
			return fail(409, "configuration receipt unavailable")
		}
		if receipt.RestoreID != "" || organization.Digest(receipt.After) != organization.Digest(before) {
			return fail(409, "configuration changed after this receipt or was already undone; do not overwrite it")
		}
		after = append([]organization.SyncRecord{}, receipt.Review.Before...)
		review.RestoreID = input.RestoreID
	} else {
		if input.RestoreID != "" {
			return fail(400, "unexpected configuration receipt")
		}
		index := -1
		if input.Action != "create" {
			id, err := syncConfigID(input.ID)
			if err != nil {
				return fail(400, "select an exact positive configuration ID")
			}
			for i, r := range before {
				if r.ID == id {
					index = i
					break
				}
			}
			if index < 0 {
				return fail(404, "sync configuration not found")
			}
		} else if input.ID != "" {
			return fail(400, "new configuration ID is assigned by the database")
		}
		if input.Action == "delete" {
			if input.Patch != (syncConfigPatch{}) {
				return fail(400, "delete must not modify fields")
			}
			after = append(after[:index], after[index+1:]...)
		} else {
			if input.Action != "create" && input.Action != "update" && input.Action != "status" {
				return fail(400, "unsupported sync configuration action")
			}
			if input.Action == "status" {
				p := input.Patch
				count := 0
				for _, value := range []*bool{p.Enabled, p.Rename, p.Compatibility, p.Locating} {
					if value != nil {
						count++
					}
				}
				if count != 1 || p.Source != nil || p.Dest != nil || p.Unknown != nil || p.Mode != nil || p.Note != nil {
					return fail(400, "status changes one switch only")
				}
			}
			if input.Patch == (syncConfigPatch{}) {
				return fail(400, "no configuration fields selected")
			}
			r := organization.SyncRecord{}
			if index >= 0 {
				r = before[index]
			} else {
				if len(after) >= 128 {
					return fail(422, "sync configuration limit reached")
				}
				if input.Patch.Source == nil {
					return fail(400, "new source directory is required")
				}
				r.Mode = "copy"
			}
			r, err = applySyncPatch(r, input.Patch)
			if err != nil {
				return fail(422, "invalid sync field or transfer mode")
			}
			if index < 0 {
				after = append(after, r)
				index = len(after) - 1
			} else {
				after[index] = r
			}
			// Disabled records can be repaired even while their old mounts are
			// absent. Enabling/new sources must be existing local directories.
			if r.Enabled == 1 || input.Action == "create" || input.Patch.Source != nil {
				physical, err := configuredSyncPath(r.Source)
				if err != nil {
					return fail(422, "source must be a local absolute directory")
				}
				info, err := os.Stat(physical)
				if err != nil || !info.IsDir() {
					return fail(422, "source directory must exist")
				}
				review.Roots = append(review.Roots, "source:"+physical+":"+organization.Identity(info))
			}
			mode, known := syncConfigMode(r.Mode)
			if r.Enabled == 1 && !known {
				return fail(422, "select a known transfer mode before enabling")
			}
			if r.Enabled == 1 && r.Rename == 0 && r.Dest == "" {
				return fail(422, "direct sync requires an explicit destination")
			}
			for _, field := range []struct{ key, path string }{{"destination", r.Dest}, {"unknown", r.Unknown}} {
				if r.Enabled != 1 && input.Action != "create" && (field.key == "destination" && input.Patch.Dest == nil || field.key == "unknown" && input.Patch.Unknown == nil) {
					continue
				}
				if field.path == "" {
					continue
				}
				local := field.key == "unknown" || organization.SupportedMode(mode) && !organization.RemoteMode(mode)
				if !local {
					if _, err := logicalRemoteRoot(field.path); err != nil {
						return fail(422, "remote destination must be an absolute logical root")
					}
					continue
				}
				physical, err := configuredSyncPath(field.path)
				if err != nil {
					return fail(422, "local destination/unknown paths must be absolute and safe")
				}
				review.Roots = append(review.Roots, field.key+":"+physical)
				if info, err := os.Stat(physical); err == nil && !info.IsDir() {
					return fail(422, "configured destination must be a directory, not a file")
				}
			}
			if mode == "link" && r.Dest != "" && organization.CopySupported && (r.Enabled == 1 || input.Action == "create" || input.Patch.Source != nil || input.Patch.Dest != nil || input.Patch.Mode != nil) {
				sourceInfo, err := os.Stat(r.Source)
				if err != nil {
					return fail(422, "hard-link source is unavailable")
				}
				parent := r.Dest
				for {
					info, err := os.Stat(parent)
					if err == nil {
						if strings.Split(organization.Identity(sourceInfo), ":")[0] != strings.Split(organization.Identity(info), ":")[0] {
							return fail(422, "hard links require source and destination on the same filesystem")
						}
						break
					}
					if !os.IsNotExist(err) {
						return fail(422, "hard-link destination is unavailable")
					}
					next := filepath.Dir(parent)
					if next == parent {
						return fail(422, "hard-link destination is unavailable")
					}
					parent = next
				}
			}
			if r.Enabled == 1 {
				physical, _ := configuredSyncPath(r.Source)
				for i, other := range after {
					if i == index || other.Enabled != 1 {
						continue
					}
					otherPath, err := configuredSyncPath(other.Source)
					if err != nil {
						return fail(422, "an enabled source has an invalid path; repair or disable it first")
					}
					if insideSyncPath(physical, otherPath) || insideSyncPath(otherPath, physical) {
						other.Enabled = 0
						nulls := []string{}
						for _, key := range other.NullFields {
							if key != "enabled" {
								nulls = append(nulls, key)
							}
						}
						other.NullFields = nulls
						after[i] = other
					}
				}
				snapshot, err := api.service.configStore.Snapshot()
				if err != nil {
					return fail(503, "configuration is unavailable")
				}
				review.ConfigDigest = organization.Digest(snapshot)
				if validateDirectSyncGraph(r, after, objectValue(snapshot["media"])) != nil {
					return fail(422, "sources, destinations, unknown directories or media libraries form an unsafe overlap/cycle")
				}
			}
		}
	}
	review.After = after
	review.Jobs, err = api.records.SyncAffectedJobs(ctx, before, after)
	if err != nil {
		return fail(503, "pending sync job state unavailable")
	}
	return organization.FinalizeSyncReview(review), nil
}

func (api organizationAPI) serveSyncConfigReview(w http.ResponseWriter, r *http.Request) {
	api.serveSyncConfigMutation(w, r, false)
}
func (api organizationAPI) serveSyncConfigApply(w http.ResponseWriter, r *http.Request) {
	api.serveSyncConfigMutation(w, r, true)
}
func (api organizationAPI) serveSyncConfigMutation(w http.ResponseWriter, r *http.Request, apply bool) {
	if !api.recordsAuthorized(w, r) {
		return
	}
	var input syncConfigInput
	if !decodeOrganizationRequest(w, r, &input, "invalid sync configuration request") {
		return
	}
	if apply && (!input.Confirm || len(input.Fingerprint) != 64) {
		writeAPIError(w, 400, 400, "review and explicitly confirm the configuration change")
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	review, failure := api.reviewSyncConfig(ctx, input, claims.Username)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	if !apply {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
		return
	}
	if review.Fingerprint != input.Fingerprint {
		writeAPIError(w, 409, 409, "configuration, filesystem roots or pending jobs changed; review again")
		return
	}
	if len(review.Jobs) > 0 && !input.ConfirmInvalidateJobs {
		writeAPIError(w, 409, 409, "separate confirmation is required to invalidate the listed pending jobs; they will not be cancelled or retried")
		return
	}
	receipt, err := api.records.ApplySyncConfig(ctx, review, input.ConfirmInvalidateJobs)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": receipt})
}
func (api organizationAPI) serveSyncConfigReceipts(w http.ResponseWriter, r *http.Request) {
	if !api.recordsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	receipts, err := api.records.ListSyncConfigReceipts(ctx)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": receipts})
}

func decodeSyncCompat(w http.ResponseWriter, r *http.Request, allowed map[string]bool) (map[string]string, bool) {
	values := map[string]string{}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media == "application/json" {
		raw := map[string]json.RawMessage{}
		if !decodeOrganizationRequest(w, r, &raw, "invalid legacy sync request") {
			return nil, false
		}
		for key, value := range raw {
			var s string
			if json.Unmarshal(value, &s) != nil {
				s = string(value)
			}
			values[key] = s
		}
	} else if media == "application/x-www-form-urlencoded" {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if r.ParseForm() != nil {
			writeAPIError(w, 400, 400, "invalid sync form")
			return nil, false
		}
		for key, value := range r.PostForm {
			if len(value) != 1 {
				writeAPIError(w, 400, 400, "duplicate sync field")
				return nil, false
			}
			values[key] = value[0]
		}
	} else {
		writeAPIError(w, 400, 400, "unsupported sync content type")
		return nil, false
	}
	for key := range values {
		if !allowed[key] {
			writeAPIError(w, 400, 400, "unsupported sync field")
			return nil, false
		}
	}
	return values, true
}
func syncCompatRecord(r organization.SyncRecord) map[string]any {
	mode, _ := syncConfigMode(r.Mode)
	label := organization.ModeLabel(mode)
	for name, code := range transferModes {
		if code == mode {
			label = name
			break
		}
	}
	return map[string]any{"id": strconv.FormatInt(r.ID, 10), "from": r.Source, "to": r.Dest, "unknown": r.Unknown, "syncmod": r.Mode, "syncmod_name": label, "compatibility": r.Compatibility != 0, "rename": r.Rename != 0, "enabled": r.Enabled != 0, "locating": r.Locating != 0}
}
func (api organizationAPI) serveSyncCompatList(w http.ResponseWriter, r *http.Request) {
	if !api.recordsAuthorized(w, r) {
		return
	}
	items, err := api.records.ListSync(r.Context())
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	result := map[string]any{}
	for _, item := range items {
		result[strconv.FormatInt(item.ID, 10)] = syncCompatRecord(item)
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "result": result})
}
func (api organizationAPI) serveSyncCompatInfo(w http.ResponseWriter, r *http.Request) {
	if !api.recordsAuthorized(w, r) {
		return
	}
	values, ok := decodeSyncCompat(w, r, map[string]bool{"sid": true})
	if !ok {
		return
	}
	id, err := syncConfigID(values["sid"])
	if err != nil {
		writeAPIError(w, 400, 400, "invalid sync ID")
		return
	}
	record, err := api.records.GetSync(r.Context(), id)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "result": syncCompatRecord(record)})
}
func (api organizationAPI) serveSyncCompatUpdate(w http.ResponseWriter, r *http.Request) {
	api.serveSyncCompatMutation(w, r, "update")
}
func (api organizationAPI) serveSyncCompatStatus(w http.ResponseWriter, r *http.Request) {
	api.serveSyncCompatMutation(w, r, "status")
}
func (api organizationAPI) serveSyncCompatDelete(w http.ResponseWriter, r *http.Request) {
	api.serveSyncCompatMutation(w, r, "delete")
}
func (api organizationAPI) serveSyncCompatMutation(w http.ResponseWriter, r *http.Request, action string) {
	if !api.recordsAuthorized(w, r) {
		return
	}
	allowed := map[string]bool{"sid": true, "confirmInvalidateJobs": true}
	if action == "update" {
		for _, key := range []string{"from", "to", "unknown", "syncmod", "compatibility", "rename", "enabled", "locating", "note"} {
			allowed[key] = true
		}
	}
	if action == "status" {
		allowed["flag"], allowed["checked"] = true, true
	}
	values, ok := decodeSyncCompat(w, r, allowed)
	if !ok {
		return
	}
	input := syncConfigInput{Action: action, ID: values["sid"]}
	parseBool := func(value string) (bool, bool) {
		switch value {
		case "1", "true":
			return true, true
		case "0", "false":
			return false, true
		}
		return false, false
	}
	if value, exists := values["confirmInvalidateJobs"]; exists {
		b, ok := parseBool(value)
		if !ok {
			writeAPIError(w, 400, 400, "invalid pending-job confirmation")
			return
		}
		input.ConfirmInvalidateJobs = b
	}
	if action == "update" {
		if input.ID == "" || input.ID == "0" {
			input.Action = "create"
			input.ID = ""
		}
		if _, ok := values["from"]; !ok {
			writeAPIError(w, 400, 400, "source is required")
			return
		}
		for key, out := range map[string]**string{"from": &input.Patch.Source, "to": &input.Patch.Dest, "unknown": &input.Patch.Unknown, "syncmod": &input.Patch.Mode, "note": &input.Patch.Note} {
			if value, exists := values[key]; exists {
				copy := value
				*out = &copy
			}
		}
		for key, out := range map[string]**bool{"enabled": &input.Patch.Enabled, "rename": &input.Patch.Rename, "compatibility": &input.Patch.Compatibility, "locating": &input.Patch.Locating} {
			if value, exists := values[key]; exists {
				b, ok := parseBool(value)
				if !ok {
					writeAPIError(w, 400, 400, "sync switches must be explicit 0/1 or booleans")
					return
				}
				copy := b
				*out = &copy
			}
		}
	} else if action == "status" {
		b, ok := parseBool(values["checked"])
		if !ok {
			writeAPIError(w, 400, 400, "checked must be explicit 0/1")
			return
		}
		switch values["flag"] {
		case "enable":
			input.Patch.Enabled = &b
		case "rename":
			input.Patch.Rename = &b
		case "compatibility":
			input.Patch.Compatibility = &b
		case "locating":
			input.Patch.Locating = &b
		default:
			writeAPIError(w, 400, 400, "unsupported sync switch")
			return
		}
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	review, failure := api.reviewSyncConfig(ctx, input, claims.Username)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	if len(review.Jobs) > 0 && !input.ConfirmInvalidateJobs {
		writeAPIError(w, 409, 409, "pending sync jobs require explicit invalidation confirmation; inspect their saved state first")
		return
	}
	receipt, err := api.records.ApplySyncConfig(ctx, review, input.ConfirmInvalidateJobs)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "msg": "", "data": receipt})
}
