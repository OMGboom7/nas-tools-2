package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

type subscriptionFilesPublic struct {
	ID             string                                 `json:"id"`
	DownloaderID   string                                 `json:"downloaderId"`
	DownloaderType string                                 `json:"downloaderType"`
	Title          string                                 `json:"title"`
	SubscriptionID string                                 `json:"subscriptionId"`
	Season         int                                    `json:"season"`
	Episodes       []int                                  `json:"episodes"`
	InfoHash       string                                 `json:"infoHash"`
	State          string                                 `json:"state"`
	Recorded       bool                                   `json:"sourceRecorded"`
	Files          []torrentfiles.File                    `json:"files"`
	Seasons        []downloaderconfig.FileSelectionSource `json:"seasons,omitempty"`
}

func publicSubscriptionFiles(p downloaderconfig.FileSelection) subscriptionFilesPublic {
	d := p.Definition
	return subscriptionFilesPublic{d.ID, strconv.FormatInt(d.DownloaderID, 10), d.DownloaderType, d.Title, strconv.FormatInt(d.SubscriptionID, 10), d.Season, append([]int{}, d.Episodes...), d.InfoHash, p.State, p.Committed, append([]torrentfiles.File{}, d.Files...), append([]downloaderconfig.FileSelectionSource{}, d.Sources...)}
}

type subscriptionFilesReview struct {
	Receipt        subscriptionFilesPublic `json:"receipt"`
	Current        *torrentfiles.Snapshot  `json:"current"`
	Next           string                  `json:"next"`
	Actor          string                  `json:"actor"`
	PresetEvidence string                  `json:"presetEvidence"`
	Fingerprint    string                  `json:"fingerprint"`
}

