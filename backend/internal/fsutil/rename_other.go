//go:build !darwin && !linux

package fsutil

import "errors"

func RenameExclusive(from, to string) error { return errors.ErrUnsupported }
