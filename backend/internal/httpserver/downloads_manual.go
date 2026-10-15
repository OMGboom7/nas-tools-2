package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

var errManualUnauthorized = errors.New("manual download authorization failed")
var errManualForbidden = errors.New("manual download permission required")
var errManualConfiguration = errors.New("invalid manual downloader configuration")

func (service downloadService) manualActor(ctx context.Context, token string) (string, error) {
	claims, err := service.auth.service.VerifyToken(token)
	if err != nil {
		return "", errManualUnauthorized
	}
	user, err := service.auth.service.FindUser(ctx, claims.Username)
	if err != nil {
		return "", errManualUnauthorized
	}
	for _, p := range user.Permissions {
		if p == "下载管理" {
			return strconv.FormatInt(user.ID, 10) + ":" + user.Name, nil
		}
	}
	return "", errManualForbidden
}
func manualSubmissionError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errManualUnauthorized):
		writeAPIError(w, 401, 401, "invalid authorization")
	case errors.Is(err, errManualForbidden):
		writeAPIError(w, 403, 403, "download permission required")
	case errors.Is(err, errManualConfiguration):
		writeAPIError(w, 400, 400, "invalid downloader configuration; no submission attempted")
	case errors.Is(err, downloaderconfig.ErrManualPending):
		writeAPIError(w, 409, 409, "manual submission pending or scope changed; read its receipt and verify the original downloader, do not resubmit")
	default:
		return false
	}
	return true
}
func (service downloadService) submitManual(ctx context.Context, r *http.Request, magnet string, torrent []byte, selected *downloaderconfig.Downloader, options downloadAddOptions, title, source string) (string, bool, error) {
	if service.downloaders == nil || service.auth == nil {
		return "", false, nil
	}
	actor, err := service.manualActor(ctx, r.Header.Get("Authorization"))
	if err != nil {
		return "", true, err
	}
	var item downloaderconfig.Downloader
	if selected != nil {
		item = *selected
	} else {
		var handled bool
		item, _, handled, err = service.configuredDownloader(ctx, "")
		if err != nil || !handled {
			return "", handled, err
		}
	}
	if item.ID == 0 || item.Enabled == 0 {
		return "", true, errors.New("downloader is unavailable")
	}
	var configuration map[string]any
	if json.Unmarshal([]byte(item.Config), &configuration) != nil || configuration == nil {
		return "", true, errManualConfiguration
	}
	if item.Type != "qbittorrent" && item.Type != "transmission" && item.Type != "aria2" && item.Type != "pan115" || item.Type == "pan115" && torrent != nil {
		return "", false, nil
	}
	hash := magnetInfoHash(magnet)
	if torrent != nil {
		metadata, err := torrentmeta.Parse(torrent)
		if err != nil {
			return "", true, err
		}
		hash = metadata.InfoHash
		if title == "" {
			title = metadata.Name
		}
	}
	if hash == "" {
		return "", true, errors.New("torrent identity unavailable")
	}
	if service.manualGuards != nil && *service.manualGuards != nil {
		guard, err := (*service.manualGuards).AcquireJob(ctx, "manual-download:"+hash)
		if err != nil {
			return "", true, downloaderconfig.ErrManualPending
		}
		defer guard.Close()
	}
	if title == "" {
		title = "手动任务 " + hash[:12]
	}
	title = bus.Bounded(title, 600)
	d := downloaderconfig.ManualDefinition{DownloaderID: item.ID, DownloaderName: item.Name, DownloaderType: item.Type, ConfigDigest: bus.Hash(item), OptionsDigest: bus.Hash(options), InfoHash: hash, Actor: actor, Title: title, Source: source, SavePath: options.SavePath}
	receipt, created, err := service.downloaders.ReserveManual(ctx, d)
	if err != nil {
		return "", true, err
	}
	if !created {
		return receipt.TaskID, true, nil
	}
	id, handled, err := service.nativeAddConfiguredDownload(ctx, item, magnet, torrent, options)
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err != nil || !handled {
		if downloadDefinitelyNotSubmitted(handled, err) {
			if rejectErr := service.downloaders.RejectManual(persist, receipt); rejectErr != nil {
				return "", true, downloaderconfig.ErrManualPending
			}
			return id, handled, err
		}
		return "", true, downloaderconfig.ErrManualPending
	}
	id = strings.ToLower(id)
	if item.Type == "aria2" {
		if !aria2.ValidGID(id) {
			return "", true, downloaderconfig.ErrManualPending
		}
	} else if id != hash {
		return "", true, downloaderconfig.ErrManualPending
	}
	if err := service.downloaders.CompleteManual(persist, receipt, id, "acknowledged", actor); err != nil {
		return "", true, downloaderconfig.ErrManualPending
	}
	return id, true, nil
}

type manualReceipt struct {
	downloaderconfig.ManualSubmission
	DownloaderID   string `json:"downloaderId"`
	DownloaderName string `json:"downloaderName"`
	DownloaderType string `json:"downloaderType"`
	InfoHash       string `json:"infoHash"`
	Actor          string `json:"actor"`
	Title          string `json:"title"`
	Source         string `json:"source"`
}

