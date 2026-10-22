package httpserver

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type libraryTriggerInput struct {
	Driver                     string `json:"driver,omitempty"`
	ConfirmRemoteExclusive     bool   `json:"confirmRemoteExclusive"`
	ConfirmRemoteRecoveryCopy  bool   `json:"confirmRemoteRecoveryCopy"`
	AllowDiscExpansion         bool   `json:"allowDiscExpansion,omitempty"`
	Path                       string `json:"path"`
	Force                      bool   `json:"force"`
	Review                     bool   `json:"review"`
	RequestID                  string `json:"requestId"`
	Fingerprint                string `json:"fingerprint"`
	Confirm                    bool   `json:"confirm"`
	ConfirmOverwrite           bool   `json:"confirmOverwrite"`
	ConfirmOldExecutorsStopped bool   `json:"confirmOldExecutorsStopped"`
}

func (api *libraryScraper) serveLegacyScrape(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	var in struct {
		Cmd  string              `json:"cmd"`
		Data libraryTriggerInput `json:"data"`
	}
	if !decodeOrganizationRequest(w, r, &in, "invalid legacy scrape command") {
		return
	}
	if in.Cmd != "media_path_scrap" {
		writeAPIError(w, 501, 501, "this legacy command has not been migrated; no Python request made")
		return
	}
	in.Data.Force = true
	api.trigger(w, r, in.Data, false)
}
func (api *libraryScraper) serveScrapeEvent(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	var in struct {
		Event string              `json:"event"`
		Data  libraryTriggerInput `json:"data"`
	}
	if !decodeOrganizationRequest(w, r, &in, "invalid scrape event") {
		return
	}
	if in.Event != "media.scrap.start" {
		writeAPIError(w, 400, 400, "only media.scrap.start is supported here")
		return
	}
	api.trigger(w, r, in.Data, true)
}
func (api *libraryScraper) trigger(w http.ResponseWriter, r *http.Request, in libraryTriggerInput, event bool) {
	if in.Driver != "" && organization.LibraryRemoteMode(in.Driver) == "" {
		writeOrganizationError(w, organization.ErrMode)
		return
	}
	if !in.Review && in.Driver != "" && (!in.ConfirmRemoteExclusive || !in.ConfirmRemoteRecoveryCopy) {
		writeAPIError(w, 400, 400, "remote scrape requires separate exclusive/non-atomic and retained-copy consent")
		return
	}
	if !in.Review && (!in.Confirm || !in.ConfirmOldExecutorsStopped || in.Force && !in.ConfirmOverwrite || len(in.RequestID) != 32 || len(in.Fingerprint) != 64) {
		writeAPIError(w, 400, 400, "request a readonly review, then confirm its fingerprint, stopped writers and overwrite consent")
		return
	}
	claims, err := api.intake.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if !in.Review {
		old, err := api.intake.organizer.jobs.ExistingLibraryRequest(ctx, claims.Username, in.RequestID, in.Fingerprint)
		if err == nil {
			writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": old, "msg": "原任务回执；没有重复提交"})
			return
		}
		if !errors.Is(err, organization.ErrNotFound) {
			writeOrganizationError(w, err)
			return
		}
	}
	if in.RequestID == "" {
		in.RequestID, err = newIndexerClaim()
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
	}
	if len(in.Path) > 4096 || !filepath.IsAbs(in.Path) || strings.ContainsAny(in.Path, "\x00\r\n\\") {
		writeAPIError(w, 400, 400, "an explicitly configured library path is required")
		return
	}
	roots, err := api.intake.organizer.roots(ctx)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	rootID, relative := "", ""
	longest := -1
	for _, root := range roots.Targets {
		if root.Type != "MOV" && root.Type != "TV" && root.Type != "ANIME" {
			continue
		}
		var bound organization.LibraryRoot
		if in.Driver != "" {
			bound, _, err = api.remoteDefinition(ctx, root, in.Driver)
		} else {
			bound, err = libraryRootSnapshot(root)
		}
		if err != nil {
			continue
		}
		for _, base := range []string{root.Path, bound.Path} {
			rel, err := filepath.Rel(base, filepath.Clean(in.Path))
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && len(base) > longest {
				rootID, relative, longest = root.ID, rel, len(base)
			}
		}
	}
	if rootID == "" {
		writeAPIError(w, 409, 409, "path is outside the configured media libraries")
		return
	}
	mode := "no_force"
	if in.Force {
		mode = "force_all"
	}
	p, err := api.plan(ctx, libraryPlanInput{Driver: in.Driver, AllowDiscExpansion: in.AllowDiscExpansion, RootID: rootID, Path: relative, Mode: mode, Excludes: []string{}, RequestID: in.RequestID}, claims.Username)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	p.definition.Root.Kind = "auto"
	var raw map[string]string
	var control libraryPluginControl
	if event {
		raw, err = api.intake.system.ReadMany(ctx, libraryPluginKeys)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		installed, e := indexerInstalled(raw["UserInstalledPlugins"], libraryPluginID)
		if e != nil || !installed {
			writeAPIError(w, 409, 409, "LibraryScraper is not installed")
			return
		}
		control, err = decodeLibraryControl(raw[libraryControlKey])
		if err != nil || len(control.Epoch) != 32 {
			writeAPIError(w, 409, 409, "explicit native plugin handoff required before accepting scrape events")
			return
		}
		p.definition.Schedule = &organization.LibraryScheduleOrigin{Epoch: control.Epoch, Batch: in.RequestID, Event: true}
	}
	p.Fingerprint = organization.Digest([]any{p.definition, p.files})
	if in.Review {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "previewOnly": true, "data": p, "msg": "只读审核；没有登记刮削任务"})
		return
	}
	if p.Fingerprint != in.Fingerprint {
		writeAPIError(w, 409, 409, "reviewed scrape event or media scope changed")
		return
	}
	if p.Total == 0 {
		writeAPIError(w, 422, 422, "no eligible media in reviewed scope")
		return
	}
	if err := approveLibraryRemote(&p.definition, claims.Username, "library-event", in.ConfirmRemoteExclusive, in.ConfirmRemoteRecoveryCopy); err != nil {
		writeOrganizationError(w, err)
		return
	}
	id := organization.Digest([]string{claims.Username, in.RequestID})
	if event {
		run := organization.LibraryPluginRun{ID: in.RequestID, Epoch: control.Epoch, State: "queued", Source: "event", Created: time.Now().UTC().Format(time.RFC3339Nano), Updated: time.Now().UTC().Format(time.RFC3339Nano), Jobs: []string{id}}
		err = api.intake.organizer.jobs.LibraryPluginMutation(ctx, raw, map[string]string{}, "", &run, []organization.LibraryPrepared{{Fingerprint: p.Fingerprint, Definition: p.definition, Files: p.files}})
	} else {
		id, err = api.intake.organizer.jobs.CreateLibraryJob(ctx, p.Fingerprint, p.definition, p.files)
	}
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	job, err := api.intake.organizer.jobs.LibraryJob(ctx, id)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": job, "msg": "刮削任务已持久登记，等待原生后台处理"})
}
