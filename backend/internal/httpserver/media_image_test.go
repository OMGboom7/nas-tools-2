package httpserver

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestMediaImageProxyServesSignedSource(t *testing.T) {
	t.Parallel()

	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "http://media.local/poster.png" {
			t.Fatalf("image URL = %q", request.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(png)),
			Request:    request,
		}, nil
	})
	proxy, err := newMediaImageProxy(transport)
	if err != nil {
		t.Fatalf("newMediaImageProxy() error = %v", err)
	}

	path := proxy.rewrite("img?url=http%3A%2F%2Fmedia.local%2Fposter.png")
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	proxy.serveHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	if !bytes.Equal(response.Body.Bytes(), png) {
		t.Fatal("proxied image body does not match upstream")
	}
}

func TestMediaImageProxyRejectsInvalidSignature(t *testing.T) {
	t.Parallel()

	proxy, err := newMediaImageProxy(http.DefaultTransport)
	if err != nil {
		t.Fatalf("newMediaImageProxy() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/image?source=aHR0cDovL2V4YW1wbGUuY29t&signature=invalid", nil)
	response := httptest.NewRecorder()
	proxy.serveHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestMediaImageSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "direct", raw: "https://media.local/image.jpg", want: "https://media.local/image.jpg", ok: true},
		{name: "legacy proxy", raw: "img?url=http%3A%2F%2Fmedia.local%2Fimage.jpg", want: "http://media.local/image.jpg", ok: true},
		{name: "relative", raw: "/static/no-image.png", ok: false},
		{name: "unsupported scheme", raw: "file:///etc/passwd", ok: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := mediaImageSource(test.raw)
			if ok != test.ok || got != test.want {
				t.Fatalf("mediaImageSource(%q) = (%q, %v), want (%q, %v)", test.raw, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestProtectedMediaImageKeepsCredentialServerSide(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("emby:\n  host: https://media.example\n  api_key: image-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	proxy, err := newMediaImageProxy(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.String() != "https://media.example/Items/42/Images/Primary" || request.Header.Get("X-Emby-Token") != "image-secret" || strings.Contains(request.URL.String(), "image-secret") {
			t.Errorf("unexpected protected image request: %s", request.URL.String())
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(bytes.NewReader([]byte{0x89, 'P', 'N', 'G'})), Request: request}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	proxy.config = config.NewStore(path)
	imageURL := proxy.rewriteProtected("emby", "https://media.example/Items/42/Images/Primary")
	if imageURL == "" || strings.Contains(imageURL, "image-secret") {
		t.Fatalf("credential leaked in image URL: %q", imageURL)
	}
	response := httptest.NewRecorder()
	proxy.serveHTTP(response, httptest.NewRequest(http.MethodGet, imageURL, nil))
	if response.Code != http.StatusOK || requests != 1 {
		t.Fatalf("protected image status=%d requests=%d", response.Code, requests)
	}
	response = httptest.NewRecorder()
	proxy.serveHTTP(response, httptest.NewRequest(http.MethodGet, proxy.rewriteProtected("emby", "https://evil.example/collect"), nil))
	if response.Code != http.StatusForbidden || requests != 1 {
		t.Fatalf("cross-host protected image status=%d requests=%d", response.Code, requests)
	}
}
