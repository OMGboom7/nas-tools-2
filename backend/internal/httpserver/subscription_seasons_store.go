package httpserver

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

func seasonCohortSchema(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS GO_SUBSCRIPTION_SEASON_MEMBERS(RESOURCE_KEY TEXT NOT NULL,OWNER TEXT NOT NULL,SUB_ID INTEGER NOT NULL,MEDIA_ID TEXT NOT NULL,SEASON INTEGER NOT NULL,EPISODES TEXT NOT NULL,PAYLOAD TEXT NOT NULL,STATE TEXT NOT NULL,PRIMARY KEY(RESOURCE_KEY,OWNER,SUB_ID))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS GO_SUBSCRIPTION_SEASON_PENDING ON GO_SUBSCRIPTION_SEASON_MEMBERS(SUB_ID) WHERE STATE='pending'`,
		`CREATE INDEX IF NOT EXISTS GO_SUBSCRIPTION_SEASON_MEDIA ON GO_SUBSCRIPTION_SEASON_MEMBERS(MEDIA_ID,SEASON,STATE)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func pendingSeasonCohort(ctx context.Context, r subscriptionRowReader, kind string, id int64, media string, season int) (bool, error) {
	if kind != "TV" {
		return false, nil
	}
	var yes bool
	err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_SUBSCRIPTION_SEASON_MEMBERS')`).Scan(&yes)
	if err != nil || !yes {
		return false, err
	}
	var pending bool
	err = r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_SEASON_MEMBERS WHERE STATE='pending' AND (SUB_ID=? OR (?<>'' AND MEDIA_ID=? AND SEASON=?)))`, id, media, media, season).Scan(&pending)
	return pending, err
}

func (service subscriptionService) prepareSeasonReservation(ctx context.Context, tx *sql.Tx, plan subscriptionSearchPlan) error {
	if len(plan.cohort) == 0 {
		return nil
	}
	if len(plan.cohort) > 100 || plan.files == nil || plan.files.Version != 2 || len(plan.files.Sources) != len(plan.cohort) {
		return errSubscriptionSubmissionConflict
	}
	if err := seasonCohortSchema(ctx, tx); err != nil {
		return err
	}
	for _, m := range plan.cohort {
		if m.ID < 1 || m.Plan.mediaID != plan.mediaID || m.Plan.fileSelectionID != plan.fileSelectionID || len(m.Candidate.Episodes) == 0 {
			return errSubscriptionSubmissionConflict
		}
		if err := verifySubscriptionSnapshot(ctx, tx, "TV", m.ID, m.Plan); err != nil {
			return err
		}
		if err := service.checkEditionSnapshot(ctx, tx, "TV", m.ID, m.Plan); err != nil {
			return err
		}
		pending, err := pendingSubscriptionSubmission(ctx, tx, "TV", m.ID, m.Plan)
		if err != nil || pending {
			return errSubscriptionSubmissionConflict
		}
		if err := reserveEditionCycle(ctx, tx, "TV", m.ID, m.Plan, m.Candidate); err != nil {
			return err
		}
	}
	return nil
}

// A member's old reconcile entry points at the same original leader; it cannot
// invent another root claim or add a second copy of the torrent.
func pendingSeasonLeader(ctx context.Context, r fuzzyReader, id int64) (int64, bool, error) {
	yes, err := fuzzyTable(ctx, r, "GO_SUBSCRIPTION_SEASON_MEMBERS")
	if err != nil || !yes {
		return 0, false, err
	}
	var count int
	var leader int64
	err = r.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(s.SUB_ID),0) FROM GO_SUBSCRIPTION_SEASON_MEMBERS m JOIN GO_SUBSCRIPTION_DOWNLOAD_CLAIMS s USING(RESOURCE_KEY,OWNER) JOIN GO_RSS_DOWNLOAD_CLAIMS r USING(RESOURCE_KEY) WHERE m.SUB_ID=? AND m.STATE='pending' AND s.KIND='TV' AND r.STATE='pending' AND r.TASK_ID=0`, id).Scan(&count, &leader)
	if err != nil || count > 1 {
		return 0, false, errSubscriptionSubmissionConflict
	}
	return leader, count == 1 && leader > 0, nil
}
func hasSeasonProvenance(ctx context.Context, r fuzzyReader, id int64, media string, season int) (bool, error) {
	yes, err := fuzzyTable(ctx, r, "GO_SUBSCRIPTION_SEASON_MEMBERS")
	if err != nil {
		return false, err
	}
	var found bool
	if yes {
		err = r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_SEASON_MEMBERS WHERE SUB_ID=? AND MEDIA_ID=? AND SEASON=? AND STATE='submitted')`, id, media, season).Scan(&found)
		if err != nil || found {
			return found, err
		}
	}
	yes, err = fuzzyTable(ctx, r, "GO_TORRENT_FILE_SELECTIONS")
	if err != nil || !yes {
		return false, err
	}
	err = r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_TORRENT_FILE_SELECTIONS WHERE STATE='ready' AND COMMITTED=1 AND json_valid(DOCUMENT) AND json_extract(DOCUMENT,'$.subscriptionId')=? AND json_extract(DOCUMENT,'$.season')=? AND json_valid(json_extract(DOCUMENT,'$.proof')) AND json_extract(json_extract(DOCUMENT,'$.proof'),'$.MediaID')=?)`, id, season, media).Scan(&found)
	return found, err
}

