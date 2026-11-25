package httpserver

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
)

func (service subscriptionService) resolveNativeSubscriptionInput(ctx context.Context, input subscriptionUpsertRequest) (subscriptionUpsertRequest, error) {
	if strings.HasPrefix(input.MediaID, "DB:") {
		original, localized, year, err := service.fetchNativeDoubanIdentity(ctx, strings.TrimPrefix(input.MediaID, "DB:"), input.Type)
		if err != nil {
			return input, err
		}
		return service.resolveDoubanTitles(ctx, input, original, localized, year)
	}
	if strings.HasPrefix(input.MediaID, "BG:") {
		original, localized, year, err := service.fetchNativeBangumiIdentity(ctx, strings.TrimPrefix(input.MediaID, "BG:"))
		if err != nil {
			return input, err
		}
		input.Type = "TV"
		return service.resolveSourceTitles(ctx, input, original, localized, year)
	}
	var err error
	input.MediaID, err = service.resolveNativeSubscriptionMediaID(ctx, input)
	return input, err
}

func (service subscriptionService) fetchNativeBangumiIdentity(ctx context.Context, rawID string) (original, localized, year string, err error) {
	raw, err := service.bangumiSubjectDocument(ctx, rawID)
	if err != nil {
		return "", "", "", err
	}
	var subject struct {
		Name   string
		NameCN string `json:"name_cn"`
		Date   string
	}
	if err := json.Unmarshal(raw, &subject); err != nil {
		return "", "", "", err
	}
	if len(subject.Date) >= 4 {
		if parsed, err := strconv.Atoi(subject.Date[:4]); err == nil && parsed >= 1800 && parsed <= 2200 {
			year = subject.Date[:4]
		}
	}
	return strings.TrimSpace(subject.Name), strings.TrimSpace(subject.NameCN), year, nil
}

func (service subscriptionService) bangumiSubjectDocument(ctx context.Context, rawID string) (json.RawMessage, error) {
	id, parseErr := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if parseErr != nil || id <= 0 {
		return nil, errInvalidSubscriptionSelector
	}
	endpoint := url.URL{Scheme: "https", Host: "api.bgm.tv", Path: "/v0/subjects/" + strconv.FormatInt(id, 10)}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "0xforee/nas-tools-go/0.1 (+https://github.com/0xforee/nas-tools)")
	client := *service.client
	client.Timeout = 15 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	upstream, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusOK {
		return nil, errors.New("Bangumi subject is unavailable")
	}
	contents, err := io.ReadAll(io.LimitReader(upstream.Body, 1<<20+1))
	if err != nil || len(contents) > 1<<20 {
		return nil, errors.New("Bangumi response is unavailable or too large")
	}
	var subject struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		NameCN string `json:"name_cn"`
	}
	if err := json.Unmarshal(contents, &subject); err != nil || subject.ID != id || strings.TrimSpace(subject.Name) == "" && strings.TrimSpace(subject.NameCN) == "" {
		return nil, errors.New("invalid Bangumi subject details")
	}
	return contents, nil
}

func (service subscriptionService) resolveDoubanTitles(ctx context.Context, input subscriptionUpsertRequest, original, localized, year string) (subscriptionUpsertRequest, error) {
	if input.Type == "TV" && input.Season == "" {
		input.Season = "1"
	}
	return service.resolveSourceTitles(ctx, input, original, localized, year)
}

func (service subscriptionService) resolveSourceTitles(ctx context.Context, input subscriptionUpsertRequest, original, localized, year string) (subscriptionUpsertRequest, error) {
	if year != "" {
		input.Year = year
	}
	input.MediaID = ""
	for i, name := range []string{original, localized} {
		if name == "" || i == 1 && name == original {
			continue
		}
		input.Name = name
		id, err := service.resolveNativeSubscriptionMediaID(ctx, input)
		if err != nil {
			return input, err
		}
		if id != "" {
			input.MediaID = id
			break
		}
	}
	return input, nil
}
