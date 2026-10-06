package builtinindexer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

func mteamBase(domain string) (*url.URL, *url.URL, error) {
	front, err := url.Parse(domain)
	if err != nil || front.Scheme != "https" || front.User != nil || front.Port() != "" || front.RawQuery != "" || front.Fragment != "" || (front.Path != "" && front.Path != "/") {
		return nil, nil, ErrConfig
	}
	host := strings.ToLower(front.Hostname())
	suffix := ""
	for _, candidate := range []string{"m-team.cc", "m-team.io"} {
		if host == candidate || strings.HasSuffix(host, "."+candidate) {
			suffix = candidate
		}
	}
	if suffix == "" {
		return nil, nil, ErrConfig
	}
	api := &url.URL{Scheme: "https", Host: "api." + suffix}
	return front, api, nil
}

func mteamRequest(ctx context.Context, api *url.URL, endpoint, key, ua, contentType string, body []byte, transport http.RoundTripper) (json.RawMessage, error) {
	if key == "" || len(key) > 1024 || strings.ContainsAny(key+ua, "\r\n") {
		return nil, ErrConfig
	}
	u := *api
	u.Path = endpoint
	request, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, ErrConfig
	}
	request.Header.Set("x-api-key", key)
	request.Header.Set("User-Agent", ua)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrResponse
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, ErrResponse
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 || !utf8.Valid(data) {
		return nil, ErrResponse
	}
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, ErrResponse
	}
	code := strings.TrimSpace(string(envelope.Code))
	if code != "0" && code != `"0"` {
		return nil, ErrResponse
	}
	return envelope.Data, nil
}

func mteamInteger(raw json.RawMessage, required bool) (*int64, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		if required {
			return nil, ErrResponse
		}
		return nil, nil
	}
	value := string(raw)
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value = text
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 {
		return nil, ErrResponse
	}
	return &number, nil
}

func SearchMTeam(ctx context.Context, definition indexercatalog.Definition, key, ua, keyword string, page int, transport http.RoundTripper) ([]externalindexer.Resource, error) {
	if definition.Parser != "MTeamSpider" || definition.ID == "" || keyword == "" || len(keyword) > 1024 || page < 0 || page > 10000 {
		return nil, ErrConfig
	}
	front, api, err := mteamBase(definition.Domain)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"categories": []int{}, "keyword": keyword, "mode": "normal", "pageNumber": page + 1, "pageSize": 100, "visible": 1})
	data, err := mteamRequest(ctx, api, "/api/torrent/search", key, ua, "application/json", body, transport)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data []struct {
			ID          json.RawMessage `json:"id"`
			Name        string          `json:"name"`
			Description string          `json:"smallDescr"`
			Size        json.RawMessage `json:"size"`
			Status      *struct {
				Discount string          `json:"discount"`
				Seeders  json.RawMessage `json:"seeders"`
				Leechers json.RawMessage `json:"leechers"`
			} `json:"status"`
			Episode *struct {
				IMDb string `json:"imdb"`
			} `json:"episode_info"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &result) != nil || result.Data == nil || len(result.Data) > 100 {
		return nil, ErrResponse
	}
	resources := []externalindexer.Resource{}
	seen := map[int64]bool{}
	for _, row := range result.Data {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, err := mteamInteger(row.ID, true)
		if err != nil || *id <= 0 || seen[*id] {
			return nil, ErrResponse
		}
		seen[*id] = true
		size, err := mteamInteger(row.Size, true)
		if err != nil {
			return nil, err
		}
		if row.Name == "" || len(row.Name) > 4096 || len(row.Description) > 64<<10 || row.Status == nil {
			return nil, ErrResponse
		}
		seeders, err := mteamInteger(row.Status.Seeders, false)
		if err != nil {
			return nil, err
		}
		peers, err := mteamInteger(row.Status.Leechers, false)
		if err != nil {
			return nil, err
		}
		u := *front
		u.Path = "/detail/" + strconv.FormatInt(*id, 10)
		resource := externalindexer.Resource{IndexerID: definition.ID, Indexer: definition.Name, Title: row.Name, Description: row.Description, PageURL: u.String(), DownloadURL: u.String(), DownloadResolver: "mteam", Size: *size, Seeders: seeders, Peers: peers}
		if row.Episode != nil {
			resource.IMDbID = row.Episode.IMDb
			if resource.IMDbID != "" && !imdbPattern.MatchString(resource.IMDbID) {
				return nil, ErrResponse
			}
		}
		promotions := map[string][2]float64{"NORMAL": {1, 1}, "PERCENT_50": {1, 0.5}, "PERCENT_70": {1, 0.7}, "FREE": {1, 0}, "_2X_FREE": {2, 0}, "_2X": {2, 1}, "_2X_PERCENT_50": {2, 0.5}}
		if factors, ok := promotions[row.Status.Discount]; ok {
			up, down := factors[0], factors[1]
			free := down == 0
			resource.UploadFactor = &up
			resource.DownloadFactor = &down
			resource.Freeleech = &free
		}
		resources = append(resources, resource)
	}
	return resources, nil
}

func DownloadMTeam(ctx context.Context, detail, key, ua string, transport http.RoundTripper) ([]byte, error) {
	u, err := url.Parse(detail)
	if err != nil {
		return nil, ErrConfig
	}
	domain := *u
	domain.Path = "/"
	_, api, err := mteamBase(domain.String())
	if err != nil {
		return nil, err
	}
	id := strings.TrimPrefix(u.Path, "/detail/")
	number, err := strconv.ParseInt(id, 10, 64)
	if err != nil || number <= 0 || strconv.FormatInt(number, 10) != id {
		return nil, ErrConfig
	}
	data, err := mteamRequest(ctx, api, "/api/torrent/genDlToken", key, ua, "application/x-www-form-urlencoded", []byte(url.Values{"id": {id}}.Encode()), transport)
	if err != nil {
		return nil, err
	}
	var link string
	if json.Unmarshal(data, &link) != nil || !externalindexer.ValidDownloadURL(link) {
		return nil, ErrResponse
	}
	download, err := url.Parse(link)
	if err != nil || !sameOrigin(api, download) {
		return nil, ErrResponse
	}
	request, err := http.NewRequestWithContext(ctx, "GET", link, nil)
	if err != nil {
		return nil, ErrResponse
	}
	// The signed URL carries its own token. Do not forward API credentials.
	request.Header.Set("User-Agent", ua)
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrResponse
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, ErrResponse
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(contents) > 8<<20 {
		return nil, ErrResponse
	}
	if _, err := torrentmeta.Parse(contents); err != nil {
		return nil, ErrResponse
	}
	return contents, nil
}
