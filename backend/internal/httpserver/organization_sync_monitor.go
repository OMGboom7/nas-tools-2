package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type monitorInput struct {
	ConfirmRemoteExclusive     bool     `json:"confirmRemoteExclusive,omitempty"`
	ConfirmRemoteRecoveryCopy  bool     `json:"confirmRemoteRecoveryCopy,omitempty"`
	IDs                        []string `json:"ids"`
	IncludeExisting            bool     `json:"includeExisting"`
	Fingerprint                string   `json:"fingerprint,omitempty"`
	Confirm                    bool     `json:"confirm,omitempty"`
	ConfirmOldExecutorsStopped bool     `json:"confirmOldExecutorsStopped,omitempty"`
	ConfirmSourceRemoval       bool     `json:"confirmSourceRemoval,omitempty"`
}
type monitorReview struct {
	RemoteTargets      []remoteMonitorTarget          `json:"remoteTargets,omitempty"`
	Lease              organization.MonitorLease      `json:"lease"`
	Snapshots          []organization.MonitorSnapshot `json:"snapshots"`
	Records            []organization.SyncRecord      `json:"records"`
	Fingerprint        string                         `json:"fingerprint"`
	NeedsSourceRemoval bool                           `json:"needsSourceRemoval"`
}

func (api organizationAPI) collectMonitor(ctx context.Context, ids []string) ([]organization.SyncRecord, []organization.MonitorSnapshot, []string, *recognitionFailure) {
	fail := func(status int, message string) ([]organization.SyncRecord, []organization.MonitorSnapshot, []string, *recognitionFailure) {
		return nil, nil, nil, &recognitionFailure{status, message}
	}
	if len(ids) == 0 || len(ids) > 16 {
		return fail(400, "select 1 to 16 exact enabled sync configuration IDs")
	}
	records, err := api.jobs.ListSync(ctx)
	if err != nil {
		return fail(503, "sync configurations unavailable")
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return fail(503, "configuration unavailable")
	}
	media := objectValue(snapshot["media"])
	roots, err := api.roots(ctx)
	if err != nil {
		return fail(503, "configured roots unavailable")
	}
	proofs := []string{}
	seenProof := map[string]bool{}
	snapshots := []organization.MonitorSnapshot{}
	seenIDs := map[int64]bool{}
	total := 0
	for _, id := range ids {
		n, err := syncConfigID(id)
		if err != nil || seenIDs[n] {
			return fail(400, "invalid or duplicate sync configuration ID")
		}
		seenIDs[n] = true
		var record *organization.SyncRecord
		for i := range records {
			if records[i].ID == n {
				record = &records[i]
				break
			}
		}
		if record == nil || record.Enabled != 1 {
			return fail(422, "selected sync record is missing or disabled")
		}
		mode, known := syncConfigMode(record.Mode)
		if !known || !organization.SupportedMode(mode) || organization.RemoteMode(mode) && !api.remoteFactory(mode).Available() {
			return fail(501, "selected remote/unknown sync mode is not migrated; no local fallback")
		}
		if record.Rename != 0 && record.Rename != 1 {
			return fail(422, "invalid recognition switch")
		}
		if record.Rename == 0 && record.Dest == "" {
			return fail(422, "direct sync requires a destination")
		}
		if validateDirectSyncGraph(*record, records, media) != nil {
			return fail(422, "unsafe sync source/destination graph")
		}
		if _, err := compileDirectSyncRules(media); err != nil {
			return fail(422, "invalid sync ignore rules")
		}
		source, err := filepath.EvalSymlinks(record.Source)
		if err != nil {
			return fail(422, "monitor source must exist")
		}
		info, err := os.Stat(source)
		if err != nil || !info.IsDir() {
			return fail(422, "monitor source must be a directory")
		}
		proof := "source:" + id + ":" + source + ":" + organization.Identity(info)
		proofs = append(proofs, proof)
		usable := 0
		for _, root := range roots.Targets {
			matches := historyPathMatches(root.Path, ".", record.Dest)
			if organization.RemoteMode(mode) {
				matches = filepath.Clean(root.Path) == filepath.Clean(record.Dest)
			}
			if record.Dest != "" && !matches || record.Dest == "" && root.Type == "" {
				continue
			}
			if organization.RemoteMode(mode) {
				remoteProof, _, _, err := api.monitorRemoteRoot(ctx, root, nil, mode)
				if err != nil {
					return fail(422, "remote provider profile or destination unavailable; no automatic activation")
				}
				usable++
				if !seenProof[remoteProof] {
					seenProof[remoteProof] = true
					proofs = append(proofs, remoteProof)
				}
				continue
			}
			path, err := filepath.EvalSymlinks(root.Path)
			if err != nil {
				proof = "missing:" + root.ID + ":" + root.Path
			} else {
				info, err := os.Stat(path)
				if err != nil || !info.IsDir() {
					return fail(422, "monitor destination is not a directory")
				}
				usable++
				proof = "target:" + root.ID + ":" + path + ":" + organization.Identity(info)
			}
			if !seenProof[proof] {
				seenProof[proof] = true
				proofs = append(proofs, proof)
			}
		}
		if usable == 0 {
			return fail(422, "no existing configured destination is available; activation creates no directories")
		}
		scanFunc := organization.ScanAll
		if record.Rename == 1 {
			scanFunc = organization.Scan
		}
		scan, err := scanFunc(ctx, source, ".")
		if err != nil {
			return fail(422, "monitor scan is unsafe or exceeds bounds; narrow the configured source")
		}
		total += len(scan.Files)
		if total > 10000 {
			return fail(422, "monitor activation exceeds 10000 files")
		}
		groups := map[string][]organization.File{}
		for _, file := range scan.Files {
			if strings.Contains(strings.ToLower(file.Path), ".!qb") {
				continue
			}
			scope := file.Path
			if record.Rename == 1 {
				if file.DiscInfo == nil {
					file.Kind = organization.FileKind(file.Path)
				}
				if file.Kind == "" {
					continue
				}
				scope = filepath.Dir(file.Path)
				if file.DiscInfo != nil {
					scope = file.Path
				}
			}
			groups[scope] = append(groups[scope], file)
		}
		for scope, files := range groups {
			snapshots = append(snapshots, organization.MonitorSnapshot{ConfigID: id, Scope: scope, Files: files})
		}
	}
	if len(snapshots) > 2048 {
		return fail(422, "monitor scope count exceeds bounds")
	}
	sort.Strings(proofs)
	sort.Slice(snapshots, func(i, j int) bool {
		a, b := snapshots[i], snapshots[j]
		if a.ConfigID == b.ConfigID {
			return a.Scope < b.Scope
		}
		return a.ConfigID < b.ConfigID
	})
	return records, snapshots, proofs, nil
}
func (api organizationAPI) monitorReview(ctx context.Context, input monitorInput, actor string) (monitorReview, *recognitionFailure) {
	ids := append([]string{}, input.IDs...)
	sort.Strings(ids)
	records, snapshots, roots, failure := api.collectMonitor(ctx, ids)
	if failure != nil {
		return monitorReview{}, failure
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return monitorReview{}, &recognitionFailure{503, "configuration unavailable"}
	}
	review := monitorReview{Records: records, Snapshots: snapshots, Lease: organization.MonitorLease{Actor: actor, IDs: ids, ConfigDigest: organization.Digest(snapshot), GraphDigest: organization.SyncGraphDigest(records), Roots: roots, IncludeExisting: input.IncludeExisting}}
	review.Lease.Remote = remoteAutomationFromRoots(roots)
	configured, err := api.roots(ctx)
	if err != nil {
		return monitorReview{}, &recognitionFailure{503, "configured roots unavailable"}
	}
	review.RemoteTargets = remoteTargets(configured, review.Lease.Remote)
	for _, r := range records {
		for _, id := range ids {
			if strconv.FormatInt(r.ID, 10) == id {
				mode, _ := syncConfigMode(r.Mode)
				review.NeedsSourceRemoval = review.NeedsSourceRemoval || organization.MoveMode(mode)
			}
		}
	}
	review.Fingerprint = organization.Digest(review)
	return review, nil
}
func (api organizationAPI) serveMonitorReview(w http.ResponseWriter, r *http.Request) {
	api.serveMonitorEnable(w, r, false)
}
func (api organizationAPI) serveMonitorActivate(w http.ResponseWriter, r *http.Request) {
	api.serveMonitorEnable(w, r, true)
}
func (api organizationAPI) serveMonitorEnable(w http.ResponseWriter, r *http.Request, activate bool) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input monitorInput
	if !decodeOrganizationRequest(w, r, &input, "invalid monitor activation request") {
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	review, failure := api.monitorReview(ctx, input, claims.Username)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	if !activate {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
		return
	}
	if !input.Confirm || !input.ConfirmOldExecutorsStopped || input.Fingerprint != review.Fingerprint || review.NeedsSourceRemoval && !input.ConfirmSourceRemoval {
		writeAPIError(w, 409, 409, "fresh scope review, actual old-executor-stop confirmation and separate move consent are required")
		return
	}
	review.Lease.SourceRemoval = input.ConfirmSourceRemoval
	if err := approveRemoteAutomation(review.Lease.Remote, input.ConfirmRemoteExclusive, input.ConfirmRemoteRecoveryCopy); err != nil {
		writeAPIError(w, 400, 400, "remote automatic execution requires separate exclusive namespace/non-atomic visibility and retained recovery-copy consent")
		return
	}
	control, err := api.jobs.EnableSyncMonitor(ctx, review.Lease, review.Snapshots, review.Records, time.Now())
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": control})
}
func (api organizationAPI) serveMonitorStatus(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	control, err := api.jobs.MonitorControl(r.Context())
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	events, err := api.jobs.MonitorEvents(r.Context(), control.Lease.Generation, false)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"control": control, "events": events}})
}
func (api organizationAPI) serveMonitorStop(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid stop request") {
		return
	}
	if !input.Confirm {
		writeAPIError(w, 400, 400, "explicit stop confirmation required")
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	if err := api.jobs.StopSyncMonitor(r.Context(), claims.Username); err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "stop requested; inspect active job receipts, no cleanup or retry"})
}
func (api organizationAPI) serveMonitorDismiss(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input struct {
		Key     string `json:"key"`
		Confirm bool   `json:"confirm"`
		Reason  string `json:"reason"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid event dismissal") {
		return
	}
	if !input.Confirm || len(input.Key) != 64 || strings.TrimSpace(input.Reason) == "" || len(input.Reason) > 512 {
		writeAPIError(w, 400, 400, "review one event and provide a dismissal reason")
		return
	}
	guard, err := api.jobs.AcquireJob(r.Context(), "native-sync-monitor")
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	event, err := api.jobs.GetMonitorEvent(r.Context(), input.Key)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	jobs, err := api.jobs.MonitorEventJobs(r.Context(), event.Key)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	ids := []string{}
	for _, job := range jobs {
		ids = append(ids, job.ID)
		if job.State != "completed" && job.State != "cancelled" && job.State != "abandoned" && job.State != organization.SourceRestored && job.State != "remote_closed" {
			writeAPIError(w, 409, 409, "saved jobs must be explicitly completed/cancelled/abandoned/source-restored before dismissing their event")
			return
		}
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	if err := api.jobs.SetMonitorEvent(r.Context(), event.Key, "dismissed", ids, claims.Username+": "+input.Reason); err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "event dismissed only; no file operation, retry or history success"})
}

func (api organizationAPI) recognizedSyncPlans(ctx context.Context, record organization.SyncRecord, scope, actor string) ([]organizationPlan, *recognitionFailure) {
	records, err := api.jobs.ListSync(ctx)
	if err != nil {
		return nil, &recognitionFailure{503, "sync configurations unavailable"}
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return nil, &recognitionFailure{503, "configuration unavailable"}
	}
	if validateDirectSyncGraph(record, records, objectValue(snapshot["media"])) != nil {
		return nil, &recognitionFailure{422, "unsafe sync graph"}
	}
	source, err := filepath.EvalSymlinks(record.Source)
	if err != nil {
		return nil, &recognitionFailure{422, "source unavailable"}
	}
	scan, err := organization.ScanDirectory(ctx, source, scope)
	if err != nil {
		return nil, &recognitionFailure{422, "recognition scope is unsafe or too large"}
	}
	root, err := organization.OpenRoot(source)
	if err != nil {
		return nil, &recognitionFailure{422, "source root unavailable"}
	}
	info, err := root.Lstat(scope)
	root.Close()
	if err != nil {
		return nil, &recognitionFailure{422, "source scope changed"}
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return nil, &recognitionFailure{503, "roots unavailable"}
	}
	mode, _ := syncConfigMode(record.Mode)
	input := organizationPlanInput{SourceID: subscriptionResourceKey("source:" + filepath.Clean(record.Source) + ":"), Path: scope, Mode: mode, LegacyTargetOverride: record.Dest != "", Sync: &organization.SyncOrigin{Record: record, Scope: scope, ScopeIdentity: organization.Identity(info), GraphDigest: organization.SyncGraphDigest(records), Actor: actor}}
	plans := []organizationPlan{}
	covered := map[string]bool{}
	filtered := map[string]bool{}
	for _, target := range roots.Targets {
		matches := historyPathMatches(target.Path, ".", record.Dest)
		if organization.RemoteMode(mode) {
			matches = filepath.Clean(target.Path) == filepath.Clean(record.Dest)
		}
		if record.Dest != "" && !matches || record.Dest == "" && target.Type == "" {
			continue
		}
		input.TargetID = target.ID
		plan, failure := api.plan(ctx, input)
		if failure != nil {
			if ctx.Err() != nil {
				return nil, failure
			}
			continue
		}
		selected := map[string]bool{}
		for _, item := range plan.Items {
			if item.Kind == "media" && item.Status == "filtered" {
				filtered[item.Source] = true
			}
			if item.Kind == "media" && item.Status == "available" && !covered[item.Source] {
				selected[item.Source] = true
			}
		}
		for _, item := range plan.Items {
			if item.Kind == "media" || item.Status == "available" || item.Status == "filtered" {
				continue
			}
			for media := range selected {
				stem := strings.TrimSuffix(filepath.Base(media), filepath.Ext(media))
				if filepath.Dir(media) == filepath.Dir(item.Source) && strings.HasPrefix(filepath.Base(item.Source), stem+".") {
					return nil, &recognitionFailure{422, "companion is conflicting or ambiguous; review the whole media group"}
				}
			}
		}
		entries := []organization.Entry{}
		for _, entry := range plan.definition.Entries {
			take := selected[entry.Source]
			if entry.Kind != "media" {
				for media := range selected {
					stem := strings.TrimSuffix(filepath.Base(media), filepath.Ext(media))
					if filepath.Dir(media) == filepath.Dir(entry.Source) && strings.HasPrefix(filepath.Base(entry.Source), stem+".") {
						take = true
					}
				}
			}
			if take {
				entries = append(entries, entry)
			}
		}
		if len(entries) == 0 {
			continue
		}
		plan.definition.Entries = entries
		for media := range selected {
			covered[media] = true
		}
		plan.Fingerprint = organization.Digest(struct {
			Definition organization.Definition
			Items      []organizationPlanItem
		}{plan.definition, plan.Items})
		plans = append(plans, plan)
	}
	for _, file := range scan.Files {
		if file.Kind == "media" && !covered[file.Path] && !filtered[file.Path] {
			return nil, &recognitionFailure{422, "media recognition/naming/conflict requires review; unknown/bluray/remote handling is not silently substituted"}
		}
	}
	return plans, nil
}
func (api organizationAPI) monitorPlans(ctx context.Context, event organization.MonitorEvent, lease organization.MonitorLease) ([]organizationPlan, *recognitionFailure) {
	id, err := syncConfigID(event.Snapshot.ConfigID)
	if err != nil {
		return nil, &recognitionFailure{422, "invalid saved sync ID"}
	}
	record, err := api.jobs.GetSync(ctx, id)
	if err != nil {
		return nil, &recognitionFailure{409, "sync record changed"}
	}
	if record.Rename == 0 {
		plan, failure := api.directSyncPlan(ctx, id, event.Snapshot.Scope, "monitor:"+lease.Actor)
		if failure != nil {
			return nil, failure
		}
		if len(plan.definition.Entries) == 0 {
			return nil, nil
		}
		return []organizationPlan{plan}, nil
	}
	return api.recognizedSyncPlans(ctx, record, event.Snapshot.Scope, "monitor:"+lease.Actor)
}

func (api organizationAPI) syncMonitorTick(ctx context.Context, now time.Time) error {
	if !api.pureGo || api.jobs == nil {
		return organization.ErrState
	}
	initial, err := api.jobs.MonitorControl(ctx)
	if err != nil || initial.State != "enabled" {
		return err
	}
	guard, err := api.jobs.AcquireJob(ctx, "native-sync-monitor")
	if err != nil {
		return err
	}
	defer guard.Close()
	control, err := api.jobs.MonitorControl(ctx)
	if err != nil || control.State != "enabled" {
		return err
	}
	lease := control.Lease
	ctx = organization.AutomaticRemoteContext(ctx, "sync-monitor", lease.Generation)
	records, snapshots, roots, failure := api.collectMonitor(ctx, lease.IDs)
	if failure != nil {
		return api.jobs.PauseSyncMonitor(ctx, failure.message)
	}
	config, err := api.service.configStore.Snapshot()
	if err != nil {
		return err
	}
	if organization.SyncGraphDigest(records) != lease.GraphDigest || organization.Digest(config) != lease.ConfigDigest || organization.Digest(roots) != organization.Digest(lease.Roots) {
		return api.jobs.PauseSyncMonitor(ctx, "configuration or physical roots changed; explicit reactivation required")
	}
	if remoteAutomationFromRoots(roots) != nil && !lease.Remote.Valid() {
		return api.jobs.PauseSyncMonitor(ctx, "remote execution has no current independent grant; explicit new review required")
	}
	if err := api.jobs.ObserveSyncMonitor(ctx, lease, snapshots, now); err != nil {
		return err
	}
	events, err := api.jobs.MonitorEvents(ctx, lease.Generation, true)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.State == "reserving" || event.State == "reserved" {
			jobs, err := api.jobs.MonitorEventJobs(ctx, event.Key)
			if err != nil {
				return err
			}
			ids := []string{}
			for _, job := range jobs {
				ids = append(ids, job.ID)
			}
			if err := api.jobs.SetMonitorEvent(ctx, event.Key, "needs_review", ids, "previous reservation/response interrupted; inspect saved jobs, never automatically restart"); err != nil {
				return err
			}
			continue
		}
		first, err := time.Parse(time.RFC3339Nano, event.FirstSeen)
		if err != nil {
			return organization.ErrState
		}
		if now.Sub(first) < 30*time.Second {
			continue
		}
		quiet := true
		for _, file := range event.Snapshot.Files {
			quiet = quiet && now.Sub(time.Unix(0, file.Modified)) >= 30*time.Second
			if file.DiscInfo != nil {
				quiet = quiet && now.Sub(time.Unix(0, file.DiscInfo.LatestModified)) >= 30*time.Second
			}
		}
		if !quiet {
			continue
		}
		blocked, err := api.jobs.MonitorScopeBlocked(ctx, event)
		if err != nil {
			return err
		}
		if blocked {
			if err := api.jobs.SetMonitorEvent(ctx, event.Key, "needs_review", nil, "an older event in this scope still requires review"); err != nil {
				return err
			}
			continue
		}
		plans, failure := api.monitorPlans(ctx, event, lease)
		if failure != nil {
			return api.jobs.SetMonitorEvent(ctx, event.Key, "needs_review", nil, failure.message)
		}
		fresh, failure := api.monitorPlans(ctx, event, lease)
		if failure != nil {
			return api.jobs.SetMonitorEvent(ctx, event.Key, "needs_review", nil, failure.message)
		}
		if len(fresh) != len(plans) {
			return api.jobs.SetMonitorEvent(ctx, event.Key, "needs_review", nil, "planning scope changed")
		}
		for i := range plans {
			if fresh[i].Fingerprint != plans[i].Fingerprint {
				return api.jobs.SetMonitorEvent(ctx, event.Key, "needs_review", nil, "source/metadata plan changed")
			}
		}
		observed := map[string]organization.File{}
		for _, file := range event.Snapshot.Files {
			observed[file.Path] = file
		}
		for _, plan := range fresh {
			for _, entry := range plan.definition.Entries {
				file, ok := observed[entry.Source]
				if !ok || file.Identity != entry.Identity || file.Size != entry.Size || strconv.FormatInt(file.Modified, 10) != entry.Modified || organization.Digest(file.DiscInfo) != organization.Digest(entry.Disc) {
					return api.jobs.SetMonitorEvent(ctx, event.Key, "needs_review", nil, "source is not the exact twice-observed snapshot")
				}
			}
		}
		if len(plans) == 0 {
			return api.jobs.SetMonitorEvent(ctx, event.Key, "filtered", nil, "no verified executable primary media, or history/ignore rules suppress transfer; not a new successful transfer")
		}
		if err := api.jobs.ReserveMonitorEvent(ctx, event); err != nil {
			return err
		}
		ids := []string{}
		for _, plan := range fresh {
			plan.definition.Sync.MonitorGeneration, plan.definition.Sync.MonitorEvent = lease.Generation, event.Key
			fingerprint := organization.Digest(struct {
				Original   string
				Definition organization.Definition
			}{plan.Fingerprint, plan.definition})
			id, err := api.jobs.Create(ctx, fingerprint, plan.definition)
			if err != nil {
				_ = api.jobs.SetMonitorEvent(context.WithoutCancel(ctx), event.Key, "needs_review", ids, "reservation interrupted; inspect all saved jobs")
				return err
			}
			ids = append(ids, id)
		}
		if err := api.jobs.SetMonitorEvent(ctx, event.Key, "reserved", ids, ""); err != nil {
			return err
		}
		worker := api
		worker.monitorGeneration = lease.Generation
		if lease.Remote != nil {
			worker.remoteApproval = lease.Remote.Approval(lease.Actor, "sync-monitor", lease.Generation)
		}
		for _, id := range ids {
			job, err := api.jobs.Get(ctx, id)
			if err != nil {
				return err
			}
			if organization.MoveMode(job.Mode) && !lease.SourceRemoval {
				return organization.ErrState
			}
			jobGuard, err := api.jobs.AcquireJob(ctx, id)
			if err != nil {
				return err
			}
			job.AutomaticGeneration = lease.Generation
			err = worker.validateJob(ctx, job)
			if err == nil {
				err = worker.runJob(ctx, job, false)
			}
			jobGuard.Close()
			current, readErr := api.jobs.Get(context.WithoutCancel(ctx), id)
			if err != nil || readErr != nil || current.State != "completed" {
				_ = api.jobs.SetMonitorEvent(context.WithoutCancel(ctx), event.Key, "needs_review", ids, "execution or receipt interrupted; explicit job review required")
				if err != nil {
					return err
				}
				return readErr
			}
		}
		return api.jobs.SetMonitorEvent(ctx, event.Key, "completed", ids, "")
	}
	return nil
}

func startSyncMonitorWorker(ctx context.Context, api *organizationAPI) func() {
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
				run, cancel := context.WithTimeout(ctx, organization.ExecutionTimeout)
				err := api.syncMonitorTick(run, now)
				cancel()
				if err != nil && !errors.Is(err, organization.ErrBusy) && !errors.Is(err, context.Canceled) {
					slog.Warn("native sync monitor requires review", "error", fmt.Sprint(err))
				}
			}
		}
	}()
	return func() { <-done }
}
