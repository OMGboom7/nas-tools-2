package cloudflare

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type installTransport func(*http.Request) (*http.Response, error)

func (f installTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func cachedFixture(t *testing.T, root string) (string, []byte) {
	t.Helper()
	file, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	name := "CloudflareST_" + runtime.GOOS + "_" + arch
	var buffer bytes.Buffer
	files := map[string][]byte{"CloudflareST": binary, "ip.txt": []byte("192.0.2.0/24\n")}
	if runtime.GOOS == "darwin" {
		name += ".zip"
		writer := zip.NewWriter(&buffer)
		for key, data := range files {
			f, err := writer.Create(key)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if err = writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		name += ".tar.gz"
		compressed := gzip.NewWriter(&buffer)
		writer := tar.NewWriter(compressed)
		for key, data := range files {
			if err = writer.WriteHeader(&tar.Header{Name: key, Size: int64(len(data)), Mode: 0700}); err != nil {
				t.Fatal(err)
			}
			if _, err = writer.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if err = writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err = compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	data := buffer.Bytes()
	if err = os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
		t.Fatal(err)
	}
	return name, data
}
func TestLocalArchiveWorksWithBlockedMetadataOrAssetDownloads(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(map[bool]string{false: "metadata-unavailable", true: "asset-download-unavailable"}[metadata], func(t *testing.T) {
			root := t.TempDir()
			name, data := cachedFixture(t, root)
			keep := []byte("preserve original plugin data")
			if err := os.WriteFile(filepath.Join(root, "keep.txt"), keep, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			transport := installTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if _, ok := r.Context().Deadline(); !ok {
					t.Error("installer lost default timeout")
				}
				if !metadata {
					return nil, ErrResult
				}
				if r.URL.Host != "api.github.com" {
					t.Errorf("cached archive attempted asset network request %s", r.URL)
					return nil, ErrResult
				}
				payload, _ := json.Marshal(release{Version: "v2.3.5", Assets: []releaseAsset{{Name: name, URL: "https://github.com/XIU2/CloudflareSpeedTest/releases/download/v2.3.5/" + name, Digest: "sha256:" + sum(data)}}})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(payload)), Header: http.Header{}}, nil
			})
			ops := []string{}
			in, err := (Installer{Root: root, Client: &http.Client{Transport: transport}}).Ensure(context.Background(), Settings{IPv4: true, Version: "v2.3.5"}, func(_ context.Context, op Operation) error { ops = append(ops, op.Kind); return nil })
			if err != nil || in.Version != "v2.3.5" || calls != 1 || !strings.Contains(strings.Join(ops, ","), "local_archive,install") {
				t.Fatalf("installation=%+v requests=%d ops=%v error=%v", in, calls, ops, err)
			}
			retained, _ := os.ReadFile(filepath.Join(root, "keep.txt"))
			cached, _ := os.ReadFile(filepath.Join(root, name))
			if !bytes.Equal(retained, keep) || !bytes.Equal(cached, data) {
				t.Fatal("offline install removed original plugin data")
			}
		})
	}
}
