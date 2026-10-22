package remotestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/testutil/s3test"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestMinioReviewedDeleteCapabilityRetainsPathAndDNSAddressingWithoutExpandingAuthority(t *testing.T) {
	base, _ := url.Parse("https://storage.test:9000")
	p := minioDeletePermit{key: "bucket/library/payload", etag: "etag", endpoint: *base}
	for _, test := range []struct {
		target string
		want   bool
	}{
		{"https://storage.test:9000/bucket/library/payload", true}, {"https://bucket.storage.test:9000/library/payload", true},
		{"https://other.storage.test:9000/library/payload", false}, {"https://bucket.storage.test:9000/library/other", false},
		{"https://bucket.unrelated.test:9000/library/payload", false}, {"http://bucket.storage.test:9000/library/payload", false},
		{"https://bucket.storage.test:9000/library/payload?versionId=old", false}, {"https://storage.test:9000/bucket", false},
	} {
		r, _ := http.NewRequest(http.MethodDelete, test.target, nil)
		if got := p.matches(r); got != test.want {
			t.Fatal(test.target, got, test.want)
		}
	}
	aws, _ := url.Parse("https://s3.amazonaws.com")
	p.endpoint = *aws
	for _, target := range []string{"https://s3.us-west-2.amazonaws.com/bucket/library/payload", "https://bucket.s3.us-west-2.amazonaws.com/library/payload"} {
		r, _ := http.NewRequest(http.MethodDelete, target, nil)
		if !p.matches(r) {
			t.Fatal("existing regional SDK routing rejected", target)
		}
	}
	r, _ := http.NewRequest(http.MethodDelete, "https://bucket.s3.cn-north-1.amazonaws.com.cn/library/payload", nil)
	if p.matches(r) {
		t.Fatal("capability crossed AWS partition")
	}
	r, _ = http.NewRequest(http.MethodDelete, "https://bucket.s3.us-gov-west-1.amazonaws.com/library/payload", nil)
	if p.matches(r) {
		t.Fatal("capability crossed GovCloud partition")
	}
}

type reviewedDeleteTransport func(*http.Request) (*http.Response, error)

func (f reviewedDeleteTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestMinioReviewedDeleteUsesRealSDKDNSShapeAgainstAnIsolatedMappedEndpoint(t *testing.T) {
	f, s := minioFixture(t)
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fixtureURL, _ := url.Parse(s.HTTP.URL)
	wire := s.HTTP.Client().Transport
	mapped := reviewedDeleteTransport(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Host = fixtureURL.Host
		u.Path = "/bucket" + r.URL.Path
		u.RawPath = ""
		copy.URL = &u
		copy.Host = ""
		return wire.RoundTrip(copy)
	})
	sdk, err := minio.New("storage.test:9000", &minio.Options{Creds: credentials.NewStaticV4(s3test.AccessKey, s3test.SecretKey, ""), Secure: true, BucketLookup: minio.BucketLookupDNS, Transport: minioSealedTransport{c, mapped}, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	c.s3.client = sdk
	key := "bucket/library/.nastool-remote/" + strings.Repeat("a", 32) + "/payload"
	s.Put(key, []byte("old-data"))
	n, err := c.Entry(t.Context(), "/bucket/library", strings.TrimPrefix(key, "bucket/library/"))
	if err != nil || n == nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("old-data"))
	if err := c.DeleteReviewed(t.Context(), "/"+key, *n, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if _, yes := s.Get(key); yes {
		t.Fatal("DNS-shaped SDK delete did not reach exact object")
	}
	deletes := 0
	for _, request := range s.Requests() {
		if request.Method == "DELETE" {
			deletes++
			if request.Key != key || request.IfMatch == "" {
				t.Fatal(request)
			}
		}
	}
	if deletes != 1 {
		t.Fatal("unexpected delete count", deletes)
	}
}

func TestMinioReviewedDeleteIsSingleObjectCapabilityAndNeverOpensGenericDelete(t *testing.T) {
	f, s := minioFixture(t)
	c, err := f.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	key := "bucket/library/.nastool-remote/" + strings.Repeat("a", 32) + "/payload"
	location := "/" + key
	s.Put(key, []byte("old-data"))
	s.Put(key+"-other", []byte("preserve"))
	n, err := c.Entry(t.Context(), "/bucket/library", strings.TrimPrefix(key, "bucket/library/"))
	if err != nil || n == nil {
		t.Fatal(err)
	}
	if err := c.s3.client.RemoveObject(t.Context(), "bucket", strings.TrimPrefix(key, "bucket/"), minio.RemoveObjectOptions{}); err == nil {
		t.Fatal("generic SDK client acquired deletion authority")
	}
	sum := sha256.Sum256([]byte("old-data"))
	digest := hex.EncodeToString(sum[:])
	if err := c.DeleteReviewed(t.Context(), location, *n, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong full content accepted")
	}
	if _, yes := s.Get(key); !yes {
		t.Fatal("failed proof deleted object")
	}
	s.FailNextDelete(key, 500)
	before := len(s.Requests())
	if err := c.DeleteReviewed(t.Context(), location, *n, digest); err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal(err)
	}
	deletes := 0
	for _, request := range s.Requests()[before:] {
		if request.Method == "DELETE" {
			deletes++
			if request.Key != key || request.IfMatch == "" || request.Query != "" {
				t.Fatal("broadened delete", request)
			}
		}
	}
	if deletes != 1 {
		t.Fatal("delete automatically retried", deletes)
	}
	if data, _ := s.Get(key + "-other"); string(data) != "preserve" {
		t.Fatal("neighbor deleted")
	}
	before = len(s.Requests())
	if err := c.s3.client.RemoveObject(context.Background(), "bucket", strings.TrimPrefix(key+"-other", "bucket/"), minio.RemoveObjectOptions{}); err == nil {
		t.Fatal("capability leaked after return")
	}
	for _, request := range s.Requests()[before:] {
		if request.Method == "DELETE" {
			t.Fatal("unrelated delete reached wire")
		}
	}
}
