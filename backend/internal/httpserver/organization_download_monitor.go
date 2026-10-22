package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type downloadMonitorInput struct {
	ConfirmRemoteExclusive     bool     `json:"confirmRemoteExclusive,omitempty"`
	ConfirmRemoteRecoveryCopy  bool     `json:"confirmRemoteRecoveryCopy,omitempty"`
	IDs                        []string `json:"ids"`
	IncludeExisting            bool     `json:"includeExisting"`
	Fingerprint                string   `json:"fingerprint,omitempty"`
	Confirm                    bool     `json:"confirm,omitempty"`
	ConfirmOldExecutorsStopped bool     `json:"confirmOldExecutorsStopped,omitempty"`
	ConfirmSourceRemoval       bool     `json:"confirmSourceRemoval,omitempty"`
	ConfirmPauseTasks          bool     `json:"confirmPauseTasks,omitempty"`
	ConfirmResultWrites        bool     `json:"confirmResultWrites,omitempty"`
	ConfirmTaskRemoval         bool     `json:"confirmTaskRemoval,omitempty"`
	ConfirmExclusiveLabels     bool     `json:"confirmExclusiveLabels,omitempty"`
	AllowOmitted               bool     `json:"allowOmitted,omitempty"`
}
type downloadMonitorSource struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Mode        string   `json:"mode"`
	Directories []string `json:"directories"`
}
type downloadMonitorReview struct {
	RemoteTargets        []remoteMonitorTarget             `json:"remoteTargets,omitempty"`
	Lease                organization.DownloadMonitorLease `json:"lease"`
	Sources              []downloadMonitorSource           `json:"sources"`
	Candidates           map[string][]string               `json:"candidates"`
	NeedsSourceRemoval   bool                              `json:"needsSourceRemoval"`
	NeedsPauseTasks      bool                              `json:"needsPauseTasks"`
	NeedsTaskRemoval     bool                              `json:"needsTaskRemoval"`
	NeedsExclusiveLabels bool                              `json:"needsExclusiveLabels"`
	Fingerprint          string                            `json:"fingerprint"`
}

