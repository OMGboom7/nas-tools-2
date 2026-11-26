package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func CheckMTeamProfile(ctx context.Context, origin, key, ua string, transport http.RoundTripper) error {
	_, api, err := mteamBase(origin)
	if err != nil {
		return err
	}
	data, err := mteamRequest(ctx, api, "/api/member/profile", key, ua, "application/json", nil, transport)
	if err != nil {
		return err
	}
	var member struct{ Username string }
	if json.Unmarshal(data, &member) != nil || strings.TrimSpace(member.Username) == "" {
		return ErrResponse
	}
	return nil
}

// A subscription supplies the feed title, so both identity fields must match.
// Older brush jobs supply only the URL. Always check any ID returned by the API.
func MTeamTorrentDetail(ctx context.Context, resource externalindexer.Resource, key, ua string, now time.Time, transport http.RoundTripper) (externalindexer.Resource, *string, error) {
	target, ok := parseMTeamLocator(resource.PageURL)
	if !ok || target.id == "" {
		return resource, nil, ErrConfig
	}
	if enclosure, ok := parseMTeamLocator(resource.DownloadURL); ok && enclosure.id != "" {
		if enclosure.id != target.id || !sameOrigin(enclosure.address, target.address) {
			return resource, nil, ErrConfig
		}
	}
	target.api.RawQuery = url.Values{"id": {target.id}}.Encode()
	data, err := mteamRequest(ctx, target.api, "/api/torrent/detail", key, ua, "application/json", nil, transport)
	if err != nil {
		return resource, nil, err
	}
	var row struct {
		ID     json.RawMessage
		Name   string
		Status *struct {
			Discount       string
			Seeders        json.RawMessage
			ToppingEndTime *string
			MallSingleFree json.RawMessage
		}
	}
	if json.Unmarshal(data, &row) != nil || row.Status == nil {
		return resource, nil, ErrResponse
	}
	if resource.Title != "" || len(row.ID) != 0 {
		id, err := mteamInteger(row.ID, true)
		if err != nil || strconv.FormatInt(*id, 10) != target.id {
			return resource, nil, ErrResponse
		}
	}
	if resource.Title != "" && !strings.EqualFold(strings.Join(strings.Fields(resource.Title), " "), strings.Join(strings.Fields(row.Name), " ")) {
		return resource, nil, ErrResponse
	}
	seeders, err := mteamInteger(row.Status.Seeders, true)
	if err != nil {
		return resource, nil, err
	}
	resource.Seeders = seeders
	setMTeamPromotion(&resource, row.Status.Discount)
	end := row.Status.ToppingEndTime
	sale, err := mteamSaleDeadline(row.Status.MallSingleFree)
	if err != nil {
		return resource, nil, err
	}
	if sale != "" {
		until, err := time.ParseInLocation(time.DateTime, sale, time.FixedZone("CST", 8*3600))
		if err != nil {
			return resource, nil, ErrResponse
		}
		if now.Before(until) {
			// A personal offer supplies its own end date. A pin expiry cannot
			// establish when a public free promotion stops.
			if resource.Freeleech != nil && !*resource.Freeleech {
				resource.PromotionEnd = until.Local().Format(time.DateTime)
			}
			free, down := true, 0.0
			resource.Freeleech, resource.DownloadFactor, end = &free, &down, &sale
		}
	}
	return resource, end, nil
}

// The API uses false as well as null for an absent personal promotion.
func mteamSaleDeadline(raw json.RawMessage) (string, error) {
	switch string(raw) {
	case "", "false", "null":
		return "", nil
	}
	var sale struct{ EndDate string }
	if json.Unmarshal(raw, &sale) != nil {
		return "", ErrResponse
	}
	return sale.EndDate, nil
}

func MTeamInspectCommand(ctx context.Context, command string, in io.Reader, out io.Writer) error {
	var q struct {
		URL, Key, UA string
		Proxy        *string
	}
	input := &io.LimitedReader{R: in, N: 128 << 10}
	decoder := json.NewDecoder(input)
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
		return ErrConfig
	}
	var transport http.RoundTripper
	if q.Proxy != nil {
		var err error
		transport, err = siteconfig.ProxyTransport(nil, *q.Proxy)
		if err != nil {
			return ErrConfig
		}
	}
	var result any
	var err error
	switch command {
	case "mteam-profile":
		origin, ok := MTeamFeedOrigin(q.URL)
		if !ok {
			return ErrConfig
		}
		err = CheckMTeamProfile(ctx, origin, q.Key, q.UA, transport)
		result = map[string]bool{"ok": err == nil}
	case "mteam-detail":
		var res externalindexer.Resource
		var end *string
		res, end, err = MTeamTorrentDetail(ctx, externalindexer.Resource{PageURL: q.URL}, q.Key, q.UA, time.Now(), transport)
		if err == nil {
			free := res.Freeleech != nil && *res.Freeleech
			result = map[string]any{"free": free, "2xfree": free && res.UploadFactor != nil && *res.UploadFactor == 2, "hr": false, "peer_count": res.Seeders, "free_deadline": end}
		}
	default:
		return ErrConfig
	}
	var status MTeamHTTPStatus
	if errors.As(err, &status) {
		return json.NewEncoder(out).Encode(map[string]int{"status": int(status)})
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
