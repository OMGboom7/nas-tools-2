package httpserver

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type legacyRedoReviewItem struct {
	ID         string            `json:"id"`
	Plan       *organizationPlan `json:"plan,omitempty"`
	SourceRoot string            `json:"sourceRoot,omitempty"`
	TargetRoot string            `json:"targetRoot,omitempty"`
	Code       int               `json:"code"`
	Message    string            `json:"message,omitempty"`
}
type legacyRedoReview struct {
	Flag        string                 `json:"flag"`
	Items       []legacyRedoReviewItem `json:"items"`
	Fingerprint string                 `json:"fingerprint"`
}

func makeLegacyRedoReview(flag string, selectors []organization.UnknownDeleteSelector, plans []organizationPlan, failures []*recognitionFailure) legacyRedoReview {
	review := legacyRedoReview{Flag: flag, Items: []legacyRedoReviewItem{}}
	for i, selector := range selectors {
		item := legacyRedoReviewItem{ID: strconv.FormatInt(selector.ID, 10)}
		if failures[i] != nil {
			item.Code, item.Message = failures[i].status, failures[i].message
		} else {
			item.Plan = &plans[i]
			item.SourceRoot, item.TargetRoot = plans[i].definition.SourceRoot, plans[i].definition.TargetRoot
		}
		review.Items = append(review.Items, item)
	}
	// Public plans carry opaque hashes of their full private definition,
	// including account and original-row snapshots, but never the Binding.
	review.Fingerprint = organization.Digest(review)
	return review
}

func (api organizationAPI) serveLegacyRedoPreview(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	input, ok := decodeLegacyRedo(w, r)
	if !ok {
		return
	}
	selectors, err := legacyUnknownIDs(input.IDs)
	if err != nil {
		writeAPIError(w, 400, 400, "invalid re-identification IDs")
		return
	}
	seen := map[int64]bool{}
	for _, selector := range selectors {
		if seen[selector.ID] {
			writeAPIError(w, 400, 400, "duplicate re-identification ID")
			return
		}
		seen[selector.ID] = true
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid or expired authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	plans := make([]organizationPlan, len(selectors))
	failures := make([]*recognitionFailure, len(selectors))
	for i, selector := range selectors {
		plans[i], failures[i] = api.legacyRedoPlan(ctx, input.Flag, selector.ID, claims.Username)
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": makeLegacyRedoReview(input.Flag, selectors, plans, failures)})
}
