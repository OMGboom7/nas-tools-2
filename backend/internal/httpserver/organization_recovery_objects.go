package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

type recoveryInput struct {
	RootID                     string `json:"rootId"`
	Driver                     string `json:"driver"`
	Path                       string `json:"path"`
	RequestID                  string `json:"requestId"`
	Decision                   string `json:"decision"`
	Fingerprint                string `json:"fingerprint"`
	ConfirmRead                bool   `json:"confirmRead"`
	Confirm                    bool   `json:"confirm"`
	ConfirmPermanentDeletion   bool   `json:"confirmPermanentDeletion"`
	ConfirmOnlyCopyRisk        bool   `json:"confirmOnlyCopyRisk"`
	ConfirmOldExecutorsStopped bool   `json:"confirmOldExecutorsStopped"`
	ConfirmCloseRelated        bool   `json:"confirmCloseRelated"`
	ConfirmRemoteExclusive     bool   `json:"confirmRemoteExclusive"`
	ConfirmReturnCapture       bool   `json:"confirmReturnCapture"`
	ConfirmClose               bool   `json:"confirmClose"`
}

func recoveryGrants(in recoveryInput, remote bool) bool {
	return in.Confirm && in.ConfirmRead && in.ConfirmPermanentDeletion && in.ConfirmOnlyCopyRisk && in.ConfirmOldExecutorsStopped && in.ConfirmCloseRelated && (!remote || in.ConfirmRemoteExclusive)
}
func (api *organizationAPI) recoveryScope(ctx context.Context, rootID, driver string) (organization.RecoveryScope, *remotestore.Client, error) {
	s := organization.RecoveryScope{}
	if driver != "" && organization.LibraryRemoteMode(driver) == "" {
		return s, nil, organization.ErrMode
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return s, nil, err
	}
	var root *organizationRoot
	for _, r := range append(append([]organizationRoot{}, roots.Sources...), roots.Targets...) {
		if r.ID == rootID {
			copy := r
			root = &copy
			break
		}
	}
	if root == nil {
		return s, nil, organization.ErrPath
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return s, nil, err
	}
	s.ConfigDigest = organization.Digest(snapshot)
	if driver == "" {
		s.Root, err = libraryRootSnapshot(*root)
		return s, nil, err
	}
	logical, err := logicalRemoteRoot(root.Path)
	if err != nil {
		return s, nil, err
	}
	c, err := api.remoteFactory(organization.LibraryRemoteMode(driver)).Open(nil)
	if err != nil {
		return s, nil, err
	}
	if err := c.Probe(ctx, logical); err != nil {
		c.Close()
		return s, nil, err
	}
	b := c.Binding()
	s.Remote = &b
	s.Root = organization.LibraryRoot{ID: root.ID, Path: logical, Identity: organization.RemoteTargetIdentityFor(organization.LibraryRemoteMode(driver), logical), Kind: "recovery"}
	return s, c, nil
}
func (api *organizationAPI) recoveryScopeCurrent(ctx context.Context, s organization.RecoveryScope) error {
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return err
	}
	if organization.Digest(snapshot) != s.ConfigDigest {
		return organization.ErrState
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return err
	}
	for _, root := range append(append([]organizationRoot{}, roots.Sources...), roots.Targets...) {
		if root.ID != s.Root.ID {
			continue
		}
		if s.Remote != nil {
			logical, err := logicalRemoteRoot(root.Path)
			if err != nil || logical != s.Root.Path {
				return organization.ErrState
			}
			return api.remoteFactory(organization.LibraryRemoteMode(s.Remote.Driver)).Validate(*s.Remote)
		}
		current, err := libraryRootSnapshot(root)
		if err != nil || current.ID != s.Root.ID || current.Path != s.Root.Path || current.Identity != s.Root.Identity {
			return organization.ErrState
		}
		return nil
	}
	return organization.ErrState
}
func (api *organizationAPI) openRecoveryScope(ctx context.Context, s organization.RecoveryScope) (*remotestore.Client, error) {
	if err := api.recoveryScopeCurrent(ctx, s); err != nil {
		return nil, err
	}
	if s.Remote == nil {
		return nil, nil
	}
	return api.remoteFactory(organization.LibraryRemoteMode(s.Remote.Driver)).Open(s.Remote)
}
func (api *organizationAPI) recoveryGuards(ctx context.Context, d organization.RecoveryDefinition) (func(), error) {
	names := organization.RecoveryOwnerGuardNames(d)
	// AcquireJob is nonblocking. Every operation uses these existing guards;
	// a busy old writer is not mistaken for a safely stopped executor.
	static := names[:4]
	related := append([]string{}, names[4:]...)
	sort.Strings(related)
	names = append(static, related...)
	closers := []func(){}
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	for _, name := range names {
		guard, err := api.jobs.AcquireJob(ctx, name)
		if err != nil {
			closeAll()
			return func() {}, err
		}
		closers = append(closers, func() { guard.Close() })
	}
	return closeAll, nil
}
func recoveryWatch(ctx context.Context, check func(context.Context) error) (context.Context, func()) {
	call, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-call.Done():
				return
			case <-ticker.C:
				if check(call) != nil {
					cancel()
					return
				}
			}
		}
	}()
	return call, func() { cancel(); <-done }
}
func (api *organizationAPI) recoveryActor(r *http.Request) (string, error) {
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		return "", err
	}
	return claims.Username, nil
}
func (api *organizationAPI) serveRecoveryStatus(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	roots, err := api.roots(ctx)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	disposals, err := api.jobs.ListRecoveryDisposals(ctx)
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
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"roots": append(append([]organizationRoot{}, roots.Sources...), roots.Targets...), "drivers": drivers, "disposals": disposals}})
}
func (api *organizationAPI) serveRecoveryBrowse(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var in recoveryInput
	if !decodeOrganizationRequest(w, r, &in, "invalid recovery browse") {
		return
	}
	if in.Path == "" {
		in.Path = "."
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	s, c, err := api.recoveryScope(ctx, in.RootID, in.Driver)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if c != nil {
		defer c.Close()
	}
	closeGuard, err := api.recoveryGuards(ctx, organization.RecoveryDefinition{})
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer closeGuard()
	entries, err := organization.RecoveryBrowse(ctx, s, in.Path, c)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if err := api.recoveryScopeCurrent(ctx, s); err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"rootId": in.RootID, "driver": in.Driver, "path": in.Path, "entries": entries}})
}
func (api *organizationAPI) reviewRecovery(ctx context.Context, in recoveryInput, actor string) (organization.RecoveryObjectReview, func(), *remotestore.Client, error) {
	empty := func() {}
	if !in.ConfirmRead {
		return organization.RecoveryObjectReview{}, empty, nil, organization.ErrState
	}
	s, c, err := api.recoveryScope(ctx, in.RootID, in.Driver)
	if err != nil {
		return organization.RecoveryObjectReview{}, empty, nil, err
	}
	closeClient := func() {
		if c != nil {
			_ = c.Close()
		}
	}
	if in.RequestID == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			closeClient()
			return organization.RecoveryObjectReview{}, empty, nil, err
		}
		in.RequestID = hex.EncodeToString(nonce[:])
	}
	d := organization.RecoveryDefinition{Scope: s, Path: in.Path, Actor: actor, RequestID: in.RequestID, Decision: strings.TrimSpace(in.Decision)}
	owners, err := api.jobs.RecoveryObjectOwners(ctx, s, in.Path)
	if err != nil {
		closeClient()
		return organization.RecoveryObjectReview{}, empty, nil, err
	}
	d.Owners = owners
	closeGuards, err := api.recoveryGuards(ctx, d)
	if err != nil {
		closeClient()
		return organization.RecoveryObjectReview{}, empty, nil, err
	}
	closeAll := func() { closeGuards(); closeClient() }
	call, stopWatch := recoveryWatch(ctx, func(call context.Context) error { return api.recoveryScopeCurrent(call, s) })
	nodes, err := organization.RecoverySnapshot(call, s, in.Path, c)
	stopWatch()
	if err != nil {
		closeAll()
		return organization.RecoveryObjectReview{}, empty, nil, err
	}
	d.Nodes = nodes
	preview, err := api.jobs.ReviewRecoveryObjects(ctx, d)
	if err != nil || organization.Digest(preview.Owners) != organization.Digest(owners) {
		closeAll()
		return preview, empty, nil, organization.ErrState
	}
	if err := api.recoveryScopeCurrent(ctx, s); err != nil {
		closeAll()
		return preview, empty, nil, err
	}
	return preview, closeAll, c, nil
}
func (api *organizationAPI) serveRecoveryPreview(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var in recoveryInput
	if !decodeOrganizationRequest(w, r, &in, "invalid independent recovery review") {
		return
	}
	actor, err := api.recoveryActor(r)
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	preview, closeAll, _, err := api.reviewRecovery(ctx, in, actor)
	defer closeAll()
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": preview})
}
func (api *organizationAPI) serveRecoveryCreate(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var in recoveryInput
	if !decodeOrganizationRequest(w, r, &in, "invalid independent recovery disposal") {
		return
	}
	if !recoveryGrants(in, in.Driver != "") || len(in.RequestID) != 32 || len(in.Fingerprint) != 64 {
		writeAPIError(w, 400, 400, "fresh full review and separate irreversible/only-copy/stopped-writer/related-task consent required")
		return
	}
	actor, err := api.recoveryActor(r)
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	id := organization.RecoveryDisposalID(actor, in.RequestID)
	if saved, err := api.jobs.RecoveryDisposal(ctx, id); err == nil {
		if saved.Definition.Removal != nil {
			writeAPIError(w, 404, 404, "use the complete media-removal coordinator")
			return
		}
		if saved.RootID != in.RootID || saved.Driver != in.Driver || saved.Path != in.Path || organization.Digest(saved.Definition) != in.Fingerprint {
			writeOrganizationError(w, organization.ErrState)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": saved})
		return
	} else if !errors.Is(err, organization.ErrNotFound) {
		writeOrganizationError(w, err)
		return
	}
	preview, closeAll, _, err := api.reviewRecovery(ctx, in, actor)
	defer closeAll()
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if preview.Fingerprint != in.Fingerprint {
		writeOrganizationError(w, organization.ErrState)
		return
	}
	id, err = api.jobs.CreateRecoveryDisposal(ctx, preview.Definition, preview.Fingerprint)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	// Persist the reviewed worklist first. There is deliberately no worker:
	// the operator can now inspect this stable ID, explicitly execute once,
	// and stop an in-flight attempt from another request without losing its ID.
	result, err := api.jobs.RecoveryDisposal(ctx, id)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"code": 0, "success": true, "data": result})
}
func (api *organizationAPI) serveRecoveryRecord(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	saved, err := api.jobs.RecoveryDisposal(ctx, r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if saved.Definition.Removal != nil {
		writeAPIError(w, 404, 404, "use the complete reviewed media-removal coordinator")
		return
	}
	var in recoveryInput
	if r.Method != http.MethodGet {
		if !decodeOrganizationRequest(w, r, &in, "invalid recovery disposition action") {
			return
		}
	}
	action := r.PathValue("action")
	if action == "close" {
		if !in.Confirm || !in.ConfirmClose {
			writeAPIError(w, 400, 400, "explicit stop confirmation required")
			return
		}
		if saved.State == "closed" || saved.State == "completed" || saved.State == "returned" {
			writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": saved})
			return
		}
		if err := api.jobs.CloseRecoveryDisposal(ctx, saved.ID); err != nil {
			writeOrganizationError(w, err)
			return
		}
		result, err := api.jobs.RecoveryDisposal(ctx, saved.ID)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": result})
		return
	}
	if r.Method != http.MethodGet && action != "continue" && action != "return-capture" {
		writeAPIError(w, 404, 404, "recovery action not found")
		return
	}
	if action == "continue" && !recoveryGrants(in, saved.Driver != "") {
		writeAPIError(w, 400, 400, "all independent disposal confirmations required")
		return
	}
	if action == "return-capture" && (!in.Confirm || !in.ConfirmReturnCapture || !in.ConfirmOldExecutorsStopped) {
		writeAPIError(w, 400, 400, "separate whole-capture return and stopped-writer confirmation required")
		return
	}
	if r.Method != http.MethodGet && in.Fingerprint != "" && in.Fingerprint == saved.LastReview {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": saved})
		return
	}
	if r.Method == http.MethodGet && (saved.State == "completed" || saved.State == "returned" || saved.State == "closed") {
		review, err := api.jobs.ReviewRecoveryDisposal(ctx, saved, nil)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
		return
	}
	closeGuards, err := api.recoveryGuards(ctx, saved.Definition)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer closeGuards()
	c, err := api.openRecoveryScope(ctx, saved.Definition.Scope)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if c != nil {
		defer c.Close()
	}
	call, stopWatch := recoveryWatch(ctx, func(call context.Context) error {
		if err := api.recoveryScopeCurrent(call, saved.Definition.Scope); err != nil {
			return err
		}
		return api.jobs.CheckRecoveryOpen(call, saved.ID)
	})
	defer stopWatch()
	review, err := api.jobs.ReviewRecoveryDisposal(call, saved, c)
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
	validate := func() error { return api.recoveryScopeCurrent(call, saved.Definition.Scope) }
	if action == "return-capture" {
		err = api.jobs.ReturnRecoveryCapture(call, saved, review, validate)
	} else if review.CanContinue {
		err = api.jobs.ExecuteRecoveryDisposal(call, saved, c, review.Fingerprint, validate)
	} else {
		err = organization.ErrState
	}
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	result, err := api.jobs.RecoveryDisposal(ctx, saved.ID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": result})
}
