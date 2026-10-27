package httpserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

var errSubscriptionSubmissionConflict = errors.New("subscription submission conflicts with current state")

// Older databases have no native ledger yet. A present ledger with missing
// shared claims is an error, not proof that an uncertain submission is absent.
func readPendingSubscriptionSubmission(ctx context.Context, db subscriptionRowReader, kind string, id int64) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_SUBSCRIPTION_DOWNLOAD_CLAIMS')`).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	var pending bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS s JOIN GO_RSS_DOWNLOAD_CLAIMS r USING(RESOURCE_KEY) WHERE r.STATE='pending' AND s.KIND=? AND s.SUB_ID=?)`, kind, id).Scan(&pending)
	return pending, err
}

func subscriptionResourceKey(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

func subscriptionTable(kind string) string {
	if kind == "TV" {
		return "RSS_TVS"
	}
	return "RSS_MOVIES"
}

func ensureSubscriptionSubmissionSchema(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		// TASK_ID=0 belongs to subscription execution, positive IDs to RSS tasks.
		// Sharing the unique key prevents RSS/subscription duplicate submission.
		`CREATE TABLE IF NOT EXISTS GO_RSS_DOWNLOAD_CLAIMS (RESOURCE_KEY TEXT PRIMARY KEY,TASK_ID INTEGER NOT NULL,STATE TEXT NOT NULL,UPDATED_AT TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_SUBSCRIPTION_DOWNLOAD_CLAIMS (RESOURCE_KEY TEXT PRIMARY KEY,OWNER TEXT NOT NULL,KIND TEXT NOT NULL,SUB_ID INTEGER NOT NULL,MEDIA_ID TEXT NOT NULL,SEASON INTEGER NOT NULL,EPISODES TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS GO_SUBSCRIPTION_CLAIMS_OWNER ON GO_SUBSCRIPTION_DOWNLOAD_CLAIMS(KIND,SUB_ID)`,
		`CREATE INDEX IF NOT EXISTS GO_SUBSCRIPTION_CLAIMS_MEDIA ON GO_SUBSCRIPTION_DOWNLOAD_CLAIMS(KIND,MEDIA_ID,SEASON)`,
		`CREATE TABLE IF NOT EXISTS GO_SUBSCRIPTION_SUBMISSION_PROOFS (RESOURCE_KEY TEXT PRIMARY KEY,OWNER TEXT NOT NULL,PAYLOAD TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// Compare all columns, including unknown user fields. The snapshot is read
// before external lookups; edits, deletion and metadata refresh invalidate it.
func verifySubscriptionSnapshot(ctx context.Context, tx *sql.Tx, kind string, id int64, plan subscriptionSearchPlan) error {
	if plan.fuzzy || plan.edition != nil || plan.typedSource {
		if err := verifyFuzzySource(ctx, tx, kind, id, plan.raw); err != nil {
			return err
		}
		if plan.fuzzy {
			return nil
		}
	} else {
		keys := make([]string, 0, len(plan.raw))
		for key := range plan.raw {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		where, args := "ID=?", []any{id}
		for _, key := range keys {
			where += ` AND "` + strings.ReplaceAll(key, `"`, `""`) + `" IS ?`
			args = append(args, plan.raw[key])
		}
		var found bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+subscriptionTable(kind)+" WHERE "+where+")", args...).Scan(&found); err != nil {
			return err
		}
		if !found {
			return errSubscriptionSubmissionConflict
		}
	}
	if kind == "TV" && !plan.fuzzy {
		total, e1 := strconv.Atoi(text(plan.raw["TOTAL"]))
		lack, e2 := strconv.Atoi(text(plan.raw["LACK"]))
		if e1 != nil || e2 != nil {
			return errSubscriptionSubmissionConflict
		}
		missing, err := refreshMissingEpisodes(ctx, tx, strconv.FormatInt(id, 10), total, lack, plan.total)
		if err != nil {
			return err
		}
		if !slices.Equal(missing, plan.storedMissing) {
			return errSubscriptionSubmissionConflict
		}
	}
	return nil
}

