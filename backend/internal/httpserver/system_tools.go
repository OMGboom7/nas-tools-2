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

	"github.com/0xforee/nas-tools/backend/internal/config"
)

type systemToolsAPI struct {
	config    *config.Store
	transport http.RoundTripper
}

func (api systemToolsAPI) networkTest(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid network test request")
		return
	}
	target := strings.TrimSpace(request.Form.Get("url"))
	if target == "image.tmdb.org" {
		target += "/t/p/w500/wwemzKWzjKYJFfCeiB57q3r4Bcm.png"
	}
	if target == "qyapi.weixin.qq.com" {
		target += "/cgi-bin/message/send"
	}
	if target == "" || len(target) > 2048 || strings.ContainsAny(target, "\r\n\t ") || strings.Contains(target, "://") {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid network target")
		return
	}
	parsed, err := url.Parse("https://" + target)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Scheme != "https" {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid network target")
		return
	}
	transport := api.transport
	if api.config != nil {
		configuration, err := api.config.Snapshot()
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "network configuration is unavailable")
			return
		}
		host := strings.ToLower(parsed.Hostname())
		if strings.Contains(host, "themoviedb") || strings.Contains(host, "telegram") || strings.Contains(host, "fanart") || strings.Contains(host, "tmdb") {
			proxies := objectValue(objectValue(configuration["app"])["proxies"])
			if text(proxies["https"]) != "" {
				transport, err = siteProxyTransport(transport, proxies, "https")
				if err != nil {
					writeAPIError(response, http.StatusBadRequest, 400, "network proxy is invalid")
					return
				}
			}
		}
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid network target")
		return
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	result, err := client.Do(probe)
	connected := err == nil && result != nil && result.StatusCode >= 200 && result.StatusCode < 400
	if result != nil {
		_ = result.Body.Close()
	}
	writeJSON(response, http.StatusOK, map[string]any{"res": connected, "time": strconv.FormatInt(time.Since(start).Milliseconds(), 10) + " 毫秒"})
}

func (api systemToolsAPI) version(response http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 12*time.Second)
	defer cancel()
	transport := api.transport
	if api.config != nil {
		configuration, err := api.config.Snapshot()
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "version configuration is unavailable")
			return
		}
		proxies := objectValue(objectValue(configuration["app"])["proxies"])
		if text(proxies["https"]) != "" {
			transport, err = siteProxyTransport(transport, proxies, "https")
			if err != nil {
				writeAPIError(response, http.StatusBadRequest, 400, "version proxy is invalid")
				return
			}
		}
	}
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	release, err := githubVersionJSON(ctx, client, "https://api.github.com/repos/0xforee/nas-tools/releases/latest")
	if err != nil {
		writeJSON(response, http.StatusOK, map[string]any{"code": -1, "version": "", "url": ""})
		return
	}
	commit, err := githubVersionJSON(ctx, client, "https://api.github.com/repos/0xforee/nas-tools/commits/master")
	if err != nil || text(commit["sha"]) == "" {
		writeJSON(response, http.StatusOK, map[string]any{"code": -1, "version": "", "url": ""})
		return
	}
	version := strings.TrimSpace(text(release["tag_name"]))
	rawURL := strings.TrimSpace(text(release["html_url"]))
	parsed, err := url.Parse(rawURL)
	if version == "" || len(version) > 120 || err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || len(rawURL) > 2048 {
		writeJSON(response, http.StatusOK, map[string]any{"code": -1, "version": "", "url": ""})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "version": version, "url": rawURL})
}

func githubVersionJSON(ctx context.Context, client *http.Client, address string) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "nas-tools-go")
	result, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK {
		return nil, errors.New("GitHub API returned non-success status")
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("GitHub API response too large")
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}
