package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

// A cohort owns one remote task, not one task per season. Every member retains
// its own complete source, missing ledger and quality cycle.
type subscriptionSeasonMember struct {
	ID        int64
	Plan      subscriptionSearchPlan
	Candidate subscriptionCandidate
	Payload   string
}
type subscriptionSeasonProof struct {
	ID      int64  `json:"id,string"`
	Payload string `json:"proof"`
}
type subscriptionSeasonResult struct {
	SubscriptionID string `json:"subscriptionId"`
	Season         int    `json:"season"`
	Remaining      []int  `json:"remaining"`
	Completed      bool   `json:"completed"`
}

func (r *subscriptionSearchRunResult) recordSeasons(members []subscriptionSeasonMember) {
	for _, m := range members {
		v := subscriptionSeasonResult{strconv.FormatInt(m.ID, 10), m.Plan.season, append([]int{}, m.Plan.needed...), subscriptionPlanCompleted("TV", m.Plan, true)}
		found := false
		for i := range r.Seasons {
			if r.Seasons[i].SubscriptionID == v.SubscriptionID {
				r.Seasons[i], found = v, true
			}
		}
		if !found {
			r.Seasons = append(r.Seasons, v)
		}
	}
}

// Preserve concurrency between media, not between seasons that may share one
// torrent. This is scheduling only; durable reservation still protects manual
// and other-process races, and no old receipt is used to skip a new job.
func (api *subscriptionSearchRunner) groupSeasonJobs(ctx context.Context, jobs []scheduledSubscription) ([][]scheduledSubscription, error) {
	service := api.planner.runner.recognition.service
	db, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	groups := [][]scheduledSubscription{}
	positions := map[string]int{}
	for _, job := range jobs {
		key := job.kind + ":" + strconv.FormatInt(job.id, 10)
		if job.kind == "TV" {
			raw, err := readNativeSubscriptionRow(ctx, db, "RSS_TVS", job.id)
			if err != nil {
				return nil, err
			}
			if raw != nil {
				source := normalizeNativeSubscriptionRow(raw)
				if !truthy(source["fuzzy_match"]) && numericTMDBID(text(source["tmdbid"])) {
					key = "TV-media:" + text(source["tmdbid"])
				}
			}
		}
		if i, ok := positions[key]; ok {
			groups[i] = append(groups[i], job)
		} else {
			positions[key] = len(groups)
			groups = append(groups, []scheduledSubscription{job})
		}
	}
	return groups, nil
}

