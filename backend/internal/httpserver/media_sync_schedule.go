package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediasync"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type mediaScheduleInput struct {
	Libraries         []string `json:"libraries"`
	Hours             float64  `json:"hours"`
	ConfigFingerprint string   `json:"configFingerprint"`
	Fingerprint       string   `json:"fingerprint"`
	Confirm           bool     `json:"confirm"`
	LegacyStopped     bool     `json:"legacyStopped"`
}
type mediaScheduleReview struct {
	Definition      mediasync.ScheduleDefinition `json:"definition"`
	Revision        string                       `json:"revision"`
	ReplacesEnabled bool                         `json:"replacesEnabled"`
	Fingerprint     string                       `json:"fingerprint"`
}

func mediaScheduleSeconds(hours float64) (int64, error) {
	if math.IsNaN(hours) || math.IsInf(hours, 0) || hours < 1.0/60 || hours > 8760 {
		return 0, mediasync.ErrSchedule
	}
	return int64(math.Round(hours * 3600)), nil
}

// Legacy strings were rounded with Python round(), but numeric YAML values
// were passed directly to APScheduler. This is a suggestion, never consent.
func legacyMediaScheduleHours(value any) (float64, error) {
	if value == nil || strings.TrimSpace(text(value)) == "" {
		return 0, nil
	}
	hours, err := strconv.ParseFloat(strings.TrimSpace(text(value)), 64)
	if err != nil || math.IsNaN(hours) || math.IsInf(hours, 0) || hours < 0 || hours > 8760 {
		return 0, mediasync.ErrSchedule
	}
	if _, ok := value.(string); ok {
		hours = math.RoundToEven(hours)
	}
	if hours == 0 {
		return 0, nil
	}
	_, err = mediaScheduleSeconds(hours)
	return hours, err
}
func (api mediaSyncAPI) scheduleAuthority(actor string) (string, error) {
	snapshot, err := api.config.Snapshot()
	if err != nil {
		return "", err
	}
	app := objectValue(snapshot["app"])
	if actor == "" || text(app["login_user"]) != actor || !api.auth.service.IsAdministrator(actor) {
		return "", mediasync.ErrSchedule
	}
	raw, err := json.Marshal(map[string]any{"actor": actor, "password": app["login_password"], "key": objectValue(snapshot["security"])["api_key"], "legacyInterval": objectValue(snapshot["media"])["mediasync_interval"]})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
func (api mediaSyncAPI) scheduleStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	schedule, err := api.store.Schedule(r.Context())
	if err != nil {
		writeAPIError(w, 503, 503, "media schedule unavailable")
		return
	}
	runs, err := api.store.Runs(r.Context())
	if err != nil {
		writeAPIError(w, 503, 503, "media sync run receipts unavailable")
		return
	}
	_, kind, _, digest, connectionErr := api.connection()
	authority, authorityErr := api.scheduleAuthority(schedule.Definition.Actor)
	current := connectionErr == nil && authorityErr == nil && kind == schedule.Definition.Server && digest == schedule.Definition.ConfigDigest && authority == schedule.Definition.AuthorityDigest
	var suggested float64
	warning := ""
	if snapshot, err := api.config.Snapshot(); err == nil {
		suggested, err = legacyMediaScheduleHours(objectValue(snapshot["media"])["mediasync_interval"])
		if err != nil {
			warning = "legacy interval invalid or outside supported bounds; choose an explicit period"
		}
	} else {
		warning = "configuration unavailable"
	}
	running := api.workerRunning != nil && api.workerRunning.Load()
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"schedule": schedule, "runs": runs, "current": current, "workerRunning": running, "suggestedHours": suggested, "warning": warning}})
}
func (api mediaSyncAPI) reviewSchedule(ctx context.Context, actor string, input mediaScheduleInput) (mediaScheduleReview, error) {
	review := mediaScheduleReview{}
	seconds, err := mediaScheduleSeconds(input.Hours)
	if err != nil {
		return review, err
	}
	if len(input.Libraries) == 0 || len(input.Libraries) > 100 {
		return review, mediasync.ErrSchedule
	}
	selected := append([]string{}, input.Libraries...)
	sort.Strings(selected)
	for i, id := range selected {
		if id == "" || i > 0 && id == selected[i-1] {
			return review, mediasync.ErrSchedule
		}
	}
	client, kind, username, digest, err := api.connection()
	if err != nil {
		return review, err
	}
	if input.ConfigFingerprint != digest {
		return review, mediasync.ErrSchedule
	}
	authority, err := api.scheduleAuthority(actor)
	if err != nil {
		return review, err
	}
	libraries, err := client.SyncLibraries(ctx, username)
	if err != nil {
		return review, err
	}
	available := map[string]bool{}
	for _, library := range libraries {
		available[library.ID] = true
	}
	for _, id := range selected {
		if !available[id] {
			return review, mediasync.ErrSchedule
		}
	}
	_, currentKind, _, currentDigest, err := api.connection()
	if err != nil || currentKind != kind || currentDigest != digest {
		return review, mediasync.ErrSchedule
	}
	currentAuthority, err := api.scheduleAuthority(actor)
	if err != nil || currentAuthority != authority {
		return review, mediasync.ErrSchedule
	}
	schedule, err := api.store.Schedule(ctx)
	if err != nil {
		return review, err
	}
	review.Definition = mediasync.ScheduleDefinition{Server: kind, ConfigDigest: digest, AuthorityDigest: authority, Actor: actor, Libraries: selected, IntervalSeconds: seconds}
	review.Revision = schedule.Revision
	review.ReplacesEnabled = schedule.Enabled
	raw, err := json.Marshal(review)
	if err != nil {
		return review, err
	}
	hash := sha256.Sum256(raw)
	review.Fingerprint = hex.EncodeToString(hash[:])
	return review, nil
}
func (api mediaSyncAPI) scheduleReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	var input mediaScheduleInput
	if !decodeOrganizationRequest(w, r, &input, "invalid schedule review") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	review, err := api.reviewSchedule(ctx, actor, input)
	if err != nil {
		writeAPIError(w, 409, 409, "schedule scope unavailable or changed; choose libraries and a period from one minute to one year, then reload review")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
}
func (api mediaSyncAPI) scheduleActivate(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	var input mediaScheduleInput
	if !decodeOrganizationRequest(w, r, &input, "invalid schedule activation") {
		return
	}
	if !input.Confirm || !input.LegacyStopped {
		writeAPIError(w, 400, 400, "confirm recurring snapshot replacement and stopped legacy worker")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	review, err := api.reviewSchedule(ctx, actor, input)
	if err != nil || review.Fingerprint != input.Fingerprint {
		writeAPIError(w, 409, 409, "schedule review changed; no recurring execution authorized")
		return
	}
	// No remote work starts here. ConfigureSchedule also revokes any old run
	// atomically, so an in-flight collector cannot publish after replacement.
	schedule, err := api.store.ConfigureSchedule(ctx, review.Revision, review.Definition, time.Now())
	if err != nil {
		writeAPIError(w, 409, 409, "schedule changed; read status before reviewing again")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": schedule})
}
func (api mediaSyncAPI) scheduleStop(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	var input struct {
		Confirm  bool   `json:"confirm"`
		Revision string `json:"revision"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid schedule stop") {
		return
	}
	if !input.Confirm {
		writeAPIError(w, 400, 400, "confirm stopping the exact schedule revision")
		return
	}
	schedule, err := api.store.DisableSchedule(r.Context(), input.Revision, actor, "stopped", "explicitly stopped; existing display snapshot retained", time.Now())
	if err != nil {
		writeAPIError(w, 409, 409, "schedule changed; read status before stopping again")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": schedule})
}

func (api *mediaSyncAPI) scheduleTick(parent context.Context, now time.Time) error {
	if api == nil || !api.pureGo || api.store == nil || api.guards == nil || api.auth == nil || api.config == nil {
		return nil
	}
	guard, err := api.guards.AcquireJob(parent, "media-library-sync")
	if err != nil {
		return err
	}
	defer guard.Close()
	if err := api.store.RecoverInterrupted(parent, now); err != nil {
		return err
	}
	schedule, err := api.store.Schedule(parent)
	if err != nil || !schedule.Enabled {
		return err
	}
	client, kind, username, digest, err := api.connection()
	authority, authErr := api.scheduleAuthority(schedule.Definition.Actor)
	if err != nil || authErr != nil || kind != schedule.Definition.Server || digest != schedule.Definition.ConfigDigest || authority != schedule.Definition.AuthorityDigest {
		_, err := api.store.DisableSchedule(parent, schedule.Revision, "system", "paused", "configuration or administrator authority changed; explicit new review required", now)
		return err
	}
	run, claimed, err := api.store.ClaimSchedule(parent, schedule.Revision, now)
	if err != nil || !claimed {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	items, err := client.CollectSnapshot(ctx, username, run.Definition.Libraries)
	if err != nil {
		state, reason := "failed", "inventory incomplete; prior snapshot retained; next period starts a new complete read"
		if ctx.Err() != nil {
			state, reason = "interrupted", "collector cancelled; prior snapshot retained"
		}
		persist, done := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer done()
		return api.store.FailScheduledRun(persist, run, state, reason, time.Now())
	}
	_, currentKind, _, currentDigest, err := api.connection()
	currentAuthority, authErr := api.scheduleAuthority(run.Definition.Actor)
	if err != nil || authErr != nil || currentKind != run.Definition.Server || currentDigest != run.Definition.ConfigDigest || currentAuthority != run.Definition.AuthorityDigest {
		_, err := api.store.DisableSchedule(ctx, run.Revision, "system", "paused", "configuration or administrator authority changed during collection; prior snapshot retained", time.Now())
		return err
	}
	_, err = api.store.PublishScheduled(ctx, run, items, time.Now())
	return err
}
func startMediaSyncWorker(ctx context.Context, api *mediaSyncAPI) func() {
	if api == nil || !api.pureGo || api.store == nil || api.guards == nil {
		return func() {}
	}
	done := make(chan struct{})
	if api.workerRunning != nil {
		api.workerRunning.Store(true)
	}
	go func() {
		defer close(done)
		if api.workerRunning != nil {
			defer api.workerRunning.Store(false)
		}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		runMediaSyncWorker(ctx, api, time.Now(), ticker.C)
	}()
	return func() { <-done }
}
func runMediaSyncWorker(ctx context.Context, api *mediaSyncAPI, initial time.Time, ticks <-chan time.Time) {
	poll := func(now time.Time) {
		if ctx.Err() != nil {
			return
		}
		if err := api.scheduleTick(ctx, now); err != nil && !errors.Is(err, organization.ErrBusy) && !errors.Is(err, mediasync.ErrSchedule) && !errors.Is(err, context.Canceled) {
			slog.Warn("media library scheduled sync interrupted; inspect persistent run receipts")
		}
	}
	poll(initial)
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			poll(now)
		}
	}
}
