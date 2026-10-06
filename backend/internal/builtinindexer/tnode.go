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
	"unicode"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

func tnodeResponse(ctx context.Context, client *http.Client, request *http.Request) ([]byte, error) {
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
	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 || !utf8.Valid(body) {
		return nil, ErrResponse
	}
	return body, nil
}

func SearchTNode(ctx context.Context, definition indexercatalog.Definition, cookie, ua, keyword string, page, limit int, transport http.RoundTripper) ([]externalindexer.Resource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if definition.Parser != "TNodeSpider" || definition.ID == "" || cookie == "" || len(cookie) > 64<<10 || len(ua) > 4096 || strings.ContainsAny(cookie+ua, "\r\n") || keyword == "" || len(keyword) > 1024 || !utf8.ValidString(keyword) || strings.ContainsFunc(keyword, unicode.IsControl) || page < 0 || page > 10000 || limit < 1 || limit > 100 {
		return nil, ErrConfig
	}
	base, err := url.Parse(definition.Domain)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.Port() != "" || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, ErrConfig
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, "GET", base.String(), nil)
	if err != nil {
		return nil, ErrConfig
	}
	request.Header.Set("Cookie", cookie)
	request.Header.Set("User-Agent", ua)
	body, err := tnodeResponse(ctx, client, request)
	if err != nil {
		return nil, err
	}
	doc, err := ParseDocument(ctx, body)
	if err != nil {
		return nil, err
	}
	blocked, err := trackerLoginPage(ctx, doc)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, ErrResponse
	}
	metas, err := Select(ctx, doc.Root, `meta[name="x-csrf-token"]`)
	if err != nil {
		return nil, err
	}
	if len(metas) != 1 {
		return nil, ErrResponse
	}
	csrf := ""
	for _, attribute := range metas[0].Attr {
		if attribute.Key == "content" {
			csrf = attribute.Val
		}
	}
	if strings.TrimSpace(csrf) == "" || len(csrf) > 2048 || strings.ContainsFunc(csrf, unicode.IsControl) {
		return nil, ErrResponse
	}
	params := map[string]any{"page": page + 1, "size": limit, "type": "title", "keyword": keyword, "sorter": "id", "order": "desc", "tags": []int{}, "category": []int{501, 502, 503, 504}, "medium": []int{}, "videoCoding": []int{}, "audioCoding": []int{}, "resolution": []int{}, "group": []int{}}
	payload, _ := json.Marshal(params)
	endpoint := *base
	endpoint.Path = "/api/torrent/advancedSearch"
	request, err = http.NewRequestWithContext(ctx, "POST", endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, ErrConfig
	}
	request.Header.Set("Cookie", cookie)
	request.Header.Set("User-Agent", ua)
	request.Header.Set("X-CSRF-TOKEN", csrf)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Accept", "application/json")
	body, err = tnodeResponse(ctx, client, request)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Status  json.RawMessage `json:"statusCode"`
		Success json.RawMessage `json:"success"`
		Data    *struct {
			Torrents []struct {
				ID          json.RawMessage `json:"id"`
				Title       string          `json:"title"`
				Description string          `json:"subtitle"`
				Size        json.RawMessage `json:"size"`
				Seeders     json.RawMessage `json:"seeding"`
				Peers       json.RawMessage `json:"leeching"`
				Download    json.RawMessage `json:"downloadRate"`
				Upload      json.RawMessage `json:"uploadRate"`
				IMDb        string          `json:"imdb"`
			} `json:"torrents"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Data == nil || envelope.Data.Torrents == nil || len(envelope.Data.Torrents) > limit {
		return nil, ErrResponse
	}
	if len(envelope.Code) != 0 {
		code := strings.TrimSpace(string(envelope.Code))
		if code != "0" && code != `"0"` {
			return nil, ErrResponse
		}
	}
	if len(envelope.Status) != 0 && strings.TrimSpace(string(envelope.Status)) != "200" {
		return nil, ErrResponse
	}
	if len(envelope.Success) != 0 && strings.TrimSpace(string(envelope.Success)) != "true" {
		return nil, ErrResponse
	}
	resources := []externalindexer.Resource{}
	seen := map[int64]bool{}
	for _, row := range envelope.Data.Torrents {
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
		seeders, err := mteamInteger(row.Seeders, false)
		if err != nil {
			return nil, err
		}
		peers, err := mteamInteger(row.Peers, false)
		if err != nil {
			return nil, err
		}
		down, err := tnodeFactor(row.Download)
		if err != nil {
			return nil, err
		}
		up, err := tnodeFactor(row.Upload)
		if err != nil {
			return nil, err
		}
		if row.Title == "" || len(row.Title) > 4096 || len(row.Description) > 64<<10 || (row.IMDb != "" && !imdbPattern.MatchString(row.IMDb)) {
			return nil, ErrResponse
		}
		download, detail := *base, *base
		download.Path = "/api/torrent/download/" + strconv.FormatInt(*id, 10)
		detail.Path = "/torrent/info/" + strconv.FormatInt(*id, 10)
		resource := externalindexer.Resource{IndexerID: definition.ID, Indexer: definition.Name, Title: row.Title, Description: row.Description, Size: *size, Seeders: seeders, Peers: peers, DownloadFactor: down, UploadFactor: up, IMDbID: row.IMDb, DownloadURL: download.String(), PageURL: detail.String()}
		if down != nil {
			free := *down == 0
			resource.Freeleech = &free
		}
		resources = append(resources, resource)
	}
	return resources, nil
}

func tnodeFactor(raw json.RawMessage) (*float64, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	value := string(raw)
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value = text
	}
	return resultFactor(value)
}

func trackerLoginPage(ctx context.Context, doc Document) (bool, error) {
	blocked, err := Select(ctx, doc.Root, `#challenge-form, .cf-turnstile`)
	if err != nil {
		return false, err
	}
	if len(blocked) > 0 {
		return true, nil
	}
	inputs, err := Select(ctx, doc.Root, "input")
	if err != nil {
		return false, err
	}
	for _, input := range inputs {
		for _, attribute := range input.Attr {
			if attribute.Key == "type" && strings.EqualFold(strings.TrimSpace(attribute.Val), "password") {
				return true, nil
			}
		}
	}
	return false, nil
}
