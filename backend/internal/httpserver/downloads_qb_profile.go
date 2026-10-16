package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

func qbProfileGuard(id int64, hash string) string {
	return "qbittorrent-profile:" + strconv.FormatInt(id, 10) + ":" + strings.ToLower(hash)
}
func qbDefinition(item downloaderconfig.Downloader, hash string, o downloadAddOptions) downloaderconfig.QbProfileDefinition {
	return downloaderconfig.QbProfileDefinition{DownloaderID: item.ID, Name: item.Name, ConfigDigest: bus.Hash(item), InfoHash: hash, SeedingPolicyID: downloaderconfig.SeedingPolicyIdentity(o.Seeding.policy(item, hash)), Options: qbittorrent.AddOptions{ManagedPT: true, SavePath: o.SavePath, Category: o.Category, Tags: o.Tags, Paused: o.Paused, UploadLimitKB: o.UploadLimitKB, DownloadLimitKB: o.DownloadLimitKB}}
}
func downloadProfileFingerprint(item downloaderconfig.Downloader, hash string, o downloadAddOptions) string {
	if item.Type != "qbittorrent" || o.Seeding == nil {
		return transmissionProfileFingerprint(item, hash, o)
	}
	d, err := downloaderconfig.NormalizeQbProfile(qbDefinition(item, hash, o))
	if err != nil {
		return "invalid-qb-profile"
	}
	return downloaderconfig.QbFingerprint(d)
}
func (s downloadService) qbProfileClient(item downloaderconfig.Downloader) (*qbittorrent.Client, error) {
	var cfg map[string]any
	if json.Unmarshal([]byte(item.Config), &cfg) != nil {
		return nil, qbittorrent.ErrConfiguration
	}
	return qbittorrent.New(text(cfg["host"]), text(cfg["port"]), text(cfg["username"]), text(cfg["password"]), s.client.Transport)
}
func (s downloadService) submitQbProfile(ctx context.Context, item downloaderconfig.Downloader, magnet string, torrent []byte, o downloadAddOptions) (string, error) {
	hash := magnetInfoHash(magnet)
	if torrent != nil {
		m, err := torrentmeta.Parse(torrent)
		if err != nil {
			return "", errPTAdmissionBlocked
		}
		hash = m.InfoHash
	}
	if err := s.validatePTAdmission(ctx, item, o); err != nil {
		return "", err
	}
	d, err := downloaderconfig.NormalizeQbProfile(qbDefinition(item, hash, o))
	if err != nil {
		return "", errPTAdmissionBlocked
	}
	ctx, close, err := s.seedingGuard(ctx, item.ID, hash)
	if err != nil {
		return "", errPTAdmissionBlocked
	}
	defer close()
	g, err := (*s.manualGuards).AcquireJob(ctx, qbProfileGuard(item.ID, hash))
	if err != nil {
		return "", downloaderconfig.ErrQbProfilePending
	}
	defer g.Close()
	c, err := s.qbProfileClient(item)
	if err != nil {
		return "", err
	}
	_, err = s.downloaders.FindQbProfile(ctx, item.ID, hash)
	if errors.Is(err, downloaderconfig.ErrNotFound) {
		exists, err := c.HasTorrent(ctx, hash)
		if err != nil || exists {
			return "", errPTAdmissionBlocked
		}
	} else if err != nil {
		return "", downloaderconfig.ErrQbProfilePending
	}
	if err := c.CheckPTProtocol(ctx); err != nil {
		return "", errPTAdmissionBlocked
	}
	p, created, err := s.downloaders.ReserveQbProfile(ctx, d)
	if err != nil {
		return "", downloaderconfig.ErrQbProfilePending
	}
	if s.bindAdmissionSource(ctx, item, hash, o) != nil || s.downloaders.VerifyQbSource(ctx, d) != nil {
		return "", downloaderconfig.ErrQbProfilePending
	}
	if !created {
		if p.State != "ready" {
			return "", downloaderconfig.ErrQbProfilePending
		}
		state, err := c.Profile(ctx, hash)
		if err != nil || !p.Ready(state) {
			return "", downloaderconfig.ErrQbProfilePending
		}
		return hash, nil
	}
	if s.validatePTAdmission(ctx, item, o) != nil {
		return "", downloaderconfig.ErrQbProfilePending
	}
	add := d.Options
	add.Paused = true
	add.Tags = append(append([]string{}, add.Tags...), p.Marker())
	if torrent != nil {
		err = c.AddTorrentWithOptions(ctx, torrent, add)
	} else {
		err = c.AddMagnetWithOptions(ctx, magnet, add)
	}
	if err != nil {
		if errors.Is(err, qbittorrent.ErrAuthentication) || errors.Is(err, qbittorrent.ErrConfiguration) {
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if s.downloaders.AdvanceQbProfile(persist, p, "rejected", "native-submit", "") == nil {
				return "", err
			}
		}
		return "", downloaderconfig.ErrQbProfilePending
	}
	advance := func(to string) error {
		if err := s.downloaders.AdvanceQbProfile(ctx, p, to, "native-submit", ""); err != nil {
			return err
		}
		p.State = to
		return nil
	}
	if advance("added") != nil {
		return "", downloaderconfig.ErrQbProfilePending
	}
	state, err := c.Profile(ctx, hash)
	if err != nil || !p.Owns(state) || !state.Paused() || !state.SpeedsMatch(d.Options) {
		return "", downloaderconfig.ErrQbProfilePending
	}
	if advance("configuring") != nil || c.SetUnlimited(ctx, hash, state.ShareAction) != nil {
		return "", downloaderconfig.ErrQbProfilePending
	}
	action := state.ShareAction
	state, err = c.Profile(ctx, hash)
	if err != nil || !p.Matches(state) || !state.Paused() || state.ShareAction != action {
		return "", downloaderconfig.ErrQbProfilePending
	}
	if advance("configured") != nil {
		return "", downloaderconfig.ErrQbProfilePending
	}
	if !d.Options.Paused {
		if advance("starting") != nil || c.StartProfile(ctx, hash) != nil {
			return "", downloaderconfig.ErrQbProfilePending
		}
		state, err = c.Profile(ctx, hash)
		if err != nil || !p.Ready(state) {
			return "", downloaderconfig.ErrQbProfilePending
		}
	}
	if advance("ready") != nil {
		return "", downloaderconfig.ErrQbProfilePending
	}
	return hash, nil
}
func (s downloadService) checkQbProfileReady(ctx context.Context, item downloaderconfig.Downloader, hash string) error {
	p, err := s.downloaders.FindQbProfile(ctx, item.ID, hash)
	if errors.Is(err, downloaderconfig.ErrNotFound) {
		return nil
	}
	if err != nil || p.State != "ready" || p.Definition.ConfigDigest != bus.Hash(item) {
		return downloaderconfig.ErrQbProfilePending
	}
	return s.downloaders.VerifyQbSource(ctx, p.Definition)
}
func (s downloadService) verifyDownloadProfile(ctx context.Context, item downloaderconfig.Downloader, hash, fp string) error {
	if !strings.HasPrefix(fp, "qb:") {
		if item.Type != "transmission" {
			return downloaderconfig.ErrProfilePending
		}
		return s.verifyTransmissionProfile(ctx, item, hash, fp)
	}
	if item.Type != "qbittorrent" {
		return downloaderconfig.ErrQbProfilePending
	}
	p, err := s.downloaders.FindQbProfile(ctx, item.ID, hash)
	if err != nil || p.Fingerprint != fp || s.checkQbProfileReady(ctx, item, hash) != nil {
		return downloaderconfig.ErrQbProfilePending
	}
	c, err := s.qbProfileClient(item)
	if err != nil {
		return err
	}
	state, err := c.Profile(ctx, hash)
	if err != nil || !p.Ready(state) {
		return downloaderconfig.ErrQbProfilePending
	}
	return nil
}

