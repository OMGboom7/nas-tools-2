package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func mediaRemovalGrants(in mediaRemovalInput, v organization.MediaRemovalReview) bool {
	if !in.Confirm || !in.ConfirmRead || !in.ConfirmOldExecutorsStopped || !in.ConfirmCloseRelated {
		return false
	}
	if len(v.HistoryIDs) > 0 && (!in.ConfirmHistoryDeletion || !in.ConfirmDedupeReset) {
		return false
	}
	if len(v.Units) > 0 && (!in.ConfirmPermanentDeletion || !in.ConfirmOnlyCopyRisk || !in.ConfirmExpandedScope) {
		return false
	}
	for _, u := range v.Units {
		if u.Driver != "" && !in.ConfirmRemoteExclusive {
			return false
		}
	}
	return true
}
func (api *organizationAPI) serveMediaRemovalStatus(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	roots, err := api.roots(ctx)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	items, err := api.jobs.ListMediaRemovals(ctx)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	drivers := []string{""}
	for _, driver := range []string{"rclone", "minio"} {
		if api.remoteFactory(organization.LibraryRemoteMode(driver)).Available() {
			drivers = append(drivers, driver)
		}
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"roots": append(append([]organizationRoot{}, roots.Sources...), roots.Targets...), "drivers": drivers, "removals": items}})
}
func (api *organizationAPI) serveMediaRemovalBrowse(w http.ResponseWriter, r *http.Request) {
	api.serveRecoveryBrowse(w, r)
}
func (api *organizationAPI) serveMediaRemovalPreview(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var in mediaRemovalInput
	if !decodeOrganizationRequest(w, r, &in, "invalid complete media-removal review") {
		return
	}
	api.mediaRemovalPreviewInput(w, r, in)
}
func (api *organizationAPI) mediaRemovalPreviewInput(w http.ResponseWriter, r *http.Request, in mediaRemovalInput) {
	actor, err := api.recoveryActor(r)
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	v, closeAll, err := api.prepareMediaRemoval(ctx, in, actor)
	defer closeAll()
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
}
func (api *organizationAPI) serveMediaRemovalCreate(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var in mediaRemovalInput
	if !decodeOrganizationRequest(w, r, &in, "invalid reviewed media-removal batch") {
		return
	}
	api.mediaRemovalCreateInput(w, r, in)
}
func (api *organizationAPI) mediaRemovalCreateInput(w http.ResponseWriter, r *http.Request, in mediaRemovalInput) {
	actor, err := api.recoveryActor(r)
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	if len(in.RequestID) != 32 || len(in.Fingerprint) != 64 {
		writeAPIError(w, 400, 400, "save requires the fresh complete review and stable request ID")
		return
	}
	id := organization.MediaRemovalID(actor, in.RequestID)
	if saved, err := api.jobs.MediaRemoval(ctx, id); err == nil {
		if !mediaRemovalGrants(in, saved) || saved.Definition.InputDigest != mediaRemovalInputDigest(in) || organization.Digest(saved.Definition) != in.Fingerprint {
			writeOrganizationError(w, organization.ErrState)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": saved})
		return
	} else if !errors.Is(err, organization.ErrNotFound) {
		writeOrganizationError(w, err)
		return
	}
	v, closeAll, err := api.prepareMediaRemoval(ctx, in, actor)
	defer closeAll()
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if !mediaRemovalGrants(in, v) {
		writeAPIError(w, 400, 400, "separate history/dedupe, full media and auxiliary scope, permanent/only-copy, stopped-writer and related-task confirmations required")
		return
	}
	if v.Fingerprint != in.Fingerprint {
		writeOrganizationError(w, organization.ErrState)
		return
	}
	id, err = api.jobs.CreateMediaRemoval(ctx, v.Definition, v.Fingerprint)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	saved, err := api.jobs.MediaRemoval(ctx, id)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"code": 0, "success": true, "data": saved})
}

