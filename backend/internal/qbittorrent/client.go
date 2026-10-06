package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrConfiguration  = errors.New("invalid qBittorrent configuration")
	ErrAuthentication = errors.New("qBittorrent authentication failed")
	ErrConnection     = errors.New("qBittorrent connection failed")
	ErrResponse       = errors.New("invalid qBittorrent response")
)

type Client struct {
	base               *url.URL
	http               *http.Client
	username, password string
}

// AddOptions applies per-torrent settings without changing client-wide defaults.
type AddOptions struct {
	SavePath         string
	Category         string
	Tags             []string
	Paused           bool
	UploadLimitKB    int
	DownloadLimitKB  int
	RatioLimit       float64
	SeedingTimeLimit int
}

func New(host, port, username, password string, transport http.RoundTripper) (*Client, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, ErrConfiguration
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	base, err := url.Parse(host)
	if err != nil || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Scheme != "http" && base.Scheme != "https" {
		return nil, ErrConfiguration
	}
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return nil, ErrConfiguration
		}
		base.Host = net.JoinHostPort(base.Hostname(), port)
	}
	base.Path = strings.TrimRight(base.Path, "/")
	jar, _ := cookiejar.New(nil)
	return &Client{base: base, username: username, password: password, http: &http.Client{
		Transport: transport, Jar: jar, Timeout: 15 * time.Second,
		// Never forward login credentials or session cookies across redirects.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (client *Client) request(ctx context.Context, method, path string, form url.Values) ([]byte, error) {
	endpoint := *client.base
	path, query, _ := strings.Cut(path, "?")
	endpoint.Path += "/api/v2/" + path
	endpoint.RawPath = ""
	endpoint.RawQuery = query
	body := ""
	if form != nil {
		body = form.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), strings.NewReader(body))
	if err != nil {
		return nil, ErrConfiguration
	}
	request.Header.Set("Origin", client.base.Scheme+"://"+client.base.Host)
	request.Header.Set("Referer", client.base.String()+"/")
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, ErrConnection
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return nil, ErrAuthentication
	}
	if response.StatusCode != http.StatusOK {
		return nil, ErrResponse
	}
	const limit = 8 << 20
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(contents) > limit {
		return nil, ErrResponse
	}
	return contents, nil
}

func (client *Client) login(ctx context.Context) error {
	contents, err := client.request(ctx, http.MethodPost, "auth/login", url.Values{"username": {client.username}, "password": {client.password}})
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(contents)) != "Ok." {
		return ErrAuthentication
	}
	return nil
}

// Check only authenticates and reads transfer status; it never changes client
// preferences, categories, or torrents as the legacy constructor did.
func (client *Client) Check(ctx context.Context) error {
	if err := client.login(ctx); err != nil {
		return err
	}
	contents, err := client.request(ctx, http.MethodGet, "transfer/info", nil)
	if err != nil {
		return err
	}
	var info struct {
		Download *int64 `json:"dl_info_speed"`
		Upload   *int64 `json:"up_info_speed"`
	}
	if json.Unmarshal(contents, &info) != nil || info.Download == nil || info.Upload == nil || *info.Download < 0 || *info.Upload < 0 {
		return ErrResponse
	}
	return nil
}

type Task struct {
	Hash          string  `json:"hash"`
	Name          string  `json:"name"`
	Progress      float64 `json:"progress"`
	DownloadSpeed int64   `json:"dlspeed"`
	UploadSpeed   int64   `json:"upspeed"`
	ETA           int64   `json:"eta"`
	State         string  `json:"state"`
}

func (client *Client) Tasks(ctx context.Context) ([]Task, error) {
	if err := client.login(ctx); err != nil {
		return nil, err
	}
	contents, err := client.request(ctx, http.MethodGet, "torrents/info?filter=downloading", nil)
	if err != nil {
		return nil, err
	}
	var tasks []Task
	if json.Unmarshal(contents, &tasks) != nil || tasks == nil {
		return nil, ErrResponse
	}
	for _, task := range tasks {
		if !ValidHash(task.Hash) || strings.TrimSpace(task.Name) == "" || math.IsNaN(task.Progress) || math.IsInf(task.Progress, 0) || task.Progress < 0 || task.Progress > 1 || task.DownloadSpeed < 0 || task.UploadSpeed < 0 || task.ETA < 0 {
			return nil, ErrResponse
		}
	}
	return tasks, nil
}

