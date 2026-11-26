package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/batchdownload"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/downloadhistory"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/rssreceipt"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

// The local caller resolves its user, media and directory using the original
// entry point. The Go process reads the actual client endpoint from the volume.
type classicRequest struct {
	Batch                                             *batchdownload.Proof
	UploadLimitKB, DownloadLimitKB, SeedingTimeLimit  int
	RatioLimit                                        float64
	Category, ClientType                              string
	Tags                                              []string
	Notice                                            downloadhistory.Notice
	Downloader                                        int64
	Channel, Sender, Source, Title, Directory, Magnet string
	Torrent                                           []byte
	Paused                                            bool
	Config, Directories                               json.RawMessage
	History                                           downloaderconfig.ClassicAcknowledgement
}

func ClassicDownloadCommand(ctx context.Context, path, op string, in io.Reader, out io.Writer) error {
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	dbPath := (config.Application{Path: path}).UserDatabasePath()
	if info, err := os.Stat(dbPath); err != nil || !info.Mode().IsRegular() {
		return errManualConfiguration
	}
	if op == "rss-status" {
		return classicRSSStatus(ctx, dbPath, in, out)
	}
	if op == "notices" {
		return deliverClassicNotices(ctx, path, dbPath)
	}
	if op != "submit" {
		return errManualConfiguration
	}
	var q classicRequest
	input := &io.LimitedReader{R: in, N: 16 << 20}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 || q.Downloader < 1 ||
		q.Channel == "" || q.Sender == "" || q.Source == "" || q.Title == "" || q.Notice.Title == "" || q.Notice.Source != q.Source || len(q.History.Image) > 2048 || len(q.History.Link) > 2048 ||
		q.History.Media != nil && !q.History.Media.Valid() || (q.Magnet != "") == (q.Torrent != nil) {
		return errManualConfiguration
	}
	if article := q.History.RSS; article != nil {
		if article.TaskID <= 0 || article.Enclosure == "" || article.Torrent != q.Title || q.Channel != "USERRSS" || q.Sender != "userrss:"+strconv.FormatInt(article.TaskID, 10) || q.Batch != nil {
			return errManualConfiguration
		}
	}
	if q.Torrent != nil {
		if _, _, err := torrentmeta.DownloadLayout(q.Torrent); err != nil {
			return err
		}
	}
	store, err := downloaderconfig.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	actor := "classic:" + bus.Hash([]string{q.Channel, q.Sender})
	// Before other classic clients were supported the bridge omitted the type.
	if q.ClientType == "" {
		q.ClientType = "aria2"
	}
	cloudRSS := (q.History.RSS != nil || q.History.Subscription != nil) && (q.ClientType == "pan115" || q.ClientType == "pikpak")
	item, err := store.Get(ctx, q.Downloader)
	if err != nil || item.Enabled == 0 || item.Type != q.ClientType || (item.Type != "aria2" && item.Type != "qbittorrent" && item.Type != "transmission" && !cloudRSS) {
		return json.NewEncoder(out).Encode(map[string]bool{"rejected": true})
	}
	if q.Torrent == nil {
		switch item.Type {
		case "aria2", "pan115", "pikpak":
			q.Magnet = classicAria2Magnet(q.Magnet)
		default:
			q.Magnet = classicTorrentMagnet(q.Magnet, item.Type == "qbittorrent")
		}
		if q.Magnet == "" {
			return json.NewEncoder(out).Encode(map[string]bool{"rejected": true})
		}
	}
	// A long-running classic client may have calculated its paths from stale settings.
	if !sameClassicJSON(item.Config, q.Config) || !sameClassicJSON(item.DownloadDir, q.Directories) {
		return json.NewEncoder(out).Encode(map[string]bool{"rejected": true})
	}
	service := downloadService{client: http.DefaultClient, downloaders: store}
	if item.Type == "qbittorrent" {
		var identity torrentmeta.TransferMetadata
		if q.Torrent != nil {
			identity, err = torrentmeta.ClassicIdentity(q.Torrent)
		} else {
			u, _ := url.Parse(q.Magnet)
			identity.V1Hash, identity.V2Hash, err = torrentmeta.MagnetTopics(u.Query()["xt"])
		}
		if err != nil {
			return err
		}
		q.History.V1Hash, q.History.V2Hash = identity.V1Hash, identity.V2Hash
	}
	if organization.CopySupported {
		guards, err := organization.OpenStore(dbPath)
		if err != nil {
			return err
		}
		defer guards.Close()
		service.manualGuards = &guards
	}
	// Selection evidence is derived exclusively from the submitted bytes.
	q.History.Files = nil
	q.History.Directory = ""
	q.History.Paused = false
	q.History.Title, q.History.Text = q.Notice.Render()
	result := classicDownloadResult{history: q.History}
	options := downloadAddOptions{SavePath: q.Directory, Paused: q.Paused, UploadLimitKB: q.UploadLimitKB, DownloadLimitKB: q.DownloadLimitKB, Tags: q.Tags, Category: q.Category, RatioLimit: q.RatioLimit, SeedingTimeLimit: q.SeedingTimeLimit, ApplyTransmissionLimits: item.Type == "transmission"}
	if q.Batch != nil {
		files, units, err := batchdownload.Files(q.Torrent, *q.Batch)
		if err != nil {
			return json.NewEncoder(out).Encode(map[string]bool{"rejected": true})
		}
		if h := result.history.Subscription; h != nil && h.Mode == "episodes" {
			h.Covered = units[h.Season]
		}
		label := batchdownload.EpisodeLabel(units)
		if media := result.history.Media; media != nil {
			media["SE"] = &label
			name := *media["TITLE"]
			if year := media["YEAR"]; year != nil && *year != "" {
				name += " (" + *year + ")"
			}
			notice := q.Notice
			notice.Title = name + " " + label
			result.history.Title, result.history.Text = notice.Render()
		} else {
			result.history.Text += "\n已选剧集：" + label
		}
		if batchdownload.RequiresSelection(*q.Batch, files, units) {
			if item.Type != "qbittorrent" && item.Type != "transmission" {
				return json.NewEncoder(out).Encode(map[string]bool{"rejected": true})
			}
			result.history.Files = files
			result.history.Paused = options.Paused
			result.history.Directory = options.SavePath
			options.ClassicFiles = bus.Hash(files)
			if item.Type == "transmission" {
				options.Tags = append(options.Tags, "NASTOOL-FILES-"+options.ClassicFiles[:24])
			}
		}
	}
	var check func() error
	if h := result.history.Subscription; h != nil {
		if !validOrdinaryRSS(h) || h.Marker.Name != q.Title || q.Channel != "RSS" || q.Sender != "rss:"+strconv.FormatInt(h.ID, 10) || q.History.RSS != nil {
			return errManualConfiguration
		}
		if media := q.History.Media; media != nil {
			if link := media["ENCLOSURE"]; link == nil || *link != h.Marker.Enclosure {
				return errManualConfiguration
			}
		}
		subscriptions := subscriptionService{databasePath: dbPath}
		db, err := subscriptions.openNativeSubscriptionWriteDatabase()
		if err != nil {
			return err
		}
		defer db.Close()
		check = func() error { return checkOrdinaryRSSSource(ctx, db, h) }
	}
	task, handled, err := service.submitWithReceipt(ctx, actor, q.Magnet, q.Torrent, &item, options, q.Title, q.Source, nil, check, &result)
	reply := map[string]any{}
	switch {
	case errors.Is(err, rssreceipt.ErrProcessed):
		reply["processed"] = true
	case errors.Is(err, downloaderconfig.ErrManualPending), errors.Is(err, rssreceipt.ErrClaim):
		reply["pending"] = true
	case err != nil || !handled:
		reply["rejected"] = true
	default:
		reply["task"], reply["reused"] = task, result.reused
		if q.History.Subscription != nil {
			reply["subscriptionRecorded"] = true
		}
	}
	return json.NewEncoder(out).Encode(reply)
}

