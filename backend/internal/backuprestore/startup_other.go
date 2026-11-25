//go:build !windows

package backuprestore

import "errors"

func portableStartupLock(_ Workspace, exclusive bool) (func(), error) {
	if exclusive {
		return nil, errors.ErrUnsupported
	}
	return func() {}, nil
}