// A preset's setting/start intents remain authoritative. This module never
// treats a failed setting response as permission to replay that setting.
func (s downloadService) selectionPreset(ctx context.Context, item downloaderconfig.Downloader, p downloaderconfig.FileSelection) (string, string, error) {
	d := p.Definition
	if d.PresetFingerprint == "" {
		return "", "", nil
	}
	if item.Type == "qbittorrent" {
		profile, err := s.downloaders.FindQbProfile(ctx, item.ID, d.InfoHash)
		if err != nil || profile.Fingerprint != d.PresetFingerprint || profile.Definition.ConfigDigest != d.ConfigDigest || s.downloaders.VerifyQbSource(ctx, profile.Definition) != nil {
			return "", "", downloaderconfig.ErrFileSelectionPending
		}
		c, err := s.qbProfileClient(item)
		if err != nil {
			return "", "", err
		}
		state, err := c.Profile(ctx, d.InfoHash)
		if err != nil || !profile.Owns(state) || !state.SpeedsMatch(profile.Definition.Options) {
			return "", "", downloaderconfig.ErrFileSelectionPending
		}
		evidence := bus.Hash([]any{profile, state})
		if !state.Unlimited() {
			if !state.Paused() || profile.State == "configuring" {
				return "", "", downloaderconfig.ErrFileSelectionPending
			}
			return "settings", evidence, nil
		}
		if profile.State != "configured" && profile.State != "starting" && profile.State != "ready" {
			return "adopt-settings", evidence, nil
		}
		return "", evidence, nil
	}
	profile, err := s.downloaders.FindTransmissionProfile(ctx, item.ID, d.InfoHash)
	if err != nil || profile.Fingerprint != d.PresetFingerprint || profile.Definition.ConfigDigest != d.ConfigDigest || s.downloaders.VerifyProfileSeeding(ctx, profile.Definition) != nil {
		return "", "", downloaderconfig.ErrFileSelectionPending
	}
	c, err := s.profileClient(item)
	if err != nil {
		return "", "", err
	}
	state, err := c.Profile(ctx, d.InfoHash)
	if err != nil || !profile.Owns(state) {
		return "", "", downloaderconfig.ErrFileSelectionPending
	}
	evidence := bus.Hash([]any{profile, state})
	if !state.Matches(profile.Definition.Limits) {
		if state.Status != 0 || profile.State == "configuring" {
			return "", "", downloaderconfig.ErrFileSelectionPending
		}
		return "settings", evidence, nil
	}
	if profile.State != "configured" && profile.State != "starting" && profile.State != "ready" {
		return "adopt-settings", evidence, nil
	}
	return "", evidence, nil
}
func (api *subscriptionSearchRunner) reviewSubscriptionFiles(ctx context.Context, p downloaderconfig.FileSelection, actor string) (subscriptionFilesReview, error) {
	r := subscriptionFilesReview{Receipt: publicSubscriptionFiles(p), Actor: actor, Next: "none"}
	if p.Committed || p.State == "rejected" {
		r.Fingerprint = bus.Hash([]any{p.Fingerprint, p.State, p.Committed, p.Updated, actor})
		return r, nil
	}
	if api.selectionSourceRead(ctx, p) != nil {
		return r, downloaderconfig.ErrFileSelectionPending
	}
	s := api.planner.runner.download.service
	item, err := s.downloaders.Get(ctx, p.Definition.DownloaderID)
	if err != nil {
		return r, err
	}
	state, err := s.selectionSnapshot(ctx, item, p.Definition)
	if err != nil {
		return r, downloaderconfig.ErrFileSelectionPending
	}
	r.Current = &state
	o, err := selectionOptions(p.Definition)
	if err != nil || s.validatePTAdmission(ctx, item, o) != nil {
		return r, downloaderconfig.ErrFileSelectionPending
	}
	switch p.State {
	case "adding":
		if !state.Paused {
			return r, downloaderconfig.ErrFileSelectionPending
		}
		r.Next = "adopt-add"
	case "added":
		if !state.Paused {
			return r, downloaderconfig.ErrFileSelectionPending
		}
		next, evidence, err := s.selectionPreset(ctx, item, p)
		if err != nil {
			return r, err
		}
		r.PresetEvidence = evidence
		r.Next = next
		if next == "" {
			r.Next = "select"
		}
	case "unwanting":
		if !state.Paused || !unwantedMatch(p.Definition, state) {
			return r, downloaderconfig.ErrFileSelectionPending
		}
		r.Next = "adopt-unwanted"
	case "unwanted":
		if !state.Paused || !unwantedMatch(p.Definition, state) {
			return r, downloaderconfig.ErrFileSelectionPending
		}
		r.Next = "select"
	case "wanting":
		if !state.Paused || !torrentfiles.Wanted(p.Definition.Files, state.Files) {
			return r, downloaderconfig.ErrFileSelectionPending
		}
		r.Next = "adopt-selected"
	case "selected", "starting", "ready":
		if !torrentfiles.Wanted(p.Definition.Files, state.Files) {
			return r, downloaderconfig.ErrFileSelectionPending
		}
		next, evidence, err := s.selectionPreset(ctx, item, p)
		if err != nil || next != "" {
			return r, downloaderconfig.ErrFileSelectionPending
		}
		r.PresetEvidence = evidence
		if p.State == "ready" {
			if state.Paused != o.Paused || p.Definition.PresetFingerprint != "" && s.verifyDownloadProfile(ctx, item, p.Definition.InfoHash, p.Definition.PresetFingerprint) != nil {
				return r, downloaderconfig.ErrFileSelectionPending
			}
			r.Next = "record"
		} else if p.State == "starting" {
			if state.Paused || o.Paused {
				return r, downloaderconfig.ErrFileSelectionPending
			}
			r.Next = "acknowledge"
		} else {
			if !state.Paused {
				return r, downloaderconfig.ErrFileSelectionPending
			}
			if o.Paused {
				r.Next = "acknowledge"
			} else {
				r.Next = "start"
			}
		}
	default:
		return r, downloaderconfig.ErrFileSelectionPending
	}
	r.Fingerprint = bus.Hash([]any{p.Fingerprint, p.State, p.Committed, p.Updated, r})
	return r, nil
}
func (api *subscriptionSearchRunner) recoverSelectionPreset(ctx context.Context, item downloaderconfig.Downloader, p *downloaderconfig.FileSelection, r subscriptionFilesReview) error {
	s := api.planner.runner.download.service
	if err := api.advanceSelection(ctx, p, p.State, r.Actor, r.Fingerprint); err != nil {
		return err
	}
	if item.Type == "qbittorrent" {
		profile, err := s.downloaders.FindQbProfile(ctx, item.ID, p.Definition.InfoHash)
		if err != nil {
			return err
		}
		c, err := s.qbProfileClient(item)
		if err != nil {
			return err
		}
		state, err := c.Profile(ctx, p.Definition.InfoHash)
		if err != nil || !profile.Owns(state) || !state.Paused() {
			return downloaderconfig.ErrFileSelectionPending
		}
		if r.Next == "settings" {
			if profile.State == "configuring" {
				return downloaderconfig.ErrFileSelectionPending
			}
			if err := s.downloaders.AdvanceQbProfile(ctx, profile, "configuring", r.Actor, r.Fingerprint); err != nil {
				return err
			}
			profile.State = "configuring"
			if err := c.SetUnlimited(ctx, p.Definition.InfoHash, state.ShareAction); err != nil {
				return downloaderconfig.ErrFileSelectionPending
			}
		}
		state, err = c.Profile(ctx, p.Definition.InfoHash)
		if err != nil || !profile.Matches(state) || !state.Paused() {
			return downloaderconfig.ErrFileSelectionPending
		}
		return s.downloaders.AdvanceQbProfile(ctx, profile, "configured", r.Actor, r.Fingerprint)
	}
	profile, err := s.downloaders.FindTransmissionProfile(ctx, item.ID, p.Definition.InfoHash)
	if err != nil {
		return err
	}
	c, err := s.profileClient(item)
	if err != nil {
		return err
	}
	if r.Next == "settings" {
		if profile.State == "configuring" {
			return downloaderconfig.ErrFileSelectionPending
		}
		if err := s.downloaders.AdvanceTransmissionProfile(ctx, profile, "configuring", "", r.Actor, r.Fingerprint); err != nil {
			return err
		}
		profile.State = "configuring"
		if err := c.SetLimits(ctx, p.Definition.InfoHash, profile.Definition.Limits); err != nil {
			return downloaderconfig.ErrFileSelectionPending
		}
	}
	state, err := c.Profile(ctx, p.Definition.InfoHash)
	if err != nil || !profile.Owns(state) || !state.Matches(profile.Definition.Limits) || state.Status != 0 {
		return downloaderconfig.ErrFileSelectionPending
	}
	return s.downloaders.AdvanceTransmissionProfile(ctx, profile, "configured", "", r.Actor, r.Fingerprint)
}
func (api *subscriptionSearchRunner) acknowledgeSelection(ctx context.Context, item downloaderconfig.Downloader, p *downloaderconfig.FileSelection, r subscriptionFilesReview) error {
	s := api.planner.runner.download.service
	o, err := selectionOptions(p.Definition)
	if err != nil {
		return err
	}
	state, err := s.selectionSnapshot(ctx, item, p.Definition)
	if err != nil || state.Paused != o.Paused || !torrentfiles.Wanted(p.Definition.Files, state.Files) {
		return downloaderconfig.ErrFileSelectionPending
	}
	if err := api.advanceSelection(ctx, p, p.State, r.Actor, r.Fingerprint); err != nil {
		return err
	}
	if p.Definition.PresetFingerprint != "" {
		if item.Type == "qbittorrent" {
			profile, err := s.downloaders.FindQbProfile(ctx, item.ID, p.Definition.InfoHash)
			if err != nil {
				return err
			}
			c, err := s.qbProfileClient(item)
			if err != nil {
				return err
			}
			state, err := c.Profile(ctx, p.Definition.InfoHash)
			if err != nil || !profile.Ready(state) {
				return downloaderconfig.ErrFileSelectionPending
			}
			if profile.State != "ready" {
				if err := s.downloaders.AdvanceQbProfile(ctx, profile, "ready", r.Actor, r.Fingerprint); err != nil {
					return err
				}
			}
		} else {
			profile, err := s.downloaders.FindTransmissionProfile(ctx, item.ID, p.Definition.InfoHash)
			if err != nil {
				return err
			}
			c, err := s.profileClient(item)
			if err != nil {
				return err
			}
			state, err := c.Profile(ctx, p.Definition.InfoHash)
			if err != nil || !profile.Ready(state) {
				return downloaderconfig.ErrFileSelectionPending
			}
			if profile.State != "ready" {
				if err := s.downloaders.AdvanceTransmissionProfile(ctx, profile, "ready", "", r.Actor, r.Fingerprint); err != nil {
					return err
				}
			}
		}
	}
	return api.advanceSelection(ctx, p, "ready", r.Actor, r.Fingerprint)
}
func (api *subscriptionSearchRunner) serveSubscriptionFiles(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.fuzzy.authorize(w, r)
	if !ok {
		return
	}
	s := api.planner.runner.download.service
	id := r.PathValue("selection")
	if id == "" {
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 1 || v > 10000 {
				writeAPIError(w, 400, 400, "invalid file selection page")
				return
			}
			page = v
		}
		rows, err := s.downloaders.FileSelections(r.Context(), page)
		if err != nil {
			writeAPIError(w, 503, 503, "file selection receipts unavailable")
			return
		}
		out := []subscriptionFilesPublic{}
		for _, p := range rows {
			out = append(out, publicSubscriptionFiles(p))
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": out})
		return
	}
	p, err := s.downloaders.FileSelection(r.Context(), id)
	if err != nil {
		writeAPIError(w, 404, 404, "original file selection unavailable")
		return
	}
	if r.Method == "GET" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": publicSubscriptionFiles(p)})
		return
	}
	action := r.PathValue("action")
	if action != "review" && action != "recover" {
		writeAPIError(w, 405, 405, "unsupported native file selection action")
		return
	}
	var in struct {
		Confirm         bool   `json:"confirm"`
		Fingerprint     string `json:"fingerprint"`
		ConfirmFiles    bool   `json:"confirmFiles"`
		ConfirmSettings bool   `json:"confirmSettings"`
		ConfirmStart    bool   `json:"confirmStart"`
		ConfirmRecord   bool   `json:"confirmRecord"`
		ConfirmSeasons  bool   `json:"confirmSeasons"`
	}
	if !decodeOrganizationRequest(w, r, &in, "invalid file selection action") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if s.manualGuards == nil || *s.manualGuards == nil {
		writeAPIError(w, 503, 503, "file selection guard unavailable")
		return
	}
	ctx, close, err := s.seedingGuard(ctx, p.Definition.DownloaderID, p.Definition.InfoHash)
	if err != nil {
		writeAPIError(w, 409, 409, "original operation active")
		return
	}
	defer close()
	g, err := (*s.manualGuards).AcquireJob(ctx, selectionGuard(p.Definition.DownloaderID, p.Definition.InfoHash))
	if err != nil {
		writeAPIError(w, 409, 409, "original operation active")
		return
	}
	defer g.Close()
	presetGuard := qbProfileGuard(p.Definition.DownloaderID, p.Definition.InfoHash)
	if p.Definition.DownloaderType == "transmission" {
		presetGuard = profileGuard(p.Definition.DownloaderID, p.Definition.InfoHash)
	}
	pg, err := (*s.manualGuards).AcquireJob(ctx, presetGuard)
	if err != nil {
		writeAPIError(w, 409, 409, "original preset operation active")
		return
	}
	defer pg.Close()
	p, err = s.downloaders.FileSelection(ctx, id)
	if err != nil {
		writeAPIError(w, 409, 409, "original file selection changed")
		return
	}
	review, err := api.reviewSubscriptionFiles(ctx, p, actor)
	if err != nil {
		writeAPIError(w, 409, 409, "original source, configuration, stopped state, complete file manifest or positive receipt unavailable; no request replayed")
		return
	}
	if action == "review" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
		return
	}
	if len(p.Definition.Sources) > 0 && !in.ConfirmSeasons || !in.Confirm || in.Fingerprint != review.Fingerprint || review.Next == "none" || review.Next == "select" && !in.ConfirmFiles || review.Next == "settings" && !in.ConfirmSettings || review.Next == "start" && !in.ConfirmStart || review.Next == "record" && !in.ConfirmRecord {
		writeAPIError(w, 409, 409, "fresh original review and independent next-step confirmation required")
		return
	}
	item, err := s.downloaders.Get(ctx, p.Definition.DownloaderID)
	if err != nil {
		writeAPIError(w, 409, 409, "original downloader unavailable")
		return
	}
	switch review.Next {
	case "adopt-add":
		err = api.advanceSelection(ctx, &p, "added", actor, review.Fingerprint)
	case "adopt-unwanted":
		err = api.advanceSelection(ctx, &p, "unwanted", actor, review.Fingerprint)
	case "adopt-selected":
		err = api.advanceSelection(ctx, &p, "selected", actor, review.Fingerprint)
	case "settings", "adopt-settings":
		err = api.recoverSelectionPreset(ctx, item, &p, review)
	case "select":
		err = api.writeSubscriptionFiles(ctx, item, &p, *review.Current, actor, review.Fingerprint)
	case "start":
		err = api.advanceSelection(ctx, &p, "starting", actor, review.Fingerprint)
		if err == nil && p.Definition.PresetFingerprint != "" {
			if item.Type == "qbittorrent" {
				profile, e := s.downloaders.FindQbProfile(ctx, item.ID, p.Definition.InfoHash)
				err = e
				if err == nil {
					err = s.downloaders.AdvanceQbProfile(ctx, profile, "starting", actor, review.Fingerprint)
				}
			} else {
				profile, e := s.downloaders.FindTransmissionProfile(ctx, item.ID, p.Definition.InfoHash)
				err = e
				if err == nil {
					err = s.downloaders.AdvanceTransmissionProfile(ctx, profile, "starting", "", actor, review.Fingerprint)
				}
			}
		}
		if err == nil {
			err = s.startSelectedFiles(ctx, item, p.Definition.InfoHash)
		}
		if err == nil {
			err = api.acknowledgeSelection(ctx, item, &p, review)
		}
	case "acknowledge":
		err = api.acknowledgeSelection(ctx, item, &p, review)
	case "record":
		var failure *recognitionFailure
		_, failure = api.reconcile(ctx, "TV", p.Definition.SubscriptionID)
		if failure != nil {
			err = errors.New("original source receipt unavailable")
		}
	default:
		err = downloaderconfig.ErrFileSelectionPending
	}
	if err != nil {
		writeAPIError(w, 409, 409, "file selection result not positively acknowledged; retain the original ID and review, do not replay requests")
		return
	}
	p, err = s.downloaders.FileSelection(ctx, id)
	if err != nil {
		writeAPIError(w, 503, 503, "read the original file selection receipt")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": publicSubscriptionFiles(p)})
}