// Preserve all tracker parameters and both advertised identities for clients
// that understand hybrid magnets. Transmission needs a v1 identity.
func classicTorrentMagnet(raw string, allowPureV2 bool) string {
	if validMagnet(raw) {
		return raw
	}
	if len(raw) > 4096 || !strings.HasPrefix(raw, "magnet:?") || strings.IndexFunc(raw, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" {
		return ""
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return ""
	}
	topics := q["xt"]
	v1, v2, err := torrentmeta.MagnetTopics(topics)
	if err != nil || v2 == "" || !(len(topics) == 2 && v1 != "" || len(topics) == 1 && allowPureV2) {
		return ""
	}
	return raw
}

// aria2 understands the v1 identity of a hybrid torrent. Preserve its tracker
// parameters and pass a single unambiguous topic, in either original xt order.
func classicAria2Magnet(raw string) string {
	if validMagnet(raw) {
		return raw
	}
	if len(raw) > 4096 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "magnet" || u.Fragment != "" || u.Host != "" || u.Path != "" || u.Opaque != "" {
		return ""
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["xt"]) != 2 {
		return ""
	}
	v1, v2, err := torrentmeta.MagnetTopics(q["xt"])
	if err != nil || v1 == "" || v2 == "" {
		return ""
	}
	q.Set("xt", "urn:btih:"+v1)
	u.RawQuery = q.Encode()
	if !validMagnet(u.String()) {
		return ""
	}
	return u.String()
}

func sameClassicJSON(saved string, seen json.RawMessage) bool {
	var a, b any
	return json.Unmarshal([]byte(saved), &a) == nil && json.Unmarshal(seen, &b) == nil && bus.Hash(a) == bus.Hash(b)
}

func deliverClassicNotices(ctx context.Context, path, dbPath string) error {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=rw&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	call, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	if organization.CopySupported {
		locks, err := organization.OpenJobLocks(dbPath)
		if err != nil {
			return err
		}
		g, err := locks.AcquireJob(call, businessNotificationGuard)
		if err != nil {
			return err
		}
		defer g.Close()
	}
	api := businessNotifications{queue: bus.New(db), service: notificationService{client: http.DefaultClient, config: config.NewStore(path)}}
	pending, err := api.queue.PendingClassicNotices(call)
	if err != nil {
		return err
	}
	return api.dispatch(call, pending)
}
