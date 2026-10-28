package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type businessNotifications struct {
	organizer *organizationAPI
	service   notificationService
	queue     *bus.Store
}

const businessNotificationGuard = "business-notification-outbox"

func (api *businessNotifications) authorize(w http.ResponseWriter, r *http.Request) bool {
	if api.organizer == nil || !api.organizer.authorize(w, r) {
		return false
	}
	if !api.organizer.pureGo || api.queue == nil {
		writeAPIError(w, 501, 501, "business notification execution requires disabled legacy backend")
		return false
	}
	return true
}
func (api *businessNotifications) list(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	events, err := api.queue.List(r.Context())
	if err != nil {
		writeAPIError(w, 503, 503, "business notification receipts unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": events})
}
func (api *businessNotifications) dismiss(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	var input struct {
		Confirm bool   `json:"confirm"`
		Reason  string `json:"reason"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid notification disposition") {
		return
	}
	if !input.Confirm || len(r.PathValue("id")) != 64 {
		writeAPIError(w, 400, 400, "confirm receipt disposition with its exact delivery ID and reason")
		return
	}
	claims, err := api.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	guard, err := api.organizer.jobs.AcquireJob(ctx, businessNotificationGuard)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	if err := api.queue.Dismiss(ctx, r.PathValue("id"), claims.Username, input.Reason); err != nil {
		writeAPIError(w, 409, 409, "notification receipt changed or reason is invalid; no delivery retried")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true})
}
func (api *businessNotifications) tick(ctx context.Context) error {
	if api == nil || api.organizer == nil || !api.organizer.pureGo || api.queue == nil {
		return nil
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, businessNotificationGuard)
	if err != nil {
		return err
	}
	defer guard.Close()
	pending, err := api.queue.Pending(ctx)
	if err != nil {
		return err
	}
	for _, d := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, c, err := api.queue.Claim(ctx, d)
		if errors.Is(err, bus.ErrState) {
			continue
		}
		if err != nil {
			return err
		}
		record, err := nativeNotificationRecord(c)
		state, reason := "blocked", "channel format or template is not supported; no delivery attempted"
		image, link := "", ""
		if e.Kind == "mediaserver_message" {
			var media mediaserver.WebhookEvent
			if json.Unmarshal(e.Data, &media) != nil || media.Validate() != nil {
				err = mediaserver.ErrWebhook
			} else {
				image, link = media.Image, media.Link
			}
		}
		if err == nil && nativeNotificationDeliveryEligible(c.Type, objectValue(record["config"])) {
			sent, sendErr := api.service.sendNativeNotification(ctx, c.Type, objectValue(record["config"]), e.Title, e.Text, image, link)
			state, reason = "uncertain", "notification response not positively acknowledged; do not retry automatically"
			if sendErr == nil && sent {
				state, reason = "sent", ""
			}
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = api.queue.Finish(persist, d.ID, state, reason)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
func startBusinessNotificationWorker(ctx context.Context, api *businessNotifications) func() {
	if api == nil || api.organizer == nil || !api.organizer.pureGo || api.queue == nil {
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
			case <-ticker.C:
				run, cancel := context.WithTimeout(ctx, 6*time.Minute)
				err := api.tick(run)
				cancel()
				if err != nil && !errors.Is(err, organization.ErrBusy) && !errors.Is(err, context.Canceled) {
					slog.Warn("business notification dispatch interrupted; retained receipts require review")
				}
			}
		}
	}()
	return func() { <-done }
}
