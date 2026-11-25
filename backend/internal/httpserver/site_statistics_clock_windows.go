package httpserver

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/winlock"
)

func holdVolumeReceiver(ctx context.Context, dbPath, key, windowsFile string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := filepath.EvalSymlinks(dbPath)
	if err != nil {
		return nil, err
	}
	release, err := winlock.Hold(filepath.Join(filepath.Dir(path), windowsFile), true)
	if errors.Is(err, winlock.ErrBusy) {
		return nil, organization.ErrBusy
	}
	return release, err
}
