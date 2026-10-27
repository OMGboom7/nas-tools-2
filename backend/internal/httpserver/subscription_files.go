package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

type selectionHookKey struct{}
type selectionHook func(context.Context) error

func applySubscriptionFileHook(ctx context.Context) error {
	if hook, ok := ctx.Value(selectionHookKey{}).(selectionHook); ok {
		return hook(ctx)
	}
	return nil
}
func selectionGuard(id int64, hash string) string {
	return "torrent-files:" + strconv.FormatInt(id, 10) + ":" + strings.ToLower(hash)
}

// Season packs and resources whose declared scope needs splitting enter this
// flow. Explicit, wholly wanted episode releases keep their original semantics.
func (api *subscriptionSearchRunner) prepareSubscriptionFiles(ctx context.Context, kind string, id int64, plan *subscriptionSearchPlan, candidate *subscriptionCandidate, item downloaderconfig.Downloader, options downloadAddOptions, magnet string, torrent []byte) ([]byte, downloadAddOptions, error) {
	if kind != "TV" || !candidate.SelectFiles {
		return torrent, options, nil
	}
	if item.Type != "qbittorrent" && item.Type != "transmission" {
		return nil, options, errDownloadOptionsUnsupported
	}
	if torrent == nil {
		var err error
		torrent, err = torrentmeta.ResolveMagnet(ctx, magnet, magnetInfoHash(magnet))
		if err != nil {
			return nil, options, err
		}
	}
	meta, files, err := torrentmeta.Manifest(torrent)
	if err != nil {
		return nil, options, err
	}
	if magnet != "" && magnetInfoHash(magnet) != meta.InfoHash {
		return nil, options, torrentfiles.ErrManifest
	}
	files, episodes, err := torrentfiles.Plan(files, plan.season, plan.total, candidate.Episodes)
	if err != nil {
		return nil, options, err
	}
	nonce, err := refreshNonce()
	if err != nil {
		return nil, options, err
	}
	d := &downloaderconfig.FileSelectionDefinition{Version: 1, ID: nonce, DownloaderID: item.ID, DownloaderType: item.Type, ConfigDigest: bus.Hash(item), InfoHash: meta.InfoHash, Title: candidate.Title, SubscriptionID: id, Season: plan.season, Episodes: episodes, Files: files}
	if len(d.ID) != 32 {
		return nil, options, torrentfiles.ErrManifest
	}
	options.Tags = append(append([]string{}, options.Tags...), d.Marker())
	d.Options = mustFuzzyJSON(options)
	download := api.planner.runner.download.service
	d.PresetFingerprint = downloadProfileFingerprint(item, meta.InfoHash, options)
	if strings.HasPrefix(d.PresetFingerprint, "invalid") {
		return nil, options, errDownloadOptionsUnsupported
	}
	// Bind original SQL types, even for a non-washing partial subscription.
	service := api.planner.runner.recognition.service
	err = fuzzyReadTransaction(ctx, service, func(tx *sql.Tx) error {
		if err := verifySubscriptionSnapshot(ctx, tx, kind, id, *plan); err != nil {
			return err
		}
		raw, err := readFuzzySourceRow(ctx, tx, kind, id)
		if err != nil || raw == nil {
			return errSubscriptionSubmissionConflict
		}
		scope, err := nativeSubscriptionConfiguration(ctx, tx, service)
		if err != nil {
			return err
		}
		plan.raw = raw
		plan.typedSource = true
		d.Scope = scope
		return nil
	})
	if err != nil {
		return nil, options, err
	}
	// No pre-existing task/profile is adopted as a newly selected download.
	existing, err := download.downloaders.FindFileSelection(ctx, item.ID, meta.InfoHash)
	if err != nil || existing != nil {
		return nil, options, downloaderconfig.ErrFileSelectionPending
	}
	exists := false
	if item.Type == "qbittorrent" {
		c, e := download.qbProfileClient(item)
		if e != nil {
			return nil, options, e
		}
		exists, err = c.HasTorrent(ctx, meta.InfoHash)
	} else {
		c, e := download.profileClient(item)
		if e != nil {
			return nil, options, e
		}
		exists, err = c.HasTorrent(ctx, meta.InfoHash)
	}
	if err != nil || exists {
		return nil, options, downloaderconfig.ErrFileSelectionPending
	}
	candidate.Episodes = append([]int{}, episodes...)
	plan.files = d
	plan.fileSelectionID = d.ID
	return torrent, options, nil
}
func selectionOptions(d downloaderconfig.FileSelectionDefinition) (downloadAddOptions, error) {
	var o downloadAddOptions
	decoder := json.NewDecoder(bytes.NewReader(d.Options))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&o) != nil || !slices.Contains(o.Tags, d.Marker()) {
		return o, downloaderconfig.ErrFileSelectionPending
	}
	return o, nil
}
func (api *subscriptionSearchRunner) checkSelectionSource(ctx context.Context, tx *sql.Tx, p downloaderconfig.FileSelection) error {
	d := p.Definition
	proof, plan, err := decodeSubscriptionProof(d.Proof)
	if err != nil || proof.FileSelectionID != d.ID || proof.ProfileDigest != d.ProfileFingerprint() || proof.DownloaderID != d.DownloaderID || proof.DownloaderType != d.DownloaderType || proof.InfoHash != d.InfoHash || proof.Season != d.Season || !slices.Equal(proof.Episodes, d.Episodes) || text(plan.raw["ID"]) != strconv.FormatInt(d.SubscriptionID, 10) {
		return downloaderconfig.ErrFileSelectionPending
	}
	expected, episodes, err := torrentfiles.Plan(d.Files, proof.Season, proof.Total, proof.Episodes)
	if err != nil || !slices.Equal(episodes, proof.Episodes) || !torrentfiles.Wanted(expected, d.Files) {
		return downloaderconfig.ErrFileSelectionPending
	}
	service := api.planner.runner.recognition.service
	var shared bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS p JOIN GO_SUBSCRIPTION_DOWNLOAD_CLAIMS s USING(RESOURCE_KEY,OWNER) JOIN GO_RSS_DOWNLOAD_CLAIMS r USING(RESOURCE_KEY) WHERE p.RESOURCE_KEY=? AND p.PAYLOAD=? AND s.KIND='TV' AND s.SUB_ID=? AND r.STATE='pending' AND r.TASK_ID=0)`, subscriptionResourceKey(proof.DownloadURL), d.Proof, d.SubscriptionID).Scan(&shared)
	if err != nil || !shared {
		return downloaderconfig.ErrFileSelectionPending
	}
	if err := verifySubscriptionSnapshot(ctx, tx, "TV", d.SubscriptionID, plan); err != nil {
		return err
	}
	if err := service.checkEditionSnapshot(ctx, tx, "TV", d.SubscriptionID, plan); err != nil {
		return err
	}
	scope, err := nativeSubscriptionConfiguration(ctx, tx, service)
	if err != nil || scope != d.Scope {
		return downloaderconfig.ErrFileSelectionPending
	}
	return nil
}
func (api *subscriptionSearchRunner) advanceSelection(ctx context.Context, p *downloaderconfig.FileSelection, to, actor, review string) error {
	service := api.planner.runner.recognition.service
	db, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := api.checkSelectionSource(ctx, tx, *p); err != nil {
		return err
	}
	if to == "ready" {
		if err := downloaderconfig.VerifyDownloadProfile(ctx, tx, p.Definition.PresetFingerprint); err != nil {
			return err
		}
	}
	if err := downloaderconfig.FileSelectionTransitionTx(ctx, tx, *p, to, actor, review); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.State = to
	return nil
}
func (s downloadService) selectionSnapshot(ctx context.Context, item downloaderconfig.Downloader, d downloaderconfig.FileSelectionDefinition) (torrentfiles.Snapshot, error) {
	var state torrentfiles.Snapshot
	var err error
	if item.ID != d.DownloaderID || item.Type != d.DownloaderType || item.Enabled == 0 || bus.Hash(item) != d.ConfigDigest {
		return state, downloaderconfig.ErrFileSelectionPending
	}
	if item.Type == "qbittorrent" {
		c, e := s.qbProfileClient(item)
		if e != nil {
			return state, e
		}
		state, err = c.Selection(ctx, d.InfoHash)
	} else {
		c, e := s.profileClient(item)
		if e != nil {
			return state, e
		}
		state, err = c.Selection(ctx, d.InfoHash)
	}
	if err != nil {
		return state, err
	}
	o, err := selectionOptions(d)
	if err != nil {
		return state, err
	}
	if state.Hash != d.InfoHash || !slices.Contains(state.Labels, d.Marker()) || !torrentfiles.Match(d.Files, state.Files) || o.SavePath != "" && strings.TrimRight(state.Directory, "/") != strings.TrimRight(o.SavePath, "/") {
		return state, downloaderconfig.ErrFileSelectionPending
	}
	for _, tag := range o.Tags {
		if !slices.Contains(state.Labels, tag) {
			return state, downloaderconfig.ErrFileSelectionPending
		}
	}
	return state, nil
}
func selectedRemoteFiles(d downloaderconfig.FileSelectionDefinition, state torrentfiles.Snapshot) []torrentfiles.File {
	wanted := map[string]bool{}
	for _, f := range d.Files {
		wanted[f.Path] = f.Selected
	}
	out := append([]torrentfiles.File{}, state.Files...)
	for i := range out {
		out[i].Selected = wanted[out[i].Path]
	}
	return out
}
func unwantedMatch(d downloaderconfig.FileSelectionDefinition, state torrentfiles.Snapshot) bool {
	desired := selectedRemoteFiles(d, state)
	for i, f := range desired {
		if !f.Selected && state.Files[i].Selected {
			return false
		}
	}
	return true
}
func (api *subscriptionSearchRunner) configureSubscriptionFiles(ctx context.Context, item downloaderconfig.Downloader, id string) error {
	download := api.planner.runner.download.service
	p, err := download.downloaders.FileSelection(ctx, id)
	if err != nil {
		return err
	}
	if p.State != "adding" && p.State != "added" {
		return downloaderconfig.ErrFileSelectionPending
	}
	state, err := download.selectionSnapshot(ctx, item, p.Definition)
	if err != nil || !state.Paused {
		return downloaderconfig.ErrFileSelectionPending
	}
	if p.State == "adding" {
		if err := api.advanceSelection(ctx, &p, "added", "native-submit", ""); err != nil {
			return err
		}
	}
	return api.writeSubscriptionFiles(ctx, item, &p, state, "native-submit", "")
}
func (api *subscriptionSearchRunner) writeSubscriptionFiles(ctx context.Context, item downloaderconfig.Downloader, p *downloaderconfig.FileSelection, state torrentfiles.Snapshot, actor, review string) error {
	if !state.Paused {
		return downloaderconfig.ErrFileSelectionPending
	}
	download := api.planner.runner.download.service
	desired := selectedRemoteFiles(p.Definition, state)
	wanted, unwanted := torrentfiles.IDs(desired, true), torrentfiles.IDs(desired, false)
	if len(wanted) == 0 {
		return downloaderconfig.ErrFileSelectionPending
	}
	if item.Type == "qbittorrent" {
		c, err := download.qbProfileClient(item)
		if err != nil {
			return err
		}
		if p.State == "added" {
			if len(unwanted) > 0 {
				if err := api.advanceSelection(ctx, p, "unwanting", actor, review); err != nil {
					return err
				}
				if err := c.SetFilePriority(ctx, p.Definition.InfoHash, unwanted, false); err != nil {
					return downloaderconfig.ErrFileSelectionPending
				}
				state, err = download.selectionSnapshot(ctx, item, p.Definition)
				if err != nil || !state.Paused || !unwantedMatch(p.Definition, state) {
					return downloaderconfig.ErrFileSelectionPending
				}
			}
			if err := api.advanceSelection(ctx, p, "unwanted", actor, review); err != nil {
				return err
			}
		}
		if p.State != "unwanted" {
			return downloaderconfig.ErrFileSelectionPending
		}
		if err := api.advanceSelection(ctx, p, "wanting", actor, review); err != nil {
			return err
		}
		if err := c.SetFilePriority(ctx, p.Definition.InfoHash, wanted, true); err != nil {
			return downloaderconfig.ErrFileSelectionPending
		}
	} else {
		if p.State != "added" {
			return downloaderconfig.ErrFileSelectionPending
		}
		c, err := download.profileClient(item)
		if err != nil {
			return err
		}
		if err := api.advanceSelection(ctx, p, "wanting", actor, review); err != nil {
			return err
		}
		if err := c.SelectFiles(ctx, p.Definition.InfoHash, wanted, unwanted); err != nil {
			return downloaderconfig.ErrFileSelectionPending
		}
	}
	state, err := download.selectionSnapshot(ctx, item, p.Definition)
	if err != nil || !state.Paused || !torrentfiles.Wanted(p.Definition.Files, state.Files) {
		return downloaderconfig.ErrFileSelectionPending
	}
	return api.advanceSelection(ctx, p, "selected", actor, review)
}

// The normal preset engines invoke the hook after their own settings are
// verified and before their first start intent. Other adds stay stopped here.
func (api *subscriptionSearchRunner) submitSubscriptionFiles(ctx context.Context, item downloaderconfig.Downloader, torrent []byte, o downloadAddOptions, d downloaderconfig.FileSelectionDefinition) (string, bool, error) {
	download := api.planner.runner.download.service
	if download.manualGuards == nil || *download.manualGuards == nil {
		return "", true, downloaderconfig.ErrFileSelectionPending
	}
	ctx, close, err := download.seedingGuard(ctx, item.ID, d.InfoHash)
	if err != nil {
		return "", true, downloaderconfig.ErrFileSelectionPending
	}
	defer close()
	guard, err := (*download.manualGuards).AcquireJob(ctx, selectionGuard(item.ID, d.InfoHash))
	if err != nil {
		return "", true, downloaderconfig.ErrFileSelectionPending
	}
	defer guard.Close()
	p, err := download.downloaders.FileSelection(ctx, d.ID)
	if err != nil || p.State != "adding" || p.Committed {
		return "", true, downloaderconfig.ErrFileSelectionPending
	}
	if err := download.validatePTAdmission(ctx, item, o); err != nil {
		return "", true, downloaderconfig.ErrFileSelectionPending
	}
	hook := selectionHook(func(call context.Context) error {
		if err := api.configureSubscriptionFiles(call, item, d.ID); err != nil {
			return err
		}
		p, err := download.downloaders.FileSelection(call, d.ID)
		if err != nil {
			return err
		}
		if !o.Paused {
			return api.advanceSelection(call, &p, "starting", "native-submit", "")
		}
		return nil
	})
	var id string
	handled := true
	if d.PresetFingerprint != "" {
		id, handled, err = download.nativeAddConfiguredDownload(context.WithValue(ctx, selectionHookKey{}, hook), item, "", torrent, o)
	} else {
		if err = api.selectionSourceRead(ctx, p); err != nil {
			return "", true, downloaderconfig.ErrFileSelectionPending
		}
		if item.Type == "qbittorrent" {
			c, e := download.qbProfileClient(item)
			if e != nil {
				return "", true, e
			}
			err = c.AddTorrentWithOptions(ctx, torrent, qbittorrent.AddOptions{SavePath: o.SavePath, Category: o.Category, Tags: o.Tags, Paused: true, SelectedFiles: true, UploadLimitKB: o.UploadLimitKB, DownloadLimitKB: o.DownloadLimitKB, RatioLimit: o.RatioLimit, SeedingTimeLimit: o.SeedingTimeLimit})
			id = d.InfoHash
		} else {
			c, e := download.profileClient(item)
			if e != nil {
				return "", true, e
			}
			id, err = c.AddTorrentWithOptions(ctx, torrent, transmissionSelectionOptions(o))
		}
		if err == nil && strings.EqualFold(id, d.InfoHash) {
			err = hook(ctx)
			if err == nil && !o.Paused {
				err = download.startSelectedFiles(ctx, item, d.InfoHash)
			}
		}
	}
	if err != nil || !handled || !strings.EqualFold(id, d.InfoHash) {
		if p, readErr := download.downloaders.FileSelection(ctx, d.ID); readErr == nil && p.State == "adding" && downloadDefinitelyNotSubmitted(handled, err) {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if api.advanceSelection(cleanup, &p, "rejected", "native-submit", "") == nil {
				return "", true, errDownloadOptionsUnsupported
			}
		}
		return "", true, downloaderconfig.ErrFileSelectionPending
	}
	p, err = download.downloaders.FileSelection(ctx, d.ID)
	if err != nil {
		return "", true, downloaderconfig.ErrFileSelectionPending
	}
	state, err := download.selectionSnapshot(ctx, item, p.Definition)
	if err != nil || state.Paused != o.Paused || !torrentfiles.Wanted(d.Files, state.Files) {
		return "", true, downloaderconfig.ErrFileSelectionPending
	}
	if d.PresetFingerprint != "" && download.verifyDownloadProfile(ctx, item, d.InfoHash, d.PresetFingerprint) != nil {
		return "", true, downloaderconfig.ErrFileSelectionPending
	}
	if err := api.advanceSelection(ctx, &p, "ready", "native-submit", ""); err != nil {
		return "", true, downloaderconfig.ErrFileSelectionPending
	}
	return id, true, nil
}
func (api *subscriptionSearchRunner) selectionSourceRead(ctx context.Context, p downloaderconfig.FileSelection) error {
	return fuzzyReadTransaction(ctx, api.planner.runner.recognition.service, func(tx *sql.Tx) error { return api.checkSelectionSource(ctx, tx, p) })
}
func (s downloadService) startSelectedFiles(ctx context.Context, item downloaderconfig.Downloader, hash string) error {
	if item.Type == "qbittorrent" {
		c, err := s.qbProfileClient(item)
		if err != nil {
			return err
		}
		return c.StartSelected(ctx, hash)
	}
	c, err := s.profileClient(item)
	if err != nil {
		return err
	}
	return c.Control(ctx, hash, "start")
}
func (s downloadService) verifySelectedFiles(ctx context.Context, item downloaderconfig.Downloader, hash, id string) error {
	p, err := s.downloaders.FileSelection(ctx, id)
	if err != nil || p.State != "ready" || p.Definition.InfoHash != hash {
		return downloaderconfig.ErrFileSelectionPending
	}
	state, err := s.selectionSnapshot(ctx, item, p.Definition)
	if err != nil || !torrentfiles.Wanted(p.Definition.Files, state.Files) {
		return downloaderconfig.ErrFileSelectionPending
	}
	o, err := selectionOptions(p.Definition)
	if err != nil || state.Paused != o.Paused {
		return downloaderconfig.ErrFileSelectionPending
	}
	if p.Definition.PresetFingerprint != "" {
		return s.verifyDownloadProfile(ctx, item, hash, p.Definition.PresetFingerprint)
	}
	return nil
}
func (s downloadService) guardSelectedStart(ctx context.Context, item downloaderconfig.Downloader, hash string) error {
	p, err := s.downloaders.FindFileSelection(ctx, item.ID, hash)
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	if p.State != "ready" || p.Definition.ConfigDigest != bus.Hash(item) {
		return downloaderconfig.ErrFileSelectionPending
	}
	state, err := s.selectionSnapshot(ctx, item, p.Definition)
	if err != nil || !torrentfiles.Wanted(p.Definition.Files, state.Files) {
		return downloaderconfig.ErrFileSelectionPending
	}
	return nil
}
func (s downloadService) guardSelectionPresetRecovery(ctx context.Context, itemID int64, hash string) error {
	p, err := s.downloaders.FindFileSelection(ctx, itemID, hash)
	if err != nil {
		return err
	}
	if p != nil && p.State != "ready" {
		return downloaderconfig.ErrFileSelectionPending
	}
	return nil
}

// Exact same configured-client constructors are used for both RPC dialects.
func transmissionSelectionOptions(o downloadAddOptions) transmission.AddOptions {
	return transmission.AddOptions{DownloadDir: o.SavePath, Paused: true, Labels: o.Tags}
}