func pendingSubscriptionSubmission(ctx context.Context, tx *sql.Tx, kind string, id int64, plan subscriptionSearchPlan) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS s JOIN GO_RSS_DOWNLOAD_CLAIMS r USING(RESOURCE_KEY) WHERE r.STATE='pending' AND s.KIND=? AND (s.SUB_ID=? OR (s.MEDIA_ID=? AND s.SEASON=?)))`, kind, id, plan.mediaID, plan.season).Scan(&pending)
	return pending, err
}

func (service subscriptionService) reserveSubscriptionSubmission(ctx context.Context, kind string, id int64, plan subscriptionSearchPlan, candidate subscriptionCandidate, proofs ...subscriptionSubmissionProof) (string, error) {
	db, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		return "", err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := service.checkEditionSnapshot(ctx, tx, kind, id, plan); err != nil {
		return "", err
	}
	if plan.files != nil {
		scope, err := nativeSubscriptionConfiguration(ctx, tx, service)
		if err != nil || scope != plan.files.Scope {
			return "", errSubscriptionSubmissionConflict
		}
	}
	token, err := reserveSubscriptionSubmissionTx(ctx, tx, kind, id, plan, candidate, proofs...)
	if err != nil {
		return "", err
	}
	return token, tx.Commit()
}

// Fuzzy batches commit their own item intent and this shared reservation in
// one transaction. Exact subscriptions retain the original wrapper above.
func reserveSubscriptionSubmissionTx(ctx context.Context, tx *sql.Tx, kind string, id int64, plan subscriptionSearchPlan, candidate subscriptionCandidate, proofs ...subscriptionSubmissionProof) (string, error) {
	if err := ensureSubscriptionSubmissionSchema(ctx, tx); err != nil {
		return "", err
	}
	if err := verifySubscriptionSnapshot(ctx, tx, kind, id, plan); err != nil {
		return "", err
	}
	if err := reserveEditionCycle(ctx, tx, kind, id, plan, candidate); err != nil {
		return "", err
	}
	pending, err := pendingSubscriptionSubmission(ctx, tx, kind, id, plan)
	if err != nil {
		return "", err
	}
	if pending {
		return "", errSubscriptionSubmissionConflict
	}
	var processed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?)`, candidate.resource.DownloadURL).Scan(&processed); err != nil {
		return "", err
	}
	if processed {
		return "", errSubscriptionSubmissionConflict
	}
	key := subscriptionResourceKey(candidate.resource.DownloadURL)
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_RSS_DOWNLOAD_CLAIMS(RESOURCE_KEY,TASK_ID,STATE,UPDATED_AT) VALUES (?,0,'pending',?) ON CONFLICT(RESOURCE_KEY) DO NOTHING`, key, time.Now().Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", errSubscriptionSubmissionConflict
	}
	owner := make([]byte, 16)
	if _, err := rand.Read(owner); err != nil {
		return "", err
	}
	token := hex.EncodeToString(owner)
	if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_SUBSCRIPTION_DOWNLOAD_CLAIMS(RESOURCE_KEY,OWNER,KIND,SUB_ID,MEDIA_ID,SEASON,EPISODES) VALUES (?,?,?,?,?,?,?) ON CONFLICT(RESOURCE_KEY) DO UPDATE SET OWNER=excluded.OWNER,KIND=excluded.KIND,SUB_ID=excluded.SUB_ID,MEDIA_ID=excluded.MEDIA_ID,SEASON=excluded.SEASON,EPISODES=excluded.EPISODES`, key, token, kind, id, plan.mediaID, plan.season, subscriptionEpisodeString(candidate.Episodes))); err != nil {
		return "", err
	}
	if len(proofs) > 1 {
		return "", errors.New("invalid subscription submission proof")
	}
	if len(proofs) == 1 {
		payload, err := encodeSubscriptionSubmissionProof(proofs[0], plan, candidate)
		if err != nil {
			return "", err
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_SUBSCRIPTION_SUBMISSION_PROOFS(RESOURCE_KEY,OWNER,PAYLOAD) VALUES (?,?,?) ON CONFLICT(RESOURCE_KEY) DO UPDATE SET OWNER=excluded.OWNER,PAYLOAD=excluded.PAYLOAD`, key, token, payload)); err != nil {
			return "", err
		}
		if plan.files != nil {
			if plan.files.ID != plan.fileSelectionID || proofs[0].ProfileDigest != plan.files.ProfileFingerprint() {
				return "", errSubscriptionSubmissionConflict
			}
			plan.files.Proof = payload
			if err := downloaderconfig.ReserveFileSelectionTx(ctx, tx, *plan.files); err != nil {
				return "", err
			}
		}
		if plan.edition != nil {
			if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_SUBSCRIPTION_EDITION_ATTEMPTS(CYCLE_ID,OWNER,RESOURCE_KEY,PAYLOAD,STATE) VALUES(?,?,?,?,'intent')`, plan.edition.Cycle.ID, token, key, payload)); err != nil {
				return "", err
			}
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS WHERE RESOURCE_KEY=?`, key); err != nil {
			return "", err
		}
	}
	return token, nil
}

func (service subscriptionService) releaseSubscriptionSubmission(ctx context.Context, raw, owner string) error {
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
	key := subscriptionResourceKey(raw)
	var rejectedProof string
	if err := tx.QueryRowContext(ctx, `SELECT PAYLOAD FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS WHERE RESOURCE_KEY=? AND OWNER=?`, key, owner).Scan(&rejectedProof); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if rejectedProof != "" {
		var header struct{ Version int }
		if json.Unmarshal([]byte(rejectedProof), &header) != nil {
			return errSubscriptionSubmissionConflict
		}
		if header.Version == 3 || header.Version == 4 {
			proof, _, err := decodeSubscriptionProof(rejectedProof)
			if err != nil {
				return err
			}
			if proof.Edition != nil {
				if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_SUBSCRIPTION_EDITION_ATTEMPTS SET STATE='rejected' WHERE CYCLE_ID=? AND OWNER=? AND RESOURCE_KEY=? AND PAYLOAD=? AND STATE='intent'`, proof.Edition.Cycle.ID, owner, key, rejectedProof)); err != nil {
					return err
				}
			}
			if proof.FileSelectionID != "" {
				var state string
				if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_TORRENT_FILE_SELECTIONS WHERE ID=?`, proof.FileSelectionID).Scan(&state); err != nil || state != "rejected" {
					return errSubscriptionSubmissionConflict
				}
			}
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND TASK_ID=0 AND STATE='pending' AND EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND OWNER=?)`, key, key, owner)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errSubscriptionSubmissionConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND OWNER=?`, key, owner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS WHERE RESOURCE_KEY=? AND OWNER=?`, key, owner); err != nil {
		return err
	}
	return tx.Commit()
}

