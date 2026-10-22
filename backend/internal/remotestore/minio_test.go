package remotestore

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/testutil/s3test"
)

func minioFixture(t *testing.T) (Factory, *s3test.Server) {
	t.Helper()
	s := s3test.New(t)
	f := Factory{Driver: "minio", ConfigFile: filepath.Join(t.TempDir(), "config.json"), CAFile: filepath.Join(t.TempDir(), "ca.pem"), Environment: []string{}}
	data, _ := json.Marshal(map[string]any{"version": "10", "aliases": map[string]any{"NASTOOL": map[string]string{"url": s.HTTP.URL, "accessKey": s3test.AccessKey, "secretKey": s3test.SecretKey, "api": "S3v4", "path": "on"}}})
	if err := os.WriteFile(f.ConfigFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.CAFile, s.CA, 0600); err != nil {
		t.Fatal(err)
	}
	return f, s
}
func TestMinioNativeProtocolRetainsFullContentUnicodeAndEmptyDirectories(t *testing.T) {
	f, s := minioFixture(t)
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Probe(t.Context(), "/bucket/library"); err != nil {
		t.Fatal(err)
	}
	if c.Binding().Driver != "minio" || !c.Binding().Directories || !f.Available() {
		t.Fatal(c.Binding())
	}
	data := []byte("full byte content\x00\xff")
	if err := c.Upload(t.Context(), "/bucket/library/中文 + 100%.mkv", int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := c.Read(t.Context(), "/bucket/library/中文 + 100%.mkv", int64(len(data)), &out); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatal(err, out.Bytes())
	}
	if err := c.Copy(t.Context(), "/bucket/library/中文 + 100%.mkv", "/bucket/library/nested/copy.mkv"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mkdir(t.Context(), "/bucket/library/disc/CERTIFICATE/empty"); err != nil {
		t.Fatal(err)
	}
	nodes, err := c.List(t.Context(), "/bucket/library", true)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, n := range nodes {
		found[n.Path] = true
	}
	if !found["disc/CERTIFICATE/empty"] || !found["nested/copy.mkv"] || !found["中文 + 100%.mkv"] {
		t.Fatal(nodes)
	}
	if data, ok := s.Get("bucket/library/disc/CERTIFICATE/empty/"); !ok || len(data) != 0 {
		t.Fatal("empty directory marker missing")
	}
	for _, recursive := range []bool{false, true} {
		children, err := c.List(t.Context(), "/bucket/library/disc/CERTIFICATE/empty", recursive)
		if err != nil || len(children) != 0 {
			t.Fatal("directory's own marker became an empty child name", children, err)
		}
	}
	s.Put("bucket/nonempty-marker/", []byte("not a directory marker"))
	if _, err := c.List(t.Context(), "/bucket/nonempty-marker", false); !errors.Is(err, ErrChanged) {
		t.Fatal("nonempty object hidden as a directory marker", err)
	}
	for _, r := range s.Requests() {
		if r.Method == "DELETE" {
			t.Fatal("unexpected cleanup", r)
		}
	}
	if err := c.Copy(t.Context(), "/bucket/library/中文 + 100%.mkv", "/bucket/library/nested/copy.mkv"); !errors.Is(err, ErrChanged) {
		t.Fatal("copy replaced a target", err)
	}
	if err := c.Upload(t.Context(), "/bucket/library/中文 + 100%.mkv", 1, bytes.NewReader([]byte("x"))); !errors.Is(err, ErrChanged) {
		t.Fatal("upload replaced a target", err)
	}
}
func TestMinioListingsPageCompletelyAndBoundRealDirectoryScopes(t *testing.T) {
	f, s := minioFixture(t)
	for n := range 1001 {
		s.Put(fmt.Sprintf("bucket/page/%04d.bin", n), []byte("x"))
	}
	for n := range 20010 {
		s.Put(fmt.Sprintf("bucket/tree/folder/%05d.bin", n), nil)
	}
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	nodes, err := c.List(t.Context(), "/bucket/page", false)
	if err != nil || len(nodes) != 1001 {
		t.Fatal(len(nodes), err)
	}
	nodes, err = c.List(t.Context(), "/bucket/tree", false)
	if err != nil || len(nodes) != 1 || nodes[0].Path != "folder" || !nodes[0].IsDir {
		t.Fatal("nonrecursive inventory expanded entire library", nodes, err)
	}
	if _, err := c.List(t.Context(), "/bucket/tree", true); !errors.Is(err, ErrLimit) {
		t.Fatal("unbounded recursive inventory", err)
	}
	s.Put("bucket/page/Case.mkv", nil)
	s.Put("bucket/page/case.mkv", nil)
	if _, err := c.List(t.Context(), "/bucket/page", false); !errors.Is(err, ErrChanged) {
		t.Fatal("ambiguous inventory", err)
	}
}
func TestMinioAccountAndTrustSnapshotsAreRevalidatedWithoutCredentialLeaks(t *testing.T) {
	for _, change := range []string{"account", "ca"} {
		t.Run(change, func(t *testing.T) {
			f, s := minioFixture(t)
			c, err := f.Open(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			b := c.Binding()
			if change == "account" {
				data, _ := os.ReadFile(f.ConfigFile)
				data = bytes.ReplaceAll(data, []byte(s3test.AccessKey), []byte("changed-access"))
				if err := os.WriteFile(f.ConfigFile, data, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(f.CAFile, append(s.CA, []byte("\n# trust change\n")...), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if f.Validate(b) == nil {
				t.Fatal("old binding retained")
			}
			before := len(s.Requests())
			if err := c.Upload(t.Context(), "/bucket/blocked", 1, bytes.NewReader([]byte("x"))); err == nil || strings.Contains(err.Error(), s3test.SecretKey) {
				t.Fatal(err)
			}
			if len(s.Requests()) != before {
				t.Fatal("changed account reached network")
			}
		})
	}
}
func TestMinioLostWriteResponseIsNeverRetriedOrAssumedSuccessful(t *testing.T) {
	for _, copying := range []bool{false, true} {
		t.Run(fmt.Sprint(copying), func(t *testing.T) {
			f, s := minioFixture(t)
			s.Put("bucket/source", []byte("payload"))
			c, err := f.Open(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			s.FailNextWrite("bucket/uncertain", copying, 500)
			if copying {
				err = c.Copy(t.Context(), "/bucket/source", "/bucket/uncertain")
			} else {
				err = c.Upload(t.Context(), "/bucket/uncertain", 7, bytes.NewReader([]byte("payload")))
			}
			if err == nil || strings.Contains(err.Error(), s3test.SecretKey) {
				t.Fatal(err)
			}
			calls := 0
			for _, r := range s.Requests() {
				if r.Method == "PUT" && r.Key == "bucket/uncertain" {
					calls++
				}
			}
			if calls != 1 {
				t.Fatal("write replay", calls)
			}
			if b, ok := s.Get("bucket/uncertain"); !ok || string(b) != "payload" {
				t.Fatal("uncertainty fixture lost stored content")
			}
		})
	}
}
func TestMinioMultipartUploadUsesOneShotPartsAndCompleteContent(t *testing.T) {
	f, s := minioFixture(t)
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data := bytes.Repeat([]byte("media!"), 4<<20)
	if err := c.Upload(t.Context(), "/bucket/multipart", int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Get("bucket/multipart"); !ok || !bytes.Equal(got, data) {
		t.Fatal("multipart bytes differ")
	}
	hash := sha256.New()
	if err := c.Read(t.Context(), "/bucket/multipart", int64(len(data)), hash); err != nil {
		t.Fatal("large streamed read", err)
	}
	expected := sha256.Sum256(data)
	if !bytes.Equal(hash.Sum(nil), expected[:]) {
		t.Fatal("large read content differs")
	}
	if err := c.Copy(t.Context(), "/bucket/multipart", "/bucket/large-copy"); err != nil {
		t.Fatal("large object HEAD/copy", err)
	}
	if got, ok := s.Get("bucket/large-copy"); !ok || !bytes.Equal(got, data) {
		t.Fatal("large copy content differs")
	}
	parts := map[string]bool{}
	for _, r := range s.Requests() {
		if strings.Contains(r.Query, "partNumber=") {
			if parts[r.Query] {
				t.Fatal("multipart part replay", r)
			}
			parts[r.Query] = true
		}
	}
	if len(parts) < 2 {
		t.Fatal("multipart protocol was not exercised", s.Requests())
	}
}
func TestMinioRejectsAmbiguousConfigurationAndDoesNotFallBackToRclone(t *testing.T) {
	f, _ := minioFixture(t)
	for _, raw := range []string{`{"version":"10","version":"10","aliases":{}}`, `{"version":"11","aliases":{}}`, `{"version":"10","aliases":{"NASTOOL":{"url":"https://user:secret@host"}}}`, `{"version":"10","aliases":{"NASTOOL":{"url":"https://host/path"}}}`} {
		if err := os.WriteFile(f.ConfigFile, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if f.Available() {
			t.Fatal("ambiguous account accepted", raw)
		}
	}
	f.Driver = "unknown"
	if f.Available() {
		t.Fatal("unknown provider used rclone")
	}
}

func TestMinioLegacyV2AndEnvironmentAliasRemainExplicitAndSealed(t *testing.T) {
	for _, kind := range []string{"v2", "environment", "v9"} {
		t.Run(kind, func(t *testing.T) {
			f, s := minioFixture(t)
			var cfg map[string]any
			data, _ := os.ReadFile(f.ConfigFile)
			if json.Unmarshal(data, &cfg) != nil {
				t.Fatal("fixture")
			}
			aliases := cfg["aliases"].(map[string]any)
			alias := aliases["NASTOOL"].(map[string]any)
			if kind == "v2" {
				alias["api"] = "S3v2"
			} else if kind == "v9" {
				cfg["version"] = "9"
				cfg["hosts"] = cfg["aliases"]
				delete(cfg, "aliases")
			} else {
				f.Environment = []string{"MC_HOST_NASTOOL=" + strings.Replace(s.HTTP.URL, "https://", "https://"+s3test.AccessKey+":"+s3test.SecretKey+":fixture-session@", 1)}
			}
			data, _ = json.Marshal(cfg)
			if err := os.WriteFile(f.ConfigFile, data, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := f.Open(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := c.Upload(t.Context(), "/bucket/legacy", 1, bytes.NewReader([]byte("x"))); err != nil {
				t.Fatal(err)
			}
			if kind == "v2" {
				found := false
				for _, r := range s.Requests() {
					found = found || strings.HasPrefix(r.Authorization, "AWS ")
				}
				if !found {
					t.Fatal("S3v2 mode was changed")
				}
			}
			if kind == "environment" {
				binding := c.Binding()
				f.Environment = []string{}
				if f.Validate(binding) == nil {
					t.Fatal("environment override not sealed")
				}
			}
		})
	}
}