type qbProfilePublic struct {
	downloaderconfig.QbProfile
	DownloaderID    int64                  `json:"downloaderId"`
	Name            string                 `json:"name"`
	InfoHash        string                 `json:"infoHash"`
	SeedingPolicyID string                 `json:"seedingPolicyId"`
	Options         qbittorrent.AddOptions `json:"options"`
}

func publicQbProfile(p downloaderconfig.QbProfile) qbProfilePublic {
	d := p.Definition
	return qbProfilePublic{p, d.DownloaderID, d.Name, d.InfoHash, d.SeedingPolicyID, d.Options}
}

type qbProfileReview struct {
	Profile     qbProfilePublic          `json:"profile"`
	Current     qbittorrent.ProfileState `json:"current"`
	Next        string                   `json:"next"`
	Actor       string                   `json:"actor"`
	Fingerprint string                   `json:"fingerprint"`
}

func (s downloadService) reviewQbProfile(ctx context.Context, p downloaderconfig.QbProfile, actor string) (qbProfileReview, error) {
	r := qbProfileReview{}
	item, err := s.downloaders.Get(ctx, p.Definition.DownloaderID)
	if err != nil || item.Enabled == 0 || item.Type != "qbittorrent" || p.State == "rejected" || bus.Hash(item) != p.Definition.ConfigDigest || s.downloaders.VerifyQbSource(ctx, p.Definition) != nil {
		return r, downloaderconfig.ErrQbProfilePending
	}
	c, err := s.qbProfileClient(item)
	if err != nil {
		return r, err
	}
	state, err := c.Profile(ctx, p.Definition.InfoHash)
	if err != nil || !p.Owns(state) || !state.SpeedsMatch(p.Definition.Options) {
		return r, downloaderconfig.ErrQbProfilePending
	}
	next := "acknowledge"
	if !state.Unlimited() {
		if !state.Paused() || p.State == "configuring" {
			return r, downloaderconfig.ErrQbProfilePending
		}
		next = "configure"
	} else if !p.Definition.Options.Paused && state.Paused() {
		if p.State == "starting" {
			return r, downloaderconfig.ErrQbProfilePending
		}
		next = "start"
	} else if !p.Ready(state) {
		return r, downloaderconfig.ErrQbProfilePending
	}
	r = qbProfileReview{Profile: publicQbProfile(p), Current: state, Next: next, Actor: actor}
	r.Fingerprint = bus.Hash(r)
	return r, nil
}
func (s downloadService) serveQbProfiles(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeManualReceipts(w, r); !ok {
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 10000 {
			writeAPIError(w, 400, 400, "invalid qB profile page")
			return
		}
	}
	rows, err := s.downloaders.QbProfiles(r.Context(), page)
	if err != nil {
		writeAPIError(w, 503, 503, "qB profile receipts unavailable")
		return
	}
	result := []qbProfilePublic{}
	for _, p := range rows {
		result = append(result, publicQbProfile(p))
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": result})
}
func (s downloadService) serveQbProfileAction(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorizeManualReceipts(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if action != "review" && action != "recover" {
		writeAPIError(w, 404, 404, "unknown qB profile action")
		return
	}
	var in struct {
		Confirm         bool   `json:"confirm"`
		Fingerprint     string `json:"fingerprint"`
		ConfirmSettings bool   `json:"confirmSettings"`
		ConfirmStart    bool   `json:"confirmStart"`
	}
	if !decodeServiceRequest(w, r, &in, "invalid qB profile request") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	p, err := s.downloaders.QbProfile(ctx, r.PathValue("id"))
	if err != nil {
		writeAPIError(w, 409, 409, "qB profile unavailable")
		return
	}
	if s.manualGuards == nil || *s.manualGuards == nil {
		writeAPIError(w, 503, 503, "qB profile guard unavailable")
		return
	}
	ctx, close, err := s.seedingGuard(ctx, p.Definition.DownloaderID, p.Definition.InfoHash)
	if err != nil {
		manualSubmissionError(w, err)
		return
	}
	defer close()
	g, err := (*s.manualGuards).AcquireJob(ctx, qbProfileGuard(p.Definition.DownloaderID, p.Definition.InfoHash))
	if err != nil {
		manualSubmissionError(w, downloaderconfig.ErrQbProfilePending)
		return
	}
	defer g.Close()
	p, err = s.downloaders.QbProfile(ctx, p.ID)
	if err != nil {
		manualSubmissionError(w, downloaderconfig.ErrQbProfilePending)
		return
	}
	review, err := s.reviewQbProfile(ctx, p, actor)
	if err != nil {
		manualSubmissionError(w, downloaderconfig.ErrQbProfilePending)
		return
	}
	if action == "review" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
		return
	}
	if !in.Confirm || in.Fingerprint != review.Fingerprint || review.Next == "configure" && !in.ConfirmSettings || review.Next == "start" && !in.ConfirmStart {
		writeAPIError(w, 409, 409, "fresh qB review and independent next-step consent required")
		return
	}
	item, err := s.downloaders.Get(ctx, p.Definition.DownloaderID)
	if err != nil {
		manualSubmissionError(w, downloaderconfig.ErrQbProfilePending)
		return
	}
	c, err := s.qbProfileClient(item)
	if err != nil {
		manualSubmissionError(w, downloaderconfig.ErrQbProfilePending)
		return
	}
	advance := func(to string) error {
		if err := s.downloaders.AdvanceQbProfile(ctx, p, to, actor, review.Fingerprint); err != nil {
			return err
		}
		p.State = to
		return nil
	}
	switch review.Next {
	case "configure":
		err = advance("configuring")
		if err == nil {
			err = c.SetUnlimited(ctx, p.Definition.InfoHash, review.Current.ShareAction)
		}
	case "start":
		err = advance("starting")
		if err == nil {
			err = c.StartProfile(ctx, p.Definition.InfoHash)
		}
	}
	if err == nil {
		var state qbittorrent.ProfileState
		state, err = c.Profile(ctx, p.Definition.InfoHash)
		if err == nil {
			if review.Next == "configure" {
				if !p.Matches(state) || !state.Paused() || state.ShareAction != review.Current.ShareAction {
					err = downloaderconfig.ErrQbProfilePending
				} else {
					err = advance("configured")
				}
			} else if !p.Ready(state) {
				err = downloaderconfig.ErrQbProfilePending
			} else {
				err = advance("ready")
			}
		}
	}
	if err != nil {
		manualSubmissionError(w, downloaderconfig.ErrQbProfilePending)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": publicQbProfile(p)})
}
