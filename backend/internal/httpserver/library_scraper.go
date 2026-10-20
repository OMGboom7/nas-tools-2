package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

type libraryScraper struct{ intake *intakeScraper }
type libraryPlanInput struct {
	RootID    string   `json:"rootId"`
	Path      string   `json:"path"`
	Mode      string   `json:"mode"`
	Excludes  []string `json:"excludes"`
	RequestID string   `json:"requestId"`
}
type libraryPreviewItem struct {
	Path    string `json:"path"`
	Blocked string `json:"blocked,omitempty"`
}
type libraryPreview struct {
	Input       libraryPlanInput     `json:"input"`
	Fingerprint string               `json:"fingerprint"`
	Total       int                  `json:"total"`
	Skipped     int                  `json:"skipped"`
	Blocked     int                  `json:"blocked"`
	Sample      []libraryPreviewItem `json:"sample"`
	definition  organization.LibraryDefinition
	files       []organization.LibraryMedia
}

func (api *libraryScraper) authorize(w http.ResponseWriter, r *http.Request) bool {
	if api == nil || api.intake == nil {
		writeAPIError(w, 503, 503, "native library scraper unavailable")
		return false
	}
	return api.intake.authorize(w, r)
}
func libraryRootSnapshot(root organizationRoot) (organization.LibraryRoot, error) {
	p, err := filepath.EvalSymlinks(root.Path)
	if err != nil {
		return organization.LibraryRoot{}, err
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		return organization.LibraryRoot{}, organization.ErrPath
	}
	kind := "tv"
	if root.Type == "MOV" {
		kind = "movie"
	}
	return organization.LibraryRoot{ID: root.ID, Path: p, Identity: organization.Identity(info), Kind: kind}, nil
}
func (api *libraryScraper) plan(ctx context.Context, input libraryPlanInput, actor string) (libraryPreview, error) {
	p := libraryPreview{Input: input, Sample: []libraryPreviewItem{}}
	if p.Input.Path == "" {
		p.Input.Path = "."
	}
	if !organization.LibraryMode(input.Mode) || len(input.Excludes) > 128 || len(input.RequestID) != 32 {
		return p, organization.ErrPath
	}
	if b, err := hex.DecodeString(input.RequestID); err != nil || hex.EncodeToString(b) != input.RequestID {
		return p, organization.ErrPath
	}
	sort.Strings(p.Input.Excludes)
	roots, err := api.intake.organizer.roots(ctx)
	if err != nil {
		return p, err
	}
	d := organization.LibraryDefinition{Selection: p.Input.Path, Mode: input.Mode, Excludes: p.Input.Excludes, Actor: actor, RequestID: input.RequestID, Allowed: []organization.LibraryRoot{}}
	for _, root := range roots.Targets {
		if root.ID == input.RootID && (root.Type == "MOV" || root.Type == "TV" || root.Type == "ANIME") {
			d.Root, err = libraryRootSnapshot(root)
			if err != nil {
				return p, err
			}
		}
	}
	if d.Root.ID == "" {
		return p, organization.ErrPath
	}
	for _, root := range append(roots.Targets, roots.Sources...) {
		if root.ID == d.Root.ID {
			continue
		}
		if candidate, e := libraryRootSnapshot(root); e == nil {
			d.Allowed = append(d.Allowed, candidate)
		}
	}
	snapshot, err := api.intake.organizer.service.configStore.Snapshot()
	if err != nil {
		return p, err
	}
	d.ConfigDigest = organization.Digest(snapshot)
	d.Settings, err = api.intake.system.Get(ctx, scraper.ConfigKey)
	if err != nil {
		return p, err
	}
	d.SettingsDigest = organization.ScrapeConfigDigest(d.Settings)
	if _, _, err := scraper.Parse(d.Settings); err != nil {
		return p, err
	}
	scan, err := organization.ScanLibrary(ctx, d)
	if err != nil {
		return p, err
	}
	d.Count = len(scan.Files)
	p.Total, p.Skipped = len(scan.Files), scan.Skipped
	for _, m := range scan.Files {
		if m.Blocked != "" {
			p.Blocked++
		}
		if len(p.Sample) < 200 {
			p.Sample = append(p.Sample, libraryPreviewItem{m.Path, m.Blocked})
		}
	}
	p.definition, p.files = d, scan.Files
	p.Fingerprint = organization.Digest([]any{d, scan.Files})
	return p, nil
}
func (api *libraryScraper) serveStatus(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	roots, err := api.intake.organizer.roots(ctx)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	targets := []organizationRoot{}
	for _, root := range roots.Targets {
		if root.Type == "MOV" || root.Type == "TV" || root.Type == "ANIME" {
			targets = append(targets, root)
		}
	}
	jobs, err := api.intake.organizer.jobs.LibraryJobs(ctx, false)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"roots": targets, "jobs": jobs}})
}
func (api *libraryScraper) servePreview(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	var input libraryPlanInput
	if !decodeOrganizationRequest(w, r, &input, "invalid library scope") {
		return
	}
	if input.RequestID == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			writeOrganizationError(w, err)
			return
		}
		input.RequestID = hex.EncodeToString(nonce[:])
	}
	claims, err := api.intake.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	p, err := api.plan(ctx, input, claims.Username)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": p})
}
func (api *libraryScraper) serveCreate(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	var in struct {
		libraryPlanInput
		Fingerprint                string `json:"fingerprint"`
		Confirm                    bool   `json:"confirm"`
		ConfirmOverwrite           bool   `json:"confirmOverwrite"`
		ConfirmOldExecutorsStopped bool   `json:"confirmOldExecutorsStopped"`
	}
	if !decodeOrganizationRequest(w, r, &in, "invalid library job") {
		return
	}
	if !in.Confirm || !in.ConfirmOldExecutorsStopped || in.Mode != "no_force" && !in.ConfirmOverwrite || len(in.Fingerprint) != 64 || len(in.RequestID) != 32 {
		writeAPIError(w, 400, 400, "reviewed scope, stopped external writers and explicit overwrite consent required")
		return
	}
	claims, err := api.intake.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	// A lost submission can be observed without scanning or publishing again.
	old, err := api.intake.organizer.jobs.ExistingLibraryRequest(ctx, claims.Username, in.RequestID, in.Fingerprint)
	if err == nil {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": old})
		return
	}
	if !errors.Is(err, organization.ErrNotFound) {
		writeOrganizationError(w, err)
		return
	}
	p, err := api.plan(ctx, in.libraryPlanInput, claims.Username)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if p.Fingerprint != in.Fingerprint {
		writeAPIError(w, 409, 409, "library scope changed; review again")
		return
	}
	if p.Total == 0 {
		writeAPIError(w, 422, 422, "no media files in reviewed scope")
		return
	}
	id, err := api.intake.organizer.jobs.CreateLibraryJob(ctx, p.Fingerprint, p.definition, p.files)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	job, err := api.intake.organizer.jobs.LibraryJob(ctx, id)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": job})
}
func libraryPage(r *http.Request) (int, error) {
	s := r.URL.Query().Get("page")
	if s == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 1000 {
		return 0, organization.ErrLimit
	}
	return n, nil
}
func (api *libraryScraper) serveJob(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	page, err := libraryPage(r)
	if err != nil {
		writeAPIError(w, 400, 400, "invalid library receipt page")
		return
	}
	job, err := api.intake.organizer.jobs.LibraryJob(ctx, r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	items, err := api.intake.organizer.jobs.LibraryItems(ctx, job.ID, page)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	// Output paging is separate from media paging: a file can create many outputs.
	opage := 1
	if s := r.URL.Query().Get("outputPage"); s != "" {
		opage, err = strconv.Atoi(s)
	}
	if err != nil || opage < 1 || opage > 10000 {
		writeAPIError(w, 400, 400, "invalid output page")
		return
	}
	outputs, err := api.intake.organizer.jobs.LibraryOutputs(ctx, job.ID, opage)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"job": job, "items": items, "outputs": outputs, "page": page, "outputPage": opage}})
}
func (api *libraryScraper) serveCancel(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeOrganizationRequest(w, r, &in, "invalid stop request") {
		return
	}
	if !in.Confirm {
		writeAPIError(w, 400, 400, "explicit stop confirmation required")
		return
	}
	if err := api.intake.organizer.jobs.CancelLibraryJob(r.Context(), r.PathValue("id")); err != nil {
		writeOrganizationError(w, err)
		return
	}
	api.serveJob(w, r)
}