func (api *organizationAPI) mediaRemovalClients(ctx context.Context, v organization.MediaRemovalReview) ([]*remotestore.Client, func(), error) {
	clients := make([]*remotestore.Client, len(v.Definition.Units))
	opened := map[string]*remotestore.Client{}
	closeAll := func() {
		for _, c := range opened {
			if c != nil {
				c.Close()
			}
		}
	}
	for i, u := range v.Definition.Units {
		key := organization.Digest(u.Definition.Scope)
		c, yes := opened[key]
		if !yes {
			var err error
			c, err = api.openRecoveryScope(ctx, u.Definition.Scope)
			if err != nil {
				closeAll()
				return nil, func() {}, err
			}
			opened[key] = c
		}
		clients[i] = c
	}
	return clients, closeAll, nil
}
func (api *organizationAPI) serveMediaRemovalRecord(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	v, err := api.jobs.MediaRemoval(ctx, r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	var in mediaRemovalInput
	if r.Method != http.MethodGet && !decodeOrganizationRequest(w, r, &in, "invalid media-removal action") {
		return
	}
	action := r.PathValue("action")
	if action == "close" {
		if !in.Confirm || !in.ConfirmClose {
			writeAPIError(w, 400, 400, "explicit whole-batch stop confirmation required")
			return
		}
		if v.State != "closed" && v.State != "completed" {
			if err := api.jobs.CloseMediaRemoval(ctx, v.ID); err != nil {
				writeOrganizationError(w, err)
				return
			}
			v, err = api.jobs.MediaRemoval(ctx, v.ID)
			if err != nil {
				writeOrganizationError(w, err)
				return
			}
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if r.Method != http.MethodGet && action != "continue" && action != "return-capture" {
		writeAPIError(w, 404, 404, "media-removal action not found")
		return
	}
	if action == "continue" && !mediaRemovalGrants(in, v) {
		writeAPIError(w, 400, 400, "all separate confirmations and a newly read batch review are required")
		return
	}
	if action == "return-capture" && (!in.Confirm || !in.ConfirmReturnCapture || !in.ConfirmOldExecutorsStopped) {
		writeAPIError(w, 400, 400, "independent no-overwrite capture return and stopped-writer confirmation required")
		return
	}
	if r.Method != http.MethodGet && in.Fingerprint != "" && in.Fingerprint == v.LastReview {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if r.Method == http.MethodGet && (v.State == "closed" || v.State == "completed") {
		v, err = api.jobs.ReviewMediaRemoval(ctx, v, nil)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	closeGuards, err := api.recoveryGuards(ctx, organization.RecoveryDefinition{Owners: v.Definition.Owners})
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer closeGuards()
	clients, closeClients, err := api.mediaRemovalClients(ctx, v)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer closeClients()
	validate := func(call context.Context) error {
		for _, u := range v.Definition.Units {
			if err := api.recoveryScopeCurrent(call, u.Definition.Scope); err != nil {
				return err
			}
		}
		return api.jobs.CheckMediaRemovalOpen(call, v.ID)
	}
	call, stop := recoveryWatch(ctx, validate)
	defer stop()
	review, err := api.jobs.ReviewMediaRemoval(call, v, clients)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
		return
	}
	if in.Fingerprint != review.Fingerprint {
		writeOrganizationError(w, organization.ErrState)
		return
	}
	current := func(s organization.RecoveryScope) error { return api.recoveryScopeCurrent(call, s) }
	if action == "return-capture" {
		err = api.jobs.ReturnMediaRemovalCapture(call, review, in.Ordinal, current)
	} else if review.CanContinue {
		err = api.jobs.ExecuteMediaRemoval(call, review, clients, review.Fingerprint, current)
	} else {
		err = organization.ErrState
	}
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	v, err = api.jobs.MediaRemoval(ctx, v.ID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
}

func decodeLegacyMediaRemoval(w http.ResponseWriter, r *http.Request) (mediaRemovalInput, bool) {
	var body struct {
		mediaRemovalInput
		LogIDs json.RawMessage `json:"logids"`
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media == "application/json" {
		if !decodeOrganizationRequest(w, r, &body, "invalid history-removal request") {
			return body.mediaRemovalInput, false
		}
	} else if media == "application/x-www-form-urlencoded" || media == "" {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if r.ParseForm() != nil {
			writeAPIError(w, 400, 400, "invalid history-removal form")
			return body.mediaRemovalInput, false
		}
		for key := range r.PostForm {
			if key != "logids" && key != "flag" {
				writeAPIError(w, 400, 400, "use JSON for independently reviewed file deletion")
				return body.mediaRemovalInput, false
			}
		}
		if len(r.PostForm["flag"]) > 1 || len(r.PostForm["logids"]) == 0 {
			writeAPIError(w, 400, 400, "invalid history-removal fields")
			return body.mediaRemovalInput, false
		}
		body.Flag = r.PostForm.Get("flag")
		raw := r.PostForm["logids"]
		if len(raw) == 1 && strings.HasPrefix(raw[0], "[") {
			body.LogIDs = json.RawMessage(raw[0])
		} else {
			body.LogIDs, _ = json.Marshal(raw)
		}
	} else {
		writeAPIError(w, 400, 400, "unsupported history-removal content type")
		return body.mediaRemovalInput, false
	}
	if len(body.LogIDs) > 0 {
		if len(body.HistoryIDs) > 0 {
			writeAPIError(w, 400, 400, "select logids or historyIds, not both")
			return body.mediaRemovalInput, false
		}
		ids, err := legacyUnknownIDs(body.LogIDs)
		if err != nil {
			writeAPIError(w, 400, 400, "invalid exact history IDs")
			return body.mediaRemovalInput, false
		}
		for _, id := range ids {
			body.HistoryIDs = append(body.HistoryIDs, strconv.FormatInt(id.ID, 10))
		}
	}
	return body.mediaRemovalInput, true
}
func (api *organizationAPI) serveCompatHistoryRemovalPreview(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	in, ok := decodeLegacyMediaRemoval(w, r)
	if !ok {
		return
	}
	api.mediaRemovalPreviewInput(w, r, in)
}
func (api *organizationAPI) serveCompatHistoryRemoval(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	in, ok := decodeLegacyMediaRemoval(w, r)
	if !ok {
		return
	}
	if in.Flag != "" || len(in.Paths) > 0 || in.Fingerprint != "" {
		api.mediaRemovalCreateInput(w, r, in)
		return
	}
	// Old REST's form only requested record deletion. No file/account lookup,
	// implicit cleanup or automatic closure of active associations is granted.
	in.ConfirmRead = true
	if in.Decision == "" {
		in.Decision = "legacy explicit record-only history deletion"
	}
	actor, err := api.recoveryActor(r)
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	v, closeAll, err := api.prepareMediaRemoval(ctx, in, actor)
	defer closeAll()
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if len(v.Owners) > 0 {
		writeAPIError(w, 409, 409, "related native jobs require a complete independent review before closing or removing history")
		return
	}
	id, err := api.jobs.CreateMediaRemoval(ctx, v.Definition, v.Fingerprint)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	v, err = api.jobs.MediaRemoval(ctx, id)
	if err == nil {
		v, err = api.jobs.ReviewMediaRemoval(ctx, v, []*remotestore.Client{})
	}
	if err == nil {
		err = api.jobs.ExecuteMediaRemoval(ctx, v, []*remotestore.Client{}, v.Fingerprint, func(organization.RecoveryScope) error { return nil })
	}
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	saved, err := api.jobs.MediaRemoval(ctx, id)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "retcode": 0, "data": saved})
}

func (api *organizationAPI) decodeCompatMediaFiles(w http.ResponseWriter, r *http.Request) (mediaRemovalInput, bool) {
	var body struct {
		mediaRemovalInput
		Files  []string `json:"files"`
		Driver string   `json:"driver"`
	}
	if !decodeOrganizationRequest(w, r, &body, "invalid exact media-file selection") {
		return body.mediaRemovalInput, false
	}
	if len(body.Files) > 0 {
		if len(body.Paths) > 0 || len(body.HistoryIDs) > 0 || body.Flag != "" || len(body.Files) > 1024 {
			writeAPIError(w, 400, 400, "select legacy files or a complete structured deletion batch, not both")
			return body.mediaRemovalInput, false
		}
		for _, file := range body.Files {
			p, err := api.mediaRemovalLocation(r.Context(), file, "file", body.Driver)
			if err != nil {
				writeOrganizationError(w, err)
				return body.mediaRemovalInput, false
			}
			body.Paths = append(body.Paths, p)
		}
	}
	return body.mediaRemovalInput, true
}
func (api *organizationAPI) serveCompatMediaFilesPreview(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	in, ok := api.decodeCompatMediaFiles(w, r)
	if !ok {
		return
	}
	api.mediaRemovalPreviewInput(w, r, in)
}
func (api *organizationAPI) serveCompatMediaFilesRemoval(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	in, ok := api.decodeCompatMediaFiles(w, r)
	if !ok {
		return
	}
	api.mediaRemovalCreateInput(w, r, in)
}
