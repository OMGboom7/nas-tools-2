package qbittorrent

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
)

type GlobalRateSession struct{ client *Client }

func (client *Client) OpenGlobalRates(ctx context.Context) (*GlobalRateSession, speedlimit.Rates, error) {
	if err := client.login(ctx); err != nil {
		return nil, speedlimit.Rates{}, err
	}
	s := &GlobalRateSession{client}
	r, err := s.Read(ctx)
	return s, r, err
}
func (s *GlobalRateSession) mode(ctx context.Context) (bool, error) {
	raw, err := s.client.request(ctx, http.MethodGet, "transfer/speedLimitsMode", nil)
	if err != nil {
		return false, err
	}
	value := strings.TrimSpace(string(raw))
	if value != "0" && value != "1" {
		return false, ErrResponse
	}
	return value == "1", nil
}
func (s *GlobalRateSession) Read(ctx context.Context) (speedlimit.Rates, error) {
	r := speedlimit.Rates{Unit: "B/s"}
	mode, err := s.mode(ctx)
	if err != nil {
		return r, err
	}
	for path, out := range map[string]*float64{"transfer/uploadLimit": &r.Upload, "transfer/downloadLimit": &r.Download} {
		raw, err := s.client.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return r, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || n < 0 || n > 1<<40 {
			return r, ErrResponse
		}
		*out = float64(n)
	}
	r.Alternative = mode
	r.UploadLimited, r.DownloadLimited = r.Upload > 0, r.Download > 0
	last, err := s.mode(ctx)
	if err != nil || last != mode || !r.Valid("qbittorrent") {
		return r, ErrResponse
	}
	return r, nil
}

// Write performs exactly one dimension mutation after the caller has durably
// recorded intent. It does not authenticate, negotiate, retry, toggle modes,
// change per-torrent preferences, or perform any stop/delete operation.
func (s *GlobalRateSession) Write(ctx context.Context, dimension string, after speedlimit.Rates) error {
	if !after.Valid("qbittorrent") {
		return ErrConfiguration
	}
	var value float64
	var path string
	switch dimension {
	case "upload":
		value, path = after.Upload, "transfer/setUploadLimit"
	case "download":
		value, path = after.Download, "transfer/setDownloadLimit"
	default:
		return ErrConfiguration
	}
	if value > 1<<31-1 {
		return ErrConfiguration
	}
	_, err := s.client.request(ctx, http.MethodPost, path, url.Values{"limit": {strconv.FormatInt(int64(value), 10)}})
	return err
}