func publicManual(r downloaderconfig.ManualSubmission) manualReceipt {
	d := r.Definition
	return manualReceipt{r, strconv.FormatInt(d.DownloaderID, 10), d.DownloaderName, d.DownloaderType, d.InfoHash, d.Actor, d.Title, d.Source}
}
func (service downloadService) authorizeManualReceipts(w http.ResponseWriter, r *http.Request) (string, bool) {
	if service.auth == nil || service.downloaders == nil {
		writeAPIError(w, 503, 503, "manual submission storage unavailable")
		return "", false
	}
	claims, err := service.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return "", false
	}
	if !service.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission required")
		return "", false
	}
	if !service.pureGo {
		writeAPIError(w, 501, 501, "manual reconciliation requires disabled legacy backend")
		return "", false
	}
	user, err := service.auth.service.FindUser(r.Context(), claims.Username)
	if err != nil {
		writeAPIError(w, 401, 401, "administrator account unavailable")
		return "", false
	}
	return strconv.FormatInt(user.ID, 10) + ":" + user.Name, true
}
func (service downloadService) serveManualSubmissions(w http.ResponseWriter, r *http.Request) {
	if _, ok := service.authorizeManualReceipts(w, r); !ok {
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 10000 {
			writeAPIError(w, 400, 400, "invalid manual receipt page")
			return
		}
		page = n
	}
	items, err := service.downloaders.ManualSubmissionsPage(r.Context(), page)
	if err != nil {
		writeAPIError(w, 503, 503, "manual submission receipts unavailable")
		return
	}
	result := []manualReceipt{}
	for _, item := range items {
		result = append(result, publicManual(item))
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": result})
}
func (service downloadService) serveManualReconcile(w http.ResponseWriter, r *http.Request) {
	actor, ok := service.authorizeManualReceipts(w, r)
	if !ok {
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid manual reconciliation") {
		return
	}
	if !input.Confirm {
		writeAPIError(w, 400, 400, "confirm readonly downloader verification and local receipt completion; no resubmission")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	receipt, err := service.downloaders.ManualSubmission(ctx, r.PathValue("id"))
	if err != nil {
		writeAPIError(w, 409, 409, "manual submission receipt unavailable")
		return
	}
	if service.manualGuards == nil || *service.manualGuards == nil {
		writeAPIError(w, 503, 503, "manual submission execution guard unavailable")
		return
	}
	guard, err := (*service.manualGuards).AcquireJob(ctx, "manual-download:"+receipt.Definition.InfoHash)
	if err != nil {
		writeAPIError(w, 409, 409, "manual submission or verification is active; read its receipt")
		return
	}
	defer guard.Close()
	receipt, err = service.downloaders.ManualSubmission(ctx, receipt.ID)
	if err != nil {
		writeAPIError(w, 409, 409, "manual receipt changed")
		return
	}
	if receipt.State == "submitted" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": publicManual(receipt)})
		return
	}
	if receipt.State != "pending" {
		writeAPIError(w, 409, 409, "manual submission is not pending")
		return
	}
	d := receipt.Definition
	item, err := service.downloaders.Get(ctx, d.DownloaderID)
	if err != nil || item.Enabled == 0 || bus.Hash(item) != d.ConfigDigest {
		writeAPIError(w, 409, 409, "original downloader configuration changed; pending receipt retained")
		return
	}
	configuration := map[string]any{}
	if json.Unmarshal([]byte(item.Config), &configuration) != nil {
		writeAPIError(w, 409, 409, "original downloader configuration invalid")
		return
	}
	found := false
	switch item.Type {
	case "qbittorrent":
		client, e := qbittorrent.New(text(configuration["host"]), text(configuration["port"]), text(configuration["username"]), text(configuration["password"]), service.client.Transport)
		if e != nil {
			err = e
		} else {
			found, err = client.HasTorrent(ctx, d.InfoHash)
		}
	case "transmission":
		if !transmission.ValidHash(d.InfoHash) {
			writeAPIError(w, 501, 501, "hash format not supported for positive reconciliation")
			return
		}
		client, e := transmission.New(text(configuration["host"]), text(configuration["port"]), text(configuration["username"]), text(configuration["password"]), service.client.Transport)
		if e != nil {
			err = e
		} else {
			found, err = client.HasTorrent(ctx, d.InfoHash)
		}
	default:
		writeAPIError(w, 501, 501, "pending task identity reconciliation for this downloader is not migrated; no retry or release")
		return
	}
	if err != nil || !found {
		writeAPIError(w, 409, 409, "positive exact-task presence not established; pending receipt retained")
		return
	}
	if err := service.downloaders.CompleteManual(ctx, receipt, d.InfoHash, "verified_present", actor); err != nil {
		writeAPIError(w, 409, 409, "verified task receipt could not be committed; no resubmission")
		return
	}
	receipt, err = service.downloaders.ManualSubmission(ctx, receipt.ID)
	if err != nil {
		writeAPIError(w, 503, 503, "read committed receipt before further action")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": publicManual(receipt)})
}
