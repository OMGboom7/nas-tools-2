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
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

func needsTransmissionProfile(item downloaderconfig.Downloader, o downloadAddOptions) bool {
	return item.Type == "transmission" && (o.Seeding != nil || o.ApplyTransmissionLimits || o.UploadLimitKB != 0 || o.DownloadLimitKB != 0 || o.RatioLimit != 0 || o.SeedingTimeLimit != 0)
}
func transmissionDefinition(item downloaderconfig.Downloader, hash string, o downloadAddOptions) downloaderconfig.TransmissionProfileDefinition {
	d := downloaderconfig.TransmissionProfileDefinition{DownloaderID: item.ID, Name: item.Name, ConfigDigest: bus.Hash(item), InfoHash: hash, Options: transmission.AddOptions{DownloadDir: o.SavePath, Paused: o.Paused, Labels: o.Tags}, Limits: transmission.Limits{UploadKB: o.UploadLimitKB, DownloadKB: o.DownloadLimitKB, Ratio: o.RatioLimit, IdleMinutes: o.SeedingTimeLimit}}
	if o.Seeding != nil {
		d.SeedingPolicyID = downloaderconfig.SeedingPolicyIdentity(o.Seeding.policy(item, hash))
	}
	return d
}
func transmissionProfileFingerprint(item downloaderconfig.Downloader, hash string, o downloadAddOptions) string {
	if !needsTransmissionProfile(item, o) {
		return ""
	}
	d, err := downloaderconfig.NormalizeTransmissionProfile(transmissionDefinition(item, hash, o))
	if err != nil {
		return "invalid-profile"
	}
	return bus.Hash(d)
}
func profileGuard(itemID int64, hash string) string {
	return "transmission-profile:" + strconv.FormatInt(itemID, 10) + ":" + hash
}
func (service downloadService) profileClient(item downloaderconfig.Downloader) (*transmission.Client, error) {
	var c map[string]any
	if json.Unmarshal([]byte(item.Config), &c) != nil {
		return nil, transmission.ErrConfiguration
	}
	return transmission.New(text(c["host"]), text(c["port"]), text(c["username"]), text(c["password"]), service.client.Transport)
}
func (service downloadService) submitTransmissionProfile(ctx context.Context, item downloaderconfig.Downloader, magnet string, torrent []byte, o downloadAddOptions) (string, error) {
	hash := magnetInfoHash(magnet)
	if torrent != nil {
		m, err := torrentmeta.Parse(torrent)
		if err != nil {
			return "", transmission.ErrConfiguration
		}
		hash = m.InfoHash
	}
	d, err := downloaderconfig.NormalizeTransmissionProfile(transmissionDefinition(item, hash, o))
	if err != nil {
		return "", errDownloadOptionsUnsupported
	}
	c, err := service.profileClient(item)
	if err != nil {
		return "", err
	}
	if o.Seeding != nil {
		if err := service.validatePTAdmission(ctx, item, o); err != nil {
			return "", err
		}
		guarded, close, err := service.seedingGuard(ctx, item.ID, hash)
		if err != nil {
			return "", errPTAdmissionBlocked
		}
		defer close()
		ctx = guarded
	}
	if service.manualGuards != nil && *service.manualGuards != nil {
		guard, err := (*service.manualGuards).AcquireJob(ctx, profileGuard(item.ID, hash))
		if err != nil {
			return "", downloaderconfig.ErrProfilePending
		}
		defer guard.Close()
	}
	if o.Seeding != nil {
		_, err := service.downloaders.FindTransmissionProfile(ctx, item.ID, hash)
		if errors.Is(err, downloaderconfig.ErrNotFound) {
			exists, err := c.HasTorrent(ctx, hash)
			if err != nil || exists {
				return "", errPTAdmissionBlocked
			}
		} else if err != nil {
			return "", downloaderconfig.ErrProfilePending
		}
	}
	p, created, err := service.downloaders.ReserveTransmissionProfile(ctx, d)
	if err != nil {
		return "", downloaderconfig.ErrProfilePending
	}
	if err := service.bindAdmissionSource(ctx, item, hash, o); err != nil {
		return "", downloaderconfig.ErrProfilePending
	}
	if err := service.downloaders.VerifyProfileSeeding(ctx, p.Definition); err != nil {
		return "", err
	}
	if !created {
		if p.State != "ready" {
			return "", downloaderconfig.ErrProfilePending
		}
		s, err := c.Profile(ctx, hash)
		if err != nil || !p.Ready(s) {
			return "", downloaderconfig.ErrProfilePending
		}
		return hash, nil
	}
	options := d.Options
	options.Paused = true
	options.Labels = append(append([]string{}, options.Labels...), p.Marker())
	if err := service.validatePTAdmission(ctx, item, o); err != nil {
		return "", downloaderconfig.ErrProfilePending
	}
	var id string
	if torrent != nil {
		id, err = c.AddTorrentWithOptions(ctx, torrent, options)
	} else {
		id, err = c.AddMagnetWithOptions(ctx, magnet, options)
	}
	if err != nil {
		if errors.Is(err, transmission.ErrConfiguration) || errors.Is(err, transmission.ErrAuthentication) {
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if e := service.downloaders.AdvanceTransmissionProfile(persist, p, "rejected", "rejected before acceptance", "native-submit", ""); e == nil {
				return "", err
			}
		}
		return "", downloaderconfig.ErrProfilePending
	}
	if strings.ToLower(id) != hash {
		return "", downloaderconfig.ErrProfilePending
	}
	advance := func(to string) error {
		if err := service.downloaders.AdvanceTransmissionProfile(ctx, p, to, "", "native-submit", ""); err != nil {
			return err
		}
		p.State = to
		return nil
	}
	if err := advance("added"); err != nil {
		return "", downloaderconfig.ErrProfilePending
	}
	s, err := c.Profile(ctx, hash)
	if err != nil || !p.Owns(s) || s.Status != 0 {
		return "", downloaderconfig.ErrProfilePending
	}
	if advance("configuring") != nil {
		return "", downloaderconfig.ErrProfilePending
	}
	if c.SetLimits(ctx, hash, d.Limits) != nil {
		return "", downloaderconfig.ErrProfilePending
	}
	s, err = c.Profile(ctx, hash)
	if err != nil || !p.Owns(s) || !s.Matches(d.Limits) || s.Status != 0 {
		return "", downloaderconfig.ErrProfilePending
	}
	if advance("configured") != nil {
		return "", downloaderconfig.ErrProfilePending
	}
	if !d.Options.Paused {
		if advance("starting") != nil || c.Control(ctx, hash, "start") != nil {
			return "", downloaderconfig.ErrProfilePending
		}
		s, err = c.Profile(ctx, hash)
		if err != nil || !p.Ready(s) {
			return "", downloaderconfig.ErrProfilePending
		}
	}
	if advance("ready") != nil {
		return "", downloaderconfig.ErrProfilePending
	}
	return hash, nil
}

