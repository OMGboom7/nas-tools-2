package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

type directFileProof struct {
	ExactSeason bool
	Totals      map[int]int
}

func (p directFileProof) selectFiles(files []torrentfiles.File, wanted map[int][]int) ([]torrentfiles.File, map[int][]int, error) {
	joined, err := torrentfiles.Normalize(files)
	if err != nil {
		return nil, nil, err
	}
	for i := range joined {
		joined[i].Selected = false
	}
	actual := map[int][]int{}
	for season, units := range wanted {
		plan := torrentfiles.Plan
		if p.ExactSeason {
			plan = torrentfiles.PlanExactSeason
		}
		selected, episodes, err := plan(files, season, p.Totals[season], units)
		if errors.Is(err, torrentfiles.ErrNoCoverage) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		for i, f := range selected {
			joined[i].Selected = joined[i].Selected || f.Selected
		}
		actual[season] = episodes
	}
	return joined, actual, nil
}

func (api *subscriptionSearchRunner) checkDirectFiles(ctx context.Context, tx *sql.Tx, d downloaderconfig.FileSelectionDefinition) error {
	receipt, err := downloaderconfig.DirectFileSubmissionTx(ctx, tx, d)
	if err != nil {
		return err
	}
	m := receipt.Definition.Direct
	var proof directFileProof
	if json.Unmarshal([]byte(d.Proof), &proof) != nil {
		return downloaderconfig.ErrFileSelectionPending
	}
	wanted := map[int][]int{m.Season: m.Units}
	maps.Copy(wanted, m.ExtraSeasons)
	expected, actual, err := proof.selectFiles(d.Files, wanted)
	if err != nil || !maps.EqualFunc(wanted, actual, slices.Equal[[]int]) || !torrentfiles.Wanted(expected, d.Files) {
		return downloaderconfig.ErrFileSelectionPending
	}
	runner := api.planner.runner
	authority, _, err := runner.siteLogin.authority(receipt.Definition.Actor)
	if err != nil {
		return err
	}
	config, err := nativeSubscriptionConfiguration(ctx, tx, runner.recognition.service)
	if err != nil {
		return err
	}
	return systemconfig.UpdateManyTx(ctx, tx, doubanKeys, func(raw map[string]string) (map[string]string, error) {
		c, err := doubanState(raw[doubanControlKey])
		installed, e := indexerInstalled(raw["UserInstalledPlugins"], douban.SyncPlugin)
		if err != nil || e != nil || !installed || c.Actor != receipt.Definition.Actor || c.Scope != d.Scope || sitelogin.Hash([]any{authority, raw[doubanSettingsKey], config}) != d.Scope {
			return nil, downloaderconfig.ErrFileSelectionPending
		}
		return nil, nil
	})
}

func (api *doubanSyncAPI) submitDirectFiles(ctx context.Context, c doubanControl, item downloaderconfig.Downloader, opts downloadAddOptions, title, hash string, torrent []byte, files []torrentfiles.File, m downloaderconfig.DirectMedia, proof directFileProof, current func() error) error {
	download := api.runner.download.service
	guard, err := (*api.login.guards).AcquireJob(ctx, "manual-download:"+hash)
	if err != nil {
		return downloaderconfig.ErrManualPending
	}
	defer guard.Close()
	var exists bool
	if item.Type == "qbittorrent" {
		q, err := download.qbProfileClient(item)
		if err != nil {
			return err
		}
		exists, err = q.HasTorrent(ctx, hash)
		if err != nil {
			return err
		}
	} else {
		tr, err := download.profileClient(item)
		if err != nil {
			return err
		}
		exists, err = tr.HasTorrent(ctx, hash)
		if err != nil {
			return err
		}
	}
	if exists {
		return downloaderconfig.ErrFileSelectionPending
	}
	id, err := refreshNonce()
	if err != nil {
		return err
	}
	d := downloaderconfig.FileSelectionDefinition{Version: 3, ID: id, DownloaderID: item.ID, DownloaderType: item.Type, ConfigDigest: bus.Hash(item), InfoHash: hash, Title: title, Season: m.Season, Episodes: m.Units, Scope: c.Scope, Proof: string(mustFuzzyJSON(proof)), Files: files}
	opts.Tags = append(slices.Clone(opts.Tags), d.Marker())
	d.Options = mustFuzzyJSON(opts)
	d.PresetFingerprint = downloadProfileFingerprint(item, hash, opts)
	record := downloaderconfig.ManualDefinition{Direct: &m, DownloaderID: item.ID, DownloaderName: item.Name, DownloaderType: item.Type, ConfigDigest: d.ConfigDigest, OptionsDigest: bus.Hash(opts), InfoHash: hash, Actor: c.Actor, Title: title, Source: "豆瓣同步", SavePath: opts.SavePath, ProfileDigest: d.ProfileFingerprint()}
	if err := current(); err != nil {
		return err
	}
	r, created, err := download.downloaders.ReserveDirectFiles(ctx, record, d)
	if err != nil {
		return err
	}
	if !created {
		return downloaderconfig.ErrManualPending
	}
	task, handled, err := api.runner.search.submitSubscriptionFiles(ctx, item, torrent, opts, d)
	book, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	if err != nil || !handled {
		if p, e := download.downloaders.FileSelection(book, id); e == nil && p.State == "rejected" {
			if e = download.downloaders.RejectManual(book, r, true); e == nil {
				return errDownloadOptionsUnsupported
			}
		}
		return downloaderconfig.ErrManualPending
	}
	if err = download.downloaders.CompleteManual(book, r, task, "acknowledged", c.Actor); err != nil {
		return downloaderconfig.ErrManualPending
	}
	return nil
}