// This is backup provenance, not permission to reuse a prior task. A later
// ordinary episode may finish a source first partially served by a file plan.
func latestSourceSelection(ctx context.Context, tx *sql.Tx, id int64, media string, season int) (string, error) {
	cohorts, err := fuzzyTable(ctx, tx, "GO_SUBSCRIPTION_SEASON_MEMBERS")
	if err != nil {
		return "", err
	}
	if cohorts {
		var payload string
		err := tx.QueryRowContext(ctx, `SELECT p.PAYLOAD FROM GO_SUBSCRIPTION_SEASON_MEMBERS m JOIN GO_SUBSCRIPTION_SUBMISSION_PROOFS p USING(RESOURCE_KEY,OWNER) WHERE m.SUB_ID=? AND m.MEDIA_ID=? AND m.SEASON=? AND m.STATE='submitted' ORDER BY m.rowid DESC LIMIT 1`, id, media, season).Scan(&payload)
		if err == nil {
			proof, plan, err := decodeSubscriptionProof(payload)
			if err != nil || proof.Version != 5 {
				return "", errFuzzyScope
			}
			for _, m := range plan.cohort {
				if m.ID == id && m.Plan.mediaID == media && m.Plan.season == season {
					return proof.FileSelectionID, nil
				}
			}
			return "", errFuzzyScope
		}
		if err != sql.ErrNoRows {
			return "", err
		}
	}
	exists, err := fuzzyTable(ctx, tx, "GO_TORRENT_FILE_SELECTIONS")
	if err != nil || !exists {
		return "", errFuzzyScope
	}
	var selectionID string
	err = tx.QueryRowContext(ctx, `SELECT ID FROM GO_TORRENT_FILE_SELECTIONS WHERE STATE='ready' AND COMMITTED=1 AND json_valid(DOCUMENT) AND json_extract(DOCUMENT,'$.subscriptionId')=? AND json_extract(DOCUMENT,'$.season')=? AND json_valid(json_extract(DOCUMENT,'$.proof')) AND json_extract(json_extract(DOCUMENT,'$.proof'),'$.MediaID')=? ORDER BY UPDATED DESC,ID LIMIT 1`, id, season, media).Scan(&selectionID)
	if err != nil {
		return "", err
	}
	p, err := downloaderconfig.FileSelectionTx(ctx, tx, selectionID)
	if err != nil {
		return "", err
	}
	proof, _, err := decodeSubscriptionProof(p.Definition.Proof)
	if err != nil || proof.MediaID != media || proof.Season != season || p.Definition.SubscriptionID != id {
		return "", errFuzzyScope
	}
	return selectionID, nil
}

