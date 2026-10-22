//go:build !linux && !darwin

package scraper

import (
	"context"
	"os"
)

func RenderThumbnail(context.Context, *os.File) ([]byte, error) { return nil, ErrThumbnail }
func RenderRemoteThumbnail(context.Context, ThumbnailSource) ([]byte, error) {
	return nil, ErrThumbnail
}
