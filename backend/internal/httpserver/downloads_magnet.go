package httpserver

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/pan115"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

func (service downloadService) addMagnet(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input struct {
		Magnet string `json:"magnet"`
	}
	if err := decoder.Decode(&input); err != nil || decoder.Decode(new(any)) != io.EOF || !validMagnet(input.Magnet) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid magnet link")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	id, handled, err := service.nativeAddMagnet(ctx, input.Magnet)
	if !handled {
		writeAPIError(response, http.StatusNotImplemented, 501, "downloader type is not migrated")
		return
	}
	if errors.Is(err, qbittorrent.ErrConfiguration) || errors.Is(err, transmission.ErrConfiguration) || errors.Is(err, aria2.ErrConfiguration) || errors.Is(err, pan115.ErrConfiguration) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid downloader configuration")
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "download task could not be added")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"id": id}})
}

func validMagnet(raw string) bool {
	if len(raw) < 20 || len(raw) > 4096 || !strings.HasPrefix(raw, "magnet:?") {
		return false
	}
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "magnet" || parsed.Host != "" || parsed.Fragment != "" || parsed.RawQuery == "" {
		return false
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query["xt"]) != 1 || !strings.HasPrefix(strings.ToLower(query.Get("xt")), "urn:btih:") {
		return false
	}
	hash := query.Get("xt")[9:]
	if len(hash) == 40 {
		return qbittorrent.ValidHash(hash)
	}
	if len(hash) != 32 {
		return false
	}
	for _, r := range strings.ToUpper(hash) {
		if r < 'A' || r > 'Z' {
			if r < '2' || r > '7' {
				return false
			}
		}
	}
	return true
}

func (service downloadService) nativeAddMagnet(ctx context.Context, magnet string) (string, bool, error) {
	return service.nativeAddDownload(ctx, magnet, nil)
}

func (service downloadService) nativeAddTorrent(ctx context.Context, torrent []byte) (string, bool, error) {
	return service.nativeAddDownload(ctx, "", torrent)
}

func (service downloadService) nativeAddDownload(ctx context.Context, magnet string, torrent []byte) (string, bool, error) {
	return service.nativeAddDownloadWithOptions(ctx, magnet, torrent, "", downloadAddOptions{})
}

var errDownloadOptionsUnsupported = errors.New("downloader does not support these per-task settings")

// Only validation/authentication failures known to precede acceptance may be
// retried. Connection errors and malformed replies remain ambiguous.
func downloadDefinitelyNotSubmitted(handled bool, err error) bool {
	return !handled || errors.Is(err, errDownloadOptionsUnsupported) || errors.Is(err, qbittorrent.ErrConfiguration) || errors.Is(err, qbittorrent.ErrAuthentication) || errors.Is(err, transmission.ErrConfiguration) || errors.Is(err, aria2.ErrConfiguration) || errors.Is(err, pan115.ErrConfiguration)
}

type downloadAddOptions struct {
	SavePath         string
	Category         string
	Tags             []string
	Paused           bool
	UploadLimitKB    int
	DownloadLimitKB  int
	RatioLimit       float64
	SeedingTimeLimit int
}

func (options downloadAddOptions) empty() bool {
	return options.SavePath == "" && options.Category == "" && len(options.Tags) == 0 && !options.Paused && options.UploadLimitKB == 0 && options.DownloadLimitKB == 0 && options.RatioLimit == 0 && options.SeedingTimeLimit == 0
}

