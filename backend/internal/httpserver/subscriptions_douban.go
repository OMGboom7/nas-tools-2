package httpserver

import (
	"context"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/douban"
)

func (service subscriptionService) fetchNativeDoubanIdentity(ctx context.Context, rawID, kind string) (original, localized, year string, err error) {
	id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if err != nil || id <= 0 || service.client == nil {
		return "", "", "", errInvalidSubscriptionSelector
	}
	mediaKind := "movie"
	if kind == "TV" {
		mediaKind = "tv"
	}
	subject, err := (douban.Client{Transport: service.client.Transport}).Subject(ctx, mediaKind, strconv.FormatInt(id, 10))
	if err != nil {
		return "", "", "", err
	}
	return subject.Original, subject.Title, subject.Year, nil
}
