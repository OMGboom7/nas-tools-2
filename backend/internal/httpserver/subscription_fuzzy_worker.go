package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

func normalizeFuzzySite(site siteconfig.Site) (siteconfig.Site, error) {
	note, _, _, err := subscriptionRSSSiteNote(site)
	if err != nil {
		return site, err
	}
	b, err := json.Marshal(note)
	if err != nil {
		return site, err
	}
	site.Note = string(b)
	return site, nil
}
func (api *fuzzySubscriptionAPI) originalSite(ctx context.Context, c fuzzyCandidateRecord) (*siteconfig.Site, error) {
	site, err := api.service().sites.Get(ctx, c.SiteID)
	if err != nil {
		return nil, err
	}
	site, err = normalizeFuzzySite(site)
	if err != nil {
		return nil, err
	}
	if fuzzyDigest(site) != c.SiteDigest || site.Name != c.View.Site {
		return nil, errFuzzyScope
	}
	return &site, nil
}
func (api *fuzzySubscriptionAPI) active(ctx context.Context, j fuzzyBatch) error {
	return fuzzyReadTransaction(ctx, api.service(), func(tx *sql.Tx) error {
		current, err := readFuzzyBatch(ctx, tx, j.ID)
		if err != nil {
			return err
		}
		if current.State != "running" || current.authorization != j.authorization {
			return errFuzzyScope
		}
		return api.current(ctx, tx, j.authorization)
	})
}
func (api *fuzzySubscriptionAPI) run(ctx context.Context, id string) error {
	if api.records == nil {
		return errFuzzyScope
	}
	guard, err := api.records.AcquireJob(ctx, "native-fuzzy-"+id)
	if err != nil {
		return err
	}
	defer guard.Close()
	j, err := api.start(ctx, id)
	if err != nil {
		current, readErr := api.get(ctx, id)
		if readErr == nil && current.State != "pending" {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		return api.finishState(ctx, id, "blocked", "original fuzzy source, administrator or configuration changed before execution; no submission sent")
	}
	call, closeWatch := recoveryWatch(ctx, func(c context.Context) error { return api.active(c, j) })
	defer closeWatch()
	save := func(action func(context.Context) error) error {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return action(c)
	}
	block := func(reason string) error {
		return save(func(c context.Context) error { return api.finishState(c, id, "blocked", reason) })
	}
	for index, item := range j.Items {
		if item.State == "accepted" {
			continue
		}
		if item.State != "pending" {
			return block("original item has an unresolved intent; no automatic continuation")
		}
		if err := api.active(call, j); err != nil {
			return block("original authority changed or this batch was stopped; prior receipts retained")
		}
		c := j.definition.Candidates[index]
		site, err := api.originalSite(call, c)
		if err != nil {
			return block("original RSS site configuration changed; no replacement endpoint followed")
		}
		source := normalizeNativeSubscriptionRow(j.definition.Raw)
		download := api.runner.planner.runner.download.service
		downloader, options, err := download.searchDownloadSettings(call, c.View.Site, addResourceRequest{Setting: text(source["download_setting"]), Directory: text(source["save_path"])})
		if err != nil {
			return block("original downloader settings could not be established; no submission sent")
		}
		options, err = download.preparePTSource(call, downloader, c.Resource, options)
		if err != nil {
			return block("PT source still requires its independent supported downloader grant and evidence; no submission sent")
		}
		candidate := subscriptionCandidate{Title: c.View.Title, resource: c.Resource, site: site}
		magnet, torrent, err := download.prepareSubscriptionTorrent(call, candidate)
		if err != nil {
			return block("original torrent could not be verified; no submission sent")
		}
		proof := newSubscriptionSubmissionProof(downloader, magnet, torrent)
		proof.ProfileDigest = downloadProfileFingerprint(downloader, proof.InfoHash, options)
		if err := api.active(call, j); err != nil {
			return block("authority changed while preparing the original torrent; no submission sent")
		}
		if err := api.intent(call, id, index, proof, downloader, options); err != nil {
			return block("exact item intent and shared submission claim could not be committed; no submission sent")
		}
		_, handled, err := download.nativeAddConfiguredDownload(call, downloader, magnet, torrent, options)
		if err != nil || !handled {
			if downloadDefinitelyNotSubmitted(handled, err) {
				return save(func(c context.Context) error { return api.rejected(c, id, index) })
			}
			return save(func(c context.Context) error { return api.result(c, id, index, false, false) })
		}
		if err := save(func(c context.Context) error { return api.result(c, id, index, true, false) }); err != nil {
			return err
		}
	}
	if err := api.finalize(call, id); err != nil {
		return block("accepted submission facts retained; original source completion needs a new explicit receipt review")
	}
	return nil
}
func (api *fuzzySubscriptionAPI) verify(ctx context.Context, id string, index int) error {
	if api.records == nil {
		return errFuzzyScope
	}
	guard, err := api.records.AcquireJob(ctx, "native-fuzzy-"+id)
	if err != nil {
		return err
	}
	defer guard.Close()
	j, err := api.get(ctx, id)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(j.Items) {
		return errFuzzyScope
	}
	v := j.Items[index]
	if v.State == "accepted" {
		return nil
	}
	if v.State != "intent" && v.State != "uncertain" {
		return errFuzzyScope
	}
	proof, err := decodeFuzzyItemProof(v.proof, j, index)
	if err != nil {
		return err
	}
	p := proof.Native
	download := api.runner.planner.runner.download.service
	item, _, handled, err := download.configuredDownloader(ctx, strconv.FormatInt(p.DownloaderID, 10))
	if err != nil || !handled || item.Enabled == 0 || item.Type != p.DownloaderType || subscriptionResourceKey(item.Config) != p.ConfigHash {
		return errFuzzyScope
	}
	configuration := map[string]any{}
	if json.Unmarshal([]byte(item.Config), &configuration) != nil {
		return errFuzzyScope
	}
	if p.ProfileDigest != "" {
		if err := download.verifyDownloadProfile(ctx, item, p.InfoHash, p.ProfileDigest); err != nil {
			return err
		}
	}
	found := false
	switch item.Type {
	case "qbittorrent":
		if !qbittorrent.ValidHash(p.InfoHash) {
			return errFuzzyScope
		}
		client, e := qbittorrent.New(text(configuration["host"]), text(configuration["port"]), text(configuration["username"]), text(configuration["password"]), download.client.Transport)
		if e != nil {
			return e
		}
		found, err = client.HasTorrent(ctx, p.InfoHash)
	case "transmission":
		if !transmission.ValidHash(p.InfoHash) {
			return errFuzzyScope
		}
		client, e := transmission.New(text(configuration["host"]), text(configuration["port"]), text(configuration["username"]), text(configuration["password"]), download.client.Transport)
		if e != nil {
			return e
		}
		found, err = client.HasTorrent(ctx, p.InfoHash)
	default:
		return errFuzzyScope
	}
	if err != nil {
		return err
	}
	if !found {
		return errFuzzyScope
	}
	// No "not found means safe to resend". Positive adoption checks the
	// original frozen downloader again in the same receipt transaction.
	return api.result(ctx, id, index, true, true)
}
func (api *fuzzySubscriptionAPI) tick(ctx context.Context) error {
	if api == nil || api.records == nil || !api.runner.planner.runner.pureGo {
		return nil
	}
	jobs, err := api.list(ctx, "", 0, true)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := api.run(ctx, j.ID); err != nil && !errors.Is(err, organization.ErrBusy) {
			return err
		}
	}
	return nil
}
func startFuzzySubscriptionWorker(ctx context.Context, api *fuzzySubscriptionAPI) func() {
	if api == nil || api.records == nil || api.service().databasePath == "" || !api.runner.planner.runner.pureGo {
		return func() {}
	}
	api.workers.Add(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer api.workers.Add(-1)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				call, cancel := context.WithTimeout(ctx, 6*time.Minute)
				err := api.tick(call)
				cancel()
				if err != nil && ctx.Err() == nil && !errors.Is(err, organization.ErrBusy) {
					slog.Warn("native fuzzy subscription dispatch retained unresolved receipts; explicit review required")
				}
			}
		}
	}()
	return func() { <-done }
}
