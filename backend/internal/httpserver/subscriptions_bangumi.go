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
		if year != "" {
			input.Year = year
		}
		if input.Type == "TV" && input.Season == "" {
			input.Season = "1"
		}
		input.Name = original
		input.MediaID, err = service.resolveNativeSubscriptionMediaID(ctx, input)
		if err != nil {
			return input, err
		}
		if input.MediaID == "" && localized != "" && localized != original {
			input.Name = localized
			input.MediaID, err = service.resolveNativeSubscriptionMediaID(ctx, input)
			if err != nil {
				return input, err
			}
		}
		return input, nil
	}
	if strings.HasPrefix(input.MediaID, "BG:") {
		original, localized, year, err := service.fetchNativeBangumiIdentity(ctx, strings.TrimPrefix(input.MediaID, "BG:"))
		if err != nil {
			return input, err
		}
		input.Type = "TV"
		if year != "" {
			input.Year = year
		}
		input.Name = original
		input.MediaID, err = service.resolveNativeSubscriptionMediaID(ctx, input)
		if err != nil {
			return input, err
		}
		if input.MediaID == "" && localized != "" && localized != original {
			input.Name = localized
			input.MediaID, err = service.resolveNativeSubscriptionMediaID(ctx, input)
			if err != nil {
				return input, err
			}
		}
		return input, nil
	}
	var err error
	input.MediaID, err = service.resolveNativeSubscriptionMediaID(ctx, input)
	return input, err
}

func (service subscriptionService) fetchNativeBangumiIdentity(ctx context.Context, rawID string) (original, localized, year string, err error) {
	id, parseErr := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if parseErr != nil || id <= 0 {
		return "", "", "", errInvalidSubscriptionSelector
	}
	endpoint := url.URL{Scheme: "https", Host: "api.bgm.tv", Path: "/v0/subjects/" + strconv.FormatInt(id, 10)}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", "", "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "0xforee/nas-tools-go/0.1 (+https://github.com/0xforee/nas-tools)")
	client := *service.client
	client.Timeout = 15 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	upstream, err := client.Do(request)
	if err != nil {
		return "", "", "", err
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusOK {
		return "", "", "", errors.New("Bangumi subject is unavailable")
	}
	contents, err := io.ReadAll(io.LimitReader(upstream.Body, 1<<20+1))
	if err != nil || len(contents) > 1<<20 {
		return "", "", "", errors.New("Bangumi response is unavailable or too large")
	}
	var subject struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		NameCN string `json:"name_cn"`
		Date   string `json:"date"`
	}
	if err := json.Unmarshal(contents, &subject); err != nil || subject.ID != id || strings.TrimSpace(subject.Name) == "" {
		return "", "", "", errors.New("invalid Bangumi subject details")
	}
	if len(subject.Date) >= 4 {
		if parsed, err := strconv.Atoi(subject.Date[:4]); err == nil && parsed >= 1800 && parsed <= 2200 {
			year = subject.Date[:4]
		}
	}
	return strings.TrimSpace(subject.Name), strings.TrimSpace(subject.NameCN), year, nil
}
