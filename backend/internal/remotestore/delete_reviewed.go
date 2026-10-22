package remotestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/s3utils"
)

// A private request capability, never a client-wide deletion switch. Existing
// intake/publication clients remain unable to delete objects or buckets.
type minioDeleteKey struct{}
type minioDeletePermit struct {
	key, etag string
	endpoint  url.URL
}

func (p minioDeletePermit) matches(r *http.Request) bool {
	if r.Method != http.MethodDelete || r.URL == nil || r.URL.RawQuery != "" || r.URL.User != nil || p.etag == "" || r.URL.Scheme != p.endpoint.Scheme {
		return false
	}
	bucket, object, ok := strings.Cut(p.key, "/")
	if !ok || bucket == "" || object == "" {
		return false
	}
	if strings.EqualFold(r.URL.Host, p.endpoint.Host) && r.URL.Path == "/"+p.key {
		return true
	}
	if strings.EqualFold(r.URL.Host, bucket+"."+p.endpoint.Host) && r.URL.Path == "/"+object {
		return true
	}
	// The existing SDK can resolve an AWS bucket to its regional endpoint.
	// Preserve that SDK-supported routing, but never change bucket, object,
	// port, TLS scheme or AWS partition under this single-request capability.
	if !s3utils.IsAmazonEndpoint(p.endpoint) || r.URL.Port() != p.endpoint.Port() {
		return false
	}
	u := *r.URL
	if r.URL.Path == "/"+object && strings.HasPrefix(strings.ToLower(u.Hostname()), bucket+".") {
		u.Host = strings.TrimPrefix(strings.ToLower(u.Host), bucket+".")
	} else if r.URL.Path != "/"+p.key {
		return false
	}
	if minioDeletePartition(u) != minioDeletePartition(p.endpoint) || (s3utils.IsAmazonFIPSEndpoint(p.endpoint) && !s3utils.IsAmazonFIPSEndpoint(u)) {
		return false
	}
	return s3utils.IsAmazonEndpoint(u)
}
func minioDeletePartition(u url.URL) string {
	if strings.HasSuffix(u.Hostname(), ".amazonaws.com.cn") {
		return "aws-cn"
	}
	if strings.HasPrefix(s3utils.GetRegionFromURL(u), "us-gov-") || s3utils.IsAmazonGovCloudEndpoint(u) {
		return "aws-us-gov"
	}
	return "aws"
}

// DeleteReviewed removes one independently reviewed leaf, never a prefix,
// bucket, version history, retention policy or recursively discovered tree.
// There is no portable compare-and-delete guarantee: exclusive writers and a
// durable caller intent are required, including on Rclone. Never auto-retry.
func (c *Client) DeleteReviewed(ctx context.Context, location string, expected Node, digest string) error {
	if err := c.Current(); err != nil {
		return err
	}
	n, err := c.Entry(ctx, path.Dir(location), path.Base(location))
	if err != nil {
		return err
	}
	if n == nil || n.IsDir != expected.IsDir {
		return ErrChanged
	}
	if expected.IsDir {
		nodes, err := c.List(ctx, location, false)
		if err != nil {
			return err
		}
		if len(nodes) != 0 {
			return ErrChanged
		}
	} else {
		if expected.Size < 0 || expected.Size > 1<<50 || len(digest) != 64 {
			return ErrLimit
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return ErrChanged
		}
		current := *n
		current.Path, current.Name = expected.Path, expected.Name
		if current != expected {
			return ErrChanged
		}
		h := sha256.New()
		counter := &deleteHashCounter{writer: h}
		if err := c.Read(ctx, location, expected.Size, counter); err != nil {
			return err
		}
		if counter.bytes != expected.Size || hex.EncodeToString(h.Sum(nil)) != digest {
			return ErrChanged
		}
		after, err := c.Entry(ctx, path.Dir(location), path.Base(location))
		if err != nil || after == nil {
			return ErrChanged
		}
		after.Path, after.Name = expected.Path, expected.Name
		if *after != expected {
			return ErrChanged
		}
	}
	if c.s3 == nil {
		a, err := address(location)
		if err != nil {
			return err
		}
		verb := "deletefile"
		if expected.IsDir {
			verb = "rmdir"
		}
		return c.command(ctx, []string{verb, a}, nil, io.Discard, 1<<20)
	}
	bucket, object, err := minioLocation(location)
	if err != nil || object == "" {
		return ErrUnavailable
	}
	etag := expected.ID
	if expected.IsDir {
		object = strings.TrimSuffix(object, "/") + "/"
		info, err := c.s3.client.StatObject(ctx, bucket, object, minio.StatObjectOptions{})
		if err != nil {
			return minioError(ctx, err)
		}
		if info.Size != 0 {
			return ErrChanged
		}
		etag = info.ETag
	}
	if etag == "" {
		return ErrChanged
	}
	endpoint := c.s3.client.EndpointURL()
	if endpoint == nil {
		return ErrUnavailable
	}
	call := context.WithValue(ctx, minioDeleteKey{}, minioDeletePermit{key: bucket + "/" + object, etag: etag, endpoint: *endpoint})
	// No ForceDelete, GovernanceBypass or VersionID. Versioned stores may
	// retain old versions; this does not promise their space has been reclaimed.
	if err := c.s3.client.RemoveObject(call, bucket, object, minio.RemoveObjectOptions{}); err != nil {
		return minioError(ctx, err)
	}
	return c.Current()
}

type deleteHashCounter struct {
	writer io.Writer
	bytes  int64
}

func (w *deleteHashCounter) Write(b []byte) (int, error) {
	n, err := w.writer.Write(b)
	w.bytes += int64(n)
	return n, err
}
