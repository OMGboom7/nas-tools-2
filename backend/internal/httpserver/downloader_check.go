package httpserver

import (
	"context"
	"net/http"

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/pan115"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

func nativeDownloaderCheck(ctx context.Context, downloaderType string, configuration map[string]any, transport http.RoundTripper) (bool, error, string) {
	host, port := text(configuration["host"]), text(configuration["port"])
	username, password := text(configuration["username"]), text(configuration["password"])
	switch downloaderType {
	case "qbittorrent":
		client, err := qbittorrent.New(host, port, username, password, transport)
		if err == nil {
			err = client.Check(ctx)
		}
		return true, err, "qBittorrent"
	case "transmission":
		client, err := transmission.New(host, port, username, password, transport)
		if err == nil {
			err = client.Check(ctx)
		}
		return true, err, "Transmission"
	case "aria2":
		client, err := aria2.New(host, port, text(configuration["secret"]), transport)
		if err == nil {
			err = client.Check(ctx)
		}
		return true, err, "Aria2"
	case "pan115":
		client, err := pan115.New(text(configuration["cookie"]), transport)
		if err == nil {
			err = client.Check(ctx)
		}
		return true, err, "115网盘"
	default:
		return false, nil, ""
	}
}
