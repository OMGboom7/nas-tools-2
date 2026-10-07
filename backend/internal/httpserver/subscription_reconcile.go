package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

// Private durable evidence, never an HTTP response. No downloader credentials
// are copied: a digest binds verification to the configuration used to submit.
type subscriptionSubmissionProof struct {
	Version                              int
	DownloaderID                         int64
	DownloaderType, ConfigHash, InfoHash string
	Raw                                  json.RawMessage
	MediaID                              string
	Season, Total                        int
	StoredMissing, Needed, Episodes      []int
	Title, DownloadURL                   string
}

type subscriptionReconcileGuard struct {
	proof   subscriptionSubmissionProof
	payload string
}

func newSubscriptionSubmissionProof(downloader downloaderconfig.Downloader, magnet string, torrent []byte) subscriptionSubmissionProof {
	proof := subscriptionSubmissionProof{Version: 1, DownloaderID: downloader.ID, DownloaderType: downloader.Type, ConfigHash: subscriptionResourceKey(downloader.Config)}
	if magnet != "" {
		proof.InfoHash = magnetInfoHash(magnet)
	} else if meta, err := torrentmeta.Parse(torrent); err == nil {
		proof.InfoHash = meta.InfoHash
	}
	return proof
}

func encodeSubscriptionSubmissionProof(proof subscriptionSubmissionProof, plan subscriptionSearchPlan, candidate subscriptionCandidate) (string, error) {
	if proof.Version != 1 || proof.DownloaderID <= 0 || len(proof.ConfigHash) != 64 {
		return "", errors.New("invalid subscription proof identity")
	}
	raw, err := json.Marshal(plan.raw)
	if err != nil {
		return "", err
	}
	proof.Raw, proof.MediaID, proof.Season, proof.Total = raw, plan.mediaID, plan.season, plan.total
	proof.StoredMissing, proof.Needed, proof.Episodes = plan.storedMissing, plan.needed, candidate.Episodes
	proof.Title, proof.DownloadURL = candidate.Title, candidate.resource.DownloadURL
	body, err := json.Marshal(proof)
	if err != nil || len(body) > 2<<20 {
		return "", errors.New("subscription proof exceeded limits")
	}
	return string(body), nil
}

