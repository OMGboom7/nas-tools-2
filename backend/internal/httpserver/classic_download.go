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
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/downloadhistory"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

// The local caller resolves its user, media and directory using the original
// entry point. The Go process reads the actual client endpoint from the volume.
type classicRequest struct {
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
	// Before other classic clients were supported the bridge omitted the type.
	if q.ClientType == "" {
		q.ClientType = "aria2"
	}
	item, err := store.Get(ctx, q.Downloader)
	if err != nil || item.Enabled == 0 || item.Type != q.ClientType || (item.Type != "aria2" && item.Type != "qbittorrent" && item.Type != "transmission") {
		return json.NewEncoder(out).Encode(map[string]bool{"rejected": true})
	}
	if q.Torrent == nil {
		switch item.Type {
		case "aria2":
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
	q.History.Title, q.History.Text = q.Notice.Render()
	result := classicDownloadResult{history: q.History}
	options := downloadAddOptions{SavePath: q.Directory, Paused: q.Paused, UploadLimitKB: q.UploadLimitKB, DownloadLimitKB: q.DownloadLimitKB, Tags: q.Tags, Category: q.Category, RatioLimit: q.RatioLimit, SeedingTimeLimit: q.SeedingTimeLimit, ApplyTransmissionLimits: item.Type == "transmission"}
	actor := "classic:" + bus.Hash([]string{q.Channel, q.Sender})
	task, handled, err := service.submitWithReceipt(ctx, actor, q.Magnet, q.Torrent, &item, options, q.Title, q.Source, nil, nil, &result)
	reply := map[string]any{}
	switch {
	case errors.Is(err, downloaderconfig.ErrManualPending):
		reply["pending"] = true
	case err != nil || !handled:
		reply["rejected"] = true
	default:
		reply["task"], reply["reused"] = task, result.reused
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
	pending, err := api.queue.PendingClassicDownloads(call)
	if err != nil {
		return err
	}
	return api.dispatch(call, pending)
}
