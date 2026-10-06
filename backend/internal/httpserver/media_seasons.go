package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (service subscriptionService) serveCompatTVSeasons(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid season request")
		return
	}
	id := strings.TrimSpace(request.Form.Get("tmdbid"))
	if id == "" || len(id) > 64 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid media ID")
		return
	}
	if service.configStore == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "TMDB configuration is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	if !numericTMDBID(id) {
		if !strings.HasPrefix(id, "DB:") && !strings.HasPrefix(id, "BG:") {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid media ID")
			return
		}
		resolved, err := service.resolveNativeSubscriptionInput(ctx, subscriptionUpsertRequest{Type: "TV", MediaID: id})
		if err != nil {
			if errors.Is(err, errInvalidSubscriptionSelector) {
				writeAPIError(response, http.StatusBadRequest, 400, "invalid media ID")
			} else {
				writeAPIError(response, http.StatusBadGateway, 502, "media identity lookup is unavailable")
			}
			return
		}
		if !numericTMDBID(resolved.MediaID) {
			writeAPIError(response, http.StatusNotFound, 404, "matching TV show was not found")
			return
		}
		id = resolved.MediaID
	}
	canonicalID, _ := strconv.ParseInt(id, 10, 64) // numericTMDBID has validated the ID.
	id = strconv.FormatInt(canonicalID, 10)
	seasons, err := service.fetchNativeTVSeasons(ctx, id)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "TMDB TV seasons are unavailable")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "seasons": seasons})
}

type tvSeasonOption struct {
	Text string `json:"text"`
	Num  int    `json:"num"`
}

func (service subscriptionService) fetchNativeTVSeasons(ctx context.Context, id string) ([]tvSeasonOption, error) {
	detail, err := fetchNativeTMDBDetails(ctx, service.configStore, service.client.Transport, "tv", id)
	if err != nil {
		return nil, err
	}
	seasons := make([]tvSeasonOption, 0, len(detail.Seasons))
	for offset := len(detail.Seasons) - 1; offset >= 0; offset-- {
		number := detail.Seasons[offset].Number
		if number <= 0 {
			continue
		}
		seasons = append(seasons, tvSeasonOption{Text: "第" + chineseSeasonNumber(number) + "季", Num: number})
	}
	return seasons, nil
}

func chineseSeasonNumber(number int) string {
	if number <= 0 || number > 9999 {
		return strconv.Itoa(number)
	}
	digits := []string{"零", "一", "二", "三", "四", "五", "六", "七", "八", "九"}
	units := []string{"", "十", "百", "千"}
	parts := []string{}
	zero := false
	for place := 3; place >= 0; place-- {
		base := 1
		for i := 0; i < place; i++ {
			base *= 10
		}
		digit := (number / base) % 10
		if digit == 0 {
			zero = len(parts) > 0
			continue
		}
		if zero {
			parts = append(parts, "零")
			zero = false
		}
		if !(place == 1 && digit == 1 && len(parts) == 0) {
			parts = append(parts, digits[digit])
		}
		parts = append(parts, units[place])
	}
	return strings.Join(parts, "")
}
