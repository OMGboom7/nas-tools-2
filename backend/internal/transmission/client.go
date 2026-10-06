package transmission

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrConfiguration  = errors.New("invalid Transmission configuration")
	ErrAuthentication = errors.New("Transmission authentication failed")
	ErrConnection     = errors.New("Transmission connection failed")
	ErrResponse       = errors.New("invalid Transmission response")
)

type Client struct {
	endpoint           *url.URL
	http               *http.Client
	username, password string
}

func New(host, port, username, password string, transport http.RoundTripper) (*Client, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, ErrConfiguration
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	endpoint, err := url.Parse(host)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, ErrConfiguration
	}
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return nil, ErrConfiguration
		}
		endpoint.Host = net.JoinHostPort(endpoint.Hostname(), port)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/transmission/rpc"
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{endpoint: endpoint, username: username, password: password, http: &http.Client{
		Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (client *Client) call(ctx context.Context, payload []byte, sessionID string) ([]byte, int, http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, ErrConfiguration
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", client.endpoint.Scheme+"://"+client.endpoint.Host)
	if sessionID != "" {
		request.Header.Set("X-Transmission-Session-Id", sessionID)
	}
	if client.username != "" || client.password != "" {
		request.SetBasicAuth(client.username, client.password)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, 0, nil, ErrConnection
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return nil, response.StatusCode, response.Header, ErrAuthentication
	}
	const limit = 8 << 20
	contents, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if readErr != nil || len(contents) > limit {
		return nil, response.StatusCode, response.Header, ErrResponse
	}
	return contents, response.StatusCode, response.Header, nil
}

func (client *Client) exchange(ctx context.Context, legacyPayload, modernPayload []byte) ([]byte, bool, error) {
	contents, status, headers, err := client.call(ctx, legacyPayload, "")
	if err != nil {
		return nil, false, err
	}
	modern := false
	if status == http.StatusConflict {
		sessionID := strings.TrimSpace(headers.Get("X-Transmission-Session-Id"))
		if sessionID == "" || len(sessionID) > 4096 {
			return nil, false, ErrResponse
		}
		modern = modernRPC(headers)
		payload := legacyPayload
		if modern {
			payload = modernPayload
		}
		contents, status, _, err = client.call(ctx, payload, sessionID)
		if err != nil {
			return nil, modern, err
		}
	}
	if status != http.StatusOK {
		return nil, modern, ErrResponse
	}
	return contents, modern, nil
}

func modernRPC(header http.Header) bool {
	version := strings.TrimSpace(header.Get("X-Transmission-Rpc-Version"))
	major, _, _ := strings.Cut(version, ".")
	value, _ := strconv.Atoi(major)
	return value >= 6
}

// Check negotiates the CSRF session token, then performs only session-get.
// It does not change Transmission's session settings.
func (client *Client) Check(ctx context.Context) error {
	contents, modern, err := client.exchange(ctx,
		[]byte(`{"method":"session-get","arguments":{"fields":["version"]}}`),
		[]byte(`{"jsonrpc":"2.0","method":"session_get","params":{"fields":["version"]},"id":1}`))
	if err != nil {
		return err
	}
	if modern {
		var response struct {
			JSONRPC string         `json:"jsonrpc"`
			Result  map[string]any `json:"result"`
			Error   any            `json:"error"`
		}
		if json.Unmarshal(contents, &response) != nil || response.JSONRPC != "2.0" || response.Error != nil || strings.TrimSpace(textValue(response.Result["version"])) == "" {
			return ErrResponse
		}
		return nil
	}
	var response struct {
		Result    string         `json:"result"`
		Arguments map[string]any `json:"arguments"`
	}
	if json.Unmarshal(contents, &response) != nil || response.Result != "success" || strings.TrimSpace(textValue(response.Arguments["version"])) == "" {
		return ErrResponse
	}
	return nil
}

type Task struct {
	Hash, Name                 string
	Progress                   float64
	DownloadSpeed, UploadSpeed int64
	ETA                        int64
	Status                     int
}

func (client *Client) Tasks(ctx context.Context) ([]Task, error) {
	legacy := []byte(`{"method":"torrent-get","arguments":{"fields":["hashString","name","percentDone","rateDownload","rateUpload","eta","status"]}}`)
	modern := []byte(`{"jsonrpc":"2.0","method":"torrent_get","params":{"fields":["hash_string","name","percent_done","rate_download","rate_upload","eta","status"]},"id":1}`)
	contents, modernProtocol, err := client.exchange(ctx, legacy, modern)
	if err != nil {
		return nil, err
	}
	tasks, err := parseTasks(contents, modernProtocol)
	if err != nil {
		return nil, err
	}
	result := make([]Task, 0, len(tasks))
	for _, task := range tasks {
		if !ValidHash(task.Hash) || strings.TrimSpace(task.Name) == "" || math.IsNaN(task.Progress) || math.IsInf(task.Progress, 0) || task.Progress < 0 || task.Progress > 1 || task.DownloadSpeed < 0 || task.UploadSpeed < 0 || task.Status < 0 || task.Status > 6 {
			return nil, ErrResponse
		}
		// Show incomplete torrents, including stopped and verification states, so
		// users can resume them from the downloads page.
		if task.Progress < 1 && task.Status <= 4 {
			result = append(result, task)
		}
	}
	return result, nil
}

func parseTasks(contents []byte, modern bool) ([]Task, error) {
	if modern {
		var response struct {
			JSONRPC string `json:"jsonrpc"`
			Result  struct {
				Torrents []struct {
					Hash          string  `json:"hash_string"`
					Name          string  `json:"name"`
					Progress      float64 `json:"percent_done"`
					DownloadSpeed int64   `json:"rate_download"`
					UploadSpeed   int64   `json:"rate_upload"`
					ETA           int64   `json:"eta"`
					Status        int     `json:"status"`
				} `json:"torrents"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(contents, &response) != nil || response.JSONRPC != "2.0" || hasJSONError(response.Error) || response.Result.Torrents == nil {
			return nil, ErrResponse
		}
		result := make([]Task, 0, len(response.Result.Torrents))
		for _, task := range response.Result.Torrents {
			result = append(result, Task{Hash: task.Hash, Name: task.Name, Progress: task.Progress, DownloadSpeed: task.DownloadSpeed, UploadSpeed: task.UploadSpeed, ETA: task.ETA, Status: task.Status})
		}
		return result, nil
	}
	var response struct {
		Result    string `json:"result"`
		Arguments struct {
			Torrents []struct {
				Hash          string  `json:"hashString"`
				Name          string  `json:"name"`
				Progress      float64 `json:"percentDone"`
				DownloadSpeed int64   `json:"rateDownload"`
				UploadSpeed   int64   `json:"rateUpload"`
				ETA           int64   `json:"eta"`
				Status        int     `json:"status"`
			} `json:"torrents"`
		} `json:"arguments"`
	}
	if json.Unmarshal(contents, &response) != nil || response.Result != "success" || response.Arguments.Torrents == nil {
		return nil, ErrResponse
	}
	result := make([]Task, 0, len(response.Arguments.Torrents))
	for _, task := range response.Arguments.Torrents {
		result = append(result, Task{Hash: task.Hash, Name: task.Name, Progress: task.Progress, DownloadSpeed: task.DownloadSpeed, UploadSpeed: task.UploadSpeed, ETA: task.ETA, Status: task.Status})
	}
	return result, nil
}

func hasJSONError(value json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(value))
	return trimmed != "" && trimmed != "null"
}

func ValidHash(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			lower := character | 0x20
			if lower < 'a' || lower > 'f' {
				return false
			}
		}
	}
	return true
}

func (client *Client) Control(ctx context.Context, hash, action string) error {
	if !ValidHash(hash) {
		return ErrConfiguration
	}
	legacyMethod, modernMethod := "", ""
	legacyArguments := map[string]any{"ids": []string{hash}}
	modernParams := map[string]any{"ids": []string{hash}}
	switch action {
	case "start":
		legacyMethod, modernMethod = "torrent-start", "torrent_start"
	case "stop":
		legacyMethod, modernMethod = "torrent-stop", "torrent_stop"
	case "remove":
		legacyMethod, modernMethod = "torrent-remove", "torrent_remove"
		legacyArguments["delete-local-data"] = true
		modernParams["delete_local_data"] = true
	default:
		return ErrConfiguration
	}
	legacy, _ := json.Marshal(map[string]any{"method": legacyMethod, "arguments": legacyArguments})
	modern, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": modernMethod, "params": modernParams, "id": 1})
	contents, modernProtocol, err := client.exchange(ctx, legacy, modern)
	if err != nil {
		return err
	}
	if modernProtocol {
		var response struct {
			JSONRPC string          `json:"jsonrpc"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if json.Unmarshal(contents, &response) != nil || response.JSONRPC != "2.0" || hasJSONError(response.Error) || len(response.Result) == 0 {
			return ErrResponse
		}
		return nil
	}
	var response struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(contents, &response) != nil || response.Result != "success" {
		return ErrResponse
	}
	return nil
}

func (client *Client) AddMagnet(ctx context.Context, magnet string) (string, error) {
	return client.AddMagnetWithOptions(ctx, magnet, AddOptions{})
}

type AddOptions struct {
	DownloadDir string
	Paused      bool
	Labels      []string
}

func (client *Client) AddMagnetWithOptions(ctx context.Context, magnet string, options AddOptions) (string, error) {
	if !strings.HasPrefix(magnet, "magnet:?") || len(magnet) > 4096 {
		return "", ErrConfiguration
	}
	return client.addTorrent(ctx, "filename", magnet, options)
}

func (client *Client) AddTorrent(ctx context.Context, torrent []byte) (string, error) {
	return client.AddTorrentWithOptions(ctx, torrent, AddOptions{})
}

func (client *Client) AddTorrentWithOptions(ctx context.Context, torrent []byte, options AddOptions) (string, error) {
	if len(torrent) == 0 || len(torrent) > 8<<20 {
		return "", ErrConfiguration
	}
	return client.addTorrent(ctx, "metainfo", base64.StdEncoding.EncodeToString(torrent), options)
}

func (client *Client) addTorrent(ctx context.Context, key, value string, options AddOptions) (string, error) {
	if len(options.DownloadDir) > 4096 || len(options.Labels) > 32 {
		return "", ErrConfiguration
	}
	legacyArgs, modernArgs := map[string]any{key: value}, map[string]any{key: value}
	if options.DownloadDir != "" {
		legacyArgs["download-dir"] = options.DownloadDir
		modernArgs["download_dir"] = options.DownloadDir
	}
	if options.Paused {
		legacyArgs["paused"], modernArgs["paused"] = true, true
	}
	if len(options.Labels) > 0 {
		labels := make([]string, 0, len(options.Labels))
		for _, label := range options.Labels {
			label = strings.TrimSpace(label)
			if label == "" {
				continue
			}
			if len(label) > 128 || strings.ContainsAny(label, "\r\n") {
				return "", ErrConfiguration
			}
			labels = append(labels, label)
		}
		if len(labels) > 0 {
			legacyArgs["labels"], modernArgs["labels"] = labels, labels
		}
	}
	legacy, _ := json.Marshal(map[string]any{"method": "torrent-add", "arguments": legacyArgs})
	modern, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "torrent_add", "params": modernArgs, "id": 1})
	contents, modernProtocol, err := client.exchange(ctx, legacy, modern)
	if err != nil {
		return "", err
	}
	if modernProtocol {
		var response struct {
			JSONRPC string `json:"jsonrpc"`
			Result  struct {
				Added struct {
					Hash string `json:"hash_string"`
				} `json:"torrent_added"`
				Duplicate struct {
					Hash string `json:"hash_string"`
				} `json:"torrent_duplicate"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(contents, &response) != nil || response.JSONRPC != "2.0" || hasJSONError(response.Error) {
			return "", ErrResponse
		}
		hash := response.Result.Added.Hash
		if hash == "" {
			hash = response.Result.Duplicate.Hash
		}
		if !ValidHash(hash) {
			return "", ErrResponse
		}
		return hash, nil
	}
	var response struct {
		Result    string `json:"result"`
		Arguments struct {
			Added struct {
				Hash string `json:"hashString"`
			} `json:"torrent-added"`
			Duplicate struct {
				Hash string `json:"hashString"`
			} `json:"torrent-duplicate"`
		} `json:"arguments"`
	}
	if json.Unmarshal(contents, &response) != nil || response.Result != "success" {
		return "", ErrResponse
	}
	hash := response.Arguments.Added.Hash
	if hash == "" {
		hash = response.Arguments.Duplicate.Hash
	}
	if !ValidHash(hash) {
		return "", ErrResponse
	}
	return hash, nil
}

func (client *Client) Address() string {
	return client.endpoint.Scheme + "://" + client.endpoint.Host + strings.TrimSuffix(client.endpoint.Path, "/transmission/rpc")
}

func textValue(value any) string {
	text, _ := value.(string)
	return text
}
