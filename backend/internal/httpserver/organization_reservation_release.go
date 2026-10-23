package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type reservationReleaseInput struct {
	Seed                       organization.ReservationOwnerRef `json:"seed"`
	RequestID                  string                           `json:"requestId"`
	Decision                   string                           `json:"decision"`
	Fingerprint                string                           `json:"fingerprint"`
	ConfirmRead                bool                             `json:"confirmRead"`
	Confirm                    bool                             `json:"confirm"`
	ConfirmReleaseReservations bool                             `json:"confirmReleaseReservations"`
	ConfirmCloseRelated        bool                             `json:"confirmCloseRelated"`
	ConfirmNoContentProof      bool                             `json:"confirmNoContentProof"`
	ConfirmFutureOperations    bool                             `json:"confirmFutureOperations"`
	ConfirmOldExecutorsStopped bool                             `json:"confirmOldExecutorsStopped"`
}

func reservationReleaseGrants(in reservationReleaseInput) bool {
	return in.ConfirmRead && in.Confirm && in.ConfirmReleaseReservations && in.ConfirmCloseRelated && in.ConfirmNoContentProof && in.ConfirmFutureOperations && in.ConfirmOldExecutorsStopped
}
func (api *organizationAPI) serveReservationReleaseStatus(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v, err := api.jobs.ReservationReleases(ctx, r.URL.Query().Get("after"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
}
func (api *organizationAPI) serveReservationReleaseRecord(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v, err := api.jobs.ReservationRelease(ctx, r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
}
func (api *organizationAPI) reservationReleaseReview(ctx context.Context, in reservationReleaseInput, actor string) (organization.ReservationRelease, func(), error) {
	empty := func() {}
	if !in.ConfirmRead {
		return organization.ReservationRelease{}, empty, organization.ErrState
	}
	if in.RequestID == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return organization.ReservationRelease{}, empty, err
		}
		in.RequestID = hex.EncodeToString(nonce[:])
	}
	v, err := api.jobs.ReviewReservationRelease(ctx, in.Seed, actor, in.RequestID, strings.TrimSpace(in.Decision))
	if err != nil {
		return v, empty, err
	}
	owners := []organization.RecoveryOwner{}
	for _, o := range v.Owners {
		if o.Kind == "organization" || o.Kind == "library" {
			owners = append(owners, organization.RecoveryOwner{Kind: o.Kind, ID: o.ID, BatchID: o.BatchID, State: o.State})
		}
	}
	closeGuards, err := api.recoveryGuards(ctx, organization.RecoveryDefinition{Owners: owners})
	if err != nil {
		return v, empty, err
	}
	// Acquiring every existing publication/namespace/job guard is nonblocking.
	// Re-read after acquisition; a newly related owner is not silently omitted.
	locked, err := api.jobs.ReviewReservationRelease(ctx, in.Seed, actor, in.RequestID, strings.TrimSpace(in.Decision))
	if err != nil || locked.Fingerprint != v.Fingerprint {
		closeGuards()
		return v, empty, organization.ErrState
	}
	return locked, closeGuards, nil
}
func (api *organizationAPI) serveReservationReleasePreview(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var in reservationReleaseInput
	if !decodeOrganizationRequest(w, r, &in, "invalid complete reservation release review") {
		return
	}
	actor, err := api.recoveryActor(r)
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v, closeGuards, err := api.reservationReleaseReview(ctx, in, actor)
	defer closeGuards()
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
}
func (api *organizationAPI) serveReservationRelease(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var in reservationReleaseInput
	if !decodeOrganizationRequest(w, r, &in, "invalid independently authorized reservation release") {
		return
	}
	if !reservationReleaseGrants(in) || len(in.Fingerprint) != 64 || len(in.RequestID) != 32 {
		writeAPIError(w, 400, 400, "fresh complete ledger review and all independent reservation-release confirmations required")
		return
	}
	actor, err := api.recoveryActor(r)
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	id := organization.ReservationReleaseID(actor, in.RequestID)
	if saved, e := api.jobs.ReservationRelease(ctx, id); e == nil {
		if saved.Seed != in.Seed || saved.Fingerprint != in.Fingerprint || saved.Decision != strings.TrimSpace(in.Decision) {
			writeOrganizationError(w, organization.ErrState)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": saved})
		return
	} else if !errors.Is(e, organization.ErrNotFound) {
		writeOrganizationError(w, e)
		return
	}
	v, closeGuards, err := api.reservationReleaseReview(ctx, in, actor)
	defer closeGuards()
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if v.Fingerprint != in.Fingerprint {
		writeOrganizationError(w, organization.ErrState)
		return
	}
	result, err := api.jobs.ReleaseReservations(ctx, v)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": result})
}