func (api organizationAPI) downloadMonitorScope(ctx context.Context, ids []string, enumerate bool, expected ...*organization.RemoteAutomation) (downloadMonitorReview, *recognitionFailure) {
	fail := func(code int, msg string) (downloadMonitorReview, *recognitionFailure) {
		return downloadMonitorReview{}, &recognitionFailure{code, msg}
	}
	if len(ids) == 0 || len(ids) > 16 {
		return fail(400, "select 1 to 16 exact downloaders")
	}
	ids = append([]string{}, ids...)
	sort.Strings(ids)
	review := downloadMonitorReview{Sources: []downloadMonitorSource{}, Candidates: map[string][]string{}, Lease: organization.DownloadMonitorLease{IDs: ids, Downloaders: map[string]string{}, Roots: []string{}}}
	seen := map[string]bool{}
	total := 0
	localTargets := false
	cloudModes := map[string]string{}
	cfg, err := api.service.configStore.Snapshot()
	if err != nil {
		return fail(503, "configuration unavailable")
	}
	review.Lease.ConfigDigest = organization.Digest(cfg)
	for _, id := range ids {
		n, err := syncConfigID(id)
		if err != nil || seen[id] {
			return fail(400, "invalid or duplicate downloader ID")
		}
		seen[id] = true
		item, err := api.service.downloaders.Get(ctx, n)
		if err != nil || item.Enabled != 1 || item.Transfer != 1 {
			return fail(422, "selected downloader must enable completion transfer")
		}
		mode, known := syncConfigMode(item.RmtMode)
		if !known || !organization.SupportedMode(mode) || organization.RemoteMode(mode) && !api.remoteFactory(mode).Available() {
			return fail(501, "remote or unknown completion mode is not migrated")
		}
		if organization.RemoteMode(mode) {
			cloudModes[organization.RemoteDriver(mode)] = mode
		} else {
			localTargets = true
		}
		if item.Type != "qbittorrent" && item.Type != "transmission" && item.Type != "aria2" {
			return fail(501, "selected completion protocol is not migrated")
		}
		review.Lease.Downloaders[id] = organization.Digest(item)
		source := downloadMonitorSource{ID: id, Name: item.Name, Type: item.Type, Mode: mode, Directories: []string{}}
		var dirs []downloadcompletion.Mapping
		if json.Unmarshal([]byte(item.DownloadDir), &dirs) != nil || len(dirs) == 0 || len(dirs) > 128 {
			return fail(422, "invalid completion directory mapping")
		}
		for _, d := range dirs {
			location, err := downloadcompletion.Resolve(item.DownloadDir, d.SavePath, true)
			if err != nil {
				return fail(422, "unsafe or ambiguous completion mapping")
			}
			root, err := filepath.EvalSymlinks(location.Root)
			if err != nil {
				return fail(422, "all selected source roots must exist")
			}
			info, err := os.Stat(root)
			if err != nil || !info.IsDir() {
				return fail(422, "selected source root unavailable")
			}
			source.Directories = append(source.Directories, location.Root)
			review.Lease.Roots = append(review.Lease.Roots, "source:"+id+":"+root+":"+organization.Identity(info))
		}
		review.Sources = append(review.Sources, source)
		review.NeedsSourceRemoval = review.NeedsSourceRemoval || organization.MoveMode(mode)
		review.NeedsPauseTasks = review.NeedsPauseTasks || organization.MoveMode(mode) && item.Type != "aria2"
		review.NeedsTaskRemoval = review.NeedsTaskRemoval || organization.MoveMode(mode) || item.Type == "aria2"
		review.NeedsExclusiveLabels = review.NeedsExclusiveLabels || item.Type == "transmission" && !organization.MoveMode(mode)
		if enumerate {
			c, err := api.clientFromDownloader(item)
			if err != nil {
				return fail(422, "invalid downloader client")
			}
			list, err := c.completed(ctx)
			if err != nil {
				return fail(502, "complete candidate enumeration unavailable or exceeds bounds; no partial baseline")
			}
			review.Candidates[id] = list
			total += len(list)
			if total > 2048 {
				return fail(422, "candidate scope exceeds 2048 tasks")
			}
		}
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return fail(422, "configured media roots unavailable")
	}
	usable := 0
	remoteUsable := 0
	for _, target := range roots.Targets {
		if target.Type == "" {
			continue
		}
		for _, driver := range []string{"rclone", "minio"} {
			mode, selected := cloudModes[driver]
			if !selected {
				continue
			}
			var grant *organization.RemoteAutomation
			if len(expected) > 0 {
				grant = expected[0]
			}
			proof, public, _, err := api.monitorRemoteRoot(ctx, target, grant, mode)
			if err != nil {
				return fail(422, "reviewed remote provider profile or media destination changed/unavailable")
			}
			review.Lease.Roots = append(review.Lease.Roots, proof)
			review.RemoteTargets = append(review.RemoteTargets, public)
			remoteUsable++
		}
		if !localTargets {
			continue
		}
		root, err := filepath.EvalSymlinks(target.Path)
		if err != nil {
			review.Lease.Roots = append(review.Lease.Roots, "missing:"+target.ID+":"+target.Path)
			continue
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return fail(422, "media destination unavailable")
		}
		usable++
		review.Lease.Roots = append(review.Lease.Roots, "target:"+target.ID+":"+root+":"+organization.Identity(info))
	}
	if localTargets && usable == 0 || len(cloudModes) > 0 && remoteUsable == 0 {
		return fail(422, "no existing configured media library")
	}
	sort.Strings(review.Lease.Roots)
	review.Lease.Remote = remoteAutomationFromRoots(review.Lease.Roots)
	return review, nil
}
func (api organizationAPI) downloadMonitorAuthorized(ctx context.Context) error {
	if api.downloadGeneration == "" {
		return nil
	}
	if err := api.jobs.VerifyDownloadMonitor(ctx, api.downloadGeneration); err != nil {
		return err
	}
	c, err := api.jobs.DownloadMonitorControl(ctx)
	if err != nil {
		return err
	}
	if api.auth == nil || !api.auth.service.IsAdministrator(c.Lease.Actor) {
		return organization.ErrState
	}
	current, failure := api.downloadMonitorScope(ctx, c.Lease.IDs, false, c.Lease.Remote)
	if failure != nil {
		return organization.ErrState
	}
	if current.Lease.ConfigDigest != c.Lease.ConfigDigest || organization.Digest(current.Lease.Roots) != organization.Digest(c.Lease.Roots) {
		return organization.ErrState
	}
	if current.Lease.Remote != nil && !c.Lease.Remote.Valid() {
		return organization.ErrState
	}
	return nil
}
func (api organizationAPI) serveDownloadMonitorReview(w http.ResponseWriter, r *http.Request) {
	api.serveDownloadMonitorEnable(w, r, false)
}
func (api organizationAPI) serveDownloadMonitorActivate(w http.ResponseWriter, r *http.Request) {
	api.serveDownloadMonitorEnable(w, r, true)
}
func (api organizationAPI) serveDownloadMonitorEnable(w http.ResponseWriter, r *http.Request, activate bool) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input downloadMonitorInput
	if !decodeOrganizationRequest(w, r, &input, "invalid completion monitor scope") {
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	review, failure := api.downloadMonitorScope(ctx, input.IDs, true)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	review.Lease.Actor = claims.Username
	review.Lease.IncludeExisting = input.IncludeExisting
	review.Lease.AllowOmitted = input.AllowOmitted
	review.Fingerprint = organization.Digest(review)
	if !activate {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
		return
	}
	if !input.Confirm || !input.ConfirmOldExecutorsStopped || !input.ConfirmResultWrites || review.Fingerprint != input.Fingerprint || review.NeedsSourceRemoval && !input.ConfirmSourceRemoval || review.NeedsPauseTasks && !input.ConfirmPauseTasks || review.NeedsTaskRemoval && !input.ConfirmTaskRemoval || review.NeedsExclusiveLabels && !input.ConfirmExclusiveLabels {
		writeAPIError(w, 409, 409, "fresh scope review and separate automatic execution/pause/move/result/removal/label authority required")
		return
	}
	review.Lease.SourceRemoval = input.ConfirmSourceRemoval
	review.Lease.PauseTasks = input.ConfirmPauseTasks
	review.Lease.ResultWrites = input.ConfirmResultWrites
	review.Lease.TaskRemoval = input.ConfirmTaskRemoval
	review.Lease.ExclusiveLabels = input.ConfirmExclusiveLabels
	if err := approveRemoteAutomation(review.Lease.Remote, input.ConfirmRemoteExclusive, input.ConfirmRemoteRecoveryCopy); err != nil {
		writeAPIError(w, 400, 400, "remote automation requires separate exclusive namespace/non-atomic visibility and retained recovery-copy consent")
		return
	}
	control, err := api.jobs.EnableDownloadMonitor(ctx, review.Lease, review.Candidates, time.Now())
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": control})
}
func (api organizationAPI) serveDownloadMonitorStatus(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	c, err := api.jobs.DownloadMonitorControl(r.Context())
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	events, err := api.jobs.DownloadMonitorEvents(r.Context(), c.Lease.Generation, false)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"control": c, "events": events}})
}
func (api organizationAPI) serveDownloadMonitorStop(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid monitor pause") {
		return
	}
	if !input.Confirm {
		writeAPIError(w, 400, 400, "explicit stop request confirmation required")
		return
	}
	if err := api.jobs.PauseDownloadMonitor(r.Context(), "administrator requested stop; inspect active file/pause/result receipts"); err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "pause requested; not a cleanup or completed-operation claim"})
}
func downloadObservation(review downloadCompletionReview) organization.DownloadObservation {
	task := review.Task
	task.State = ""
	return organization.DownloadObservation{Task: task, Files: review.Files, Root: review.SourceRoot, Identity: review.RootIdentity, Config: review.configDigest}
}

