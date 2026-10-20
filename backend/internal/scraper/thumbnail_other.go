//go:build !linux && !darwin

package scraper

import (
	"context"
	"errors"
	"os"
)

var ErrThumbnail = errors.New("video thumbnail unavailable on this platform")

func RenderThumbnail(context.Context, *os.File) ([]byte, error) { return nil, ErrThumbnail }