func (service downloadService) checkTransmissionProfileReady(ctx context.Context, item downloaderconfig.Downloader, hash string) error {
	p, err := service.downloaders.FindTransmissionProfile(ctx, item.ID, hash)
	if errors.Is(err, downloaderconfig.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.State != "ready" || p.Definition.ConfigDigest != bus.Hash(item) {
		return downloaderconfig.ErrProfilePending
	}
	return service.downloaders.VerifyProfileSeeding(ctx, p.Definition)
}

type transmissionProfilePublic struct {
	downloaderconfig.TransmissionProfile
	DownloaderID    string                  `json:"downloaderId"`
	Name            string                  `json:"name"`
	InfoHash        string                  `json:"infoHash"`
	Options         transmission.AddOptions `json:"options"`
	Limits          transmission.Limits     `json:"limits"`
	SeedingPolicyID string                  `json:"seedingPolicyId,omitempty"`
}

func publicProfile(p downloaderconfig.TransmissionProfile) transmissionProfilePublic {
	d := p.Definition
	return transmissionProfilePublic{p, strconv.FormatInt(d.DownloaderID, 10), d.Name, d.InfoHash, d.Options, d.Limits, d.SeedingPolicyID}
}

type transmissionProfileReview struct {
	Profile     transmissionProfilePublic `json:"profile"`
	Current     transmission.ProfileState `json:"current"`
	Next        string                    `json:"next"`
	Actor       string                    `json:"actor"`
	Fingerprint string                    `json:"fingerprint"`
}

func (service downloadService) reviewTransmissionProfile(ctx context.Context, p downloaderconfig.TransmissionProfile, actor string) (transmissionProfileReview, error) {
	r := transmissionProfileReview{}
	if p.State == "rejected" {
		return r, downloaderconfig.ErrProfilePending
	}
	item, err := service.downloaders.Get(ctx, p.Definition.DownloaderID)
	if err != nil || item.Enabled == 0 || item.Type != "transmission" || bus.Hash(item) != p.Definition.ConfigDigest {
		return r, downloaderconfig.ErrProfilePending
	}
	if err := service.downloaders.VerifyProfileSeeding(ctx, p.Definition); err != nil {
		return r, err
	}
	c, err := service.profileClient(item)
	if err != nil {
		return r, err
	}
	current, err := c.Profile(ctx, p.Definition.InfoHash)
	if err != nil || !p.Owns(current) {
		return r, downloaderconfig.ErrProfilePending
	}
	next := "acknowledge"
	if !current.Matches(p.Definition.Limits) {
		if current.Status != 0 {
			return r, downloaderconfig.ErrProfilePending
		}
		next = "configure"
	} else if !p.Definition.Options.Paused && current.Status == 0 {
		next = "start"
	} else if p.Definition.Options.Paused && current.Status != 0 {
		return r, downloaderconfig.ErrProfilePending
	}
	if p.State == "configuring" && next == "configure" || p.State == "starting" && next == "start" {
		return r, downloaderconfig.ErrProfilePending
	}
	r = transmissionProfileReview{Profile: publicProfile(p), Current: current, Next: next, Actor: actor}
	r.Fingerprint = bus.Hash(r)
	return r, nil
}

func (service downloadService) verifyTransmissionProfile(ctx context.Context, item downloaderconfig.Downloader, hash, fp string) error {
	p, err := service.downloaders.FindTransmissionProfile(ctx, item.ID, hash)
	if err != nil || p.State != "ready" || p.Fingerprint != fp || p.Definition.ConfigDigest != bus.Hash(item) {
		return downloaderconfig.ErrProfilePending
	}
	if err := service.downloaders.VerifyProfileSeeding(ctx, p.Definition); err != nil {
		return err
	}
	c, err := service.profileClient(item)
	if err != nil {
		return err
	}
	s, err := c.Profile(ctx, hash)
	if err != nil || !p.Ready(s) {
		return downloaderconfig.ErrProfilePending
	}
	return nil
}
func (service downloadService) serveTransmissionProfiles(w http.ResponseWriter, r *http.Request) {
	if _, ok := service.authorizeManualReceipts(w, r); !ok {
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 10000 {
			writeAPIError(w, 400, 400, "invalid settings page")
			return
		}
		page = n
	}
	profiles, err := service.downloaders.TransmissionProfilesPage(r.Context(), page)
	if err != nil {
		writeAPIError(w, 503, 503, "Transmission setting receipts unavailable")
		return
	}
	result := []transmissionProfilePublic{}
	for _, p := range profiles {
		result = append(result, publicProfile(p))
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": result})
}
func (service downloadService) serveTransmissionProfileAction(w http.ResponseWriter, r *http.Request) {
	actor, ok := service.authorizeManualReceipts(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if action != "review" && action != "recover" {
		writeAPIError(w, 404, 404, "Transmission setting action unavailable")
		return
	}
	var input struct {
		Confirm         bool   `json:"confirm"`
		Fingerprint     string `json:"fingerprint"`
		ConfirmSettings bool   `json:"confirmSettings"`
		ConfirmStart    bool   `json:"confirmStart"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid Transmission setting action") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	p, err := service.downloaders.TransmissionProfile(ctx, r.PathValue("id"))
	if err != nil {
		writeAPIError(w, 409, 409, "Transmission setting receipt unavailable")
		return
	}
	if service.manualGuards == nil || *service.manualGuards == nil {
		writeAPIError(w, 503, 503, "operation guard unavailable")
		return
	}
	guard, err := (*service.manualGuards).AcquireJob(ctx, profileGuard(p.Definition.DownloaderID, p.Definition.InfoHash))
	if err != nil {
		writeAPIError(w, 409, 409, "Transmission setting operation active")
		return
	}
	defer guard.Close()
	p, err = service.downloaders.TransmissionProfile(ctx, p.ID)
	if err != nil {
		writeAPIError(w, 409, 409, "Transmission setting receipt changed")
		return
	}
	review, err := service.reviewTransmissionProfile(ctx, p, actor)
	if err != nil {
		writeAPIError(w, 409, 409, "exact task marker, original configuration, stopped state or setting evidence unavailable; no mutation performed")
		return
	}
	if action == "review" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
		return
	}
	if !input.Confirm || input.Fingerprint != review.Fingerprint || review.Next == "configure" && !input.ConfirmSettings || review.Next == "start" && !input.ConfirmStart {
		writeAPIError(w, 409, 409, "fresh review and separate settings/start confirmation required")
		return
	}
	if p.State == "ready" && review.Next == "acknowledge" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": publicProfile(p)})
		return
	}
	item, err := service.downloaders.Get(ctx, p.Definition.DownloaderID)
	if err != nil || bus.Hash(item) != p.Definition.ConfigDigest {
		writeAPIError(w, 409, 409, "original downloader changed")
		return
	}
	c, err := service.profileClient(item)
	if err != nil {
		writeAPIError(w, 409, 409, "original client unavailable")
		return
	}
	advance := func(to string) error {
		if err := service.downloaders.AdvanceTransmissionProfile(ctx, p, to, "", actor, review.Fingerprint); err != nil {
			return err
		}
		p.State = to
		return nil
	}
	switch review.Next {
	case "configure":
		if advance("configuring") == nil {
			err = c.SetLimits(ctx, p.Definition.InfoHash, p.Definition.Limits)
			if err == nil {
				current, e := c.Profile(ctx, p.Definition.InfoHash)
				if e != nil || !p.Owns(current) || !current.Matches(p.Definition.Limits) || current.Status != 0 {
					err = downloaderconfig.ErrProfilePending
				} else if p.Definition.Options.Paused {
					err = advance("ready")
				} else {
					err = advance("configured")
				}
			}
		} else {
			err = downloaderconfig.ErrProfilePending
		}
	case "start":
		if advance("starting") == nil {
			err = c.Control(ctx, p.Definition.InfoHash, "start")
			if err == nil {
				current, e := c.Profile(ctx, p.Definition.InfoHash)
				if e != nil || !p.Ready(current) {
					err = downloaderconfig.ErrProfilePending
				} else {
					err = advance("ready")
				}
			}
		} else {
			err = downloaderconfig.ErrProfilePending
		}
	case "acknowledge":
		err = advance("ready")
	}
	if err != nil {
		writeAPIError(w, 409, 409, "setting operation not positively acknowledged; read/review the original receipt, do not replay writes")
		return
	}
	p, err = service.downloaders.TransmissionProfile(ctx, p.ID)
	if err != nil {
		writeAPIError(w, 503, 503, "read original setting receipt before further action")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": publicProfile(p)})
}