func (api organizationAPI) serveDownloadMonitorDismiss(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input struct {
		Key     string `json:"key"`
		Reason  string `json:"reason"`
		Confirm bool   `json:"confirm"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid completion event dismissal") {
		return
	}
	if !input.Confirm || len(input.Key) != 64 || strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 512 {
		writeAPIError(w, 400, 400, "review one event and provide a dismissal reason")
		return
	}
	guard, err := api.jobs.AcquireJob(r.Context(), "native-download-monitor")
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	event, err := api.jobs.GetDownloadMonitorEvent(r.Context(), input.Key)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	batch, err := api.jobs.FindDownloadBatch(r.Context(), event.DownloaderID, event.TaskID)
	batchID := event.BatchID
	if err == nil {
		batchID = batch.ID
		for _, id := range batch.Jobs {
			job, err := api.jobs.Get(r.Context(), id)
			if err != nil {
				writeOrganizationError(w, err)
				return
			}
			if job.State != "completed" && job.State != "cancelled" && job.State != "abandoned" && job.State != organization.SourceRestored && job.State != "remote_closed" && job.State != organization.RecoveryClosed {
				writeAPIError(w, 409, 409, "complete/cancel/abandon every linked job before dismissing its event")
				return
			}
		}
	} else if !errors.Is(err, organization.ErrNotFound) {
		writeOrganizationError(w, err)
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	if err := api.jobs.SetDownloadMonitorEvent(r.Context(), event, "dismissed", event.Snapshot, event.FirstSeen, batchID, claims.Username+": "+input.Reason, time.Now()); err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "event dismissed only; no retry, file operation, task resume or success history"})
}
func (api organizationAPI) downloadMonitorTick(ctx context.Context, now time.Time) error {
	if !api.pureGo || api.jobs == nil {
		return organization.ErrState
	}
	control, err := api.jobs.DownloadMonitorControl(ctx)
	if err != nil || control.State != "enabled" {
		return err
	}
	guard, err := api.jobs.AcquireJob(ctx, "native-download-monitor")
	if err != nil {
		return err
	}
	defer guard.Close()
	control, err = api.jobs.DownloadMonitorControl(ctx)
	if err != nil || control.State != "enabled" {
		return err
	}
	lease := control.Lease
	ctx = organization.AutomaticRemoteContext(ctx, "download-monitor", lease.Generation)
	worker := api
	worker.downloadGeneration = lease.Generation
	if lease.Remote != nil {
		worker.remoteApproval = lease.Remote.Approval(lease.Actor, "download-monitor", lease.Generation)
	}
	if err := worker.downloadMonitorAuthorized(ctx); err != nil {
		return api.jobs.PauseDownloadMonitorGeneration(ctx, lease.Generation, "authorization, configuration or physical roots changed; explicit reactivation required")
	}
	scope, failure := api.downloadMonitorScope(ctx, lease.IDs, true)
	if failure != nil {
		return api.jobs.PauseDownloadMonitorGeneration(ctx, lease.Generation, failure.message)
	}
	if err := api.jobs.ObserveDownloadCandidates(ctx, lease, scope.Candidates, now); err != nil {
		return err
	}
	events, err := api.jobs.DownloadMonitorEvents(ctx, lease.Generation, true)
	if err != nil {
		return err
	}
	finish := func(e organization.DownloadMonitorEvent, state, batchID, reason string) error {
		persist, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return api.jobs.SetDownloadMonitorEvent(persist, e, state, e.Snapshot, e.FirstSeen, batchID, reason, now)
	}
	checked := 0
	for _, event := range events {
		if event.State == "pausing" || event.State == "reserving" || event.State == "running" {
			b, err := api.jobs.FindDownloadBatch(ctx, event.DownloaderID, event.TaskID)
			id := ""
			if err == nil {
				id = b.ID
			} else if !errors.Is(err, organization.ErrNotFound) {
				return err
			}
			if err := finish(event, "needs_review", id, "previous pause/reservation/execution interrupted; inspect receipts, never automatically repeat"); err != nil {
				return err
			}
			continue
		}
		if checked >= 20 {
			break
		}
		checked++
		present := false
		for _, id := range scope.Candidates[event.DownloaderID] {
			present = present || id == event.TaskID
		}
		if !present {
			if err := finish(event, "filtered", event.BatchID, "task is not currently in the positively enumerated complete-candidate set"); err != nil {
				return err
			}
			continue
		}
		blocked, err := api.jobs.DownloadMonitorScopeBlocked(ctx, event)
		if err != nil {
			return err
		}
		if blocked {
			if err := finish(event, "needs_review", "", "an older generation of this task still requires review"); err != nil {
				return err
			}
			continue
		}
		b, err := api.jobs.FindDownloadBatch(ctx, event.DownloaderID, event.TaskID)
		if err == nil {
			if err := finish(event, "linked", b.ID, "existing immutable batch retained; its old receipt is not proof that current files exist"); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, organization.ErrNotFound) {
			return err
		}
		id, _ := syncConfigID(event.DownloaderID)
		item, err := api.service.downloaders.Get(ctx, id)
		if err != nil {
			return err
		}
		if organization.Digest(item) != lease.Downloaders[event.DownloaderID] {
			return organization.ErrState
		}
		c, err := api.clientFromDownloader(item)
		if err != nil {
			return err
		}
		task, err := c.read(ctx, event.TaskID)
		if err != nil {
			if err := finish(event, "needs_review", "", "task evidence unavailable; no failed request is treated as completion"); err != nil {
				return err
			}
			continue
		}
		owned := true
		if item.OnlyNastool == 1 {
			if item.Type == "aria2" {
				owned, err = api.service.downloaders.HasTrackedTask(ctx, event.DownloaderID, event.TaskID)
				if err != nil {
					return err
				}
			} else {
				owned = task.HasLabel("NASTOOL")
			}
		}
		if !task.Complete || task.HasLabel("已整理") || !owned || item.Type != "aria2" && !task.LabelsKnown {
			if err := finish(event, "filtered", "", "task incomplete, already organized, unowned or label state unknown; no transfer recorded"); err != nil {
				return err
			}
			continue
		}
		completion, failure := api.reviewDownloadCompletion(ctx, downloadCompletionInput{DownloaderID: event.DownloaderID, TaskID: event.TaskID}, "monitor:"+lease.Actor)
		if failure != nil {
			if err := finish(event, "needs_review", "", failure.message); err != nil {
				return err
			}
			continue
		}
		if completion.configDigest != lease.Downloaders[event.DownloaderID] {
			return organization.ErrState
		}
		observation := downloadObservation(completion)
		raw, _ := json.Marshal(observation)
		snapshot := string(raw)
		if snapshot != event.Snapshot || event.FirstSeen == "" {
			if err := api.jobs.SetDownloadMonitorEvent(ctx, event, "observed", snapshot, now.UTC().Format(time.RFC3339Nano), "", "waiting for a second stable snapshot", now); err != nil {
				return err
			}
			continue
		}
		first, err := time.Parse(time.RFC3339Nano, event.FirstSeen)
		if err != nil {
			return err
		}
		if now.Sub(first) < 30*time.Second || !completion.Eligible {
			if err := finish(event, "observed", "", "waiting for a stable completed local snapshot"); err != nil {
				return err
			}
			continue
		}
		taskGuard, err := api.jobs.AcquireJob(ctx, "download-completion:"+organization.DownloadTaskKey(event.DownloaderID, event.TaskID))
		if err != nil {
			return err
		}
		err = worker.runAutomaticDownload(ctx, lease, event, completion, c, now)
		taskGuard.Close()
		if err != nil {
			current, readErr := api.jobs.GetDownloadMonitorEvent(context.WithoutCancel(ctx), event.Key)
			if readErr == nil {
				persist, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if b, err := api.jobs.FindDownloadBatch(persist, event.DownloaderID, event.TaskID); err == nil {
					current.BatchID = b.ID
				}
				cancel()
				_ = finish(current, "needs_review", current.BatchID, "automatic step interrupted; review pause, file and result receipts without replay")
			}
			return err
		}
		return nil
	}
	return nil
}
func (api organizationAPI) runAutomaticDownload(ctx context.Context, lease organization.DownloadMonitorLease, event organization.DownloadMonitorEvent, completion downloadCompletionReview, c completionOrganizationClient, now time.Time) error {
	transition := func(state, batchID, reason string) error {
		err := api.jobs.SetDownloadMonitorEvent(ctx, event, state, event.Snapshot, event.FirstSeen, batchID, reason, now)
		if err == nil {
			event.State, event.BatchID, event.Reason = state, batchID, reason
		}
		return err
	}
	if err := api.downloadMonitorAuthorized(ctx); err != nil {
		return err
	}
	if _, err := api.jobs.FindDownloadBatch(ctx, event.DownloaderID, event.TaskID); !errors.Is(err, organization.ErrNotFound) {
		if err == nil {
			return organization.ErrState
		}
		return err
	}
	if organization.MoveMode(completion.Mode) && !downloadTaskStopped(completion.DownloaderType, completion.Task.State) {
		if !lease.SourceRemoval || !lease.PauseTasks || c.stop == nil {
			return organization.ErrState
		}
		if err := transition("pausing", "", "pause intent persisted before one exact-task pause request"); err != nil {
			return err
		}
		if err := api.downloadMonitorAuthorized(ctx); err != nil {
			return err
		}
		if err := c.stop(ctx, event.TaskID); err != nil {
			return err
		}
		var stopped bool
		for i := 0; i < 5; i++ {
			task, err := c.read(ctx, event.TaskID)
			if err != nil {
				return err
			}
			if !organization.DownloadTaskMatches(completion.Task, task, false) {
				return organization.ErrState
			}
			if downloadTaskStopped(completion.DownloaderType, task.State) {
				stopped = true
				break
			}
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if !stopped {
			return organization.ErrState
		}
	}
	review, failure := api.planDownload(ctx, downloadCompletionInput{DownloaderID: event.DownloaderID, TaskID: event.TaskID}, "monitor:"+lease.Actor)
	if failure != nil {
		return organization.ErrState
	}
	if organization.Digest(downloadObservation(review.Completion)) != organization.Digest(downloadObservation(completion)) {
		return organization.ErrState
	}
	fresh, failure := api.planDownload(ctx, downloadCompletionInput{DownloaderID: event.DownloaderID, TaskID: event.TaskID}, "monitor:"+lease.Actor)
	if failure != nil || fresh.Fingerprint != review.Fingerprint {
		return organization.ErrState
	}
	if len(review.Omitted) > 0 && !lease.AllowOmitted {
		return organization.ErrState
	}
	if err := api.downloadMonitorAuthorized(ctx); err != nil {
		return err
	}
	if err := transition("reserving", "", "reserving exact reviewed batch; no automatic retry after interruption"); err != nil {
		return err
	}
	review.origin.MonitorGeneration, review.origin.MonitorEvent = lease.Generation, event.Key
	defs := []organization.Definition{}
	for _, p := range review.Plans {
		defs = append(defs, p.definition)
	}
	batch, defs, err := api.jobs.BeginDownloadBatch(ctx, organization.Digest([]string{review.Fingerprint, lease.Generation, event.Key}), review.origin, defs, review.Omitted)
	if err != nil {
		return err
	}
	if err := transition("reserving", batch.ID, ""); err != nil {
		return err
	}
	for _, d := range defs {
		if _, err := api.jobs.Create(ctx, organization.Digest(d), d); err != nil {
			api.failDownloadBatch(batch, "automatic reservation interrupted; inspect all linked jobs")
			return err
		}
	}
	if err := api.jobs.SetDownloadBatchAuthorized(ctx, batch.ID, "reserving", "ready", "", "", lease.Generation); err != nil {
		return err
	}
	batch, err = api.jobs.GetDownloadBatch(ctx, batch.ID)
	if err != nil {
		return err
	}
	if err := transition("running", batch.ID, ""); err != nil {
		return err
	}
	if err := api.executeDownloadBatch(ctx, batch, downloadBatchRequest{Confirm: true, ConfirmSourceRemoval: lease.SourceRemoval, ConfirmTaskStopped: lease.PauseTasks || batch.Origin.DownloaderType == "aria2"}); err != nil {
		return err
	}
	batch, err = api.jobs.GetDownloadBatch(ctx, batch.ID)
	if err != nil {
		return err
	}
	result, err := api.reviewDownloadResult(ctx, batch, "monitor:"+lease.Actor)
	if err != nil {
		return err
	}
	if !lease.ResultWrites {
		return organization.ErrState
	}
	if err := api.finalizeDownloadBatch(ctx, batch, downloadBatchRequest{actor: "monitor:" + lease.Actor, Confirm: true, Fingerprint: result.Fingerprint, ConfirmTaskRemoval: lease.TaskRemoval, ConfirmTaskStopped: lease.PauseTasks || batch.Origin.DownloaderType == "aria2", ConfirmExclusiveLabels: lease.ExclusiveLabels}); err != nil {
		return err
	}
	return transition("completed", batch.ID, "")
}
func startDownloadMonitorWorker(ctx context.Context, api *organizationAPI) func() {
	if api == nil || !api.pureGo || api.jobs == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				run, cancel := context.WithTimeout(ctx, 6*time.Hour)
				err := api.downloadMonitorTick(run, now)
				cancel()
				if err != nil && !errors.Is(err, organization.ErrBusy) && !errors.Is(err, context.Canceled) {
					slog.Warn("download completion monitor requires review", "error", err)
				}
			}
		}
	}()
	return func() { <-done }
}
