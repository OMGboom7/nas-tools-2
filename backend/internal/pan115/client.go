package pan115

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrConfiguration  = errors.New("invalid 115 configuration")
	ErrConnection     = errors.New("115 connection failed")
	ErrAuthentication = errors.New("115 authentication failed")
	ErrResponse       = errors.New("invalid 115 response")
)

type Client struct {
	cookie string
	http   *http.Client
}

func New(cookie string, transport http.RoundTripper) (*Client, error) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" || len(cookie) > 16<<10 || strings.ContainsAny(cookie, "\r\n") {
		return nil, ErrConfiguration
	}
	return &Client{cookie: cookie, http: &http.Client{
		Transport:     transport,
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Check mirrors the legacy account-space probe without returning the cookie,
// account details, or an upstream error body to callers.
func (client *Client) Check(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://webapi.115.com/files/index_info", nil)
	if err != nil {
		return ErrConfiguration
	}
	request.Header.Set("Cookie", client.cookie)
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return ErrConnection
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrAuthentication
	}
	if response.StatusCode != http.StatusOK {
		return ErrResponse
	}
	const limit = 1 << 20
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(contents) > limit {
		return ErrResponse
	}
	var payload struct {
		State json.RawMessage `json:"state"`
		Data  json.RawMessage `json:"data"`
	}
	if json.Unmarshal(contents, &payload) != nil || len(payload.Data) == 0 || string(payload.Data) == "null" {
		return ErrResponse
	}
	switch strings.TrimSpace(string(payload.State)) {
	case "true", "1", `"1"`:
		return nil
	case "false", "0", `"0"`:
		return ErrAuthentication
	default:
		return ErrResponse
	}
}

type Task struct {
	Hash     string
	Name     string
	Progress float64
}

func (client *Client) Tasks(ctx context.Context) ([]Task, error) {
	result := make([]Task, 0)
	seen := map[string]bool{}
	for page := 1; page <= 100; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://115.com/web/lixian/?ct=lixian&ac=task_lists", strings.NewReader(url.Values{"page": {strconv.Itoa(page)}}.Encode()))
		if err != nil {
			return nil, ErrConfiguration
		}
		request.Header.Set("Cookie", client.cookie)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Accept", "application/json")
		response, err := client.http.Do(request)
		if err != nil {
			return nil, ErrConnection
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			response.Body.Close()
			return nil, ErrAuthentication
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, ErrResponse
		}
		const limit = 1 << 20
		contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		response.Body.Close()
		if err != nil || len(contents) > limit {
			return nil, ErrResponse
		}
		var payload struct {
			State     json.RawMessage `json:"state"`
			Count     int             `json:"count"`
			PageCount int             `json:"page_count"`
			Tasks     []struct {
				Hash     string          `json:"info_hash"`
				Name     string          `json:"name"`
				Status   int             `json:"status"`
				Progress json.RawMessage `json:"percentDone"`
			} `json:"tasks"`
		}
		if json.Unmarshal(contents, &payload) != nil {
			return nil, ErrResponse
		}
		if !successfulState(payload.State) {
			return nil, ErrAuthentication
		}
		if payload.Count < 0 || payload.PageCount < 0 || payload.PageCount > 100 || payload.PageCount > 0 && page > payload.PageCount {
			return nil, ErrResponse
		}
		if payload.Count == 0 {
			return result, nil
		}
		if payload.PageCount == 0 || payload.Tasks == nil {
			return nil, ErrResponse
		}
		for _, raw := range payload.Tasks {
			if raw.Status != 0 && raw.Status != 1 {
				continue
			}
			if !validHash(raw.Hash) || strings.TrimSpace(raw.Name) == "" {
				return nil, ErrResponse
			}
			progress := 0.0
			if len(raw.Progress) > 0 && string(raw.Progress) != "null" {
				text := strings.Trim(string(raw.Progress), `"`)
				progress, err = strconv.ParseFloat(text, 64)
				if err != nil || math.IsNaN(progress) || math.IsInf(progress, 0) || progress < 0 || progress > 100 {
					return nil, ErrResponse
				}
			}
			if !seen[raw.Hash] {
				seen[raw.Hash] = true
				result = append(result, Task{Hash: raw.Hash, Name: strings.TrimSpace(raw.Name), Progress: progress})
			}
		}
		if page >= payload.PageCount {
			return result, nil
		}
	}
	return nil, ErrResponse
}

