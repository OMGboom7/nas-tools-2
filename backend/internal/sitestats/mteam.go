package sitestats

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

// MTeamStatistics reads the complete account over the site's API, independent
// of homepage availability and browser settings. No partial snapshot escapes.
func MTeamStatistics(ctx context.Context, client *Client, site string) (Snapshot, error) {
	api, ok := builtinindexer.MTeamAPIOrigin(client.Origin)
	if !ok || strings.TrimSpace(client.APIKey) == "" {
		return Snapshot{}, ErrOrigin
	}
	c := Client{Origin: client.Origin, APIOrigin: api, APIKey: client.APIKey, UserAgent: client.UserAgent,
		Transport: client.Transport, Guard: client.Guard, Before: client.Before}
	s, err := Parse(ctx, &c, nil, Options{Site: site, URL: c.Origin, MTeamAPI: api})
	if err != nil {
		return Snapshot{}, err
	}
	// A missing icon leaves the previous one intact. Some front ends return
	// an HTML login page with status 200 for this request.
	iconCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if icon, err := c.Fetch(iconCtx, Request{Path: "/favicon.ico", Raw: true}); err == nil && len(icon) > 0 && len(icon) <= 128<<10 {
		if mime := http.DetectContentType(icon); strings.HasPrefix(mime, "image/") {
			s.Favicon = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(icon)
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// MTeamStatisticsCommand is the stdin/stdout bridge for existing schedulers.
func MTeamStatisticsCommand(ctx context.Context, in io.Reader, out io.Writer) error {
	var q struct {
		URL, Key, UA string
		Proxy        *string
	}
	input := &io.LimitedReader{R: in, N: 128 << 10}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
		return ErrResponse
	}
	origin, ok := builtinindexer.MTeamFeedOrigin(q.URL)
	if !ok {
		return ErrOrigin
	}
	client := Client{Origin: origin, APIKey: q.Key, UserAgent: q.UA}
	if q.Proxy != nil {
		var err error
		client.Transport, err = siteconfig.ProxyTransport(nil, *q.Proxy)
		if err != nil {
			return ErrOrigin
		}
	}
	snapshot, err := MTeamStatistics(ctx, &client, "")
	if err != nil {
		return err
	}
	// The local scheduler stores numeric seed pairs. Snapshot.MarshalJSON
	// converts them to decimal strings specifically for JavaScript clients.
	type schedulerSnapshot Snapshot
	return json.NewEncoder(out).Encode(schedulerSnapshot(snapshot))
}