func subscriptionEpisodeString(episodes []int) string {
	parts := make([]string, len(episodes))
	for i, n := range episodes {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// One transaction confirms ownership, consumes only accepted episodes, records
// the resource and, when complete, moves the subscription to legacy-compatible
// history. Rollback leaves the durable pending claim intact after acceptance.
func (service subscriptionService) persistSubscriptionSubmission(ctx context.Context, kind string, id int64, plan *subscriptionSearchPlan, candidate *subscriptionCandidate, owner string, guards ...subscriptionReconcileGuard) error {
	profileDigest := ""
	selectionScope := ""
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
	if err := ensureSubscriptionSubmissionSchema(ctx, tx); err != nil {
		return err
	}
	if len(guards) > 1 {
		return errSubscriptionSubmissionConflict
	}
	if len(guards) == 1 {
		guard := guards[0]
		var payload, configuration, downloaderType string
		var enabled int
		if candidate == nil {
			return errSubscriptionSubmissionConflict
		}
		if err := tx.QueryRowContext(ctx, `SELECT PAYLOAD FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS WHERE RESOURCE_KEY=? AND OWNER=?`, subscriptionResourceKey(candidate.resource.DownloadURL), owner).Scan(&payload); err != nil {
			return err
		}
		if payload != guard.payload {
			return errSubscriptionSubmissionConflict
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(CONFIG,'{}'),COALESCE(TYPE,''),COALESCE(ENABLED,0) FROM DOWNLOADER WHERE ID=?`, guard.proof.DownloaderID).Scan(&configuration, &downloaderType, &enabled); err != nil {
			return err
		}
		if enabled == 0 || downloaderType != guard.proof.DownloaderType || subscriptionResourceKey(configuration) != guard.proof.ConfigHash {
			return errSubscriptionSubmissionConflict
		}
	}
	if err := verifySubscriptionSnapshot(ctx, tx, kind, id, *plan); err != nil {
		return err
	}
	if err := service.checkEditionSnapshot(ctx, tx, kind, id, *plan); err != nil {
		return err
	}
	remaining := append([]int{}, plan.needed...)
	if candidate == nil {
		pending, err := pendingSubscriptionSubmission(ctx, tx, kind, id, *plan)
		if err != nil {
			return err
		}
		if pending {
			return errSubscriptionSubmissionConflict
		}
	} else {
		key := subscriptionResourceKey(candidate.resource.DownloadURL)
		result, err := tx.ExecContext(ctx, `UPDATE GO_RSS_DOWNLOAD_CLAIMS SET STATE='submitted',UPDATED_AT=? WHERE RESOURCE_KEY=? AND TASK_ID=0 AND STATE='pending' AND EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND OWNER=? AND KIND=? AND SUB_ID=?)`, time.Now().Format(time.RFC3339), key, key, owner, kind, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errSubscriptionSubmissionConflict
		}
		remaining = slices.DeleteFunc(remaining, func(n int) bool { return slices.Contains(candidate.Episodes, n) })
		var proofBody string
		if err := tx.QueryRowContext(ctx, `SELECT PAYLOAD FROM GO_SUBSCRIPTION_SUBMISSION_PROOFS WHERE RESOURCE_KEY=? AND OWNER=?`, key, owner).Scan(&proofBody); err != nil {
			return err
		}
		var storedProof subscriptionSubmissionProof
		if json.Unmarshal([]byte(proofBody), &storedProof) != nil {
			return errSubscriptionSubmissionConflict
		}
		if storedProof.FileSelectionID != "" {
			if storedProof.FileSelectionID != plan.fileSelectionID || !slices.Equal(storedProof.Episodes, candidate.Episodes) {
				return errSubscriptionSubmissionConflict
			}
			selection, err := downloaderconfig.FileSelectionTx(ctx, tx, storedProof.FileSelectionID)
			if err != nil || selection.Definition.Proof != proofBody {
				return errSubscriptionSubmissionConflict
			}
			scope, err := nativeSubscriptionConfiguration(ctx, tx, service)
			if err != nil || scope != selection.Definition.Scope {
				return errSubscriptionSubmissionConflict
			}
			selectionScope = scope
			if err := downloaderconfig.CommitFileSelectionTx(ctx, tx, storedProof.FileSelectionID, proofBody); err != nil {
				return err
			}
		}
		if plan.edition != nil {
			p, saved, err := decodeSubscriptionProof(proofBody)
			if err != nil || saved.edition == nil || p.Priority != candidate.Priority || p.GroupID != candidate.groupID || p.TopOrder != candidate.topOrder || fuzzyDigest(saved.edition.Cycle) != fuzzyDigest(plan.edition.Cycle) || !slices.Equal(p.Episodes, candidate.Episodes) {
				return errSubscriptionSubmissionConflict
			}
			if err := service.checkEditionSnapshot(ctx, tx, kind, id, saved); err != nil {
				return err
			}
			if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_SUBSCRIPTION_EDITION_ATTEMPTS SET STATE='accepted' WHERE CYCLE_ID=? AND OWNER=? AND RESOURCE_KEY=? AND PAYLOAD=? AND STATE='intent'`, saved.edition.Cycle.ID, owner, key, proofBody)); err != nil {
				return err
			}
		}
		profileDigest = storedProof.ProfileDigest
		if err := downloaderconfig.VerifyDownloadProfile(ctx, tx, storedProof.ProfileDigest); err != nil {
			return err
		}
		values := []any{candidate.Title, candidate.resource.DownloadURL, kind, plan.raw["NAME"], plan.raw["YEAR"], plan.raw["SEASON"], subscriptionEpisodeString(candidate.Episodes)}
		if plan.typedSource || plan.edition != nil {
			for i, v := range values {
				value, err := fuzzySQLValue(v)
				if err != nil {
					return err
				}
				values[i] = value
			}
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO RSS_TORRENTS(TORRENT_NAME,ENCLOSURE,TYPE,TITLE,YEAR,SEASON,EPISODE) VALUES (?,?,?,?,?,?,?)`, values...)); err != nil {
			return err
		}
		if err := bus.Emit(ctx, tx, "download_start", key+":"+owner, "subscription:"+kind+":"+strconv.FormatInt(id, 10), text(plan.raw["NAME"])+" 开始下载", "来自：订阅；类型："+kind+"；年份："+text(plan.raw["YEAR"])+"；季："+text(plan.raw["SEASON"])+"；集："+subscriptionEpisodeString(candidate.Episodes)); err != nil {
			return err
		}
	}
	complete := plan.LibraryComplete || kind == "TV" && len(remaining) == 0 || kind == "MOV" && candidate != nil
	var edition *subscriptionEditionSnapshot
	if plan.edition != nil {
		var err error
		complete, edition, err = persistEditionTx(ctx, tx, kind, id, *plan, candidate, remaining)
		if err != nil {
			return err
		}
	}
	if complete {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS RSS_HISTORY (ID INTEGER PRIMARY KEY,TYPE TEXT,RSSID TEXT,NAME TEXT,YEAR TEXT,TMDBID TEXT,SEASON TEXT,IMAGE TEXT,DESC TEXT,TOTAL INTEGER,START INTEGER,FINISH_TIME TEXT)`); err != nil {
			return err
		}
		values := []any{kind, strconv.FormatInt(id, 10), plan.raw["NAME"], plan.raw["YEAR"], plan.mediaID, plan.raw["SEASON"], plan.raw["IMAGE"], plan.raw["DESC"], plan.raw["TOTAL_EP"], plan.raw["CURRENT_EP"], time.Now().Format("2006-01-02 15:04:05")}
		if plan.edition != nil || plan.typedSource {
			for i, v := range values {
				value, err := fuzzySQLValue(v)
				if err != nil {
					return err
				}
				values[i] = value
			}
		}
		insert, err := tx.ExecContext(ctx, `INSERT INTO RSS_HISTORY(TYPE,RSSID,NAME,YEAR,TMDBID,SEASON,IMAGE,DESC,TOTAL,START,FINISH_TIME) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, values...)
		if err := fuzzyOne(insert, err); err != nil {
			return err
		}
		if plan.edition != nil {
			historyID, err := insert.LastInsertId()
			if err != nil || historyID < 1 {
				return errSubscriptionSubmissionConflict
			}
			if err := archiveEditionSource(ctx, tx, kind, historyID, plan.raw); err != nil {
				return err
			}
		}
		if kind == "TV" {
			if err := removeNativeTVEpisodes(ctx, tx, id); err != nil {
				return err
			}
		}
		if err := fuzzyOne(tx.ExecContext(ctx, "DELETE FROM "+subscriptionTable(kind)+" WHERE ID=?", id)); err != nil {
			return err
		}
		reference := "subscription:" + kind + ":" + strconv.FormatInt(id, 10)
		if err := bus.Emit(ctx, tx, "rss_finished", reference+":"+time.Now().UTC().Format(time.RFC3339Nano), reference, text(plan.raw["NAME"])+" 订阅完成", "类型："+kind+"；年份："+text(plan.raw["YEAR"])+"；季："+text(plan.raw["SEASON"])+"。此为订阅计划完成，不代表文件已入库。"); err != nil {
			return err
		}
	} else if plan.edition != nil {
		// persistEditionTx preserved the ordinary episode ledger.
	} else if kind == "TV" {
		if err := fuzzyOne(tx.ExecContext(ctx, `UPDATE RSS_TVS SET STATE='R',TOTAL=?,LACK=? WHERE ID=?`, plan.total, len(remaining), id)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS RSS_TV_EPISODES (ID INTEGER PRIMARY KEY,RSSID TEXT,EPISODES TEXT)`); err != nil {
			return err
		}
		if err := removeNativeTVEpisodes(ctx, tx, id); err != nil {
			return err
		}
		if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES (?,?)`, strconv.FormatInt(id, 10), subscriptionEpisodeString(remaining))); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE RSS_MOVIES SET STATE='R' WHERE ID=?`, id); err != nil {
			return err
		}
	}
	if err := downloaderconfig.VerifyDownloadProfile(ctx, tx, profileDigest); err != nil {
		return err
	}
	if selectionScope != "" {
		scope, err := nativeSubscriptionConfiguration(ctx, tx, service)
		if err != nil || scope != selectionScope {
			return errSubscriptionSubmissionConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	plan.needed = remaining
	if plan.edition != nil {
		plan.edition = edition
		plan.Upgrade = editionView(edition)
		if edition != nil && edition.Cycle.State == "completed" {
			plan.raw["FILTER_ORDER"] = edition.Cycle.Order
		}
	} else {
		plan.storedMissing = append([]int{}, remaining...)
	}
	if !complete {
		plan.raw["STATE"] = "R"
		if kind == "TV" && plan.edition == nil {
			plan.raw["TOTAL"], plan.raw["LACK"] = plan.total, len(remaining)
		}
	}
	return nil
}
