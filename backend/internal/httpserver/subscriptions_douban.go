package httpserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// These values reproduce the request format of app/media/doubanapi/apiv2.py.
// They are server-side only and are never sent to the browser.
const doubanFrodoKey = "054022eaeae0b00e0fc068c0c0a2102a"
const doubanFrodoSigningKey = "bf7dddc7c9cfe6f7"

func (service subscriptionService) fetchNativeDoubanIdentity(ctx context.Context, rawID, kind string) (original, localized, year string, err error) {
	id, parseErr := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if parseErr != nil || id <= 0 {
		return "", "", "", errInvalidSubscriptionSelector
	}
	mediaType := "movie"
	if kind == "TV" {
		mediaType = "tv"
	}
	endpoint := url.URL{Scheme: "https", Host: "frodo.douban.com", Path: "/api/v2/" + mediaType + "/" + strconv.FormatInt(id, 10)}
	ts := time.Now().Format("20060102")
	message := "GET&" + url.QueryEscape(endpoint.Path) + "&" + ts
	mac := hmac.New(sha1.New, []byte(doubanFrodoSigningKey))
	_, _ = mac.Write([]byte(message))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	endpoint.RawQuery = url.Values{
		"apiKey": {doubanFrodoKey}, "os_rom": {"android"}, "_ts": {ts}, "_sig": {signature},
	}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", "", "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "MicroMessenger/")
	request.Header.Set("Referer", "https://servicewechat.com/wx2f9b06c1de1ccfca/91/page-frame.html")
	client := *service.client
	client.Timeout = 15 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	upstream, err := client.Do(request)
	if err != nil {
		return "", "", "", err
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusOK {
		return "", "", "", errors.New("Douban subject is unavailable")
	}
	contents, err := io.ReadAll(io.LimitReader(upstream.Body, 1<<20+1))
	if err != nil || len(contents) > 1<<20 {
		return "", "", "", errors.New("Douban response is unavailable or too large")
	}
	var subject struct {
		ID             json.RawMessage `json:"id"`
		Title          string          `json:"title"`
		OriginalTitle  string          `json:"original_title"`
		Year           json.RawMessage `json:"year"`
		LocalizedError string          `json:"localized_message"`
	}
	if err := json.Unmarshal(contents, &subject); err != nil || subject.LocalizedError != "" {
		return "", "", "", errors.New("invalid Douban subject details")
	}
	var returnedID string
	if err := json.Unmarshal(subject.ID, &returnedID); err != nil {
		var numericID int64
		if err := json.Unmarshal(subject.ID, &numericID); err != nil {
			return "", "", "", errors.New("invalid Douban subject ID")
		}
		returnedID = strconv.FormatInt(numericID, 10)
	}
	if returnedID != strconv.FormatInt(id, 10) || strings.TrimSpace(subject.Title) == "" {
		return "", "", "", errors.New("invalid Douban subject identity")
	}
	localized = strings.TrimSpace(subject.Title)
	original = strings.TrimSpace(subject.OriginalTitle)
	if original == "" {
		original = localized
	}
	if err := json.Unmarshal(subject.Year, &year); err != nil {
		var numericYear int
		if err := json.Unmarshal(subject.Year, &numericYear); err == nil {
			year = strconv.Itoa(numericYear)
		}
	}
	if numericYear, err := strconv.Atoi(year); err != nil || numericYear < 1800 || numericYear > 2200 {
		year = ""
	}
	return original, localized, year, nil
}