func (api *libraryScraper) rootCurrent(ctx context.Context, d organization.LibraryDefinition) error {
	roots, err := api.intake.organizer.roots(ctx)
	if err != nil {
		return err
	}
	for _, root := range roots.Targets {
		if root.ID == d.Root.ID {
			current, err := libraryRootSnapshot(root)
			if err == nil && current == d.Root {
				return nil
			}
		}
	}
	return organization.ErrState
}
func (api *libraryScraper) current(ctx context.Context, job organization.LibraryJob) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := api.intake.organizer.jobs.CheckLibraryRunning(ctx, job.ID); err != nil {
		return err
	}
	snapshot, err := api.intake.organizer.service.configStore.Snapshot()
	if err != nil {
		return err
	}
	if organization.Digest(snapshot) != job.Definition.ConfigDigest {
		return organization.ErrState
	}
	raw, err := api.intake.system.Get(ctx, scraper.ConfigKey)
	if err != nil {
		return err
	}
	if organization.ScrapeConfigDigest(raw) != job.Definition.SettingsDigest {
		return organization.ErrState
	}
	roots, err := api.intake.organizer.roots(ctx)
	if err != nil {
		return err
	}
	allowed := []organization.LibraryRoot{}
	for _, root := range append(roots.Targets, roots.Sources...) {
		if root.ID == job.Definition.Root.ID {
			continue
		}
		if candidate, e := libraryRootSnapshot(root); e == nil {
			allowed = append(allowed, candidate)
		}
	}
	if organization.Digest(allowed) != organization.Digest(job.Definition.Allowed) {
		return organization.ErrState
	}
	return api.rootCurrent(ctx, job.Definition)
}

