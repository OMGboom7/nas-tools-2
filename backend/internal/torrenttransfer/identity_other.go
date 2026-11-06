//go:build !linux && !darwin

package torrenttransfer

import "os"

func fileIdentity(os.FileInfo) (string, error) { return "", ErrUnsupported }
