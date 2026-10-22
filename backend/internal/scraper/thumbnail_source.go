package scraper

import (
	"context"
	"errors"
)

var ErrThumbnail = errors.New("video thumbnail unavailable; no media was changed")

// ThumbnailSource is an in-process readonly capability, not an API URL. The
// organization layer lends it only for the lifetime of one verified job read.
type ThumbnailSource interface {
	Size() int64
	Current(context.Context) error
	ReadAt(context.Context, []byte, int64) (int, error)
}