func reserveSeasonMembers(ctx context.Context, tx *sql.Tx, payload, owner string) error {
	proof, plan, err := decodeSubscriptionProof(payload)
	if err != nil || proof.Version != 5 {
		return errSubscriptionSubmissionConflict
	}
	key := subscriptionResourceKey(proof.DownloadURL)
	for _, m := range plan.cohort {
		if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_SUBSCRIPTION_SEASON_MEMBERS(RESOURCE_KEY,OWNER,SUB_ID,MEDIA_ID,SEASON,EPISODES,PAYLOAD,STATE) VALUES(?,?,?,?,?,?,?,'pending')`, key, owner, m.ID, m.Plan.mediaID, m.Plan.season, subscriptionEpisodeString(m.Candidate.Episodes), m.Payload)); err != nil {
			return err
		}
		if m.Plan.edition != nil {
			if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_SUBSCRIPTION_EDITION_ATTEMPTS(CYCLE_ID,OWNER,RESOURCE_KEY,PAYLOAD,STATE) VALUES(?,?,?,?,'intent')`, m.Plan.edition.Cycle.ID, owner, key, m.Payload)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (api *subscriptionSearchRunner) checkSeasonSelection(ctx context.Context, tx *sql.Tx, selection downloaderconfig.FileSelection, proof subscriptionSubmissionProof, plan subscriptionSearchPlan) error {
	return api.planner.runner.recognition.service.checkSeasonSelectionTx(ctx, tx, selection, proof, plan, "")
}

// Recompute the union from every exact original member. Neither a missing
// member nor an extra selected file can be adopted as a positive receipt.
func (service subscriptionService) checkSeasonSelectionTx(ctx context.Context, tx *sql.Tx, p downloaderconfig.FileSelection, proof subscriptionSubmissionProof, plan subscriptionSearchPlan, expectedOwner string) error {
	d := p.Definition
	if proof.Version != 5 || d.Version != 2 || len(plan.cohort) == 0 || len(d.Sources) != len(plan.cohort) || proof.FileSelectionID != d.ID || proof.ProfileDigest != d.ProfileFingerprint() || proof.InfoHash != d.InfoHash || proof.DownloaderID != d.DownloaderID || proof.DownloaderType != d.DownloaderType || proof.Season != d.Season || !slices.Equal(proof.Episodes, d.Episodes) || text(plan.raw["ID"]) != strconv.FormatInt(d.SubscriptionID, 10) {
		return downloaderconfig.ErrFileSelectionPending
	}
	key := subscriptionResourceKey(proof.DownloadURL)
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT s.OWNER FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS s JOIN GO_SUBSCRIPTION_SUBMISSION_PROOFS p USING(RESOURCE_KEY,OWNER) JOIN GO_RSS_DOWNLOAD_CLAIMS r USING(RESOURCE_KEY) WHERE s.RESOURCE_KEY=? AND s.KIND='TV' AND s.SUB_ID=? AND s.MEDIA_ID=? AND s.SEASON=? AND s.EPISODES=? AND p.PAYLOAD=? AND r.STATE='pending' AND r.TASK_ID=0`, key, d.SubscriptionID, proof.MediaID, proof.Season, subscriptionEpisodeString(proof.Episodes), d.Proof).Scan(&owner)
	if err != nil || expectedOwner != "" && owner != expectedOwner {
		return downloaderconfig.ErrFileSelectionPending
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_SUBSCRIPTION_SEASON_MEMBERS WHERE RESOURCE_KEY=? AND OWNER=?`, key, owner).Scan(&count); err != nil || count != len(plan.cohort) {
		return downloaderconfig.ErrFileSelectionPending
	}
	union := append([]torrentfiles.File{}, d.Files...)
	for i := range union {
		union[i].Selected = false
	}
	for i, m := range plan.cohort {
		source := d.Sources[i]
		if source.SubscriptionID != m.ID || source.Season != m.Plan.season || source.Total != m.Plan.total || !slices.Equal(source.Episodes, m.Candidate.Episodes) {
			return downloaderconfig.ErrFileSelectionPending
		}
		var found bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_SEASON_MEMBERS WHERE RESOURCE_KEY=? AND OWNER=? AND SUB_ID=? AND MEDIA_ID=? AND SEASON=? AND EPISODES=? AND PAYLOAD=? AND STATE='pending')`, key, owner, m.ID, m.Plan.mediaID, m.Plan.season, subscriptionEpisodeString(m.Candidate.Episodes), m.Payload).Scan(&found)
		if err != nil || !found {
			return downloaderconfig.ErrFileSelectionPending
		}
		selected, covered, err := torrentfiles.PlanExactSeason(d.Files, m.Plan.season, m.Plan.total, m.Candidate.Episodes)
		if err != nil || !slices.Equal(covered, m.Candidate.Episodes) {
			return downloaderconfig.ErrFileSelectionPending
		}
		for j := range union {
			union[j].Selected = union[j].Selected || selected[j].Selected
		}
		if err := verifySubscriptionSnapshot(ctx, tx, "TV", m.ID, m.Plan); err != nil {
			return err
		}
		if err := service.checkEditionSnapshot(ctx, tx, "TV", m.ID, m.Plan); err != nil {
			return err
		}
	}
	if !torrentfiles.Wanted(union, d.Files) {
		return downloaderconfig.ErrFileSelectionPending
	}
	scope, err := nativeSubscriptionConfiguration(ctx, tx, service)
	if err != nil || scope != d.Scope {
		return downloaderconfig.ErrFileSelectionPending
	}
	return nil
}

func rejectSeasonMembers(ctx context.Context, tx *sql.Tx, payload, owner string) error {
	proof, plan, err := decodeSubscriptionProof(payload)
	if err != nil || proof.Version != 5 {
		return errSubscriptionSubmissionConflict
	}
	key := subscriptionResourceKey(proof.DownloadURL)
	for _, m := range plan.cohort {
		if m.Plan.edition != nil {
			if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_SUBSCRIPTION_EDITION_ATTEMPTS SET STATE='rejected' WHERE CYCLE_ID=? AND OWNER=? AND RESOURCE_KEY=? AND PAYLOAD=? AND STATE='intent'`, m.Plan.edition.Cycle.ID, owner, key, m.Payload)); err != nil {
				return err
			}
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_SUBSCRIPTION_SEASON_MEMBERS SET STATE='rejected' WHERE RESOURCE_KEY=? AND OWNER=? AND SUB_ID=? AND PAYLOAD=? AND STATE='pending'`, key, owner, m.ID, m.Payload)); err != nil {
			return err
		}
	}
	return nil
}

func (service subscriptionService) persistSeasonSubmission(ctx context.Context, id int64, plan *subscriptionSearchPlan, candidate *subscriptionCandidate, owner string, guards ...subscriptionReconcileGuard) error {
	if candidate == nil || len(guards) > 1 || plan.files != nil && plan.files.SubscriptionID != id {
		return errSubscriptionSubmissionConflict
	}
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
	key := subscriptionResourceKey(candidate.resource.DownloadURL)
	var payload string
	if err := tx.QueryRowContext(ctx, `SELECT PAYLOAD FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS WHERE RESOURCE_KEY=? AND OWNER=?`, key, owner).Scan(&payload); err != nil {
		return err
	}
	proof, saved, err := decodeSubscriptionProof(payload)
	if err != nil || proof.Version != 5 || text(saved.raw["ID"]) != strconv.FormatInt(id, 10) || !slices.Equal(proof.Episodes, candidate.Episodes) || len(guards) == 1 && guards[0].payload != payload {
		return errSubscriptionSubmissionConflict
	}
	selection, err := downloaderconfig.FileSelectionTx(ctx, tx, proof.FileSelectionID)
	if err != nil || selection.Definition.Proof != payload || selection.State != "ready" || selection.Committed {
		return errSubscriptionSubmissionConflict
	}
	if err := service.checkSeasonSelectionTx(ctx, tx, selection, proof, saved, owner); err != nil {
		return err
	}
	if err := downloaderconfig.CommitFileSelectionTx(ctx, tx, proof.FileSelectionID, payload); err != nil {
		return err
	}
	if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_RSS_DOWNLOAD_CLAIMS SET STATE='submitted',UPDATED_AT=? WHERE RESOURCE_KEY=? AND TASK_ID=0 AND STATE='pending'`, time.Now().Format(time.RFC3339), key)); err != nil {
		return err
	}
	coverage := []string{}
	seasons := []int{}
	outcomes := make([]subscriptionSourceOutcome, len(saved.cohort))
	for i := range saved.cohort {
		m := &saved.cohort[i]
		if err := verifySubscriptionSnapshot(ctx, tx, "TV", m.ID, m.Plan); err != nil {
			return err
		}
		if err := service.checkEditionSnapshot(ctx, tx, "TV", m.ID, m.Plan); err != nil {
			return err
		}
		if m.Plan.edition != nil {
			if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_SUBSCRIPTION_EDITION_ATTEMPTS SET STATE='accepted' WHERE CYCLE_ID=? AND OWNER=? AND RESOURCE_KEY=? AND PAYLOAD=? AND STATE='intent'`, m.Plan.edition.Cycle.ID, owner, key, m.Payload)); err != nil {
				return err
			}
		}
		remaining := slices.DeleteFunc(append([]int{}, m.Plan.needed...), func(n int) bool { return slices.Contains(m.Candidate.Episodes, n) })
		outcome, err := service.applySubscriptionSourceTx(ctx, tx, "TV", m.ID, &m.Plan, &m.Candidate, remaining)
		if err != nil {
			return err
		}
		outcomes[i] = outcome
		if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_SUBSCRIPTION_SEASON_MEMBERS SET STATE='submitted' WHERE RESOURCE_KEY=? AND OWNER=? AND SUB_ID=? AND PAYLOAD=? AND STATE='pending'`, key, owner, m.ID, m.Payload)); err != nil {
			return err
		}
		m.Plan.needed = remaining
		if outcome.edition != nil {
			m.Plan.edition = outcome.edition
			m.Plan.Upgrade = editionView(outcome.edition)
		} else {
			m.Plan.storedMissing = append([]int{}, remaining...)
		}
		if !outcome.complete {
			raw, err := readFuzzySourceRow(ctx, tx, "TV", m.ID)
			if err != nil || raw == nil {
				return errSubscriptionSubmissionConflict
			}
			m.Plan.raw = raw
		}
		coverage = append(coverage, "S"+strconv.Itoa(m.Plan.season)+":"+subscriptionEpisodeString(m.Candidate.Episodes))
		if !slices.Contains(seasons, m.Plan.season) {
			seasons = append(seasons, m.Plan.season)
		}
	}
	slices.Sort(seasons)
	seasonNames := []string{}
	for _, season := range seasons {
		seasonNames = append(seasonNames, fmt.Sprintf("S%02d", season))
	}
	values := []any{proof.Title, proof.DownloadURL, "TV", saved.raw["NAME"], saved.raw["YEAR"], strings.Join(seasonNames, ","), strings.Join(coverage, ";")}
	for i, value := range values {
		values[i], err = fuzzySQLValue(value)
		if err != nil {
			return err
		}
	}
	torrentRow, err := tx.ExecContext(ctx, `INSERT INTO RSS_TORRENTS(TORRENT_NAME,ENCLOSURE,TYPE,TITLE,YEAR,SEASON,EPISODE) VALUES(?,?,?,?,?,?,?)`, values...)
	if err := fuzzyOne(torrentRow, err); err != nil {
		return err
	}
	torrentID, err := torrentRow.LastInsertId()
	if err != nil || torrentID < 1 {
		return errSubscriptionSubmissionConflict
	}
	if err := bus.EmitData(ctx, tx, "download_start", key+":"+owner, "subscription-seasons:"+proof.FileSelectionID, text(saved.raw["NAME"])+" 跨季资源开始下载", "一个原下载任务；实际选集："+strings.Join(coverage, "；")+"。接收不代表下载或入库完成。", map[string]any{
		"media_info": subscriptionEventData(saved.raw, "TV", 0, false)["media_info"], "torrent_file": proof.DownloadURL, "native_download": map[string]any{"resource_title": proof.Title, "file_selection_id": proof.FileSelectionID, "coverage": coverage},
	}); err != nil {
		return err
	}
	scope, err := nativeSubscriptionConfiguration(ctx, tx, service)
	if err != nil || scope != selection.Definition.Scope {
		return errSubscriptionSubmissionConflict
	}
	if err := downloaderconfig.VerifyDownloadProfile(ctx, tx, proof.ProfileDigest); err != nil {
		return err
	}
	finalSelection, err := downloaderconfig.FileSelectionTx(ctx, tx, proof.FileSelectionID)
	if err != nil || !finalSelection.Committed || finalSelection.State != "ready" || finalSelection.Fingerprint != selection.Fingerprint {
		return errSubscriptionSubmissionConflict
	}
	var shared bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS s JOIN GO_SUBSCRIPTION_SUBMISSION_PROOFS p USING(RESOURCE_KEY,OWNER) JOIN GO_RSS_DOWNLOAD_CLAIMS r USING(RESOURCE_KEY) WHERE s.RESOURCE_KEY=? AND s.OWNER=? AND s.SUB_ID=? AND s.KIND='TV' AND s.MEDIA_ID=? AND s.SEASON=? AND s.EPISODES=? AND p.PAYLOAD=? AND r.STATE='submitted' AND r.TASK_ID=0)`, key, owner, id, proof.MediaID, proof.Season, subscriptionEpisodeString(proof.Episodes), payload).Scan(&shared)
	if err != nil || !shared {
		return errSubscriptionSubmissionConflict
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_SUBSCRIPTION_SEASON_MEMBERS WHERE RESOURCE_KEY=? AND OWNER=?`, key, owner).Scan(&count); err != nil || count != len(saved.cohort) {
		return errSubscriptionSubmissionConflict
	}
	where, expected := "rowid=?", []any{torrentID}
	for i, column := range []string{"TORRENT_NAME", "ENCLOSURE", "TYPE", "TITLE", "YEAR", "SEASON", "EPISODE"} {
		where += " AND " + column + " IS ?"
		expected = append(expected, values[i])
	}
	var torrentMatches bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE "+where+")", expected...).Scan(&torrentMatches); err != nil || !torrentMatches {
		return errSubscriptionSubmissionConflict
	}
	for i, m := range saved.cohort {
		var memberMatches bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_SEASON_MEMBERS WHERE RESOURCE_KEY=? AND OWNER=? AND SUB_ID=? AND MEDIA_ID=? AND SEASON=? AND EPISODES=? AND PAYLOAD=? AND STATE='submitted')`, key, owner, m.ID, m.Plan.mediaID, m.Plan.season, subscriptionEpisodeString(m.Candidate.Episodes), m.Payload).Scan(&memberMatches); err != nil || !memberMatches {
			return errSubscriptionSubmissionConflict
		}
		outcome := outcomes[i]
		if outcome.complete {
			var remains bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TVS WHERE ID=?)`, m.ID).Scan(&remains); err != nil || remains {
				return errSubscriptionSubmissionConflict
			}
			archive, err := readEditionArchive(ctx, tx, "TV", outcome.historyID)
			if err != nil || archive == nil || fuzzyDigest(archive.Raw) != fuzzyDigest(m.Plan.raw) {
				return errSubscriptionSubmissionConflict
			}
		} else if err := verifySubscriptionSnapshot(ctx, tx, "TV", m.ID, m.Plan); err != nil {
			return err
		}
		if m.Plan.edition != nil {
			var matches bool
			c := m.Plan.edition.Cycle
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_EDITION_CYCLES WHERE ID=? AND DOCUMENT=? AND STATE=?)`, c.ID, string(mustFuzzyJSON(c)), c.State).Scan(&matches); err != nil || !matches {
				return errSubscriptionSubmissionConflict
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	plan.cohort = saved.cohort
	plan.crossMember = true
	for _, m := range saved.cohort {
		if m.ID == id {
			plan.raw, plan.needed, plan.storedMissing, plan.edition, plan.Upgrade = m.Plan.raw, m.Plan.needed, m.Plan.storedMissing, m.Plan.edition, m.Plan.Upgrade
		}
	}
	return nil
}
