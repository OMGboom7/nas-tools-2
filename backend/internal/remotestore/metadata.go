package remotestore

import (
	"bytes"
	"context"
	"io"
	"path"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
)

// WriteMetadata is deliberately separate from absent-only Upload/Copy. Callers
// must durably bind the original and replacement bytes and preserve both before
// invoking this single, non-retried write under explicit exclusive-writer consent.
// Rclone has no portable destination CAS; MinIO additionally requires If-Match.
func (c *Client) WriteMetadata(ctx context.Context, location string, data []byte, previous *Node) error {
	if len(data) > 16<<20 || previous != nil && (previous.IsDir || previous.Size < 0 || previous.Size > 16<<20) {
		return ErrLimit
	}
	if previous == nil {
		n, err := c.Entry(ctx, path.Dir(location), path.Base(location))
		if err != nil {
			return err
		}
		if n != nil {
			return ErrChanged
		}
		return c.Upload(ctx, location, int64(len(data)), bytes.NewReader(data))
	}
	if c.s3 == nil {
		a, err := address(location)
		if err != nil {
			return err
		}
		return c.command(ctx, []string{"rcat", a, "--size", strconv.Itoa(len(data))}, bytes.NewReader(data), io.Discard, 1<<20)
	}
	if previous.ID == "" {
		return ErrChanged
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bucket, object, err := minioLocation(location)
	if err != nil || object == "" {
		return ErrUnavailable
	}
	opts := minio.PutObjectOptions{DisableMultipart: true, SendContentMd5: true, NumThreads: 1}
	opts.SetMatchETag(previous.ID)
	_, err = c.s3.client.PutObject(ctx, bucket, object, bytes.NewReader(data), int64(len(data)), opts)
	if err != nil {
		return minioError(ctx, err)
	}
	return c.Current()
}
