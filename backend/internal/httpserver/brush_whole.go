package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/downloadhistory"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

type brushWholeReceipt struct {
	digest, v1, v2 string
	media, path    *string
}

func (q *brushSubmitRequest) contentIdentity(kind string) (torrentmeta.TransferMetadata, error) {
	var identity torrentmeta.TransferMetadata
	if q.Torrent != nil {
		if kind == "pan115" || kind == "pikpak" {
			return identity, errDownloadOptionsUnsupported
		}
		if !q.Brush.Partial {
			if _, _, err := torrentmeta.DownloadLayout(q.Torrent); err != nil {
				return identity, err
			}
		}
		return torrentmeta.ClassicIdentity(q.Torrent)
	}
	switch kind {
	case "aria2", "pan115", "pikpak":
		q.Magnet = classicAria2Magnet(q.Magnet)
	default:
		q.Magnet = classicTorrentMagnet(q.Magnet, kind == "qbittorrent")
	}
	u, err := url.Parse(q.Magnet)
	if err != nil || q.Magnet == "" {
		return identity, errBrushDetail
	}
	identity.V1Hash, identity.V2Hash, err = torrentmeta.MagnetTopics(u.Query()["xt"])
	return identity, err
}

func (s downloadService) addBrushDownload(ctx context.Context, item downloaderconfig.Downloader, q brushSubmitRequest, token string, identity torrentmeta.TransferMetadata) (string, bool, error) {
	// Aria2 and the cloud clients historically received only the destination
	// for whole brush downloads. Tags and limits apply to qB and Transmission.
	opts := downloadAddOptions{SavePath: q.Directory, Paused: q.Brush.Partial}
	if item.Type == "qbittorrent" || item.Type == "transmission" {
		opts.Tags = append(slices.Clone(q.Tags), brushMarker(token))
		opts.UploadLimitKB, opts.DownloadLimitKB = q.UploadLimitKB, q.DownloadLimitKB
		opts.RatioLimit, opts.SeedingTimeLimit = q.RatioLimit, q.SeedingTimeLimit
		if item.Type == "qbittorrent" {
			opts.Category = q.Category
		} else {
			opts.ApplyTransmissionLimits = true
		}
	}
	classic := classicDownloadResult{receipt: bus.Hash(token)[:32]}
	classic.history.V1Hash, classic.history.V2Hash = identity.V1Hash, identity.V2Hash
	s.classic = &classic
	return s.nativeAddConfiguredDownload(ctx, item, q.Magnet, q.Torrent, opts)
}

func (s downloadService) submitBrushWhole(ctx context.Context, db *sql.DB, item downloaderconfig.Downloader, q brushSubmitRequest) (brushSubmitResult, error) {
	var result brushSubmitResult
	if q.Brush.Partial || q.Rule != (brushFractionRule{}) {
		return result, errBrushDetail
	}
	switch item.Type {
	case "qbittorrent", "transmission", "aria2", "pan115":
	case "pikpak":
		if !pikpakDownloadOptionsSupported(downloadAddOptions{SavePath: q.Directory}) {
			return result, errDownloadOptionsUnsupported
		}
	default:
		return result, errDownloadOptionsUnsupported
	}
	if len(q.Directory) > 4096 || strings.ContainsAny(q.Directory, "\r\n\x00") {
		return result, errDownloadOptionsUnsupported
	}
	identity, err := q.contentIdentity(item.Type)
	if err != nil {
		return result, err
	}
	proof := brushWholeReceipt{digest: bus.Hash(item), v1: identity.V1Hash, v2: identity.V2Hash, path: q.SavePath}
	if q.Media != nil {
		if !q.Media.Valid() || q.Media["ENCLOSURE"] == nil || *q.Media["ENCLOSURE"] != q.Brush.Enclosure {
			return result, downloadhistory.ErrInput
		}
		raw, err := json.Marshal(q.Media)
		if err != nil {
			return result, err
		}
		media := string(raw)
		proof.media = &media
	}
	if _, err = db.ExecContext(ctx, brushReceiptSchema); err != nil {
		return result, err
	}
	q.Brush.FractionSource = &q.RuleSource
	token, err := reserveBrushDownload(ctx, db, q.Brush, &proof)
	if err != nil || token == "" {
		return result, err
	}
	hash, handled, err := s.addBrushDownload(ctx, item, q, token, identity)
	if err != nil || !handled || hash == "" || hash == "0" {
		return result, errors.New("brush download has no confirmed task ID")
	}
	result = brushSubmitResult{Token: token, Task: hash, Size: q.Brush.Size}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := acceptBrushDownload(persist, db, token, hash); err == nil {
		if finished, err := finishBrushDownload(persist, db, token); err == nil && finished {
			return result, nil
		}
	}
	// A concurrent recovery may already have committed this exact task.
	var done bool
	err = db.QueryRowContext(persist, "SELECT EXISTS(SELECT 1 FROM SITE_BRUSH_TORRENTS WHERE TASK_ID=? AND ENCLOSURE=? AND DOWNLOADER=? AND DOWNLOAD_ID=?)", q.Brush.TaskID, q.Brush.Enclosure, q.Brush.Downloader, hash).Scan(&done)
	result.Pending = err != nil || !done
	return result, nil
}