func decodeSubscriptionProof(body string) (subscriptionSubmissionProof, subscriptionSearchPlan, error) {
	p, plan := subscriptionSubmissionProof{}, subscriptionSearchPlan{}
	if len(body) > 2<<20 {
		return p, plan, errors.New("invalid subscription proof")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil || p.Version != 1 || p.DownloaderID <= 0 || len(p.ConfigHash) != 64 || p.MediaID == "" || !rssDownloadURLAllowed(p.DownloadURL) || p.Title == "" || p.Total < 0 || p.Total > 10000 || p.Season < 0 || p.Season > 1000 {
		return p, plan, errors.New("invalid subscription proof")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return p, plan, errors.New("invalid subscription proof suffix")
	}
	decoder = json.NewDecoder(bytes.NewReader(p.Raw))
	decoder.UseNumber()
	if decoder.Decode(&plan.raw) != nil || len(plan.raw) == 0 || len(plan.raw) > 256 {
		return p, plan, errors.New("invalid source snapshot")
	}
	for key, value := range plan.raw {
		if number, ok := value.(json.Number); ok {
			if n, err := number.Int64(); err == nil {
				plan.raw[key] = n
			} else if n, err := number.Float64(); err == nil {
				plan.raw[key] = n
			} else {
				return p, plan, errors.New("invalid snapshot number")
			}
		} else {
			switch value.(type) {
			case nil, string:
			default:
				return p, plan, errors.New("invalid snapshot value")
			}
		}
	}
	plan.mediaID, plan.season, plan.total = p.MediaID, p.Season, p.Total
	plan.storedMissing, plan.needed = p.StoredMissing, p.Needed
	return p, plan, nil
}

func (api *subscriptionSearchRunner) serveReconcile(w http.ResponseWriter, r *http.Request) {
	if api.planner.runner == nil || api.planner.runner.download.service.auth == nil {
		writeAPIError(w, 503, 503, "native submission reconciliation is unavailable")
		return
	}
	auth := api.planner.runner.download.service.auth
	claims, err := auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization token is invalid or expired")
		return
	}
	if !auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if r.ParseForm() != nil {
		writeAPIError(w, 400, 400, "invalid reconciliation request")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	kind := r.PathValue("type")
	if err != nil || id <= 0 || kind != "MOV" && kind != "TV" {
		writeAPIError(w, 400, 400, "invalid subscription selector")
		return
	}
	result, failure := api.reconcile(r.Context(), kind, id)
	if failure != nil {
		writeJSON(w, failure.status, map[string]any{"code": failure.status, "success": false, "message": failure.message, "data": result})
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": result})
}

func (api *subscriptionSearchRunner) reconcile(parent context.Context, kind string, id int64) (subscriptionSearchRunResult, *recognitionFailure) {
	result := subscriptionSearchRunResult{Remaining: []int{}, Uncertain: true}
	fail := func(status int, message string) (subscriptionSearchRunResult, *recognitionFailure) {
		return result, &recognitionFailure{status, message}
	}
	if !api.planner.runner.pureGo {
		return fail(501, "native reconciliation requires disabled legacy backend")
	}
	key := kind + ":" + strconv.FormatInt(id, 10)
	api.mu.Lock()
	if api.running == nil {
		api.running = map[string]bool{}
	}
	if api.running[key] {
		api.mu.Unlock()
		return fail(409, "subscription execution is already running")
	}
	api.running[key] = true
	api.mu.Unlock()
	defer func() { api.mu.Lock(); delete(api.running, key); api.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	service := api.planner.runner.recognition.service
	db, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return fail(503, "submission ledger is unavailable")
	}
	defer db.Close()
	for _, table := range []string{"GO_SUBSCRIPTION_DOWNLOAD_CLAIMS", "GO_SUBSCRIPTION_SUBMISSION_PROOFS", "GO_RSS_DOWNLOAD_CLAIMS"} {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, table).Scan(&exists); err != nil {
			return fail(503, "submission ledger is unavailable")
		}
		if !exists {
			return fail(501, "pending submission has no durable verification evidence")
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT s.RESOURCE_KEY,s.OWNER,s.MEDIA_ID,s.SEASON,s.EPISODES,p.PAYLOAD FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS s JOIN GO_RSS_DOWNLOAD_CLAIMS r USING(RESOURCE_KEY) LEFT JOIN GO_SUBSCRIPTION_SUBMISSION_PROOFS p ON p.RESOURCE_KEY=s.RESOURCE_KEY AND p.OWNER=s.OWNER WHERE r.STATE='pending' AND r.TASK_ID=0 AND s.KIND=? AND s.SUB_ID=? LIMIT 2`, kind, id)
	if err != nil {
		return fail(503, "submission ledger is unavailable")
	}
	var resourceKey, owner, mediaID, episodes string
	var season, count int
	var payload sql.NullString
	for rows.Next() {
		count++
		if err := rows.Scan(&resourceKey, &owner, &mediaID, &season, &episodes, &payload); err != nil {
			rows.Close()
			return fail(503, "submission ledger is invalid")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fail(503, "submission ledger is unavailable")
	}
	if count == 0 {
		return fail(404, "no pending subscription submission was found")
	}
	if count != 1 {
		return fail(409, "multiple pending submissions require verification")
	}
	proof, plan, err := decodeSubscriptionProof(payload.String)
	if err != nil || !payload.Valid || proof.MediaID != mediaID || proof.Season != season || subscriptionEpisodeString(proof.Episodes) != episodes || subscriptionResourceKey(proof.DownloadURL) != resourceKey || text(plan.raw["ID"]) != strconv.FormatInt(id, 10) {
		return fail(501, "pending submission verification evidence is missing or invalid")
	}
	result.Remaining = append([]int{}, plan.needed...)
	if kind == "TV" {
		if proof.Total <= 0 || len(proof.Episodes) == 0 || !validReconcileEpisodes(proof.StoredMissing, proof.Total) || !validReconcileEpisodes(proof.Needed, proof.Total) || !validReconcileEpisodes(proof.Episodes, proof.Total) {
			return fail(501, "pending episode evidence is invalid")
		}
		for _, n := range proof.Episodes {
			if !slices.Contains(proof.Needed, n) {
				return fail(501, "pending episode evidence is invalid")
			}
		}
		for _, n := range proof.Needed {
			if !slices.Contains(proof.StoredMissing, n) {
				return fail(501, "pending episode evidence is invalid")
			}
		}
	} else if len(proof.Episodes) != 0 {
		return fail(501, "pending movie evidence is invalid")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(503, "subscription state is unavailable")
	}
	err = verifySubscriptionSnapshot(ctx, tx, kind, id, plan)
	tx.Rollback()
	if err != nil {
		return fail(409, "subscription changed after submission; pending evidence is retained")
	}
	download := api.planner.runner.download.service
	item, _, handled, err := download.configuredDownloader(ctx, strconv.FormatInt(proof.DownloaderID, 10))
	if err != nil || !handled || item.ID != proof.DownloaderID || item.Enabled == 0 || item.Type != proof.DownloaderType || subscriptionResourceKey(item.Config) != proof.ConfigHash {
		return fail(409, "submission downloader configuration changed or is unavailable")
	}
	found := false
	configuration := map[string]any{}
	if json.Unmarshal([]byte(item.Config), &configuration) != nil {
		return fail(502, "submission downloader configuration is invalid")
	}
	switch item.Type {
	case "qbittorrent":
		if !qbittorrent.ValidHash(proof.InfoHash) {
			return fail(501, "pending submission has no verifiable torrent hash")
		}
		client, e := qbittorrent.New(text(configuration["host"]), text(configuration["port"]), text(configuration["username"]), text(configuration["password"]), download.client.Transport)
		if e != nil || client == nil {
			return fail(502, "submission downloader could not be verified")
		}
		found, err = client.HasTorrent(ctx, proof.InfoHash)
	case "transmission":
		if !transmission.ValidHash(proof.InfoHash) {
			return fail(501, "pending submission has no supported torrent hash")
		}
		client, e := transmission.New(text(configuration["host"]), text(configuration["port"]), text(configuration["username"]), text(configuration["password"]), download.client.Transport)
		if e != nil || client == nil {
			return fail(502, "submission downloader could not be verified")
		}
		found, err = client.HasTorrent(ctx, proof.InfoHash)
	default:
		return fail(501, "pending reconciliation for this downloader is not migrated")
	}
	if ctx.Err() != nil {
		return fail(504, "submission verification was cancelled or timed out")
	}
	if err != nil {
		return fail(502, "submission downloader response could not be verified")
	}
	if !found {
		return fail(409, "torrent is not currently present; pending submission remains blocked")
	}
	// Do not use a new default downloader, edited endpoint, or stale user snapshot
	// to confirm another instance's reservation. Persistence rechecks ownership.
	current, _, _, err := download.configuredDownloader(ctx, strconv.FormatInt(proof.DownloaderID, 10))
	if err != nil || current.Enabled == 0 || current.Type != proof.DownloaderType || subscriptionResourceKey(current.Config) != proof.ConfigHash {
		return fail(409, "submission downloader changed during verification")
	}
	candidate := subscriptionCandidate{Title: proof.Title, Episodes: proof.Episodes, resource: externalindexer.Resource{DownloadURL: proof.DownloadURL}}
	if err := service.persistSubscriptionSubmission(ctx, kind, id, &plan, &candidate, owner, subscriptionReconcileGuard{proof, payload.String}); err != nil {
		return fail(409, "confirmed submission progress conflicts with current state; evidence is retained")
	}
	result.Submitted, result.Completed, result.Uncertain = 1, kind == "MOV" || len(plan.needed) == 0, false
	result.Remaining = append([]int{}, plan.needed...)
	return result, nil
}

func validReconcileEpisodes(values []int, total int) bool {
	if len(values) > 10000 {
		return false
	}
	seen := map[int]bool{}
	for _, n := range values {
		if n < 1 || n > total || seen[n] {
			return false
		}
		seen[n] = true
	}
	return true
}