// Delete removes one offline task. The legacy 115 API does not accept a
// delete-files option, so this operation makes no claim about cloud files.
func (client *Client) Delete(ctx context.Context, hash string) error {
	if !validHash(hash) {
		return ErrConfiguration
	}
	form := url.Values{"hash[0]": {hash}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://115.com/web/lixian/?ct=lixian&ac=task_del", strings.NewReader(form.Encode()))
	if err != nil {
		return ErrConfiguration
	}
	request.Header.Set("Cookie", client.cookie)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return ErrConnection
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrAuthentication
	}
	if response.StatusCode != http.StatusOK {
		return ErrResponse
	}
	const limit = 1 << 20
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(contents) > limit {
		return ErrResponse
	}
	var payload struct {
		State json.RawMessage `json:"state"`
	}
	if json.Unmarshal(contents, &payload) != nil {
		return ErrResponse
	}
	if !successfulState(payload.State) {
		return ErrResponse
	}
	return nil
}

// AddMagnet submits a magnet to the root directory, matching the legacy
// client's behavior when no download directory is selected.
func (client *Client) AddMagnet(ctx context.Context, magnet string) (string, error) {
	if len(magnet) < 20 || len(magnet) > 4096 || !strings.HasPrefix(magnet, "magnet:?") || strings.ContainsAny(magnet, "\r\n") {
		return "", ErrConfiguration
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://webapi.115.com/files/getid?path=%2F", nil)
	if err != nil {
		return "", ErrConfiguration
	}
	request.Header.Set("Cookie", client.cookie)
	request.Header.Set("Accept", "application/json")
	var directory struct {
		State json.RawMessage `json:"state"`
		ID    json.RawMessage `json:"id"`
	}
	if err := client.readJSON(request, &directory); err != nil {
		return "", err
	}
	if !successfulState(directory.State) {
		return "", ErrResponse
	}
	id := strings.Trim(string(directory.ID), `"`)
	if id == "" || len(id) > 20 {
		return "", ErrResponse
	}
	for _, digit := range id {
		if digit < '0' || digit > '9' {
			return "", ErrResponse
		}
	}
	form := url.Values{"url[0]": {magnet}, "savepath": {""}, "wp_path_id": {id}}
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, "https://115.com/web/lixian/?ct=lixian&ac=add_task_urls", strings.NewReader(form.Encode()))
	if err != nil {
		return "", ErrConfiguration
	}
	request.Header.Set("Cookie", client.cookie)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	var added struct {
		State  json.RawMessage `json:"state"`
		Result []struct {
			Hash string `json:"info_hash"`
		} `json:"result"`
	}
	if err := client.readJSON(request, &added); err != nil {
		return "", err
	}
	if !successfulState(added.State) || len(added.Result) != 1 || !validHash(added.Result[0].Hash) {
		return "", ErrResponse
	}
	return strings.ToLower(added.Result[0].Hash), nil
}

func (client *Client) readJSON(request *http.Request, target any) error {
	response, err := client.http.Do(request)
	if err != nil {
		return ErrConnection
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrAuthentication
	}
	if response.StatusCode != http.StatusOK {
		return ErrResponse
	}
	const limit = 1 << 20
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(contents) > limit || json.Unmarshal(contents, target) != nil {
		return ErrResponse
	}
	return nil
}

func successfulState(state json.RawMessage) bool {
	switch strings.TrimSpace(string(state)) {
	case "true", "1", `"1"`:
		return true
	default:
		return false
	}
}

func validHash(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F' {
			continue
		}
		return false
	}
	return true
}