func (service downloadService) nativeAddDownloadWithOptions(ctx context.Context, magnet string, torrent []byte, downloaderID string, options downloadAddOptions) (string, bool, error) {
	item, _, available, err := service.configuredDownloader(ctx, downloaderID)
	if !available || err != nil {
		return "", available, err
	}
	if item.ID == 0 {
		return "", true, errors.New("default downloader is not configured")
	}
	if item.Type != "qbittorrent" && item.Type != "transmission" && item.Type != "aria2" && item.Type != "pan115" {
		return "", false, nil
	}
	if item.Type == "pan115" && torrent != nil {
		return "", false, nil
	}
	var configuration map[string]any
	if json.Unmarshal([]byte(item.Config), &configuration) != nil {
		return "", true, errors.New("invalid downloader configuration")
	}
	host, port := text(configuration["host"]), text(configuration["port"])
	switch item.Type {
	case "pan115":
		if !options.empty() {
			return "", true, errDownloadOptionsUnsupported
		}
		client, err := pan115.New(text(configuration["cookie"]), service.client.Transport)
		if err != nil {
			return "", true, err
		}
		id, err := client.AddMagnet(ctx, magnet)
		return id, true, err
	case "qbittorrent":
		addOptions := qbittorrent.AddOptions{SavePath: options.SavePath, Category: options.Category, Tags: options.Tags, Paused: options.Paused, UploadLimitKB: options.UploadLimitKB, DownloadLimitKB: options.DownloadLimitKB, RatioLimit: options.RatioLimit, SeedingTimeLimit: options.SeedingTimeLimit}
		client, err := qbittorrent.New(host, port, text(configuration["username"]), text(configuration["password"]), service.client.Transport)
		if err != nil {
			return "", true, err
		}
		if torrent != nil {
			metadata, err := torrentmeta.Parse(torrent)
			if err != nil {
				return "", true, err
			}
			if err := client.AddTorrentWithOptions(ctx, torrent, addOptions); err != nil {
				return "", true, err
			}
			return metadata.InfoHash, true, nil
		}
		if err := client.AddMagnetWithOptions(ctx, magnet, addOptions); err != nil {
			return "", true, err
		}
		return magnetInfoHash(magnet), true, nil
	case "transmission":
		if options.UploadLimitKB != 0 || options.DownloadLimitKB != 0 || options.RatioLimit != 0 || options.SeedingTimeLimit != 0 || options.Category != "" {
			return "", true, errDownloadOptionsUnsupported
		}
		addOptions := transmission.AddOptions{DownloadDir: options.SavePath, Paused: options.Paused, Labels: options.Tags}
		client, err := transmission.New(host, port, text(configuration["username"]), text(configuration["password"]), service.client.Transport)
		if err != nil {
			return "", true, err
		}
		var id string
		if torrent != nil {
			id, err = client.AddTorrentWithOptions(ctx, torrent, addOptions)
		} else {
			id, err = client.AddMagnetWithOptions(ctx, magnet, addOptions)
		}
		return id, true, err
	case "aria2":
		if options.Category != "" || len(options.Tags) > 0 || options.RatioLimit != 0 || options.SeedingTimeLimit != 0 {
			return "", true, errDownloadOptionsUnsupported
		}
		addOptions := aria2.AddOptions{DownloadDir: options.SavePath, Paused: options.Paused, UploadLimitKB: options.UploadLimitKB, DownloadLimitKB: options.DownloadLimitKB}
		client, err := aria2.New(host, port, text(configuration["secret"]), service.client.Transport)
		if err != nil {
			return "", true, err
		}
		var id string
		if torrent != nil {
			id, err = client.AddTorrentWithOptions(ctx, torrent, addOptions)
		} else {
			id, err = client.AddMagnetWithOptions(ctx, magnet, addOptions)
		}
		return id, true, err
	default:
		return "", false, nil
	}
}

func magnetInfoHash(magnet string) string {
	parsed, err := url.Parse(magnet)
	if err != nil {
		return ""
	}
	exact := parsed.Query().Get("xt")
	if !strings.HasPrefix(strings.ToLower(exact), "urn:btih:") {
		return ""
	}
	hash := exact[9:]
	if len(hash) == 40 && qbittorrent.ValidHash(hash) {
		return strings.ToLower(hash)
	}
	if len(hash) != 32 {
		return ""
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(hash))
	if err != nil || len(decoded) != 20 {
		return ""
	}
	return hex.EncodeToString(decoded)
}