func ValidHash(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

func (client *Client) Control(ctx context.Context, hash, action string) error {
	if !ValidHash(hash) {
		return ErrConfiguration
	}
	path := ""
	form := url.Values{"hashes": {hash}}
	switch action {
	case "start":
		path = "torrents/resume"
	case "stop":
		path = "torrents/pause"
	case "remove":
		path = "torrents/delete"
		form.Set("deleteFiles", "true")
	default:
		return ErrConfiguration
	}
	if err := client.login(ctx); err != nil {
		return err
	}
	_, err := client.request(ctx, http.MethodPost, path, form)
	return err
}

// AddMagnet submits a magnet URI without changing the downloader's global settings.
func (client *Client) AddMagnet(ctx context.Context, magnet string) error {
	return client.AddMagnetWithOptions(ctx, magnet, AddOptions{})
}

func (client *Client) AddMagnetWithOptions(ctx context.Context, magnet string, options AddOptions) error {
	if !strings.HasPrefix(magnet, "magnet:?") || len(magnet) > 4096 {
		return ErrConfiguration
	}
	form, err := options.values()
	if err != nil {
		return err
	}
	form.Set("urls", magnet)
	if err := client.login(ctx); err != nil {
		return err
	}
	contents, err := client.request(ctx, http.MethodPost, "torrents/add", form)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(contents)) != "Ok." {
		return ErrResponse
	}
	return nil
}

func (client *Client) AddTorrent(ctx context.Context, torrent []byte) error {
	return client.AddTorrentWithOptions(ctx, torrent, AddOptions{})
}

func (client *Client) AddTorrentWithOptions(ctx context.Context, torrent []byte, options AddOptions) error {
	if len(torrent) == 0 || len(torrent) > 8<<20 {
		return ErrConfiguration
	}
	form, err := options.values()
	if err != nil {
		return err
	}
	if err := client.login(ctx); err != nil {
		return err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, values := range form {
		for _, value := range values {
			if err := writer.WriteField(key, value); err != nil {
				return ErrConfiguration
			}
		}
	}
	part, err := writer.CreateFormFile("torrents", "upload.torrent")
	if err != nil {
		return ErrConfiguration
	}
	if _, err := part.Write(torrent); err != nil {
		return ErrConfiguration
	}
	if err := writer.Close(); err != nil {
		return ErrConfiguration
	}
	endpoint := *client.base
	endpoint.Path += "/api/v2/torrents/add"
	endpoint.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), &body)
	if err != nil {
		return ErrConfiguration
	}
	request.Header.Set("Origin", client.base.Scheme+"://"+client.base.Host)
	request.Header.Set("Referer", client.base.String()+"/")
	request.Header.Set("Content-Type", writer.FormDataContentType())
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
	contents, err := io.ReadAll(io.LimitReader(response.Body, 1025))
	if err != nil || len(contents) > 1024 || strings.TrimSpace(string(contents)) != "Ok." {
		return ErrResponse
	}
	return nil
}

func (options AddOptions) values() (url.Values, error) {
	if len(options.SavePath) > 4096 || len(options.Category) > 256 || len(options.Tags) > 32 || options.UploadLimitKB < 0 || options.DownloadLimitKB < 0 || options.UploadLimitKB > 1<<30 || options.DownloadLimitKB > 1<<30 || options.RatioLimit < 0 || math.IsNaN(options.RatioLimit) || math.IsInf(options.RatioLimit, 0) || options.SeedingTimeLimit < 0 {
		return nil, ErrConfiguration
	}
	form := url.Values{}
	if options.SavePath != "" {
		form.Set("savepath", options.SavePath)
		form.Set("autoTMM", "false")
	}
	if options.Category != "" {
		form.Set("category", options.Category)
	}
	if options.Paused {
		form.Set("paused", "true")
	}
	if options.UploadLimitKB > 0 {
		form.Set("upLimit", strconv.Itoa(options.UploadLimitKB*1024))
	}
	if options.DownloadLimitKB > 0 {
		form.Set("dlLimit", strconv.Itoa(options.DownloadLimitKB*1024))
	}
	if options.RatioLimit > 0 {
		form.Set("ratioLimit", strconv.FormatFloat(options.RatioLimit, 'f', -1, 64))
	}
	if options.SeedingTimeLimit > 0 {
		form.Set("seedingTimeLimit", strconv.Itoa(options.SeedingTimeLimit))
	}
	tags := make([]string, 0, len(options.Tags))
	for _, tag := range options.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if len(tag) > 128 || strings.ContainsAny(tag, ",\r\n") {
			return nil, ErrConfiguration
		}
		tags = append(tags, tag)
	}
	if len(tags) > 0 {
		form.Set("tags", strings.Join(tags, ","))
	}
	return form, nil
}

func (client *Client) Address() string {
	return client.base.Scheme + "://" + client.base.Host + client.base.EscapedPath()
}