// Recovery is administrator-initiated, with no network metadata lookups and no
// media file operations. It is never a request to resume a scraper job.
func (api *libraryScraper) serveOutput(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	guard, err := api.intake.organizer.jobs.AcquireJob(ctx, "library-scraper-publication")
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	out, err := api.intake.organizer.jobs.LibraryOutput(ctx, r.PathValue("output"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	job, err := api.intake.organizer.jobs.LibraryJob(ctx, out.JobID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if job.State == "ready" || job.State == "running" {
		writeAPIError(w, 409, 409, "stop the job before inspecting recovery evidence")
		return
	}
	if err := api.rootCurrent(ctx, job.Definition); err != nil {
		writeOrganizationError(w, err)
		return
	}
	evidence, observeErr := organization.ObserveLibrarySidecar(ctx, job.Definition, out.Proof)
	fingerprint := organization.LibraryOutputDigest(out)
	if r.Method == http.MethodGet {
		canRestore := out.Proof.Old.Identity != "" && (evidence == "replaced" || out.State == "restore_running") && (out.State == "replaced" || out.State == "uncertain" || out.State == "writing" || out.State == "restore_running")
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"output": out, "evidence": evidence, "fingerprint": fingerprint, "canRestore": canRestore, "canReconcile": observeErr == nil && (out.State == "writing" || out.State == "uncertain")}})
		return
	}
	var in struct {
		Fingerprint                string `json:"fingerprint"`
		Confirm                    bool   `json:"confirm"`
		ConfirmOldExecutorsStopped bool   `json:"confirmOldExecutorsStopped"`
	}
	if !decodeOrganizationRequest(w, r, &in, "invalid recovery request") {
		return
	}
	if !in.Confirm || !in.ConfirmOldExecutorsStopped || in.Fingerprint != fingerprint {
		writeAPIError(w, 409, 409, "fresh recovery review and stopped external writers confirmation required")
		return
	}
	switch r.PathValue("action") {
	case "reconcile":
		if observeErr != nil {
			writeOrganizationError(w, observeErr)
			return
		}
		err = api.intake.organizer.jobs.ReconcileLibraryOutput(ctx, out.ID, fingerprint, evidence, job.Definition.Root)
	case "restore":
		_, err = organization.RestoreLibrarySidecar(ctx, job.Definition, out.Proof, func() error {
			if err := api.rootCurrent(ctx, job.Definition); err != nil {
				return err
			}
			return api.intake.organizer.jobs.LibraryRestoreIntent(ctx, out.ID, organization.Digest(out.Proof))
		})
		if err == nil {
			persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err = api.intake.organizer.jobs.FinishLibraryRestore(persist, out.ID, job.Definition.Root)
			stop()
		}
	default:
		writeAPIError(w, 404, 404, "unknown recovery action")
		return
	}
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	saved, err := api.intake.organizer.jobs.LibraryOutput(ctx, out.ID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": saved})
}
