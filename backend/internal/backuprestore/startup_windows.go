package backuprestore

import (
	"errors"
	"path/filepath"

	"github.com/0xforee/nas-tools/backend/internal/winlock"
)

// On Windows, offline restore remains unavailable. Hold a shared or exclusive
// byte-range lock during startup so separate processes cannot rotate credentials.
func portableStartupLock(w Workspace, exclusive bool) (func(), error) {
	release, err := winlock.Hold(filepath.Join(w.root, ".nastool-restore.lock"), exclusive)
	switch {
	case errors.Is(err, winlock.ErrBusy):
		return nil, ErrBusy
	case errors.Is(err, winlock.ErrFile):
		return nil, ErrState
	default:
		return release, err
	}
}