func recordWholeBrushMedia(ctx context.Context, tx *sql.Tx, token, downloader, hash string) error {
	var raw string
	var path *string
	err := tx.QueryRowContext(ctx, "SELECT media,save_path FROM GO_BRUSH_WHOLE WHERE token=? AND media IS NOT NULL", token).Scan(&raw, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var m downloadhistory.Media
	if err = json.Unmarshal([]byte(raw), &m); err != nil {
		return err
	}
	return downloadhistory.RecordTx(ctx, tx, m, downloader, hash, path)
}

// Only an ownership marker, the original content identity and the original
// client configuration can replace a lost acknowledgement. Aria2 and cloud
// submissions without an acknowledged task ID stay reserved.
func recoverBrushWhole(ctx context.Context, db *sql.DB, dbPath string, task int64) error {
	rows, err := db.QueryContext(ctx, `SELECT s.token,s.downloader,w.config_digest,w.v1,w.v2 FROM GO_BRUSH_SUBMISSION s JOIN GO_BRUSH_WHOLE w USING(token)
 WHERE s.remote_id IS NULL AND s.task_id IS NOT NULL AND (?=0 OR s.task_id=?)`, task, task)
	if err != nil {
		return err
	}
	type pending struct {
		token, digest string
		id            int64
		identity      torrentmeta.TransferMetadata
	}
	var pendingItems []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.token, &p.id, &p.digest, &p.identity.V1Hash, &p.identity.V2Hash); err != nil {
			rows.Close()
			return err
		}
		pendingItems = append(pendingItems, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(pendingItems) == 0 {
		return err
	}
	store, err := downloaderconfig.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	s := downloadService{client: http.DefaultClient, downloaders: store}
	observe, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for _, p := range pendingItems {
		item, err := store.Get(observe, p.id)
		if err != nil || item.Enabled == 0 || bus.Hash(item) != p.digest {
			continue
		}
		var hash string
		switch item.Type {
		case "qbittorrent":
			c, e := s.qbProfileClient(item)
			if e != nil {
				continue
			}
			hash, err = c.VerifyClassic(observe, bus.Hash(p.token)[:32], p.identity)
		case "transmission":
			profile, e := store.FindTransmissionProfile(observe, item.ID, p.identity.V1Hash)
			if e != nil || profile.Definition.ConfigDigest != p.digest || profile.Definition.Options.Paused || !slices.Contains(profile.Definition.Options.Labels, brushMarker(p.token)) {
				continue
			}
			c, e := s.profileClient(item)
			if e != nil {
				continue
			}
			remote, e := c.Profile(observe, p.identity.V1Hash)
			if e != nil || !profile.Ready(remote) {
				continue
			}
			hash = p.identity.V1Hash
		default:
			continue
		}
		if err == nil && hash != "" {
			if err = acceptBrushDownload(ctx, db, p.token, hash); err != nil && !errors.Is(err, errFuzzyScope) {
				return err
			}
		}
	}
	return nil
}