func (api *subscriptionSearchRunner) planSeasonCohort(ctx context.Context, id int64, plan subscriptionSearchPlan, candidate subscriptionCandidate, files []torrentfiles.File, provider subscriptionResourceProvider) ([]torrentfiles.File, []int, []subscriptionSeasonMember, error) {
	if len(candidate.SeasonRange) != 2 || candidate.SeasonRange[0] < 0 || candidate.SeasonRange[1]-candidate.SeasonRange[0] < 1 || candidate.SeasonRange[1]-candidate.SeasonRange[0] >= 100 {
		return nil, nil, nil, errSubscriptionSubmissionConflict
	}
	service := api.planner.runner.recognition.service
	db, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT ID FROM RSS_TVS WHERE CAST(TMDBID AS TEXT)=? ORDER BY ID LIMIT 1001`, plan.mediaID)
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	ids := []int64{id}
	for rows.Next() {
		var sourceID int64
		if err := rows.Scan(&sourceID); err != nil {
			rows.Close()
			db.Close()
			return nil, nil, nil, err
		}
		if sourceID != id {
			ids = append(ids, sourceID)
		}
	}
	err = rows.Err()
	rows.Close()
	db.Close()
	if err != nil || len(ids) > 1000 {
		return nil, nil, nil, errSubscriptionSubmissionConflict
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := append([]torrentfiles.File{}, files...)
	for i := range out {
		out[i].Selected = false
	}
	members := []subscriptionSeasonMember{}
	leadEpisodes := []int{}
	for _, sourceID := range ids {
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		p, c := plan, candidate
		p.cohort, p.files = nil, nil
		if sourceID != id {
			var raw map[string]any
			err := fuzzyReadTransaction(ctx, service, func(tx *sql.Tx) error {
				var e error
				raw, e = readFuzzySourceRow(ctx, tx, "TV", sourceID)
				return e
			})
			if err != nil {
				return nil, nil, nil, err
			}
			if raw == nil {
				return nil, nil, nil, errSubscriptionSubmissionConflict
			}
			source := normalizeNativeSubscriptionRow(raw)
			season, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(text(source["season"])), "S"))
			if truthy(source["fuzzy_match"]) || err != nil || season < candidate.SeasonRange[0] || season > candidate.SeasonRange[1] || text(raw["STATE"]) != "R" && text(raw["STATE"]) != "D" {
				continue
			}
			// Use this source's actual search/RSS provider. Passing the leader's
			// resource directly would bypass the member's chosen indexers/sites.
			var failure *recognitionFailure
			p, failure = api.planner.planResources(ctx, "TV", sourceID, "", true, provider)
			if failure != nil {
				return nil, nil, nil, errors.New("season source could not be independently verified")
			}
			found := false
			for _, selected := range p.Candidates {
				if selected.resource.DownloadURL == candidate.resource.DownloadURL && selected.Title == candidate.Title && selected.resource.Indexer == candidate.resource.Indexer {
					c, found = selected, true
					break
				}
			}
			if !found || p.LibraryComplete || len(p.needed) == 0 {
				continue
			}
		}
		selected, covered, err := torrentfiles.PlanExactSeason(files, p.season, p.total, c.Episodes)
		if errors.Is(err, torrentfiles.ErrNoCoverage) && sourceID != id {
			continue
		}
		if err != nil {
			return nil, nil, nil, err
		}
		c.Episodes = covered
		p.Candidates, p.cohort = nil, nil
		p.crossMember = true
		members = append(members, subscriptionSeasonMember{ID: sourceID, Plan: p, Candidate: c})
		if len(members) > 100 {
			return nil, nil, nil, errSubscriptionSubmissionConflict
		}
		for i := range out {
			out[i].Selected = out[i].Selected || selected[i].Selected
		}
		if sourceID == id {
			leadEpisodes = append([]int{}, covered...)
		}
	}
	if len(leadEpisodes) == 0 {
		return nil, nil, nil, torrentfiles.ErrNoCoverage
	}
	return out, leadEpisodes, members, nil
}

func (api *subscriptionSearchRunner) freezeSeasonMembers(ctx context.Context, tx *sql.Tx, members []subscriptionSeasonMember, leader int64, plan *subscriptionSearchPlan, item downloaderconfig.Downloader, options downloadAddOptions) error {
	download := api.planner.runner.download.service
	for i := range members {
		m := &members[i]
		if err := verifySubscriptionSnapshot(ctx, tx, "TV", m.ID, m.Plan); err != nil {
			return err
		}
		if err := api.planner.runner.recognition.service.checkEditionSnapshot(ctx, tx, "TV", m.ID, m.Plan); err != nil {
			return err
		}
		raw, err := readFuzzySourceRow(ctx, tx, "TV", m.ID)
		if err != nil || raw == nil {
			return errSubscriptionSubmissionConflict
		}
		m.Plan.raw, m.Plan.typedSource = raw, true
		m.Plan.fileSelectionID = plan.fileSelectionID
		if m.ID == leader {
			plan.raw = raw
		}
		// Resolved settings must be identical: a single torrent cannot honor
		// contradictory directories, clients, paused states or PT constraints.
		source := normalizeNativeSubscriptionRow(raw)
		d, o, err := download.searchDownloadSettings(ctx, m.Candidate.resource.Indexer, addResourceRequest{Setting: text(source["download_setting"]), Directory: text(source["save_path"])})
		if err != nil || bus.Hash(d) != bus.Hash(item) {
			return errSubscriptionSubmissionConflict
		}
		o, err = download.preparePTSource(ctx, d, m.Candidate.resource, o)
		if err != nil || bus.Hash(o) != bus.Hash(options) {
			return errSubscriptionSubmissionConflict
		}
	}
	return nil
}

func decodeSeasonCohort(p subscriptionSubmissionProof, plan *subscriptionSearchPlan) error {
	if p.Version != 5 {
		if len(p.Cohort) != 0 || len(p.CohortRange) != 0 {
			return errSubscriptionSubmissionConflict
		}
		return nil
	}
	if p.FileSelectionID == "" || len(p.Cohort) == 0 || len(p.Cohort) > 100 || len(p.CohortRange) != 2 || p.CohortRange[0] < 0 || p.CohortRange[1]-p.CohortRange[0] < 1 || p.CohortRange[1]-p.CohortRange[0] >= 100 {
		return errSubscriptionSubmissionConflict
	}
	leader, _ := strconv.ParseInt(text(plan.raw["ID"]), 10, 64)
	matched := false
	normalized := p
	normalized.Version, normalized.Cohort = 4, nil
	normalized.CohortRange = nil
	for i, member := range p.Cohort {
		if member.ID < 1 || i > 0 && p.Cohort[i-1].ID >= member.ID {
			return errSubscriptionSubmissionConflict
		}
		var header struct{ Version int }
		if json.Unmarshal([]byte(member.Payload), &header) != nil || header.Version != 4 {
			return errSubscriptionSubmissionConflict
		}
		leaf, saved, err := decodeSubscriptionProof(member.Payload)
		if err != nil || text(saved.raw["ID"]) != strconv.FormatInt(member.ID, 10) || leaf.MediaID != p.MediaID || leaf.InfoHash != p.InfoHash || leaf.DownloaderID != p.DownloaderID || leaf.DownloaderType != p.DownloaderType || leaf.ConfigHash != p.ConfigHash || leaf.FileSelectionID != p.FileSelectionID || leaf.ProfileDigest != p.ProfileDigest || leaf.DownloadURL != p.DownloadURL || leaf.Title != p.Title || leaf.Total < 1 || len(leaf.Episodes) == 0 || !validReconcileEpisodes(leaf.Episodes, leaf.Total) || !validReconcileEpisodes(leaf.Needed, leaf.Total) || !validReconcileEpisodes(leaf.StoredMissing, leaf.Total) {
			return errSubscriptionSubmissionConflict
		}
		if leaf.Season < p.CohortRange[0] || leaf.Season > p.CohortRange[1] {
			return errSubscriptionSubmissionConflict
		}
		for _, n := range leaf.Episodes {
			if !slices.Contains(leaf.Needed, n) {
				return errSubscriptionSubmissionConflict
			}
		}
		for _, n := range leaf.Needed {
			if saved.edition == nil && !slices.Contains(leaf.StoredMissing, n) {
				return errSubscriptionSubmissionConflict
			}
		}
		if member.ID == leader {
			if bus.Hash(normalized) != bus.Hash(leaf) {
				return errSubscriptionSubmissionConflict
			}
			matched = true
		}
		saved.crossMember = true
		candidate := subscriptionCandidate{Title: leaf.Title, Episodes: leaf.Episodes, Priority: leaf.Priority, topOrder: leaf.TopOrder, groupID: leaf.GroupID}
		candidate.resource.DownloadURL = leaf.DownloadURL
		plan.cohort = append(plan.cohort, subscriptionSeasonMember{member.ID, saved, candidate, member.Payload})
	}
	if !matched {
		return errSubscriptionSubmissionConflict
	}
	return nil
}
