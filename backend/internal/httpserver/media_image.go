package httpserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

const maxMediaImageSize = 12 << 20

type mediaImageProxy struct {
	secret []byte
	client *http.Client
	config *config.Store
}

func (proxy *mediaImageProxy) rewriteProtected(kind, raw string) string {
	if proxy == nil || proxy.config == nil || kind != "emby" && kind != "jellyfin" && kind != "plex" {
		return ""
	}
	source, ok := mediaImageSource(raw)
	if !ok {
		return ""
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(source))
	query := url.Values{"source": {encoded}, "kind": {kind}, "signature": {proxy.sign(kind + ":" + encoded)}}
	return "/api/v1/dashboard/image?" + query.Encode()
}

func newMediaImageProxy(transport http.RoundTripper) (*mediaImageProxy, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("create media image signing key: %w", err)
	}

	return &mediaImageProxy{
		secret: secret,
		client: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return http.ErrUseLastResponse
				}
				if len(via) > 0 && !strings.EqualFold(request.URL.Hostname(), via[0].URL.Hostname()) {
					return fmt.Errorf("media image redirect changed host")
				}
				return nil
			},
		},
	}, nil
}

func (proxy *mediaImageProxy) rewrite(raw string) string {
	source, ok := mediaImageSource(raw)
	if !ok {
		return ""
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(source))
	query := url.Values{
		"source":    {encoded},
		"signature": {proxy.sign(encoded)},
	}
	return "/api/v1/dashboard/image?" + query.Encode()
}

func (proxy *mediaImageProxy) serveHTTP(response http.ResponseWriter, request *http.Request) {
	encoded := request.URL.Query().Get("source")
	kind := request.URL.Query().Get("kind")
	signature := request.URL.Query().Get("signature")
	signedValue := encoded
	if kind != "" {
		signedValue = kind + ":" + encoded
	}
	if encoded == "" || !proxy.valid(signedValue, signature) {
		http.Error(response, "invalid image signature", http.StatusForbidden)
		return
	}

	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		http.Error(response, "invalid image source", http.StatusBadRequest)
		return
	}
	source, err := url.Parse(string(decoded))
	if err != nil || (source.Scheme != "http" && source.Scheme != "https") || source.Host == "" {
		http.Error(response, "invalid image source", http.StatusBadRequest)
		return
	}

	etag := `"` + signature + `"`
	if request.Header.Get("If-None-Match") == etag {
		response.WriteHeader(http.StatusNotModified)
		return
	}

	upstreamRequest, err := http.NewRequestWithContext(request.Context(), http.MethodGet, source.String(), nil)
	if err != nil {
		http.Error(response, "invalid image request", http.StatusBadRequest)
		return
	}
	if kind != "" {
		if proxy.config == nil || kind != "emby" && kind != "jellyfin" && kind != "plex" {
			http.Error(response, "invalid image source", http.StatusForbidden)
			return
		}
		snapshot, err := proxy.config.Snapshot()
		if err != nil {
			http.Error(response, "image source is unavailable", http.StatusBadGateway)
			return
		}
		configuration := objectValue(snapshot[kind])
		configuredHost := strings.TrimSpace(text(configuration["host"]))
		if !strings.Contains(configuredHost, "://") {
			configuredHost = "http://" + configuredHost
		}
		configured, err := url.Parse(configuredHost)
		if err != nil || !strings.EqualFold(source.Host, configured.Host) || source.Scheme != configured.Scheme {
			http.Error(response, "invalid image source", http.StatusForbidden)
			return
		}
		credential := text(configuration["api_key"])
		if kind == "plex" {
			credential = text(configuration["token"])
		}
		if credential == "" {
			http.Error(response, "image source is unavailable", http.StatusBadGateway)
			return
		}
		if kind == "plex" {
			upstreamRequest.Header.Set("X-Plex-Token", credential)
		} else {
			upstreamRequest.Header.Set("X-Emby-Token", credential)
		}
	}
	upstream, err := proxy.client.Do(upstreamRequest)
	if err != nil {
		http.Error(response, "image source is unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		http.Error(response, "image source is unavailable", http.StatusBadGateway)
		return
	}

	content, err := io.ReadAll(io.LimitReader(upstream.Body, maxMediaImageSize+1))
	if err != nil || len(content) > maxMediaImageSize {
		http.Error(response, "image is too large", http.StatusBadGateway)
		return
	}
	contentType := upstream.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		contentType = http.DetectContentType(content)
	}
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		http.Error(response, "source is not an image", http.StatusUnsupportedMediaType)
		return
	}

	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Cache-Control", "public, max-age=86400")
	response.Header().Set("ETag", etag)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(content)
}

func (proxy *mediaImageProxy) sign(value string) string {
	mac := hmac.New(sha256.New, proxy.secret)
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (proxy *mediaImageProxy) valid(value, signature string) bool {
	provided, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	expected, _ := hex.DecodeString(proxy.sign(value))
	return hmac.Equal(provided, expected)
}

func mediaImageSource(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	if (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
		return parsed.String(), true
	}

	target := parsed.Query().Get("url")
	parsedTarget, err := url.Parse(target)
	if err != nil || (parsedTarget.Scheme != "http" && parsedTarget.Scheme != "https") || parsedTarget.Host == "" {
		return "", false
	}
	return parsedTarget.String(), true
}
