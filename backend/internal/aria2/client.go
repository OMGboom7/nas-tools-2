package aria2

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
	"path"
	"strconv"
	"strings"
	"time"
)

var (
	ErrConfiguration  = errors.New("invalid Aria2 configuration")
	ErrAuthentication = errors.New("Aria2 authentication failed")
	ErrConnection     = errors.New("Aria2 connection failed")
	ErrResponse       = errors.New("invalid Aria2 response")
)

type Client struct {
	endpoint *url.URL
	secret   string
	http     *http.Client
}

func New(host, port, secret string, transport http.RoundTripper) (*Client, error) {
	host = strings.TrimSpace(host)
	if host == "" || secret == "" {
		return nil, ErrConfiguration
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	endpoint, err := url.Parse(host)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, ErrConfiguration
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return nil, ErrConfiguration
	}
	endpoint.Host = net.JoinHostPort(endpoint.Hostname(), port)
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/jsonrpc"
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{endpoint: endpoint, secret: secret, http: &http.Client{
		Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (client *Client) call(ctx context.Context, id, method string, params []any, result any) error {
	parameters := make([]any, 0, len(params)+1)
	parameters = append(parameters, "token:"+client.secret)
	parameters = append(parameters, params...)
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": parameters})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return ErrConfiguration
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", client.endpoint.Scheme+"://"+client.endpoint.Host)
	response, err := client.http.Do(request)
	if err != nil {
		return ErrConnection
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return ErrAuthentication
	}
	if response.StatusCode != http.StatusOK {
		return ErrResponse
	}
	const limit = 8 << 20
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(contents) > limit {
		return ErrResponse
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      string          `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(contents, &envelope) != nil || envelope.JSONRPC != "2.0" || envelope.ID != id || envelope.Error != nil || len(envelope.Result) == 0 || string(envelope.Result) == "null" || json.Unmarshal(envelope.Result, result) != nil {
		return ErrResponse
	}
	return nil
}

// Check calls only aria2.getVersion using method-level token authorization.
func (client *Client) Check(ctx context.Context) error {
	var result struct {
		Version string `json:"version"`
	}
	if err := client.call(ctx, "nastool-check", "aria2.getVersion", nil, &result); err != nil {
		return err
	}
	if strings.TrimSpace(result.Version) == "" {
		return ErrResponse
	}
	return nil
}

type Task struct {
	GID, Name                  string
	Progress                   float64
	DownloadSpeed, UploadSpeed int64
	Status                     string
}

type rawTask struct {
	GID             string `json:"gid"`
	Status          string `json:"status"`
	TotalLength     string `json:"totalLength"`
	CompletedLength string `json:"completedLength"`
	DownloadSpeed   string `json:"downloadSpeed"`
	UploadSpeed     string `json:"uploadSpeed"`
	BitTorrent      *struct {
		Info *struct {
			Name string `json:"name"`
		} `json:"info"`
	} `json:"bittorrent"`
	Files []struct {
		Path string `json:"path"`
	} `json:"files"`
}

var taskKeys = []string{"gid", "status", "totalLength", "completedLength", "downloadSpeed", "uploadSpeed", "bittorrent", "files"}

func (client *Client) Tasks(ctx context.Context) ([]Task, error) {
	active := []rawTask{}
	if err := client.call(ctx, "nastool-active", "aria2.tellActive", []any{taskKeys}, &active); err != nil {
		return nil, err
	}
	waiting := []rawTask{}
	if err := client.call(ctx, "nastool-waiting", "aria2.tellWaiting", []any{-1, 100, taskKeys}, &waiting); err != nil {
		return nil, err
	}
	items := append(active, waiting...)
	result := make([]Task, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, exists := seen[item.GID]; exists {
			continue
		}
		task, include, err := normalizeTask(item)
		if err != nil {
			return nil, err
		}
		if include {
			seen[item.GID] = struct{}{}
			result = append(result, task)
		}
	}
	return result, nil
}

func normalizeTask(item rawTask) (Task, bool, error) {
	if !ValidGID(item.GID) {
		return Task{}, false, ErrResponse
	}
	switch item.Status {
	case "active", "waiting", "paused":
	case "complete", "removed":
		return Task{}, false, nil
	default:
		return Task{}, false, ErrResponse
	}
	total, err := strconv.ParseUint(item.TotalLength, 10, 64)
	if err != nil {
		return Task{}, false, ErrResponse
	}
	completed, err := strconv.ParseUint(item.CompletedLength, 10, 64)
	if err != nil || completed > total {
		return Task{}, false, ErrResponse
	}
	download, err := strconv.ParseInt(item.DownloadSpeed, 10, 64)
	if err != nil || download < 0 {
		return Task{}, false, ErrResponse
	}
	upload, err := strconv.ParseInt(item.UploadSpeed, 10, 64)
	if err != nil || upload < 0 {
		return Task{}, false, ErrResponse
	}
	progress := 0.0
	if total > 0 {
		progress = float64(completed) / float64(total)
	}
	if math.IsNaN(progress) || math.IsInf(progress, 0) || progress < 0 || progress > 1 {
		return Task{}, false, ErrResponse
	}
	name := ""
	if item.BitTorrent != nil && item.BitTorrent.Info != nil {
		name = strings.TrimSpace(item.BitTorrent.Info.Name)
	}
	if name == "" && len(item.Files) > 0 {
		name = strings.TrimSpace(path.Base(strings.ReplaceAll(item.Files[0].Path, "\\", "/")))
	}
	if name == "" || name == "." || name == "/" {
		name = item.GID
	}
	return Task{GID: item.GID, Name: name, Progress: progress, DownloadSpeed: download, UploadSpeed: upload, Status: item.Status}, true, nil
}

func ValidGID(value string) bool {
	if len(value) != 16 {
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

func (client *Client) Control(ctx context.Context, gid, action string) error {
	if !ValidGID(gid) {
		return ErrConfiguration
	}
	method := map[string]string{"start": "aria2.unpause", "stop": "aria2.pause", "remove": "aria2.remove"}[action]
	if method == "" {
		return ErrConfiguration
	}
	var result string
	if err := client.call(ctx, "nastool-control", method, []any{gid}, &result); err != nil {
		return err
	}
	if result != gid {
		return ErrResponse
	}
	return nil
}

func (client *Client) AddMagnet(ctx context.Context, magnet string) (string, error) {
	return client.AddMagnetWithOptions(ctx, magnet, AddOptions{})
}

type AddOptions struct {
	DownloadDir     string
	Paused          bool
	UploadLimitKB   int
	DownloadLimitKB int
}

func (client *Client) AddMagnetWithOptions(ctx context.Context, magnet string, options AddOptions) (string, error) {
	if !strings.HasPrefix(magnet, "magnet:?") || len(magnet) > 4096 {
		return "", ErrConfiguration
	}
	values, err := options.values()
	if err != nil {
		return "", err
	}
	var gid string
	if err := client.call(ctx, "nastool-add", "aria2.addUri", []any{[]string{magnet}, values}, &gid); err != nil {
		return "", err
	}
	if !ValidGID(gid) {
		return "", ErrResponse
	}
	return gid, nil
}

func (client *Client) AddTorrent(ctx context.Context, torrent []byte) (string, error) {
	return client.AddTorrentWithOptions(ctx, torrent, AddOptions{})
}

func (client *Client) AddTorrentWithOptions(ctx context.Context, torrent []byte, options AddOptions) (string, error) {
	if len(torrent) == 0 || len(torrent) > 8<<20 {
		return "", ErrConfiguration
	}
	values, err := options.values()
	if err != nil {
		return "", err
	}
	var gid string
	if err := client.call(ctx, "nastool-add-torrent", "aria2.addTorrent", []any{base64.StdEncoding.EncodeToString(torrent), []string{}, values}, &gid); err != nil {
		return "", err
	}
	if !ValidGID(gid) {
		return "", ErrResponse
	}
	return gid, nil
}

func (options AddOptions) values() (map[string]string, error) {
	if len(options.DownloadDir) > 4096 || options.UploadLimitKB < 0 || options.DownloadLimitKB < 0 || options.UploadLimitKB > 1<<30 || options.DownloadLimitKB > 1<<30 {
		return nil, ErrConfiguration
	}
	values := map[string]string{}
	if options.DownloadDir != "" {
		values["dir"] = options.DownloadDir
	}
	if options.Paused {
		values["pause"] = "true"
	}
	if options.UploadLimitKB > 0 {
		values["max-upload-limit"] = strconv.Itoa(options.UploadLimitKB * 1024)
	}
	if options.DownloadLimitKB > 0 {
		values["max-download-limit"] = strconv.Itoa(options.DownloadLimitKB * 1024)
	}
	return values, nil
}

func (client *Client) Address() string {
	return client.endpoint.Scheme + "://" + client.endpoint.Host + strings.TrimSuffix(client.endpoint.Path, "/jsonrpc")
}
